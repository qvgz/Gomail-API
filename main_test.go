package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/zap"
)

func TestBuildMessageDisplayNames(t *testing.T) {
	for _, named := range []bool{false, true} {
		from := "robot@example.com"
		rcpt := &mail.Address{Address: "admin@example.com"}
		if named {
			from = "告警机器人 <robot@example.com>"
			rcpt.Name = "运维"
		}
		msg, err := buildMessage(from, EmailTask{RecipientAddrs: []*mail.Address{rcpt}, Subject: "告警", Content: "测试"})
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, err := msg.WriteTo(&buf); err != nil {
			t.Fatal(err)
		}
		parsed, err := mail.ReadMessage(&buf)
		if err != nil {
			t.Fatal(err)
		}
		for _, header := range []string{"From", "To"} {
			addr, err := mail.ParseAddress(parsed.Header.Get(header))
			if err != nil {
				t.Fatal(err)
			}
			want := "robot@example.com"
			if header == "To" {
				want = "admin@example.com"
			}
			wantName := want
			if named {
				wantName = "告警机器人"
				if header == "To" {
					wantName = "运维"
				}
			}
			if addr.Address != want || addr.Name != wantName {
				t.Fatalf("%s = %+v, want %s / %s", header, addr, want, wantName)
			}
		}
		if !named && rcpt.Name != "" {
			t.Fatal("shared recipient was modified")
		}
	}
}

func TestSendWithRetry(t *testing.T) {
	for _, failures := range []int{0, 2, 5, 6} {
		t.Run(fmt.Sprint(failures), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				app := &App{logger: zap.NewNop()}
				start := time.Now()
				attempts := 0
				failure := errors.New("SMTP unavailable")
				var previous context.Context
				err := app.sendWithRetry(context.Background(), 0, EmailTask{}, func(ctx context.Context, _ EmailTask) error {
					if previous != nil && previous.Err() == nil {
						t.Fatal("previous attempt context was not canceled")
					}
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) != 60*time.Second {
						t.Fatal("attempt does not have a fresh timeout")
					}
					previous = ctx
					if elapsed := time.Since(start); elapsed != time.Duration(attempts)*3*time.Second {
						t.Fatalf("attempt %d at %v", attempts+1, elapsed)
					}
					attempts++
					if attempts <= failures {
						return failure
					}
					return nil
				})
				wantAttempts := min(failures+1, 6)
				if attempts != wantAttempts || (err != nil) != (failures == 6) {
					t.Fatalf("attempts=%d error=%v", attempts, err)
				}
				if err != nil && !errors.Is(err, failure) {
					t.Fatalf("lost final error: %v", err)
				}
			})
		})
	}
}

func TestSMTPConnectionReuse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var dropNextNoop atomic.Bool
	var handlers sync.WaitGroup
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				fmt.Fprint(conn, "220 test SMTP\r\n")
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "NOOP") && dropNextNoop.CompareAndSwap(true, false):
						return
					case strings.HasPrefix(line, "QUIT"):
						fmt.Fprint(conn, "221 bye\r\n")
						return
					case strings.HasPrefix(line, "DATA"):
						fmt.Fprint(conn, "354 send data\r\n")
						for {
							line, err = reader.ReadString('\n')
							if err != nil {
								return
							}
							if line == ".\r\n" {
								break
							}
						}
						fmt.Fprint(conn, "250 queued\r\n")
					default:
						fmt.Fprint(conn, "250 OK\r\n")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-serverDone
		handlers.Wait()
	})
	sender, err := newSMTPSender(SMTPConfig{Server: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Email: "robot@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	task := EmailTask{RecipientAddrs: []*mail.Address{{Address: "admin@example.com"}}, Subject: "test", Content: "body"}
	if err := sender.send(ctx, task); err != nil {
		t.Fatal(err)
	}
	original := sender.conn
	if err := original.UpdateDeadline(-time.Second); err != nil {
		t.Fatal(err)
	}
	if err := sender.send(ctx, task); err != nil {
		t.Fatalf("send after expired deadline: %v", err)
	}
	if sender.conn != original {
		t.Fatal("healthy connection was not reused")
	}
	dropNextNoop.Store(true)
	if err := sender.send(ctx, task); err != nil {
		t.Fatalf("send after disconnect: %v", err)
	}
	if sender.conn == original || original.HasConnection() {
		t.Fatal("stale connection was not replaced and closed")
	}
}
