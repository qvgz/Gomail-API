package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"time"
)

type statusResponse struct {
	Status     string        `json:"status"`
	Version    string        `json:"version"`
	Uptime     string        `json:"uptime"`
	Goroutines int           `json:"goroutines"`
	Queue      queueStats    `json:"queue"`
	Delivery   deliveryStats `json:"delivery"`
}

type deliveryStats struct {
	Accepted  uint64 `json:"accepted"`
	Sent      uint64 `json:"sent"`
	Failed    uint64 `json:"failed"`
	Retries   uint64 `json:"retries"`
	Discarded uint64 `json:"discarded"`
}

func (s *service) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /{$}", s.handleSend)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /status", s.handleStatus)
	return mux
}

func (s *service) handleReady(w http.ResponseWriter, _ *http.Request) {
	if s.queue.isClosed() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

func (s *service) handleStatus(w http.ResponseWriter, _ *http.Request) {
	resp := statusResponse{
		Status:     "ok",
		Version:    Version,
		Uptime:     time.Since(s.started).Round(time.Second).String(),
		Goroutines: runtime.NumGoroutine(),
		Queue:      s.queue.stats(),
		Delivery: deliveryStats{
			Accepted: s.metrics.accepted.Load(), Sent: s.metrics.sent.Load(),
			Failed: s.metrics.failed.Load(), Retries: s.metrics.retries.Load(),
			Discarded: s.metrics.discarded.Load(),
		},
	}
	if s.queue.isClosed() {
		resp.Status = "stopping"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *service) handleSend(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyLength)
	// ParseMultipartForm 对非 multipart 请求会丢弃 ParseForm 的错误，需先单独解析。
	err := r.ParseForm()
	if err == nil {
		if err = r.ParseMultipartForm(MaxBodyLength); errors.Is(err, http.ErrNotMultipart) {
			err = nil
		}
	}
	if err != nil {
		http.Error(w, "request too large or invalid", http.StatusBadRequest)
		return
	}
	group := r.FormValue("target_group")
	subject := r.FormValue("subject")
	content := r.FormValue("content")
	switch {
	case group == "" || subject == "" || content == "":
		http.Error(w, "target_group, subject and content are required", http.StatusBadRequest)
		return
	case len(group) > MaxHeaderLength || len(subject) > MaxHeaderLength:
		http.Error(w, "target_group or subject too long", http.StatusBadRequest)
		return
	case hasControl(subject) || strings.TrimSpace(subject) == "":
		http.Error(w, "invalid subject", http.StatusBadRequest)
		return
	}
	recipients, ok := s.cfg.groups[group]
	if !ok {
		http.Error(w, "group not found", http.StatusNotFound)
		return
	}
	t := &task{
		ID:         rand.Text() + "@gomail-api",
		Group:      group,
		Recipients: recipients,
		Subject:    subject,
		Body:       content,
		CreatedAt:  time.Now(),
	}
	if err := s.submit(t); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("X-Message-ID", t.ID)
	_, _ = w.Write([]byte("queued"))
}
