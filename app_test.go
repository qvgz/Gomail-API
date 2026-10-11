package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/zap"
)

func testApp(cfg *runtimeConfig) *App {
	app := &App{queue: make(chan EmailTask, cfg.raw.QueueCapacity), logger: zap.NewNop(), startTime: time.Now()}
	app.cfg.Store(cfg)
	return app
}

func TestHTTPValidationAndQueue(t *testing.T) {
	cfg := validTestConfig()
	cfg.QueueCapacity = 1
	runtime, err := buildRuntimeConfig(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	app := testApp(runtime)
	handler := app.routes()
	tests := []struct {
		form   url.Values
		status int
	}{
		{url.Values{}, 400},
		{url.Values{"target_group": {"missing"}, "subject": {"test"}, "content": {"body"}}, 404},
		{url.Values{"target_group": {"it"}, "subject": {"bad\r\nBcc: other@example.com"}, "content": {"body"}}, 400},
		{url.Values{"target_group": {"it"}, "subject": {"\t"}, "content": {"body"}}, 400},
		{url.Values{"target_group": {"it"}, "subject": {strings.Repeat("x", MaxHeaderLength+1)}, "content": {"body"}}, 400},
		{url.Values{"target_group": {"it"}, "subject": {"test"}, "content": {strings.Repeat("x", MaxBodyLength+1)}}, 400},
		{url.Values{"target_group": {"it"}, "subject": {"test"}, "content": {"body"}}, 200},
		{url.Values{"target_group": {"it"}, "subject": {"test"}, "content": {"body"}}, 503},
	}
	for _, test := range tests {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != test.status {
			t.Fatalf("got %d want %d: %s", response.Code, test.status, response.Body.String())
		}
		if response.Code == 200 {
			if response.Body.String() != "queued" || response.Header().Get("X-Message-ID") == "" {
				t.Fatal("missing queue acknowledgment")
			}
		}
	}
	if app.metrics.accepted.Load() != 1 {
		t.Fatal("rejected requests counted as accepted")
	}
	task := <-app.queue
	if task.MessageID == "" || task.CreatedAt.IsZero() || task.RecipientAddrs[0].Address != "admin@example.com" {
		t.Fatal("invalid queued task")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status", nil))
	var status StatusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || status.Delivery.Accepted != 1 {
		t.Fatalf("status: %v", err)
	}
	app.stopQueue()
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != 503 {
		t.Fatal("stopping instance is ready")
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", nil))
	if response.Code != 503 {
		t.Fatal("stopping instance accepts work")
	}
}

func TestEnqueueConcurrentShutdown(t *testing.T) {
	cfg := validTestConfig()
	cfg.QueueCapacity = 10000
	runtime, err := buildRuntimeConfig(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	app := testApp(runtime)
	var wg sync.WaitGroup
	var accepted atomic.Uint64
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if app.enqueue(EmailTask{}) == nil {
					accepted.Add(1)
				}
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); app.stopQueue() }()
	wg.Wait()
	app.stopQueue()
	if err := app.enqueue(EmailTask{}); err == nil {
		t.Fatal("enqueue after shutdown succeeded")
	}
	if app.metrics.accepted.Load() != accepted.Load() || uint64(len(app.queue)) != accepted.Load() {
		t.Fatal("queue accounting mismatch")
	}
}

func TestRetryCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := &App{logger: zap.NewNop()}
		ctx, cancel := context.WithCancel(context.Background())
		attempts := 0
		go func() { time.Sleep(time.Second); cancel() }()
		start := time.Now()
		err := app.sendWithRetry(ctx, 0, EmailTask{}, func(context.Context, EmailTask) error { attempts++; return errors.New("failed") })
		if !errors.Is(err, context.Canceled) || attempts != 1 || time.Since(start) != time.Second {
			t.Fatal("retry wait did not cancel")
		}
	})
}

