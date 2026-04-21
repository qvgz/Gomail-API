package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/mail"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	gomail "github.com/wneessen/go-mail"
	gomailsmtp "github.com/wneessen/go-mail/smtp"
	"go.uber.org/zap"
)

const (
	MaxHeaderLength = 1024
	MaxBodyLength   = 1024 * 512
	PidFileName     = "/tmp/gomail-api.pid"
	SMTPTimeout     = 30 * time.Second
)

type SMTPConfig struct {
	Server     string `json:"server"`
	Port       int    `json:"port"`
	Email      string `json:"email"`
	Password   string `json:"password"`
	SkipVerify bool   `json:"skip_verify"`
}

type Config struct {
	PrimarySMTP       SMTPConfig          `json:"primary_smtp"`
	BackupSMTP        *SMTPConfig         `json:"backup_smtp"`
	RecipientGroups   map[string][]string `json:"recipient_groups"`
	RateLimitInterval int                 `json:"rate_limit_interval"`
	QueueCapacity     int                 `json:"queue_capacity"`
	WorkerCount       int                 `json:"worker_count"`
	HTTPPort          int                 `json:"http_port"`
}

type EmailTask struct {
	TargetGroup    string
	RecipientAddrs []*mail.Address
	Subject        string
	Content        string
}

type StatusResponse struct {
	Status    string      `json:"status"`
	Uptime    string      `json:"uptime"`
	BuildInfo string      `json:"build_info"`
	Queue     QueueStats  `json:"queue"`
	System    SystemStats `json:"system"`
}

type QueueStats struct {
	Current  int    `json:"current"`
	Capacity int    `json:"capacity"`
	Usage    string `json:"usage"`
}

type SystemStats struct {
	Goroutines int `json:"goroutines"`
	PID        int `json:"pid"`
}

type App struct {
	cfg        atomic.Pointer[runtimeConfig]
	queue      chan EmailTask
	startTime  time.Time
	logger     *zap.Logger
	configPath string
	wg         sync.WaitGroup
}

type recipientGroup struct {
	parsed []*mail.Address
}

type runtimeConfig struct {
	raw       *Config
	groups    map[string]recipientGroup
	rateLimit time.Duration
}

type smtpSender struct {
	client *gomail.Client
	conn   *gomailsmtp.Client
	from   string
}

type workerSenders struct {
	cfg     *runtimeConfig
	logger  *zap.Logger
	primary *smtpSender
	backup  *smtpSender
}

var Version = "dev"

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	checkMode := flag.Bool("check", false, "验证配置并测试连接")
	reloadMode := flag.Bool("reload", false, "向运行中的服务发送重载信号")
	versionMode := flag.Bool("version", false, "查看版本")
	flag.Parse()

	if *versionMode {
		fmt.Println(Version)
		os.Exit(0)
	}

	if *reloadMode {
		if err := performReload(); err != nil {
			fmt.Fprintf(os.Stderr, "重载失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("重载信号已发送。")
		os.Exit(0)
	}

	cfg, err := loadAndValidateConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置加载失败: %v\n", err)
		os.Exit(1)
	}

	if *checkMode {
		runDeepCheck(cfg)
		return
	}

	app := newApp(cfg, *configPath)
	if err := app.run(); err != nil {
		app.logger.Fatal("server exited with error", zap.Error(err))
	}
}

func newApp(cfg *runtimeConfig, configPath string) *App {
	loggerCfg := zap.NewProductionConfig()
	loggerCfg.OutputPaths = []string{"stdout"}
	loggerCfg.ErrorOutputPaths = []string{"stderr"}
	logger, err := loggerCfg.Build()
	if err != nil {
		panic(err)
	}

	qCap := cfg.raw.QueueCapacity
	if qCap <= 0 {
		qCap = 1000
	}

	app := &App{
		queue:      make(chan EmailTask, qCap),
		startTime:  time.Now(),
		logger:     logger,
		configPath: configPath,
	}
	app.cfg.Store(cfg)
	return app
}

