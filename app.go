package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
)

type deliveryMetrics struct {
	accepted  atomic.Uint64
	sent      atomic.Uint64
	failed    atomic.Uint64
	retries   atomic.Uint64
	discarded atomic.Uint64
	inFlight  atomic.Int64
}

type App struct {
	cfg        atomic.Pointer[runtimeConfig]
	queue      chan EmailTask
	queueMu    sync.RWMutex
	stopping   bool
	startTime  time.Time
	logger     *zap.Logger
	configPath string
	wg         sync.WaitGroup
	metrics    deliveryMetrics
}

func newApp(cfg *runtimeConfig, configPath string) *App {
	loggerCfg := zap.NewProductionConfig()
	loggerCfg.OutputPaths = []string{"stdout"}
	loggerCfg.ErrorOutputPaths = []string{"stderr"}
	// 保留每一次投递结果，避免默认采样隐藏批量失败。
	loggerCfg.Sampling = nil
	logger, err := loggerCfg.Build()
	if err != nil {
		panic(err)
	}
	app := &App{
		queue:     make(chan EmailTask, cfg.raw.QueueCapacity),
		startTime: time.Now(), logger: logger, configPath: configPath,
	}
	app.cfg.Store(cfg)
	return app
}

func (a *App) run() error {
	defer a.logger.Sync()
	cfg := a.cfg.Load()
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.raw.HTTPPort))
	if err != nil {
		return fmt.Errorf("listen HTTP: %w", err)
	}
	defer listener.Close()
	pidFile, err := lockPIDFile(PidFileName)
	if err != nil {
		return err
	}
	// 保留文件并只释放锁，避免删除锁文件后两个进程锁住不同 inode。
	defer pidFile.Close()

	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	stopChan := make(chan os.Signal, 8)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(stopChan)

	a.logger.Info("starting gomail api", zap.String("version", Version),
		zap.Int("workers", cfg.raw.WorkerCount), zap.Int("pid", os.Getpid()),
		zap.Int("port", cfg.raw.HTTPPort), zap.Int("queue_capacity", cap(a.queue)))
	for i := 0; i < cfg.raw.WorkerCount; i++ {
		a.wg.Add(1)
		go a.emailWorker(workerCtx, i)
	}
	server := &http.Server{
		Handler: a.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024,
	}
	errChan := make(chan error, 1)
	go func() { errChan <- server.Serve(listener) }()
	for {
		select {
		case err := <-errChan:
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
			shutdownErr := a.shutdown(ctx, server, cancelWorkers)
			cancel()
			return errors.Join(err, shutdownErr)
		case sig := <-stopChan:
			if sig == syscall.SIGHUP {
				a.reloadConfig()
				continue
			}
			a.logger.Info("shutting down", zap.String("signal", sig.String()))
			ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
			err := a.shutdown(ctx, server, cancelWorkers)
			cancel()
			return err
		}
	}
}

func (a *App) enqueue(task EmailTask) error {
	a.queueMu.RLock()
	defer a.queueMu.RUnlock()
	if a.stopping {
		return errors.New("service shutting down")
	}
	select {
	case a.queue <- task:
		a.metrics.accepted.Add(1)
		return nil
	default:
		return errors.New("queue full")
	}
}

func (a *App) stopQueue() {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	if !a.stopping {
		a.stopping = true
		close(a.queue)
	}
}

func (a *App) isStopping() bool {
	a.queueMu.RLock()
	defer a.queueMu.RUnlock()
	return a.stopping
}

func (a *App) shutdown(ctx context.Context, server *http.Server, cancelWorkers context.CancelFunc) error {
	a.stopQueue()
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	httpErr := server.Shutdown(ctx)
	if httpErr != nil {
		_ = server.Close()
	}
	select {
	case <-done:
		cancelWorkers()
		return httpErr
	case <-ctx.Done():
		cancelWorkers()
		<-done // 所有等待和网络 I/O 都受 worker context 控制。
		discarded := len(a.queue)
		a.metrics.discarded.Add(uint64(discarded))
		a.logger.Error("shutdown deadline exceeded", zap.Int("discarded_queued_tasks", discarded))
		return errors.Join(httpErr, ctx.Err())
	}
}