func TestWorkerDrainAndShutdownDeadline(t *testing.T) {
	for _, mode := range []string{"normal", "silent_data"} {
		t.Run(mode, func(t *testing.T) {
			server := startTestSMTP(t, mode)
			cfg := validTestConfig()
			cfg.PrimarySMTP = server.config
			cfg.WorkerCount = 1
			runtime, err := buildRuntimeConfig(&cfg)
			if err != nil {
				t.Fatal(err)
			}
			app := testApp(runtime)
			for i := 0; i < 3; i++ {
				if err := app.enqueue(testEmailTask()); err != nil {
					t.Fatal(err)
				}
			}
			workerCtx, cancelWorkers := context.WithCancel(context.Background())
			defer cancelWorkers()
			app.wg.Add(1)
			go app.emailWorker(workerCtx, 0)
			waitSMTPCommand(t, server, "DATA")
			timeout := 3 * time.Second
			if mode == "silent_data" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			start := time.Now()
			err = app.shutdown(ctx, &http.Server{}, cancelWorkers)
			if time.Since(start) > 2*time.Second {
				t.Fatal("shutdown exceeded bound")
			}
			if mode == "normal" {
				if err != nil || app.metrics.sent.Load() != 3 || app.metrics.failed.Load() != 0 {
					t.Fatalf("drain failed: %v", err)
				}
			} else {
				if !errors.Is(err, context.DeadlineExceeded) || app.metrics.failed.Load() != 1 || app.metrics.discarded.Load() != 2 {
					t.Fatalf("deadline accounting: %v", err)
				}
			}
			if app.metrics.inFlight.Load() != 0 {
				t.Fatal("worker remained in flight")
			}
		})
	}
}

func TestConcurrentHTTPToSMTPDelivery(t *testing.T) {
	const requests = 64
	smtp := startTestSMTP(t, "normal")
	// 模拟服务器记录命令的通道不应限制本次并发投递吞吐。
	drainCtx, stopDrain := context.WithCancel(context.Background())
	defer stopDrain()
	go func() {
		for {
			select {
			case <-smtp.commands:
			case <-drainCtx.Done():
				return
			}
		}
	}()
	cfg := validTestConfig()
	cfg.PrimarySMTP = smtp.config
	cfg.QueueCapacity = requests
	cfg.WorkerCount = 4
	runtime, err := buildRuntimeConfig(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	app := testApp(runtime)
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer func() { app.stopQueue(); cancelWorkers(); app.wg.Wait() }()
	for id := 0; id < cfg.WorkerCount; id++ {
		app.wg.Add(1)
		go app.emailWorker(workerCtx, id)
	}
	server := httptest.NewServer(app.routes())
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	failures := make(chan error, requests)
	ids := make(chan string, requests)
	var senders sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		senders.Add(1)
		go func(worker int) {
			defer senders.Done()
			for index := worker; index < requests; index += 8 {
				response, err := client.PostForm(server.URL+"/alerts/mail", url.Values{
					"target_group": {"it"}, "subject": {fmt.Sprintf("message-%d", index)}, "content": {"mail body"},
				})
				if err != nil {
					failures <- err
					continue
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || response.StatusCode != http.StatusOK || string(body) != "queued" {
					failures <- fmt.Errorf("request %d: status=%d error=%v", index, response.StatusCode, err)
					continue
				}
				ids <- response.Header.Get("X-Message-ID")
			}
		}(worker)
	}
	senders.Wait()
	client.CloseIdleConnections()
	close(failures)
	close(ids)
	for err := range failures {
		t.Error(err)
	}
	expected := make(map[string]bool, requests)
	for id := range ids {
		if id == "" || expected[id] {
			t.Fatal("missing or repeated acknowledgment ID")
		}
		expected[id] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := app.shutdown(ctx, server.Config, cancelWorkers); err != nil {
		t.Fatal(err)
	}
	if len(expected) != requests || app.metrics.accepted.Load() != requests || app.metrics.sent.Load() != requests || app.metrics.failed.Load() != 0 || app.metrics.inFlight.Load() != 0 {
		t.Fatalf("acknowledged=%d accepted=%d sent=%d failed=%d in_flight=%d", len(expected), app.metrics.accepted.Load(), app.metrics.sent.Load(), app.metrics.failed.Load(), app.metrics.inFlight.Load())
	}
	for i := 0; i < requests; i++ {
		select {
		case data := <-smtp.messages:
			message, err := mail.ReadMessage(strings.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			id := strings.Trim(message.Header.Get("Message-ID"), "<>")
			if !expected[id] {
				t.Fatal("unexpected or duplicate SMTP message")
			}
			delete(expected, id)
		default:
			t.Fatal("acknowledged task was not delivered")
		}
	}
	if len(expected) != 0 {
		t.Fatal("some acknowledged tasks were lost")
	}
}
