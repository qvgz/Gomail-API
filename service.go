package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type deliverer interface {
	send(ctx context.Context, t *task) error
	closeConns()
}

type metrics struct {
	accepted  atomic.Uint64
	sent      atomic.Uint64
	failed    atomic.Uint64
	retries   atomic.Uint64
	discarded atomic.Uint64
}

type service struct {
	cfg          *settings
	logger       *slog.Logger
	queue        *queue
	limiter      *limiter
	metrics      metrics
	started      time.Time
	newDeliverer func() (deliverer, error)

	wg            sync.WaitGroup
	cancelWorkers context.CancelFunc
}

func newService(cfg *settings, logger *slog.Logger) *service {
	primaryDown := &cooldown{}
	return &service{
		cfg: cfg, logger: logger, started: time.Now(),
		queue:   newQueue(cfg.queueCapacity),
		limiter: &limiter{interval: cfg.sendInterval},
		newDeliverer: func() (deliverer, error) {
			return newMailer(cfg, primaryDown, logger)
		},
	}
}

func run(ctx context.Context, cfg *settings, logger *slog.Logger) error {
	svc := newService(cfg, logger)
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.httpPort))
	if err != nil {
		return fmt.Errorf("listen http: %w", err)
	}
	if err := svc.start(); err != nil {
		listener.Close()
		return err
	}
	server := &http.Server{
		Handler:           svc.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 * 1024,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	logger.Info("gomail api started", "version", Version, "port", cfg.httpPort,
		"workers", cfg.workers, "queue_capacity", cfg.queueCapacity, "backup_smtp", cfg.backup != nil)

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err = <-serveErr:
		logger.Error("http server stopped unexpectedly", "error", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	// 先拒绝新任务，再并行关闭 HTTP 和排空队列，二者共用同一期限。
	svc.queue.close()
	httpDone := make(chan error, 1)
	go func() { httpDone <- server.Shutdown(shutdownCtx) }()
	drainErr := svc.drain(shutdownCtx)
	httpErr := <-httpDone
	if httpErr != nil {
		_ = server.Close()
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, drainErr, httpErr)
}

func (s *service) start() error {
	deliverers := make([]deliverer, s.cfg.workers)
	for i := range deliverers {
		d, err := s.newDeliverer()
		if err != nil {
			return err
		}
		deliverers[i] = d
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancelWorkers = cancel
	for i, d := range deliverers {
		s.wg.Add(1)
		go s.worker(ctx, i, d)
	}
	return nil
}

// drain 等待所有已接收任务完成；超过期限则取消发送，未完成任务计入 discarded。
func (s *service) drain(ctx context.Context) error {
	s.queue.close()
	select {
	case <-s.queue.drained:
		s.cancelWorkers()
		s.wg.Wait()
		return nil
	case <-ctx.Done():
		s.cancelWorkers()
		s.wg.Wait() // 所有等待和网络 I/O 都响应 worker context。
		discarded := s.queue.stats().Pending
		s.metrics.discarded.Add(uint64(discarded))
		s.logger.Error("shutdown deadline exceeded, unsent tasks discarded", "discarded", discarded)
		return ctx.Err()
	}
}

func (s *service) submit(t *task) error {
	if err := s.queue.push(t); err != nil {
		return err
	}
	s.metrics.accepted.Add(1)
	s.logger.Info("mail queued", "message_id", t.ID, "group", t.Group)
	return nil
}

func (s *service) worker(ctx context.Context, id int, d deliverer) {
	defer s.wg.Done()
	defer d.closeConns()
	idle := time.NewTimer(SMTPIdleTimeout)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.queue.drained:
			return
		case <-idle.C:
			d.closeConns()
		case t := <-s.queue.ready:
			s.process(ctx, id, d, t)
			idle.Reset(SMTPIdleTimeout)
		}
	}
}

func (s *service) process(ctx context.Context, id int, d deliverer, t *task) {
	t.Attempts++
	err := s.attempt(ctx, d, t)
	log := s.logger.With("worker", id, "message_id", t.ID, "group", t.Group, "attempts", t.Attempts)
	if err == nil {
		s.metrics.sent.Add(1)
		s.queue.done()
		log.Info("send success", "latency", time.Since(t.CreatedAt).String())
		return
	}
	if ctx.Err() != nil {
		return // 退出期限已到，由 drain 计入 discarded。
	}
	if t.Attempts > s.cfg.maxRetries {
		s.metrics.failed.Add(1)
		s.queue.done()
		log.Error("send failed", "error", err)
		return
	}
	s.metrics.retries.Add(1)
	log.Warn("send failed, retrying", "retry", t.Attempts, "max_retries", s.cfg.maxRetries,
		"retry_in", s.cfg.sendInterval.String(), "error", err)
	s.queue.retryAfter(t, s.cfg.sendInterval)
}

func (s *service) attempt(ctx context.Context, d deliverer, t *task) (err error) {
	defer func() {
		if r := recover(); r != nil {
			d.closeConns()
			err = fmt.Errorf("delivery panicked: %v", r)
		}
	}()
	if err := s.limiter.wait(ctx); err != nil {
		return err
	}
	// 超时由 mailer 按服务器分别控制（send_timeout）。
	return d.send(ctx, t)
}

// limiter 限制全局发送尝试的最小间隔，包括重试。
type limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func (l *limiter) wait(ctx context.Context) error {
	if l.interval <= 0 {
		return ctx.Err()
	}
	l.mu.Lock()
	at := time.Now()
	if l.next.After(at) {
		at = l.next
	}
	l.next = at.Add(l.interval)
	l.mu.Unlock()
	return sleep(ctx, time.Until(at))
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