func (a *App) emailWorker(ctx context.Context, id int) {
	defer a.wg.Done()
	var senders *workerSenders
	var nextSendAt time.Time
	defer func() {
		if senders != nil {
			senders.close()
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case task, ok := <-a.queue:
			if !ok {
				return
			}
			a.metrics.inFlight.Add(1)
			err := a.deliverTask(ctx, id, task, &senders, &nextSendAt)
			a.metrics.inFlight.Add(-1)
			fields := []zap.Field{zap.Int("worker", id), zap.String("group", task.TargetGroup),
				zap.String("message_id", task.MessageID)}
			if err != nil {
				a.metrics.failed.Add(1)
				a.logger.Error("send failed", append(fields, zap.Error(err))...)
			} else {
				a.metrics.sent.Add(1)
				a.logger.Info("send success", fields...)
			}
		}
	}
}

func (a *App) deliverTask(ctx context.Context, id int, task EmailTask, senders **workerSenders, nextSendAt *time.Time) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if *senders != nil {
				(*senders).close()
				*senders = nil
			}
			err = fmt.Errorf("delivery panicked: %v", recovered)
		}
	}()
	cfg := a.cfg.Load()
	if cfg == nil {
		return errors.New("config not ready")
	}
	if *senders == nil || (*senders).cfg != cfg {
		if *senders != nil {
			(*senders).close()
		}
		*senders, err = newWorkerSenders(cfg, a.logger)
		if err != nil {
			return err
		}
		*nextSendAt = time.Time{}
	}
	if cfg.rateLimit > 0 {
		if err := waitContext(ctx, time.Until(*nextSendAt)); err != nil {
			return err
		}
		*nextSendAt = time.Now().Add(cfg.rateLimit)
	}
	return a.sendWithRetry(ctx, id, task, (*senders).send)
}

func (a *App) sendWithRetry(parent context.Context, id int, task EmailTask, send func(context.Context, EmailTask) error) error {
	for attempt := 0; ; attempt++ {
		if err := parent.Err(); err != nil {
			return err
		}
		err := func() error {
			ctx, cancel := context.WithTimeout(parent, SendTimeout)
			defer cancel()
			return send(ctx, task)
		}()
		if err == nil || attempt == MaxSendRetries {
			return err
		}
		if parent.Err() != nil {
			return parent.Err()
		}
		a.logger.Warn("send failed, retrying", zap.Int("worker", id),
			zap.String("group", task.TargetGroup), zap.String("message_id", task.MessageID),
			zap.Int("retry", attempt+1), zap.Int("max_retries", MaxSendRetries),
			zap.Duration("retry_interval", RetryInterval), zap.Error(err))
		if err := waitContext(parent, RetryInterval); err != nil {
			return err
		}
		a.metrics.retries.Add(1)
	}
}

func waitContext(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func (a *App) reloadConfig() {
	a.logger.Info("reloading config")
	cfg, err := loadAndValidateConfig(a.configPath)
	if err != nil {
		a.logger.Error("reload failed", zap.Error(err))
		return
	}
	current := a.cfg.Load()
	if current.raw.HTTPPort != cfg.raw.HTTPPort || current.raw.QueueCapacity != cfg.raw.QueueCapacity || current.raw.WorkerCount != cfg.raw.WorkerCount {
		a.logger.Error("reload rejected: http_port, queue_capacity and worker_count require restart")
		return
	}
	a.cfg.Store(cfg)
	a.logger.Info("config reloaded")
}

func lockPIDFile(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fmt.Errorf("open PID file: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	fail := func(err error) (*os.File, error) { file.Close(); return nil, err }
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(fmt.Errorf("lock PID file (another instance may be running): %w", err))
	}
	if err := file.Truncate(0); err != nil {
		return fail(err)
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		return fail(err)
	}
	return file, nil
}

func performReload() error {
	file, err := os.OpenFile(PidFileName, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		return errors.New("service is not running (PID file is not locked)")
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		return err
	}
	data := make([]byte, 32)
	n, err := file.Read(data)
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data[:n])))
	if err != nil || pid <= 0 {
		return errors.New("invalid PID file")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.SIGHUP)
}