func (a *App) run() error {
	defer a.logger.Sync()

	currentPID := os.Getpid()
	if err := os.WriteFile(PidFileName, []byte(strconv.Itoa(currentPID)), 0644); err != nil {
		return fmt.Errorf("write PID file: %w", err)
	}
	defer os.Remove(PidFileName)

	cfg := a.cfg.Load()
	workerNum := cfg.raw.WorkerCount
	if workerNum <= 0 {
		workerNum = runtime.NumCPU() * 2
	}

	port := cfg.raw.HTTPPort
	if port <= 0 {
		port = 8080
	}

	a.logger.Info("starting gomail api",
		zap.String("version", Version),
		zap.Int("workers", workerNum),
		zap.Int("pid", currentPID),
		zap.Int("port", port),
		zap.Int("queue_capacity", cap(a.queue)),
	)

	for i := 0; i < workerNum; i++ {
		a.wg.Add(1)
		go a.emailWorker(i)
	}

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      a.routes(),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	errChan := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- err
		}
		close(errChan)
	}()

	cleanupWorkers := func() {
		close(a.queue)
		a.wg.Wait()
	}

	for {
		select {
		case err := <-errChan:
			cleanupWorkers()
			return err
		case sig := <-stopChan:
			switch sig {
			case syscall.SIGHUP:
				a.reloadConfig()
			default:
				a.logger.Info("shutting down", zap.String("signal", sig.String()))
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				shutdownErr := server.Shutdown(ctx)
				cancel()
				cleanupWorkers()
				return shutdownErr
			}
		}
	}
}

func (a *App) routes() http.Handler {
	r := chi.NewRouter()

	r.HandleFunc("/healthz", a.handleHealthz)
	r.Get("/status", a.handleStatus)
	r.Post("/", a.handleSendMail)
	r.Post("/*", a.handleSendMail)

	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	return r
}

func (a *App) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok"))
}

func (a *App) handleStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	qLen, qCap := len(a.queue), cap(a.queue)
	resp := StatusResponse{
		Status:    "ok",
		Uptime:    time.Since(a.startTime).String(),
		BuildInfo: Version,
		Queue: QueueStats{
			Current:  qLen,
			Capacity: qCap,
			Usage:    fmt.Sprintf("%.1f%%", float64(qLen)/float64(qCap)*100),
		},
		System: SystemStats{
			Goroutines: runtime.NumGoroutine(),
			PID:        os.Getpid(),
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (a *App) handleSendMail(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(MaxBodyLength))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Request too large or invalid", http.StatusBadRequest)
		return
	}

	target := r.FormValue("target_group")
	subject := r.FormValue("subject")
	content := r.FormValue("content")

	if target == "" || subject == "" || content == "" {
		http.Error(w, "Missing params", http.StatusBadRequest)
		return
	}
	if len(subject) > MaxHeaderLength || len(target) > MaxHeaderLength {
		http.Error(w, "Parameter too long", http.StatusBadRequest)
		return
	}

	cfg := a.cfg.Load()
	if cfg == nil {
		http.Error(w, "Service not ready", http.StatusServiceUnavailable)
		return
	}
	group, ok := cfg.groups[target]
	if !ok || len(group.parsed) == 0 {
		http.Error(w, "Group not found or empty", http.StatusNotFound)
		return
	}

	task := EmailTask{
		TargetGroup:    target,
		RecipientAddrs: group.parsed,
		Subject:        sanitizeHeader(subject),
		Content:        content,
	}

	select {
	case a.queue <- task:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("queued"))
	default:
		http.Error(w, "Queue full", http.StatusServiceUnavailable)
	}
}

func (a *App) emailWorker(id int) {
	defer a.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			a.logger.Error("worker panicked", zap.Int("id", id), zap.Any("panic", r))
		}
	}()

	var senders *workerSenders
	var nextSendAt time.Time
	defer func() {
		if senders != nil {
			senders.close()
		}
	}()

	for task := range a.queue {
		cfg := a.cfg.Load()
		if cfg == nil {
			a.logger.Error("config not ready", zap.Int("worker", id))
			continue
		}

		if senders == nil || senders.cfg != cfg {
			if senders != nil {
				senders.close()
			}
			var err error
			senders, err = newWorkerSenders(cfg, a.logger)
			if err != nil {
				a.logger.Error("initialize SMTP sender failed", zap.Int("worker", id), zap.Error(err))
				continue
			}
			nextSendAt = time.Time{}
		}

		if cfg.rateLimit > 0 {
			now := time.Now()
			if wait := time.Until(nextSendAt); wait > 0 {
				time.Sleep(wait)
				now = time.Now()
			}
			nextSendAt = now.Add(cfg.rateLimit)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		err := senders.send(ctx, task)
		cancel()

		if err != nil {
			a.logger.Error("send failed",
				zap.Int("worker", id),
				zap.String("group", task.TargetGroup),
				zap.String("subject", task.Subject),
				zap.Error(err),
			)
		} else {
			a.logger.Info("send success",
				zap.Int("worker", id),
				zap.String("group", task.TargetGroup),
				zap.String("subject", task.Subject),
			)
		}
	}
}

