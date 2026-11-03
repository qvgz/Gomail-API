package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func serveTest(handler http.Handler, method, target string, form url.Values) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestHTTPValidationAndQueue(t *testing.T) {
	s := newService(testSettings(t, func(c *Config) { c.QueueCapacity = ptr(1) }), slog.New(slog.DiscardHandler))
	handler := s.routes()
	valid := url.Values{"target_group": {"it"}, "subject": {"test"}, "content": {"body"}}
	with := func(key, value string) url.Values {
		form := url.Values{}
		for k, v := range valid {
			form[k] = v
		}
		form.Set(key, value)
		return form
	}
	tests := []struct {
		form   url.Values
		status int
	}{
		{url.Values{}, 400},
		{with("target_group", "missing"), 404},
		{with("subject", "bad\r\nBcc: other@example.com"), 400},
		{with("subject", "\t"), 400},
		{with("subject", " "), 400},
		{with("subject", strings.Repeat("x", MaxHeaderLength+1)), 400},
		{with("content", strings.Repeat("x", MaxBodyLength)), 400},
		{valid, 200},
		{valid, 503},
	}
	for i, test := range tests {
		rec := serveTest(handler, http.MethodPost, "/", test.form)
		if rec.Code != test.status {
			t.Fatalf("case %d: got %d want %d: %s", i, rec.Code, test.status, rec.Body.String())
		}
		if rec.Code == 200 && (rec.Body.String() != "queued" || rec.Header().Get("X-Message-ID") == "") {
			t.Fatal("missing queue acknowledgment")
		}
	}
	if s.metrics.accepted.Load() != 1 {
		t.Fatal("rejected requests counted as accepted")
	}
	queued := <-s.queue.ready
	if queued.ID == "" || queued.CreatedAt.IsZero() || queued.Recipients[0].Address != "admin@example.com" || queued.Body != "body" {
		t.Fatalf("invalid queued task: %+v", queued)
	}

	var status statusResponse
	if err := json.Unmarshal(serveTest(handler, http.MethodGet, "/status", nil).Body.Bytes(), &status); err != nil ||
		status.Delivery.Accepted != 1 || status.Queue.Pending != 1 || status.Queue.InFlight != 1 {
		t.Fatalf("status: %v %+v", err, status)
	}
	for target, code := range map[string]int{"/healthz": 200, "/readyz": 200, "/other": 404} {
		if got := serveTest(handler, http.MethodGet, target, nil).Code; got != code {
			t.Fatalf("GET %s = %d, want %d", target, got, code)
		}
	}
	if got := serveTest(handler, http.MethodPost, "/alerts", valid).Code; got != 404 {
		t.Fatalf("POST to unknown path = %d", got)
	}
	if got := serveTest(handler, http.MethodGet, "/", nil).Code; got != 405 {
		t.Fatalf("GET / = %d", got)
	}

	s.queue.close()
	if got := serveTest(handler, http.MethodGet, "/readyz", nil).Code; got != 503 {
		t.Fatal("stopping instance is ready")
	}
	if got := serveTest(handler, http.MethodPost, "/", valid).Code; got != 503 {
		t.Fatal("stopping instance accepts work")
	}
}

func TestHTTPMultipartForm(t *testing.T) {
	s := newService(testSettings(t, nil), slog.New(slog.DiscardHandler))
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for k, v := range map[string]string{"target_group": "it", "subject": "test", "content": "body"} {
		_ = writer.WriteField(k, v)
	}
	writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("multipart form: %d %s", rec.Code, rec.Body.String())
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
	cfg := testSettings(t, func(c *Config) {
		c.PrimarySMTP = smtp.config
		c.QueueCapacity = ptr(requests)
		c.WorkerCount = ptr(4)
	})
	s := newService(cfg, slog.New(slog.DiscardHandler))
	if err := s.start(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.routes())
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second

	var mu sync.Mutex
	expected := make(map[string]bool, requests)
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			for index := worker; index < requests; index += 8 {
				resp, err := client.PostForm(server.URL+"/", url.Values{
					"target_group": {"it"}, "subject": {fmt.Sprintf("message-%d", index)}, "content": {"mail body"},
				})
				if err != nil {
					t.Error(err)
					continue
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				id := resp.Header.Get("X-Message-ID")
				if err != nil || resp.StatusCode != http.StatusOK || string(body) != "queued" || id == "" {
					t.Errorf("request %d: status=%d error=%v", index, resp.StatusCode, err)
					continue
				}
				mu.Lock()
				if expected[id] {
					t.Error("repeated acknowledgment ID")
				}
				expected[id] = true
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.drain(ctx); err != nil {
		t.Fatal(err)
	}
	if len(expected) != requests || s.metrics.sent.Load() != requests || s.metrics.failed.Load() != 0 || s.queue.stats().Pending != 0 {
		t.Fatalf("acknowledged=%d sent=%d failed=%d", len(expected), s.metrics.sent.Load(), s.metrics.failed.Load())
	}
	for range requests {
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
}

// 表单解析错误不得被忽略：超大或非法编码的请求体必须拒绝，不能退回使用查询参数或部分字段。
func TestHTTPRejectsInvalidFormBody(t *testing.T) {
	s := newService(testSettings(t, nil), slog.New(slog.DiscardHandler))
	handler := s.routes()
	for name, body := range map[string]string{
		"too large":  "content=" + strings.Repeat("x", MaxBodyLength),
		"bad escape": "target_group=it&subject=test&content=body&x=%zz",
	} {
		req := httptest.NewRequest(http.MethodPost, "/?target_group=it&subject=test&content=body", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, rec.Code)
		}
	}
	if s.metrics.accepted.Load() != 0 {
		t.Fatal("invalid request was queued")
	}
}
