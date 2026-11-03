package main

import (
	"errors"
	"net/mail"
	"sync"
	"time"
)

var (
	errQueueFull   = errors.New("queue full")
	errQueueClosed = errors.New("service shutting down")
)

type task struct {
	ID         string
	Group      string
	Recipients []*mail.Address
	Subject    string
	Body       string
	CreatedAt  time.Time
	Attempts   int // 仅由当前持有任务的 worker 修改
}

// queue 是有界内存队列。容量统计所有未完成任务：排队、发送中和等待重试，
// 因此 ready 中的任务数不会超过容量，持锁写入 ready 不会阻塞。
type queue struct {
	mu       sync.Mutex
	capacity int
	pending  int
	closed   bool
	waiting  int
	ready    chan *task
	drained  chan struct{} // 关闭后 pending 归零时关闭
}

type queueStats struct {
	Pending  int `json:"pending"`
	Ready    int `json:"ready"`
	Waiting  int `json:"retry_waiting"`
	InFlight int `json:"in_flight"`
	Capacity int `json:"capacity"`
}

func newQueue(capacity int) *queue {
	return &queue{
		capacity: capacity,
		ready:    make(chan *task, capacity),
		drained:  make(chan struct{}),
	}
}

func (q *queue) push(t *task) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return errQueueClosed
	}
	if q.pending >= q.capacity {
		return errQueueFull
	}
	q.pending++
	q.ready <- t
	return nil
}

// retryAfter 将发送中的任务延迟放回队列，等待期间不占用 worker。
func (q *queue) retryAfter(t *task, delay time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.waiting++
	time.AfterFunc(delay, func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		q.waiting--
		q.ready <- t
	})
}

// done 标记任务最终完成（成功或放弃）。
func (q *queue) done() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending--
	if q.closed && q.pending == 0 {
		close(q.drained)
	}
}

// close 拒绝新任务；已接收的任务（含等待重试）继续处理。
func (q *queue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	if q.pending == 0 {
		close(q.drained)
	}
}

func (q *queue) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

func (q *queue) stats() queueStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := queueStats{Pending: q.pending, Ready: len(q.ready), Waiting: q.waiting, Capacity: q.capacity}
	s.InFlight = s.Pending - s.Ready - s.Waiting
	return s
}
