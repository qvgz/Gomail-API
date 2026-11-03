package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/mail"
	"strings"
	"sync/atomic"
	"time"

	gomail "github.com/wneessen/go-mail"
	gomailsmtp "github.com/wneessen/go-mail/smtp"
)

// cooldown 在主 SMTP 失败后的一段时间内让所有 worker 优先使用备用 SMTP，
// 避免主服务器挂起时每封邮件都先耗尽主服务器的超时。
type cooldown struct {
	until atomic.Int64
}

func (c *cooldown) active() bool { return time.Now().UnixNano() < c.until.Load() }
func (c *cooldown) trip()        { c.until.Store(time.Now().Add(PrimaryCooldown).UnixNano()) }
func (c *cooldown) reset()       { c.until.Store(0) }

// mailer 属于单个 worker，不并发使用；primaryDown 在 worker 间共享。
type mailer struct {
	primary     *smtpSender
	backup      *smtpSender
	primaryDown *cooldown
	logger      *slog.Logger
}

func newMailer(cfg *settings, primaryDown *cooldown, logger *slog.Logger) (*mailer, error) {
	primary, err := newSMTPSender("primary", cfg.primary)
	if err != nil {
		return nil, fmt.Errorf("primary smtp: %w", err)
	}
	primary.logger, primary.timeout = logger, cfg.sendTimeout
	m := &mailer{primary: primary, primaryDown: primaryDown, logger: logger}
	if cfg.backup != nil {
		if m.backup, err = newSMTPSender("backup", *cfg.backup); err != nil {
			return nil, fmt.Errorf("backup smtp: %w", err)
		}
		m.backup.logger, m.backup.timeout = logger, cfg.sendTimeout
	}
	return m, nil
}

// send 依次尝试主、备 SMTP，每个服务器有独立的超时预算。
func (m *mailer) send(ctx context.Context, t *task) error {
	order := []*smtpSender{m.primary}
	if m.backup != nil {
		if m.primaryDown.active() {
			order = []*smtpSender{m.backup, m.primary}
		} else {
			order = append(order, m.backup)
		}
	}
	var errs []error
	for i, sender := range order {
		if ctx.Err() != nil {
			break
		}
		attemptCtx, cancel := context.WithTimeout(ctx, sender.timeout)
		err := sender.send(attemptCtx, t)
		cancel()
		if err == nil {
			if sender == m.primary {
				m.primaryDown.reset()
			}
			return nil
		}
		if ctx.Err() != nil {
			break
		}
		if sender == m.primary && m.backup != nil {
			m.primaryDown.trip()
		}
		errs = append(errs, fmt.Errorf("%s: %w", sender.name, err))
		if i+1 < len(order) {
			m.logger.Warn("smtp failed, trying next server", "message_id", t.ID,
				"server", sender.name, "error", err)
		}
	}
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (m *mailer) closeConns() {
	m.primary.close()
	if m.backup != nil {
		m.backup.close()
	}
}

// smtpSender 维护到单个 SMTP 服务器的一条可复用连接。
type smtpSender struct {
	name    string
	config  SMTPConfig
	timeout time.Duration // 单次投递上限
	client  *gomail.Client
	conn    *gomailsmtp.Client
	dial    gomail.DialContextFunc
	logger  *slog.Logger
}

func newSMTPSender(name string, sc SMTPConfig) (*smtpSender, error) {
	client, err := newSMTPClient(sc, DefaultSendTimeout, nil)
	if err != nil {
		return nil, err
	}
	return &smtpSender{name: name, config: sc, timeout: DefaultSendTimeout, client: client,
		dial: (&net.Dialer{}).DialContext, logger: slog.New(slog.DiscardHandler)}, nil
}

func smtpTLSConfig(sc SMTPConfig) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: sc.SkipVerify, ServerName: sc.Server}
}

