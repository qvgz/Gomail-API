package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"mime" // [修复] 处理邮件头编码
	"net"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// --- 常量定义 ---

const (
	MaxHeaderLength = 1024       // 限制单个 Header 长度
	MaxBodyLength   = 1024 * 512 // 限制邮件正文 512KB
	PidFileName     = "/tmp/gomail-api.pid"
	SMTPTimeout     = 30 * time.Second // SMTP 交互硬超时
)

// --- 数据结构 ---

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
	TargetGroup string
	Recipients  []string
	Subject     string
	Content     string
	ReceivedAt  time.Time
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

type atomicConfig struct {
	sync.RWMutex
	cfg *Config
}

func (ac *atomicConfig) Get() *Config {
	ac.RLock()
	defer ac.RUnlock()
	return ac.cfg
}

func (ac *atomicConfig) Set(c *Config) {
	ac.Lock()
	defer ac.Unlock()
	ac.cfg = c
}

// --- 全局变量 ---

var (
	globalConfig atomicConfig
	taskQueue    chan EmailTask
	startTime    time.Time
	wg           sync.WaitGroup
	logger       *slog.Logger
	Version      = "dev" // 编译时注入
)

// --- 初始化与入口 ---

func init() {
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)
}

func main() {
	startTime = time.Now()

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
		logger.Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	if *checkMode {
		runDeepCheck(cfg)
		return
	}

	startServer(cfg, *configPath)
}

// --- 服务逻辑 ---

func startServer(cfg *Config, configPath string) {
	currentPID := os.Getpid()
	if err := os.WriteFile(PidFileName, []byte(strconv.Itoa(currentPID)), 0644); err != nil {
		logger.Error("Failed to write PID", "error", err)
		os.Exit(1)
	}
	defer os.Remove(PidFileName)

	globalConfig.Set(cfg)

	qCap := cfg.QueueCapacity
	if qCap <= 0 {
		qCap = 1000
	}
	taskQueue = make(chan EmailTask, qCap)

	workerNum := cfg.WorkerCount
	if workerNum <= 0 {
		workerNum = runtime.NumCPU() * 2
	}

	// [新增] 端口处理逻辑
	port := cfg.HTTPPort
	if port <= 0 {
		port = 8080
	}

	logger.Info("Starting GoMail API", "version", Version, "workers", workerNum, "pid", currentPID, "port", port)

	for i := 0; i < workerNum; i++ {
		wg.Add(1)
		go emailWorker(i)
	}

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", port), // [修改] 使用配置端口
		Handler:      http.HandlerFunc(routeHandler),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server failed", "error", err)
		}
	}()

	for {
		sig := <-stopChan
		if sig == syscall.SIGHUP {
			logger.Info("Reloading config...")
			if nCfg, err := loadAndValidateConfig(configPath); err == nil {
				globalConfig.Set(nCfg)
				logger.Info("Config reloaded")
			} else {
				logger.Error("Reload failed", "error", err)
			}
		} else {
			logger.Info("Shutting down...", "signal", sig)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			server.Shutdown(ctx)
			cancel()
			close(taskQueue)
			wg.Wait()
			return
		}
	}
}

// --- 业务逻辑 ---

func routeHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Write([]byte("ok"))
		return
	}
	if r.URL.Path == "/status" && r.Method == http.MethodGet {
		handleStatus(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

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

	cfg := globalConfig.Get()
	rcpts, ok := cfg.RecipientGroups[target]
	if !ok || len(rcpts) == 0 {
		http.Error(w, "Group not found or empty", http.StatusNotFound)
		return
	}

	task := EmailTask{
		TargetGroup: target,
		Recipients:  rcpts,
		Subject:     sanitizeHeader(subject),
		Content:     content,
		ReceivedAt:  time.Now(),
	}

	select {
	case taskQueue <- task:
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "queued")
	default:
		http.Error(w, "Queue full", http.StatusServiceUnavailable)
	}
}

func emailWorker(id int) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Worker panicked", "id", id, "panic", r)
		}
	}()

	for task := range taskQueue {
		cfg := globalConfig.Get()
		if cfg.RateLimitInterval > 0 {
			time.Sleep(time.Duration(cfg.RateLimitInterval) * time.Second)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		err := executeSendMail(ctx, cfg, task)
		cancel()

		// [修改] 在日志中增加 subject 和 content 字段
		if err != nil {
			logger.Error("Send failed", "group", task.TargetGroup, "error", err, "subject", task.Subject, "content", task.Content)
		} else {
			logger.Info("Send success", "group", task.TargetGroup, "subject", task.Subject, "content", task.Content)
		}
	}
}

func executeSendMail(ctx context.Context, cfg *Config, task EmailTask) error {
	err := sendOne(ctx, cfg.PrimarySMTP, task.Recipients, task.Subject, task.Content)
	if err != nil && cfg.BackupSMTP != nil {
		logger.Warn("Backup trial", "error", err)
		return sendOne(ctx, *cfg.BackupSMTP, task.Recipients, task.Subject, task.Content)
	}
	return err
}

