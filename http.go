package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type StatusResponse struct {
	Status    string        `json:"status"`
	Uptime    string        `json:"uptime"`
	BuildInfo string        `json:"build_info"`
	Queue     QueueStats    `json:"queue"`
	System    SystemStats   `json:"system"`
	Delivery  DeliveryStats `json:"delivery"`
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

type DeliveryStats struct {
	Accepted  uint64 `json:"accepted"`
	Sent      uint64 `json:"sent"`
	Failed    uint64 `json:"failed"`
	Retries   uint64 `json:"retries"`
	Discarded uint64 `json:"discarded"`
	InFlight  int64  `json:"in_flight"`
}

func (a *App) routes() http.Handler {
	r := chi.NewRouter()

	r.HandleFunc("/healthz", a.handleHealthz)
	r.Get("/readyz", a.handleReadyz)
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

func (a *App) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if a.isStopping() || a.cfg.Load() == nil {
		http.Error(w, "Service not ready", http.StatusServiceUnavailable)
		return
	}
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
		Delivery: DeliveryStats{
			Accepted: a.metrics.accepted.Load(), Sent: a.metrics.sent.Load(),
			Failed: a.metrics.failed.Load(), Retries: a.metrics.retries.Load(),
			Discarded: a.metrics.discarded.Load(), InFlight: a.metrics.inFlight.Load(),
		},
	}
	if a.isStopping() {
		resp.Status = "stopping"
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (a *App) handleSendMail(w http.ResponseWriter, r *http.Request) {
	if a.isStopping() {
		http.Error(w, "Service shutting down", http.StatusServiceUnavailable)
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
	if len(content) > MaxBodyLength || sanitizeHeader(subject) != subject || strings.TrimSpace(subject) == "" {
		http.Error(w, "Invalid subject or content too large", http.StatusBadRequest)
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
		MessageID:      rand.Text() + "@gomail-api",
		CreatedAt:      time.Now(),
	}

	if err := a.enqueue(task); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("X-Message-ID", task.MessageID)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("queued"))
}
