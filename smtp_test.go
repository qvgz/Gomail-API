package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type testSMTP struct {
	config    SMTPConfig
	delivered atomic.Uint64
	commands  chan string
	messages  chan string
	closed    chan struct{}
	auth      chan string
}

func startTestSMTP(t *testing.T, mode string) *testSMTP {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var tlsConfig *tls.Config
	if mode == "implicit_tls" || mode == "starttls" {
		seed := httptest.NewTLSServer(nil)
		tlsConfig = seed.TLS.Clone()
		seed.Close()
		if mode == "implicit_tls" {
			listener = tls.NewListener(listener, tlsConfig)
		}
	}
	server := &testSMTP{
		config:   SMTPConfig{Server: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Email: "robot@example.com"},
		commands: make(chan string, 128), messages: make(chan string, 128),
		closed: make(chan struct{}, 128),
		auth:   make(chan string, 128),
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
				raw := conn
				defer func() { raw.Close(); mu.Lock(); delete(connections, raw); mu.Unlock(); server.closed <- struct{}{} }()
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
						} else if mode == "starttls" {
							fmt.Fprint(conn, "250-test\r\n250-STARTTLS\r\n250 AUTH PLAIN\r\n")
						} else if mode == "implicit_tls" {
							fmt.Fprint(conn, "250-test\r\n250 AUTH PLAIN\r\n")
						} else {
							fmt.Fprint(conn, "250 test\r\n")
						}
					case "STARTTLS":
						fmt.Fprint(conn, "220 ready for TLS\r\n")
						conn = tls.Server(conn, tlsConfig)
						reader = textproto.NewReader(bufio.NewReader(conn))
					case "AUTH":
						server.auth <- strings.Fields(line)[2]
						fmt.Fprint(conn, "235 authenticated\r\n")
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

func testEmailTask() *task {
	return &task{Group: "it", Recipients: []*mail.Address{{Address: "admin@example.com"}},
		Subject: "test", Body: "body\n.next", ID: "stable-test@gomail-api", CreatedAt: time.Unix(1700000000, 0)}
}

func testMailer(t *testing.T, primary SMTPConfig, backup *SMTPConfig) *mailer {
	t.Helper()
	m, err := newMailer(&settings{primary: primary, backup: backup, sendTimeout: DefaultSendTimeout}, &cooldown{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.closeConns)
	return m
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
			sender, err := newSMTPSender("test", server.config)
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
	sender, err := newSMTPSender("test", server.config)
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
	senders := testMailer(t, primary.config, &backup.config)
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
	senders := testMailer(t, primary.config, &backup.config)
	item := testEmailTask()
	if err := senders.send(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if primary.delivered.Load() != 0 || backup.delivered.Load() != 1 {
		t.Fatal("unexpected delivery counts")
	}
	parsed, err := mail.ReadMessage(strings.NewReader(<-backup.messages))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Header.Get("Message-ID") != "<"+item.ID+">" {
		t.Fatal("message identity changed")
	}
	date, err := parsed.Header.Date()
	if err != nil || !date.Equal(item.CreatedAt) {
		t.Fatal("message date changed")
	}
}

func TestSMTPHandshakeFailureClosesTransport(t *testing.T) {
	server := startTestSMTP(t, "reject_helo")
	sender, err := newSMTPSender("test", server.config)
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

func TestSMTPTLSAndAuthentication(t *testing.T) {
	for mode, port := range map[string]int{"implicit_tls": 465, "starttls": 587} {
		t.Run(mode, func(t *testing.T) {
			server := startTestSMTP(t, mode)
			cfg := server.config
			cfg.Port = port
			cfg.Email = "告警机器人 <robot@example.com>"
			cfg.Password = "test-smtp-password"
			cfg.SkipVerify = true // 模拟内网 SMTP 的自签名证书。
			sender, err := newSMTPSender("test", cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer sender.close()
			sender.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(server.config.Server, fmt.Sprint(server.config.Port)))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			for i := 0; i < 2; i++ {
				if err := sender.send(ctx, testEmailTask()); err != nil {
					t.Fatal(err)
				}
			}
			if server.delivered.Load() != 2 {
				t.Fatal("TLS messages were not delivered")
			}
			select {
			case encoded := <-server.auth:
				decoded, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil || string(decoded) != "\x00robot@example.com\x00test-smtp-password" {
					t.Fatal("SMTP authentication did not use the parsed email address")
				}
			default:
				t.Fatal("SMTP authentication was skipped")
			}
		})
	}
}

// 使用内存连接和虚拟时间，验证失效长连接不会耗尽备用 SMTP 的发送预算。
func TestSMTPStalledConnectionLeavesTimeForBackup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := SMTPConfig{Server: "smtp.example.com", Port: 25, Email: "robot@example.com"}
		primary, err := newSMTPSender("test", cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer primary.close()
		backup, err := newSMTPSender("test", cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer backup.close()
		var primaryDials, backupDeliveries int
		primary.dial = func(context.Context, string, string) (net.Conn, error) {
			primaryDials++
			client, server := net.Pipe()
			if primaryDials == 1 {
				go serveTestPipe(server, true, nil)
			} else {
				go func() { defer server.Close(); _, _ = server.Read(make([]byte, 1)) }()
			}
			return client, nil
		}
		backup.dial = func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			go serveTestPipe(server, false, func() { backupDeliveries++ })
			return client, nil
		}
		// 建立一条已经成功发送过邮件的连接，再模拟服务器挂起。
		if err := primary.send(context.Background(), testEmailTask()); err != nil {
			t.Fatal(err)
		}
		senders := &mailer{primary: primary, backup: backup, primaryDown: &cooldown{}, logger: slog.New(slog.DiscardHandler)}
		ctx, cancel := context.WithTimeout(context.Background(), 2*DefaultSendTimeout)
		defer cancel()
		started := time.Now()
		if err := senders.send(ctx, testEmailTask()); err != nil {
			t.Fatal(err)
		}
		// 探测超时后重建一次主连接，新连接仍挂起，直到主 SMTP 预算耗尽后切换备用。
		if time.Since(started) != DefaultSendTimeout || backupDeliveries != 1 || primaryDials != 2 {
			t.Fatalf("elapsed=%v backup=%d primary dials=%d", time.Since(started), backupDeliveries, primaryDials)
		}
	})
}

// 空闲连接被静默丢弃时，探测在短超时内失败并重建连接，仍由主 SMTP 完成投递。
func TestSMTPStaleConnectionReconnectsAfterProbeTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		primary, err := newSMTPSender("test", SMTPConfig{Server: "smtp.example.com", Port: 25, Email: "robot@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		defer primary.close()
		var dials, deliveries int
		primary.dial = func(context.Context, string, string) (net.Conn, error) {
			dials++
			client, server := net.Pipe()
			go serveTestPipe(server, dials == 1, func() { deliveries++ })
			return client, nil
		}
		if err := primary.send(context.Background(), testEmailTask()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), DefaultSendTimeout)
		defer cancel()
		started := time.Now()
		if err := primary.send(ctx, testEmailTask()); err != nil {
			t.Fatal(err)
		}
		if time.Since(started) != SMTPProbeTimeout || dials != 2 || deliveries != 2 {
			t.Fatalf("elapsed=%v dials=%d deliveries=%d", time.Since(started), dials, deliveries)
		}
	})
}