func sendOne(ctx context.Context, sc SMTPConfig, rcpts []string, sub, body string) error {
	addr := net.JoinHostPort(sc.Server, strconv.Itoa(sc.Port))
	dialer := &net.Dialer{Timeout: 10 * time.Second}

	var conn net.Conn
	var err error

	if sc.Port == 465 {
		tlsConfig := &tls.Config{InsecureSkipVerify: sc.SkipVerify, ServerName: sc.Server}
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	// [修复] 确保连接最终被关闭。如果 smtp.NewClient 成功，它会管理连接并在 c.Quit 时关闭。
	// 这里通过 defer conn.Close 保证在所有中间错误路径下连接都能释放。
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()

	conn.SetDeadline(time.Now().Add(SMTPTimeout))

	host, _, _ := net.SplitHostPort(addr)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	conn = nil // [修复] 将 conn 置空，避免 defer 重复操作（c.Quit 会关闭它）
	defer c.Quit()

	if sc.Port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			tlsConfig := &tls.Config{InsecureSkipVerify: sc.SkipVerify, ServerName: sc.Server}
			if err = c.StartTLS(tlsConfig); err != nil {
				return err
			}
		} else if sc.Port == 587 {
			// [修复] 强制加密安全性：587 端口如果不提供 STARTTLS 则拒绝继续，防止凭证明文外泄
			return errors.New("STARTTLS required but not supported by server")
		}
	}

	if sc.Password != "" {
		auth := smtp.PlainAuth("", sc.Email, sc.Password, sc.Server)
		if err = c.Auth(auth); err != nil {
			return err
		}
	}

	if err = c.Mail(sc.Email); err != nil {
		return err
	}
	for _, r := range rcpts {
		if err = c.Rcpt(r); err != nil {
			return err
		}
	}

	w, err := c.Data()
	if err != nil {
		return err
	}
	msg := buildMessage(sc.Email, rcpts, sub, body)
	if _, err = w.Write(msg); err != nil {
		return err
	}
	return w.Close()
}

// [修复] buildMessage: 解决 RFC 2047 标题编码和 RFC 5321 换行符问题
func buildMessage(from string, rcpts []string, sub, body string) []byte {
	domain := "localhost"
	if parts := strings.Split(from, "@"); len(parts) > 1 {
		domain = parts[1]
	}
	msgID := fmt.Sprintf("<%d.%d@%s>", time.Now().UnixNano(), os.Getpid(), domain)

	displayTo := strings.Join(rcpts, ",")
	if len(displayTo) > 900 {
		displayTo = fmt.Sprintf("%s... (%d recipients)", rcpts[0], len(rcpts))
	}

	// [修复] 使用 Q-Encoding 编码 Subject，支持非 ASCII 字符并防止 Header 注入
	encodedSub := mime.QEncoding.Encode("utf-8", sub)

	headers := []string{
		fmt.Sprintf("From: %s", from),
		fmt.Sprintf("To: %s", displayTo),
		fmt.Sprintf("Subject: %s", encodedSub),
		fmt.Sprintf("Date: %s", time.Now().Format(time.RFC1123Z)),
		fmt.Sprintf("Message-ID: %s", msgID),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
	}

	// [修复] 换行符标准化：SMTP 协议正文必须使用 CRLF
	body = strings.ReplaceAll(body, "\r\n", "\n") // 先统一为 LF
	body = strings.ReplaceAll(body, "\n", "\r\n") // 再统一转为 CRLF

	return []byte(strings.Join(headers, "\r\n") + "\r\n\r\n" + body)
}

func sanitizeHeader(v string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, v)
}

// --- 其他辅助函数 ---

func performReload() error {
	pidBytes, err := os.ReadFile(PidFileName)
	if err != nil {
		return err
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGHUP)
}

func handleStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	qLen, qCap := len(taskQueue), cap(taskQueue)
	json.NewEncoder(w).Encode(StatusResponse{
		Status: "ok",
		Uptime: time.Since(startTime).String(),
		Queue: QueueStats{
			Current:  qLen,
			Capacity: qCap,
			Usage:    fmt.Sprintf("%.1f%%", float64(qLen)/float64(qCap)*100),
		},
		System: SystemStats{
			Goroutines: runtime.NumGoroutine(),
			PID:        os.Getpid(),
		},
	})
}

func loadAndValidateConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal([]byte(os.ExpandEnv(string(data))), &cfg); err != nil {
		return nil, err
	}
	if cfg.PrimarySMTP.Server == "" {
		return nil, errors.New("missing server")
	}
	return &cfg, nil
}

func runDeepCheck(cfg *Config) {
	addr := net.JoinHostPort(cfg.PrimarySMTP.Server, strconv.Itoa(cfg.PrimarySMTP.Port))
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Printf("[FAIL] %v\n", err)
		os.Exit(1)
	}
	conn.Close()
	fmt.Println("[PASS] Connection successful")
}
