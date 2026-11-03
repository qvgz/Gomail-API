package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestQueueCapacityCountsUnfinishedTasks(t *testing.T) {
	q := newQueue(2)
	a, b := &task{ID: "a"}, &task{ID: "b"}
	if q.push(a) != nil || q.push(b) != nil {
		t.Fatal("push within capacity failed")
	}
	<-q.ready // 发送中仍占用容量
	if err := q.push(&task{}); !errors.Is(err, errQueueFull) {
		t.Fatalf("got %v, want queue full", err)
	}
	q.retryAfter(a, time.Hour) // 等待重试仍占用容量
	if err := q.push(&task{}); !errors.Is(err, errQueueFull) {
		t.Fatalf("got %v, want queue full", err)
	}
	if s := q.stats(); s.Pending != 2 || s.Ready != 1 || s.Waiting != 1 || s.InFlight != 0 {
		t.Fatalf("stats: %+v", s)
	}
	<-q.ready
	q.done()
	if err := q.push(&task{}); err != nil {
		t.Fatalf("capacity not released: %v", err)
	}
}

func TestQueueRetryDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(1)
		item := &task{}
		if err := q.push(item); err != nil {
			t.Fatal(err)
		}
		<-q.ready
		start := time.Now()
		q.retryAfter(item, 3*time.Second)
		if got := <-q.ready; got != item || time.Since(start) != 3*time.Second {
			t.Fatalf("retry returned after %v", time.Since(start))
		}
	})
}

func TestQueueCloseKeepsRetriesAndSignalsDrained(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(2)
		item := &task{}
		if err := q.push(item); err != nil {
			t.Fatal(err)
		}
		<-q.ready
		q.retryAfter(item, 3*time.Second)
		start := time.Now()
		q.close()
		if err := q.push(&task{}); !errors.Is(err, errQueueClosed) {
			t.Fatalf("got %v, want closed", err)
		}
		if got := <-q.ready; got != item || time.Since(start) != 3*time.Second {
			t.Fatal("waiting retry lost after close")
		}
		select {
		case <-q.drained:
			t.Fatal("drained before task finished")
		default:
		}
		q.done()
		<-q.drained
	})
	q := newQueue(1)
	q.close()
	<-q.drained // 空队列关闭后立即排空
}

func TestQueueConcurrentPushAndClose(t *testing.T) {
	q := newQueue(10000)
	var wg sync.WaitGroup
	var accepted atomic.Int64
	for range 16 {
		wg.Go(func() {
			for range 100 {
				if q.push(&task{}) == nil {
					accepted.Add(1)
				}
			}
		})
	}
	wg.Go(q.close)
	wg.Wait()
	if s := q.stats(); int64(s.Pending) != accepted.Load() || int64(s.Ready) != accepted.Load() {
		t.Fatalf("accounting mismatch: accepted=%d stats=%+v", accepted.Load(), s)
	}
}