func newWorkerSenders(cfg *runtimeConfig, logger *zap.Logger) (*workerSenders, error) {
	primary, err := newSMTPSender(cfg.raw.PrimarySMTP)
	if err != nil {
		return nil, fmt.Errorf("create primary smtp sender: %w", err)
	}

	ws := &workerSenders{
		cfg:     cfg,
		logger:  logger,
		primary: primary,
	}

	if cfg.raw.BackupSMTP != nil {
		backup, err := newSMTPSender(*cfg.raw.BackupSMTP)
		if err != nil {
			primary.close()
			return nil, fmt.Errorf("create backup smtp sender: %w", err)
		}
		ws.backup = backup
	}

	return ws, nil
}

func (w *workerSenders) close() {
	if w.primary != nil {
		w.primary.close()
	}
	if w.backup != nil {
		w.backup.close()
	}
}

func (w *workerSenders) send(ctx context.Context, task EmailTask) error {
	err := w.primary.send(ctx, task)
	if err != nil && w.backup != nil {
		w.logger.Warn("primary failed, trying backup", zap.Error(err), zap.String("group", task.TargetGroup))
		return w.backup.send(ctx, task)
	}
	return err
}

func newSMTPSender(sc SMTPConfig) (*smtpSender, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: sc.SkipVerify, ServerName: sc.Server}
	opts := []gomail.Option{
		gomail.WithPort(sc.Port),
		gomail.WithTimeout(SMTPTimeout),
		gomail.WithTLSConfig(tlsCfg),
	}

	switch sc.Port {
	case 465:
		opts = append(opts, gomail.WithSSL())
	case 587:
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	default:
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSOpportunistic))
	}

	if sc.Password != "" {
		opts = append(opts,
			gomail.WithSMTPAuth(gomail.SMTPAuthPlain),
			gomail.WithUsername(sc.Email),
			gomail.WithPassword(sc.Password),
		)
	}

	client, err := gomail.NewClient(sc.Server, opts...)
	if err != nil {
		return nil, err
	}

	return &smtpSender{client: client, from: sc.Email}, nil
}

func (s *smtpSender) send(ctx context.Context, task EmailTask) error {
	msg, err := buildMessage(s.from, task)
	if err != nil {
		return err
	}

	conn, err := s.ensureConn(ctx)
	if err != nil {
		return err
	}

	if err := s.client.SendWithSMTPClient(conn, msg); err != nil {
		_ = s.client.CloseWithSMTPClient(conn)
		s.conn = nil
		return err
	}
	return nil
}

func (s *smtpSender) ensureConn(ctx context.Context) (*gomailsmtp.Client, error) {
	if s.conn != nil && s.conn.HasConnection() {
		return s.conn, nil
	}
	conn, err := s.client.DialToSMTPClientWithContext(ctx)
	if err != nil {
		return nil, err
	}
	s.conn = conn
	return s.conn, nil
}

func (s *smtpSender) close() {
	if s.conn == nil {
		return
	}
	_ = s.client.CloseWithSMTPClient(s.conn)
	s.conn = nil
}

func buildMessage(from string, task EmailTask) (*gomail.Msg, error) {
	msg := gomail.NewMsg()
	if err := msg.From(from); err != nil {
		return nil, err
	}
	msg.ToMailAddress(task.RecipientAddrs...)
	msg.Subject(task.Subject)
	msg.SetBodyString(gomail.TypeTextPlain, normalizeBody(task.Content))
	return msg, nil
}