func serveTestPipe(conn net.Conn, stallAfterReset bool, onMessage func()) {
	defer conn.Close()
	fmt.Fprint(conn, "220 test SMTP\r\n")
	reader := textproto.NewReader(bufio.NewReader(conn))
	completed := false
	for {
		line, err := reader.ReadLine()
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return
		}
		switch fields[0] {
		case "NOOP":
			if completed && stallAfterReset {
				_, _ = reader.ReadLine()
				return
			}
			fmt.Fprint(conn, "250 OK\r\n")
		case "DATA":
			fmt.Fprint(conn, "354 send data\r\n")
			if _, err := reader.ReadDotBytes(); err != nil {
				return
			}
			if onMessage != nil {
				onMessage()
			}
			fmt.Fprint(conn, "250 accepted\r\n")
		case "RSET":
			completed = true
			fmt.Fprint(conn, "250 OK\r\n")
		default:
			fmt.Fprint(conn, "250 OK\r\n")
		}
	}
}

func TestSMTPConnectionReuse(t *testing.T) {
	server := startTestSMTP(t, "normal")
	sender, err := newSMTPSender("test", server.config)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sender.send(ctx, testEmailTask()); err != nil {
		t.Fatal(err)
	}
	original := sender.conn
	// 过期 deadline 不应导致健康连接被丢弃。
	if err := original.UpdateDeadline(-time.Second); err != nil {
		t.Fatal(err)
	}
	if err := sender.send(ctx, testEmailTask()); err != nil {
		t.Fatalf("send after expired deadline: %v", err)
	}
	if sender.conn != original {
		t.Fatal("healthy connection was not reused")
	}
	// 本地关闭底层连接，模拟服务器断开。
	_ = original.Text.Close()
	if err := sender.send(ctx, testEmailTask()); err != nil {
		t.Fatalf("send after disconnect: %v", err)
	}
	if sender.conn == original || server.delivered.Load() != 3 {
		t.Fatal("stale connection was not replaced")
	}
}

