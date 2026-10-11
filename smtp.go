package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"strings"
	"time"
	"unicode"

	gomail "github.com/wneessen/go-mail"
	gomailsmtp "github.com/wneessen/go-mail/smtp"
	"go.uber.org/zap"
)

type smtpSender struct {
	client *gomail.Client
	conn   *gomailsmtp.Client
	from   string
	config SMTPConfig
	logger *zap.Logger
	dial   gomail.DialContextFunc
}

type workerSenders struct {
	cfg     *runtimeConfig
	logger  *zap.Logger
	primary *smtpSender
	backup  *smtpSender
}

func newWorkerSenders(cfg *runtimeConfig, logger *zap.Logger) (*workerSenders, error) {
	primary, err := newSMTPSender(cfg.raw.PrimarySMTP)
	if err != nil {
		return nil, fmt.Errorf("create primary smtp sender: %w", err)
	}

	ws := &workerSenders{
		cfg:     cfg,
		logger:  logger,
		primary: primary,
	}
	primary.logger = logger

	if cfg.raw.BackupSMTP != nil {
		backup, err := newSMTPSender(*cfg.raw.BackupSMTP)
		if err != nil {
			primary.close()
			return nil, fmt.Errorf("create backup smtp sender: %w", err)
		}
		ws.backup = backup
		backup.logger = logger
	}

	return ws, nil
}

func (w *workerSenders) close() {
	if w.primary != nil {
		w.primary.close()
	}
	if w.backup != nil {
		w.backup.close()
	}
}

func (w *workerSenders) send(ctx context.Context, task EmailTask) error {
	// 给每个服务器独立的预算，主服务器不响应时仍给备用服务器留出时间。
	send := func(sender *smtpSender) error {
		attemptCtx, cancel := context.WithTimeout(ctx, SMTPTimeout)
		defer cancel()
		return sender.send(attemptCtx, task)
	}
	err := send(w.primary)
	if err != nil && w.backup != nil && ctx.Err() == nil {
		w.logger.Warn("primary failed, trying backup", zap.Error(err), zap.String("group", task.TargetGroup))
		return send(w.backup)
	}
	return err
}

func newSMTPSender(sc SMTPConfig) (*smtpSender, error) {
	client, err := newSMTPClient(sc, nil)
	if err != nil {
		return nil, err
	}
	return &smtpSender{
		client: client, from: sc.Email, config: sc, logger: zap.NewNop(),
		dial: (&net.Dialer{}).DialContext,
	}, nil
}

func smtpTLSConfig(sc SMTPConfig) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: sc.SkipVerify, ServerName: sc.Server}
}

func newSMTPClient(sc SMTPConfig, dial gomail.DialContextFunc) (*gomail.Client, error) {
	tlsCfg := smtpTLSConfig(sc)
	opts := []gomail.Option{
		gomail.WithPort(sc.Port),
		gomail.WithTimeout(SMTPTimeout),
		gomail.WithTLSConfig(tlsCfg),
	}
	if dial != nil {
		opts = append(opts, gomail.WithDialContextFunc(dial))
	}

	switch sc.Port {
	case 465:
		opts = append(opts, gomail.WithSSL())
	case 587:
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	default:
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSOpportunistic))
	}

	if sc.Password != "" {
		addr, err := mail.ParseAddress(sc.Email)
		if err != nil {
			return nil, err
		}
		opts = append(opts,
			gomail.WithSMTPAuth(gomail.SMTPAuthPlain),
			gomail.WithUsername(addr.Address),
			gomail.WithPassword(sc.Password),
		)
	}

	return gomail.NewClient(sc.Server, opts...)
}

