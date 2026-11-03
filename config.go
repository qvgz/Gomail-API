package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"os"
	"strings"
	"time"
	"unicode"
)

type SMTPConfig struct {
	Server     string `json:"server"`
	Port       int    `json:"port"`
	Email      string `json:"email"`
	Password   string `json:"password"`
	SkipVerify bool   `json:"skip_verify"`
}

// 数值字段缺省时使用默认值；显式给出的值按字面生效（如 0 表示不限速、不重试）。
type Config struct {
	HTTPPort          *int                `json:"http_port,omitempty"`
	PrimarySMTP       SMTPConfig          `json:"primary_smtp"`
	BackupSMTP        *SMTPConfig         `json:"backup_smtp"`
	RecipientGroups   map[string][]string `json:"recipient_groups"`
	RateLimitInterval *int                `json:"rate_limit_interval,omitempty"`
	MaxRetries        *int                `json:"max_retries,omitempty"`
	SendTimeout       *int                `json:"send_timeout,omitempty"`
	QueueCapacity     *int                `json:"queue_capacity,omitempty"`
	WorkerCount       *int                `json:"worker_count,omitempty"`
}

// settings 是校验并补齐默认值后的只读配置。
type settings struct {
	httpPort      int
	primary       SMTPConfig
	backup        *SMTPConfig
	groups        map[string][]*mail.Address
	sendInterval  time.Duration
	sendTimeout   time.Duration
	maxRetries    int
	queueCapacity int
	workers       int
}

func loadConfig(path string) (*settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("config must contain a single JSON object")
	}
	// 解码后再展开环境变量，变量值中的引号、反斜杠不会破坏 JSON。
	fields := []*string{&cfg.PrimarySMTP.Server, &cfg.PrimarySMTP.Email, &cfg.PrimarySMTP.Password}
	if cfg.BackupSMTP != nil {
		fields = append(fields, &cfg.BackupSMTP.Server, &cfg.BackupSMTP.Email, &cfg.BackupSMTP.Password)
	}
	for _, recipients := range cfg.RecipientGroups {
		for i := range recipients {
			fields = append(fields, &recipients[i])
		}
	}
	for _, field := range fields {
		if err := expandEnv(field); err != nil {
			return nil, err
		}
	}
	return newSettings(cfg)
}

func expandEnv(value *string) error {
	var missing string
	*value = os.Expand(*value, func(name string) string {
		v, ok := os.LookupEnv(name)
		if !ok && missing == "" {
			missing = name
		}
		return v
	})
	if missing != "" {
		return fmt.Errorf("config references unset environment variable %q", missing)
	}
	return nil
}

func newSettings(cfg Config) (*settings, error) {
	if err := validateSMTP("primary_smtp", cfg.PrimarySMTP); err != nil {
		return nil, err
	}
	s := &settings{primary: cfg.PrimarySMTP}
	if cfg.BackupSMTP != nil {
		if err := validateSMTP("backup_smtp", *cfg.BackupSMTP); err != nil {
			return nil, err
		}
		backup := *cfg.BackupSMTP
		s.backup = &backup
	}

	var rateLimit, sendTimeout int
	ints := []struct {
		name               string
		value              *int
		target             *int
		min, max, fallback int
	}{
		{"http_port", cfg.HTTPPort, &s.httpPort, 1, 65535, 8080},
		{"rate_limit_interval", cfg.RateLimitInterval, &rateLimit, 0, 86400, 1},
		{"max_retries", cfg.MaxRetries, &s.maxRetries, 0, 10, 2},
		{"send_timeout", cfg.SendTimeout, &sendTimeout, 1, 300, int(DefaultSendTimeout / time.Second)},
		{"queue_capacity", cfg.QueueCapacity, &s.queueCapacity, 1, 100000, 1000},
		{"worker_count", cfg.WorkerCount, &s.workers, 1, 64, 4},
	}
	for _, field := range ints {
		*field.target = field.fallback
		if field.value == nil {
			continue
		}
		if *field.value < field.min || *field.value > field.max {
			return nil, fmt.Errorf("%s must be between %d and %d", field.name, field.min, field.max)
		}
		*field.target = *field.value
	}
	s.sendInterval = time.Duration(rateLimit) * time.Second
	s.sendTimeout = time.Duration(sendTimeout) * time.Second

	if len(cfg.RecipientGroups) == 0 {
		return nil, errors.New("recipient_groups is empty")
	}
	s.groups = make(map[string][]*mail.Address, len(cfg.RecipientGroups))
	for name, recipients := range cfg.RecipientGroups {
		if strings.TrimSpace(name) == "" || len(name) > MaxHeaderLength || hasControl(name) {
			return nil, errors.New("recipient group name is empty, too long or contains control characters")
		}
		parsed, err := parseRecipients(recipients)
		if err != nil {
			return nil, fmt.Errorf("recipient_groups.%s: %w", name, err)
		}
		s.groups[name] = parsed
	}
	return s, nil
}

// parseRecipients 校验地址并按实际邮箱去重，保留首次出现的显示名。
func parseRecipients(recipients []string) ([]*mail.Address, error) {
	if len(recipients) == 0 {
		return nil, errors.New("group is empty")
	}
	parsed := make([]*mail.Address, 0, len(recipients))
	seen := make(map[string]bool, len(recipients))
	for _, rcpt := range recipients {
		if hasControl(rcpt) {
			return nil, errors.New("address contains control characters")
		}
		addr, err := mail.ParseAddress(strings.TrimSpace(rcpt))
		if err != nil {
			return nil, fmt.Errorf("invalid address %q: %w", rcpt, err)
		}
		if !seen[addr.Address] {
			seen[addr.Address] = true
			parsed = append(parsed, addr)
		}
	}
	return parsed, nil
}

func validateSMTP(name string, sc SMTPConfig) error {
	if sc.Server == "" || strings.ContainsAny(sc.Server, " /\\") || hasControl(sc.Server) {
		return fmt.Errorf("invalid %s.server: expected hostname or IP address", name)
	}
	if strings.Contains(sc.Server, ":") && net.ParseIP(sc.Server) == nil {
		return fmt.Errorf("invalid %s.server: specify the port in the port field", name)
	}
	if sc.Port <= 0 || sc.Port > 65535 {
		return fmt.Errorf("invalid %s.port", name)
	}
	if hasControl(sc.Email) {
		return fmt.Errorf("invalid %s.email: control characters", name)
	}
	if _, err := mail.ParseAddress(sc.Email); err != nil {
		return fmt.Errorf("invalid %s.email", name)
	}
	return nil
}

func hasControl(v string) bool {
	return strings.IndexFunc(v, unicode.IsControl) >= 0
}