// 主 SMTP 失败后进入冷却，后续任务先走备用；冷却结束后恢复主 SMTP 优先。
func TestPrimaryCooldownPrefersBackup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := SMTPConfig{Server: "smtp.example.com", Port: 25, Email: "robot@example.com"}
		var primaryDials, primaryDeliveries, backupDeliveries int
		primaryUp := false
		m := testMailer(t, cfg, &cfg)
		m.primary.dial = func(context.Context, string, string) (net.Conn, error) {
			primaryDials++
			if !primaryUp {
				return nil, errors.New("connection refused")
			}
			client, server := net.Pipe()
			go serveTestPipe(server, false, func() { primaryDeliveries++ })
			return client, nil
		}
		m.backup.dial = func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			go serveTestPipe(server, false, func() { backupDeliveries++ })
			return client, nil
		}
		for range 3 {
			if err := m.send(context.Background(), testEmailTask()); err != nil {
				t.Fatal(err)
			}
		}
		if primaryDials != 1 || backupDeliveries != 3 || !m.primaryDown.active() {
			t.Fatalf("primary dials=%d backup=%d", primaryDials, backupDeliveries)
		}
		primaryUp = true
		time.Sleep(PrimaryCooldown)
		if err := m.send(context.Background(), testEmailTask()); err != nil {
			t.Fatal(err)
		}
		if primaryDeliveries != 1 || backupDeliveries != 3 || m.primaryDown.active() {
			t.Fatalf("primary=%d backup=%d", primaryDeliveries, backupDeliveries)
		}
	})
}

func TestBuildMessageDisplayNames(t *testing.T) {
	for _, named := range []bool{false, true} {
		from := "robot@example.com"
		rcpt := &mail.Address{Address: "admin@example.com"}
		if named {
			from = "告警机器人 <robot@example.com>"
			rcpt.Name = "运维"
		}
		msg, err := buildMessage(from, &task{Recipients: []*mail.Address{rcpt}, Subject: "告警", Body: "测试"})
		if err != nil {
			t.Fatal(err)
		}
		var buf strings.Builder
		if _, err := msg.WriteTo(&buf); err != nil {
			t.Fatal(err)
		}
		parsed, err := mail.ReadMessage(strings.NewReader(buf.String()))
		if err != nil {
			t.Fatal(err)
		}
		for header, want := range map[string][2]string{
			"From": {"robot@example.com", "告警机器人"}, "To": {"admin@example.com", "运维"},
		} {
			addr, err := mail.ParseAddress(parsed.Header.Get(header))
			if err != nil {
				t.Fatal(err)
			}
			wantName := want[0]
			if named {
				wantName = want[1]
			}
			if addr.Address != want[0] || addr.Name != wantName {
				t.Fatalf("%s = %+v", header, addr)
			}
		}
		if !named && rcpt.Name != "" {
			t.Fatal("shared recipient was modified")
		}
	}
	if _, err := buildMessage("robot@example.com", &task{Subject: "x", Body: "y"}); err == nil {
		t.Fatal("message without recipients accepted")
	}
}

func TestNormalizeBody(t *testing.T) {
	for input, want := range map[string]string{"": "", "a": "a", "a\nb\rc\r\nd": "a\r\nb\r\nc\r\nd", "\r\r\n": "\r\n\r\n"} {
		if got := normalizeBody(input); got != want {
			t.Fatalf("normalizeBody(%q) = %q, want %q", input, got, want)
		}
	}
}