func newSMTPClient(sc SMTPConfig, timeout time.Duration, dial gomail.DialContextFunc) (*gomail.Client, error) {
	opts := []gomail.Option{
		gomail.WithPort(sc.Port),
		gomail.WithTimeout(timeout),
		gomail.WithTLSConfig(smtpTLSConfig(sc)),
		// ensureConn 已探测复用连接，跳过库在每次发送前的重复 NOOP。
		gomail.WithoutNoop(),
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

func (s *smtpSender) send(ctx context.Context, t *task) error {
	msg, err := buildMessage(s.config.Email, t)
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
	if err == nil && ctx.Err() == nil {
		return nil
	}
	s.close()
	// DATA 已收到 250 确认，之后的 RSET 失败不应导致重复投递。
	if msg.IsDelivered() {
		s.logger.Warn("message accepted, smtp cleanup failed", "message_id", t.ID, "error", err)
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (s *smtpSender) ensureConn(ctx context.Context) (*gomailsmtp.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.conn != nil {
		// HasConnection 只反映本地状态；用短 deadline 探测，空闲连接被静默丢弃时尽快重建，
		// 避免探测耗尽本服务器的发送预算。发送前库会将 deadline 恢复为 s.timeout。
		stop := interruptOnCancel(ctx, s.conn.Text)
		alive := s.conn.HasConnection() && s.conn.UpdateDeadline(SMTPProbeTimeout) == nil && s.conn.Noop() == nil
		stop()
		if alive && ctx.Err() == nil {
			return s.conn, nil
		}
		s.close()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	// 库的 Context 仅用于拨号，握手/认证失败也不总是关闭底层连接。
	// 拨号后立即设置 deadline，并将完整握手绑定到调用方 Context。
	var raw net.Conn
	stop := func() {}
	client, err := newSMTPClient(s.config, s.timeout, func(dialCtx context.Context, network, _ string) (net.Conn, error) {
		address := net.JoinHostPort(s.config.Server, fmt.Sprint(s.config.Port))
		conn, err := s.dial(dialCtx, network, address)
		if err != nil {
			return nil, err
		}
		raw = conn
		stop = interruptOnCancel(ctx, conn)
		deadline := time.Now().Add(s.timeout)
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
	stop()
	if err == nil && ctx.Err() != nil {
		_ = conn.Close()
		err = ctx.Err()
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
	s.client, s.conn = client, conn
	return conn, nil
}

func (s *smtpSender) close() {
	if s.conn == nil {
		return
	}
	// 直接关闭，避免 QUIT 等待已失效或不响应的服务器。
	_ = s.conn.Close()
	s.conn = nil
}

// interruptOnCancel 在 ctx 取消时关闭连接以打断阻塞 I/O；返回的 stop 会等待关闭完成。
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

func buildMessage(from string, t *task) (*gomail.Msg, error) {
	fromAddr, err := mail.ParseAddress(from)
	if err != nil {
		return nil, err
	}
	recipients := make([]*mail.Address, 0, len(t.Recipients))
	for _, addr := range t.Recipients {
		if addr != nil {
			recipients = append(recipients, withDisplayName(addr))
		}
	}
	if len(recipients) == 0 {
		return nil, errors.New("message has no recipients")
	}
	msg := gomail.NewMsg()
	msg.FromMailAddress(withDisplayName(fromAddr))
	msg.ToMailAddress(recipients...)
	// 重试和切换服务器时保持 Message-ID 与日期不变，便于追踪和去重。
	if t.ID != "" {
		msg.SetMessageIDWithValue(t.ID)
	}
	if !t.CreatedAt.IsZero() {
		msg.SetDateWithValue(t.CreatedAt)
	}
	msg.Subject(t.Subject)
	msg.SetBodyString(gomail.TypeTextPlain, normalizeBody(t.Body))
	return msg, nil
}

// withDisplayName 在无显示名时使用邮箱地址，避免部分客户端显示开头的 `<`。
func withDisplayName(addr *mail.Address) *mail.Address {
	c := *addr
	if c.Name == "" {
		c.Name = c.Address
	}
	return &c
}

// normalizeBody 将 \n、\r 统一为 CRLF。
func normalizeBody(body string) string {
	if !strings.ContainsAny(body, "\r\n") {
		return body
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	return strings.ReplaceAll(body, "\n", "\r\n")
}

func checkSMTP(cfg *settings) error {
	servers := []struct {
		name string
		sc   *SMTPConfig
	}{{"primary", &cfg.primary}, {"backup", cfg.backup}}
	for _, server := range servers {
		if server.sc == nil {
			continue
		}
		if err := func() error {
			sender, err := newSMTPSender(server.name, *server.sc)
			if err != nil {
				return err
			}
			defer sender.close()
			ctx, cancel := context.WithTimeout(context.Background(), CheckTimeout)
			defer cancel()
			_, err = sender.ensureConn(ctx)
			return err
		}(); err != nil {
			return fmt.Errorf("%s smtp: %w", server.name, err)
		}
	}
	return nil
}
