package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type fakeDeliverer func(context.Context, *task) error

func (f fakeDeliverer) send(ctx context.Context, t *task) error { return f(ctx, t) }
func (fakeDeliverer) closeConns()                               {}

func testService(t *testing.T, cfg *settings, send fakeDeliverer) *service {
	t.Helper()
	s := newService(cfg, slog.New(slog.DiscardHandler))
	s.newDeliverer = func() (deliverer, error) { return send, nil }
	if err := s.start(); err != nil {
		t.Fatal(err)
	}
	return s
}

func submitTest(t *testing.T, s *service, id string) {
	t.Helper()
	if err := s.submit(&task{ID: id, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func drainTest(t *testing.T, s *service, timeout time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.drain(ctx)
}

func TestRetryFixedIntervalUntilSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testSettings(t, func(c *Config) { c.WorkerCount = ptr(1); c.MaxRetries = ptr(3); c.RateLimitInterval = ptr(3) })
		start := time.Now()
		var offsets []time.Duration
		s := testService(t, cfg, func(context.Context, *task) error {
			offsets = append(offsets, time.Since(start))
			if len(offsets) <= 3 {
				return errors.New("smtp unavailable")
			}
			return nil
		})
		submitTest(t, s, "a")
		time.Sleep(time.Minute)
		// drain 等待 worker 退出，之后读取计数无数据竞争。
		if err := drainTest(t, s, time.Second); err != nil {
			t.Fatal(err)
		}
		want := []time.Duration{0, 3 * time.Second, 6 * time.Second, 9 * time.Second}
		if len(offsets) != len(want) {
			t.Fatalf("attempts at %v", offsets)
		}
		for i := range want {
			if offsets[i] != want[i] {
				t.Fatalf("attempts at %v, want %v", offsets, want)
			}
		}
		if s.metrics.sent.Load() != 1 || s.metrics.retries.Load() != 3 || s.metrics.failed.Load() != 0 {
			t.Fatal("unexpected metrics")
		}
	})
}

func TestMaxRetriesGivesUp(t *testing.T) {
	for _, retries := range []int{0, 2} {
		t.Run(fmt.Sprint(retries), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := testSettings(t, func(c *Config) { c.WorkerCount = ptr(1); c.MaxRetries = ptr(retries) })
				attempts := 0
				s := testService(t, cfg, func(context.Context, *task) error { attempts++; return errors.New("rejected") })
				submitTest(t, s, "a")
				time.Sleep(time.Minute)
				if err := drainTest(t, s, time.Second); err != nil {
					t.Fatal(err)
				}
				if attempts != retries+1 || s.metrics.retries.Load() != uint64(retries) ||
					s.metrics.failed.Load() != 1 || s.queue.stats().Pending != 0 {
					t.Fatalf("attempts=%d retries=%d failed=%d", attempts, s.metrics.retries.Load(), s.metrics.failed.Load())
				}
			})
		})
	}
}

func TestRetryWaitDoesNotBlockWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testSettings(t, func(c *Config) { c.WorkerCount = ptr(1) })
		sentAt := map[string]time.Duration{}
		start := time.Now()
		s := testService(t, cfg, func(_ context.Context, item *task) error {
			if item.ID == "failing" {
				return errors.New("unavailable")
			}
			sentAt[item.ID] = time.Since(start)
			return nil
		})
		submitTest(t, s, "failing")
		submitTest(t, s, "ok")
		synctest.Wait()
		if at, ok := sentAt["ok"]; !ok || at != 0 {
			t.Fatal("worker blocked by retry wait")
		}
		if err := drainTest(t, s, time.Hour); err != nil {
			t.Fatal(err)
		}
		if s.metrics.failed.Load() != 1 || s.metrics.sent.Load() != 1 {
			t.Fatal("unexpected metrics")
		}
	})
}

func TestDrainDeadlineDiscardsUnfinished(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testSettings(t, func(c *Config) { c.WorkerCount = ptr(1) })
		s := testService(t, cfg, func(ctx context.Context, _ *task) error {
			<-ctx.Done()
			return ctx.Err()
		})
		for _, id := range []string{"a", "b", "c"} {
			submitTest(t, s, id)
		}
		synctest.Wait()
		start := time.Now()
		err := drainTest(t, s, time.Second)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != time.Second {
			t.Fatalf("drain: %v after %v", err, time.Since(start))
		}
		if s.metrics.discarded.Load() != 3 || s.metrics.failed.Load() != 0 || s.metrics.retries.Load() != 0 {
			t.Fatalf("discarded=%d failed=%d", s.metrics.discarded.Load(), s.metrics.failed.Load())
		}
	})
}

func TestLimiterIsGlobal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testSettings(t, func(c *Config) { c.WorkerCount = ptr(3); c.RateLimitInterval = ptr(2) })
		start := time.Now()
		var mu sync.Mutex
		var offsets []time.Duration
		s := testService(t, cfg, func(context.Context, *task) error {
			mu.Lock()
			offsets = append(offsets, time.Since(start))
			mu.Unlock()
			return nil
		})
		for _, id := range []string{"a", "b", "c"} {
			submitTest(t, s, id)
		}
		if err := drainTest(t, s, time.Minute); err != nil {
			t.Fatal(err)
		}
		want := []time.Duration{0, 2 * time.Second, 4 * time.Second}
		for i := range want {
			if len(offsets) != 3 || offsets[i] != want[i] {
				t.Fatalf("sends at %v, want %v", offsets, want)
			}
		}
	})
}

func TestDeliveryPanicIsRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testSettings(t, func(c *Config) { c.WorkerCount = ptr(1) })
		attempts := 0
		s := testService(t, cfg, func(context.Context, *task) error {
			attempts++
			if attempts == 1 {
				panic("boom")
			}
			return nil
		})
		submitTest(t, s, "a")
		time.Sleep(time.Minute)
		if err := drainTest(t, s, time.Second); err != nil {
			t.Fatal(err)
		}
		if attempts != 2 || s.metrics.sent.Load() != 1 {
			t.Fatal("panic was not recovered and retried")
		}
	})
}