func normalizeBody(body string) string {
	if body == "" {
		return body
	}

	needsNormalize := false
	for i := 0; i < len(body); i++ {
		if body[i] == '\n' || body[i] == '\r' {
			needsNormalize = true
			break
		}
	}
	if !needsNormalize {
		return body
	}

	var b strings.Builder
	b.Grow(len(body) + strings.Count(body, "\n"))
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '\r':
			if i+1 < len(body) && body[i+1] == '\n' {
				i++
			}
			b.WriteString("\r\n")
		case '\n':
			b.WriteString("\r\n")
		default:
			b.WriteByte(body[i])
		}
	}

	return b.String()
}

func sanitizeHeader(v string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, v)
}

func (a *App) reloadConfig() {
	a.logger.Info("reloading config")
	nCfg, err := loadAndValidateConfig(a.configPath)
	if err != nil {
		a.logger.Error("reload failed", zap.Error(err))
		return
	}
	a.cfg.Store(nCfg)
	a.logger.Info("config reloaded")
}

func performReload() error {
	pidBytes, err := os.ReadFile(PidFileName)
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		return err
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGHUP)
}

func loadAndValidateConfig(path string) (*runtimeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal([]byte(os.ExpandEnv(string(data))), &cfg); err != nil {
		return nil, err
	}

	return buildRuntimeConfig(&cfg)
}

func buildRuntimeConfig(cfg *Config) (*runtimeConfig, error) {
	if cfg.PrimarySMTP.Server == "" {
		return nil, errors.New("missing primary_smtp.server")
	}
	if cfg.PrimarySMTP.Port <= 0 || cfg.PrimarySMTP.Port > 65535 {
		return nil, errors.New("invalid primary_smtp.port")
	}
	if cfg.PrimarySMTP.Email == "" {
		return nil, errors.New("missing primary_smtp.email")
	}

	if cfg.BackupSMTP != nil {
		if cfg.BackupSMTP.Server == "" {
			return nil, errors.New("missing backup_smtp.server")
		}
		if cfg.BackupSMTP.Port <= 0 || cfg.BackupSMTP.Port > 65535 {
			return nil, errors.New("invalid backup_smtp.port")
		}
		if cfg.BackupSMTP.Email == "" {
			return nil, errors.New("missing backup_smtp.email")
		}
	}

	if len(cfg.RecipientGroups) == 0 {
		return nil, errors.New("recipient_groups is empty")
	}

	groups := make(map[string]recipientGroup, len(cfg.RecipientGroups))
	for name, recipients := range cfg.RecipientGroups {
		if len(recipients) == 0 {
			return nil, fmt.Errorf("recipient_groups.%s is empty", name)
		}

		parsed := make([]*mail.Address, 0, len(recipients))
		for _, rcpt := range recipients {
			addr := strings.TrimSpace(rcpt)
			if addr == "" {
				return nil, fmt.Errorf("recipient_groups.%s has empty address", name)
			}

			parsedAddr, err := mail.ParseAddress(addr)
			if err != nil {
				return nil, fmt.Errorf("recipient_groups.%s has invalid address %q: %w", name, addr, err)
			}
			parsed = append(parsed, parsedAddr)
		}

		groups[name] = recipientGroup{parsed: parsed}
	}

	return &runtimeConfig{
		raw:       cfg,
		groups:    groups,
		rateLimit: time.Duration(cfg.RateLimitInterval) * time.Second,
	}, nil
}

func runDeepCheck(cfg *runtimeConfig) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	primary, err := newSMTPSender(cfg.raw.PrimarySMTP)
	if err != nil {
		fmt.Printf("[FAIL] %v\n", err)
		os.Exit(1)
	}
	defer primary.close()

	if _, err := primary.ensureConn(ctx); err != nil {
		fmt.Printf("[FAIL] %v\n", err)
		os.Exit(1)
	}

	if cfg.raw.BackupSMTP != nil {
		backup, err := newSMTPSender(*cfg.raw.BackupSMTP)
		if err != nil {
			fmt.Printf("[FAIL] backup setup: %v\n", err)
			os.Exit(1)
		}
		defer backup.close()

		if _, err := backup.ensureConn(ctx); err != nil {
			fmt.Printf("[FAIL] backup connect: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println("[PASS] SMTP connection successful")
}