func (s *smtpSender) send(ctx context.Context, task EmailTask) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	msg, err := buildMessage(s.from, task)
	if err != nil {
		return err
	}

	conn, err := s.ensureConn(ctx)
	if err != nil {
		return err
	}

	stop := interruptOnCancel(ctx, conn.Text)
	err = s.client.SendWithSMTPClient(conn, msg)
	stop()
	if err != nil || ctx.Err() != nil {
		s.close()
		// DATA 的 250 确认已收到，后续 RSET 失败不应再投递同一封邮件。
		if msg.IsDelivered() {
			s.logger.Warn("message accepted, SMTP cleanup failed", zap.String("message_id", task.MessageID), zap.Error(err))
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func (s *smtpSender) ensureConn(ctx context.Context) (*gomailsmtp.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.conn != nil {
		// HasConnection 只反映本地状态；先刷新旧 deadline，再探测服务器是否已断开。
		stop := interruptOnCancel(ctx, s.conn.Text)
		alive := s.conn.HasConnection() && s.conn.UpdateDeadline(SMTPTimeout) == nil && s.conn.Noop() == nil
		stop()
		if alive && ctx.Err() == nil {
			return s.conn, nil
		}
		s.close()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 库的 Context 仅用于拨号，握手/认证失败也不总是关闭底层连接。
	// 在拨号后立即设置 deadline，并将完整握手绑定到调用方 Context。
	var raw net.Conn
	var stop func()
	client, err := newSMTPClient(s.config, func(dialCtx context.Context, network, _ string) (net.Conn, error) {
		address := net.JoinHostPort(s.config.Server, fmt.Sprint(s.config.Port))
		conn, err := s.dial(dialCtx, network, address)
		if err != nil {
			return nil, err
		}
		raw = conn
		stop = interruptOnCancel(ctx, conn)
		deadline := time.Now().Add(SMTPTimeout)
		if d, ok := dialCtx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
		if s.config.Port == 465 {
			return tls.Client(conn, smtpTLSConfig(s.config)), nil
		}
		return conn, nil
	})
	if err != nil {
		return nil, err
	}
	conn, err := client.DialToSMTPClientWithContext(ctx)
	if stop != nil {
		stop()
	}
	if err != nil {
		if raw != nil {
			_ = raw.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if ctx.Err() != nil {
		_ = conn.Close()
		return nil, ctx.Err()
	}
	s.client = client
	s.conn = conn
	return s.conn, nil
}

func (s *smtpSender) close() {
	if s.conn == nil {
		return
	}
	// 清理时直接关闭，避免 QUIT/RSET 等待已失效或不响应的服务器。
	_ = s.conn.Close()
	s.conn = nil
}

func interruptOnCancel(ctx context.Context, conn io.Closer) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		// 直接关闭 transport；smtp.Client.Close 会等待正在进行 I/O 的内部锁。
		_ = conn.Close()
		close(done)
	})
	return func() {
		if !stop() {
			<-done
		}
	}
}

func buildMessage(from string, task EmailTask) (*gomail.Msg, error) {
	msg := gomail.NewMsg()
	fromAddr, err := mail.ParseAddress(from)
	if err != nil {
		return nil, err
	}
	msg.FromMailAddress(addressWithDisplayName(fromAddr))
	recipients := make([]*mail.Address, 0, len(task.RecipientAddrs))
	for _, addr := range task.RecipientAddrs {
		if addr != nil {
			recipients = append(recipients, addressWithDisplayName(addr))
		}
	}
	msg.ToMailAddress(recipients...)
	if len(recipients) == 0 {
		return nil, errors.New("message has no recipients")
	}
	if task.MessageID != "" {
		msg.SetMessageIDWithValue(task.MessageID)
	}
	if !task.CreatedAt.IsZero() {
		msg.SetDateWithValue(task.CreatedAt)
	}
	msg.Subject(task.Subject)
	msg.SetBodyString(gomail.TypeTextPlain, normalizeBody(task.Content))
	return msg, nil
}

func addressWithDisplayName(addr *mail.Address) *mail.Address {
	copy := *addr
	if copy.Name == "" {
		copy.Name = copy.Address
	}
	return &copy
}

func normalizeBody(body string) string {
	if body == "" {
		return body
	}

	needsNormalize := false
	for i := 0; i < len(body); i++ {
		if body[i] == '\n' || body[i] == '\r' {
			needsNormalize = true
			break
		}
	}
	if !needsNormalize {
		return body
	}

	var b strings.Builder
	b.Grow(len(body) + strings.Count(body, "\n"))
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '\r':
			if i+1 < len(body) && body[i+1] == '\n' {
				i++
			}
			b.WriteString("\r\n")
		case '\n':
			b.WriteString("\r\n")
		default:
			b.WriteByte(body[i])
		}
	}

	return b.String()
}

func sanitizeHeader(v string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, v)
}

func runDeepCheck(cfg *runtimeConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	primary, err := newSMTPSender(cfg.raw.PrimarySMTP)
	if err != nil {
		return err
	}
	defer primary.close()

	if _, err := primary.ensureConn(ctx); err != nil {
		return err
	}

	if cfg.raw.BackupSMTP != nil {
		backup, err := newSMTPSender(*cfg.raw.BackupSMTP)
		if err != nil {
			return fmt.Errorf("backup setup: %w", err)
		}
		defer backup.close()

		if _, err := backup.ensureConn(ctx); err != nil {
			return fmt.Errorf("backup connect: %w", err)
		}
	}

	return nil
}
