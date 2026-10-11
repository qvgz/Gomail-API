package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

type testSMTP struct {
	config    SMTPConfig
	delivered atomic.Uint64
	commands  chan string
	messages  chan string
	closed    chan struct{}
}

func startTestSMTP(t *testing.T, mode string) *testSMTP {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &testSMTP{
		config:   SMTPConfig{Server: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Email: "robot@example.com"},
		commands: make(chan string, 128), messages: make(chan string, 128),
		closed: make(chan struct{}, 128),
	}
	var mu sync.Mutex
	connections := make(map[net.Conn]bool)
	var wg sync.WaitGroup
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections[conn] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { conn.Close(); mu.Lock(); delete(connections, conn); mu.Unlock(); server.closed <- struct{}{} }()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				server.commands <- "connect"
				reader := textproto.NewReader(bufio.NewReader(conn))
				if mode == "silent_greeting" {
					_, _ = reader.ReadLine()
					return
				}
				fmt.Fprint(conn, "220 test SMTP\r\n")
				for {
					line, err := reader.ReadLine()
					if err != nil {
						return
					}
					command := strings.Fields(line)[0]
					server.commands <- command
					switch command {
					case "EHLO", "HELO":
						if mode == "silent_helo" {
							_, _ = reader.ReadLine()
							return
						}
						if mode == "reject_helo" {
							fmt.Fprint(conn, "550 rejected\r\n")
						} else {
							fmt.Fprint(conn, "250 test\r\n")
						}
					case "RCPT":
						if mode == "reject_rcpt" {
							fmt.Fprint(conn, "550 no such user\r\n")
						} else {
							fmt.Fprint(conn, "250 OK\r\n")
						}
					case "DATA":
						if mode == "silent_data" {
							_, _ = reader.ReadLine()
							return
						}
						fmt.Fprint(conn, "354 send data\r\n")
						data, err := reader.ReadDotBytes()
						if err != nil {
							return
						}
						server.delivered.Add(1)
						server.messages <- string(data)
						fmt.Fprint(conn, "250 accepted\r\n")
					case "RSET":
						if mode == "reset_failure" {
							return
						}
						fmt.Fprint(conn, "250 OK\r\n")
					default:
						fmt.Fprint(conn, "250 OK\r\n")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-stopped
		mu.Lock()
		for conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return server
}

func testEmailTask() EmailTask {
	return EmailTask{TargetGroup: "it", RecipientAddrs: []*mail.Address{{Address: "admin@example.com"}},
		Subject: "test", Content: "body\n.next", MessageID: "stable-test@gomail-api", CreatedAt: time.Unix(1700000000, 0)}
}

func waitSMTPCommand(t *testing.T, server *testSMTP, command string) {
	t.Helper()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case got := <-server.commands:
			if got == command {
				return
			}
		case <-timeout.C:
			t.Fatalf("waiting for SMTP %s", command)
		}
	}
}

func TestSMTPCancellation(t *testing.T) {
	for mode, stage := range map[string]string{"silent_greeting": "connect", "silent_helo": "EHLO", "silent_data": "DATA"} {
		t.Run(mode, func(t *testing.T) {
			server := startTestSMTP(t, mode)
			sender, err := newSMTPSender(server.config)
			if err != nil {
				t.Fatal(err)
			}
			defer sender.close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- sender.send(ctx, testEmailTask()) }()
			waitSMTPCommand(t, server, stage)
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("got %v, want canceled", err)
				}
				if sender.conn != nil {
					t.Fatal("canceled connection retained")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("SMTP cancellation did not interrupt I/O")
			}
		})
	}
}

func TestSMTPHandshakeDeadline(t *testing.T) {
	server := startTestSMTP(t, "silent_greeting")
	sender, err := newSMTPSender(server.config)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = sender.send(ctx, testEmailTask())
	if err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("deadline not enforced: %v", err)
	}
}

func TestSMTPAcceptedCleanupFailureDoesNotFailover(t *testing.T) {
	primary := startTestSMTP(t, "reset_failure")
	backup := startTestSMTP(t, "normal")
	senders, err := newWorkerSenders(&runtimeConfig{raw: &Config{PrimarySMTP: primary.config, BackupSMTP: &backup.config}}, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer senders.close()
	if err := senders.send(context.Background(), testEmailTask()); err != nil {
		t.Fatal(err)
	}
	if primary.delivered.Load() != 1 || backup.delivered.Load() != 0 || senders.primary.conn != nil {
		t.Fatal("accepted message was failed over or broken connection retained")
	}
}

func TestSMTPFailoverAndStableMessage(t *testing.T) {
	primary := startTestSMTP(t, "reject_rcpt")
	backup := startTestSMTP(t, "normal")
	senders, err := newWorkerSenders(&runtimeConfig{raw: &Config{PrimarySMTP: primary.config, BackupSMTP: &backup.config}}, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer senders.close()
	task := testEmailTask()
	if err := senders.send(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if primary.delivered.Load() != 0 || backup.delivered.Load() != 1 {
		t.Fatal("unexpected delivery counts")
	}
	parsed, err := mail.ReadMessage(strings.NewReader(<-backup.messages))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Header.Get("Message-ID") != "<"+task.MessageID+">" {
		t.Fatal("message identity changed")
	}
	date, err := parsed.Header.Date()
	if err != nil || !date.Equal(task.CreatedAt) {
		t.Fatal("message date changed")
	}
}

func TestSMTPHandshakeFailureClosesTransport(t *testing.T) {
	server := startTestSMTP(t, "reject_helo")
	sender, err := newSMTPSender(server.config)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.close()
	if err := sender.send(context.Background(), testEmailTask()); err == nil {
		t.Fatal("expected handshake failure")
	}
	if sender.conn != nil {
		t.Fatal("failed connection retained")
	}
	select {
	case <-server.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("failed handshake leaked transport")
	}
}
