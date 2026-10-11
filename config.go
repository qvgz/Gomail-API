package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"os"
	"runtime"
	"strings"
	"time"
)

type SMTPConfig struct {
	Server     string `json:"server"`
	Port       int    `json:"port"`
	Email      string `json:"email"`
	Password   string `json:"password"`
	SkipVerify bool   `json:"skip_verify"`
}

type Config struct {
	PrimarySMTP       SMTPConfig          `json:"primary_smtp"`
	BackupSMTP        *SMTPConfig         `json:"backup_smtp"`
	RecipientGroups   map[string][]string `json:"recipient_groups"`
	RateLimitInterval int                 `json:"rate_limit_interval"`
	QueueCapacity     int                 `json:"queue_capacity"`
	WorkerCount       int                 `json:"worker_count"`
	HTTPPort          int                 `json:"http_port"`
}

type recipientGroup struct {
	parsed []*mail.Address
}

type runtimeConfig struct {
	raw       *Config
	groups    map[string]recipientGroup
	rateLimit time.Duration
}

func loadAndValidateConfig(path string) (*runtimeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("config must contain a single JSON object")
	}
	// 在 JSON 解码后展开字符串，避免环境变量中的引号、反斜杠破坏 JSON。
	expand := func(value *string) error {
		var missing string
		*value = os.Expand(*value, func(name string) string {
			v, ok := os.LookupEnv(name)
			if !ok {
				missing = name
			}
			return v
		})
		if missing != "" {
			return fmt.Errorf("config references unset environment variable %q", missing)
		}
		return nil
	}
	expandSMTP := func(sc *SMTPConfig) error {
		for _, value := range []*string{&sc.Server, &sc.Email, &sc.Password} {
			if err := expand(value); err != nil {
				return err
			}
		}
		return nil
	}
	if err := expandSMTP(&cfg.PrimarySMTP); err != nil {
		return nil, err
	}
	if cfg.BackupSMTP != nil {
		if err := expandSMTP(cfg.BackupSMTP); err != nil {
			return nil, err
		}
	}
	for _, recipients := range cfg.RecipientGroups {
		for i := range recipients {
			if err := expand(&recipients[i]); err != nil {
				return nil, err
			}
		}
	}

	return buildRuntimeConfig(&cfg)
}

func buildRuntimeConfig(cfg *Config) (*runtimeConfig, error) {
	if cfg == nil {
		return nil, errors.New("config is nil")
	}
	// 保存独立副本，运行配置发布后不再修改。
	copy := *cfg
	cfg = &copy
	if err := validateSMTP("primary_smtp", cfg.PrimarySMTP); err != nil {
		return nil, err
	}
	if cfg.BackupSMTP != nil {
		backup := *cfg.BackupSMTP
		cfg.BackupSMTP = &backup
		if err := validateSMTP("backup_smtp", backup); err != nil {
			return nil, err
		}
	}
	if cfg.HTTPPort < 0 || cfg.HTTPPort > 65535 {
		return nil, errors.New("http_port must be between 0 and 65535")
	}
	if cfg.QueueCapacity < 0 || cfg.QueueCapacity > 100000 {
		return nil, errors.New("queue_capacity must be between 0 and 100000")
	}
	if cfg.WorkerCount < 0 || cfg.WorkerCount > 1024 {
		return nil, errors.New("worker_count must be between 0 and 1024")
	}
	if cfg.RateLimitInterval < 0 || cfg.RateLimitInterval > 86400 {
		return nil, errors.New("rate_limit_interval must be between 0 and 86400 seconds")
	}
	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = 8080
	}
	if cfg.QueueCapacity == 0 {
		cfg.QueueCapacity = 1000
	}
	if cfg.WorkerCount == 0 {
		cfg.WorkerCount = min(runtime.NumCPU()*2, 1024)
	}

	if len(cfg.RecipientGroups) == 0 {
		return nil, errors.New("recipient_groups is empty")
	}

	groups := make(map[string]recipientGroup, len(cfg.RecipientGroups))
	rawGroups := make(map[string][]string, len(cfg.RecipientGroups))
	for name, recipients := range cfg.RecipientGroups {
		if strings.TrimSpace(name) == "" || len(name) > MaxHeaderLength || sanitizeHeader(name) != name {
			return nil, errors.New("recipient group name is empty, too long or contains control characters")
		}
		if len(recipients) == 0 {
			return nil, fmt.Errorf("recipient_groups.%s is empty", name)
		}

		parsed := make([]*mail.Address, 0, len(recipients))
		seen := make(map[string]bool, len(recipients))
		for _, rcpt := range recipients {
			if sanitizeHeader(rcpt) != rcpt {
				return nil, fmt.Errorf("recipient_groups.%s has control characters in address", name)
			}
			addr := strings.TrimSpace(rcpt)
			if addr == "" {
				return nil, fmt.Errorf("recipient_groups.%s has empty address", name)
			}

			parsedAddr, err := mail.ParseAddress(addr)
			if err != nil {
				return nil, fmt.Errorf("recipient_groups.%s has invalid address %q: %w", name, addr, err)
			}
			if !seen[parsedAddr.Address] {
				parsed = append(parsed, parsedAddr)
				seen[parsedAddr.Address] = true
			}
		}

		groups[name] = recipientGroup{parsed: parsed}
		rawGroups[name] = append([]string(nil), recipients...)
	}
	cfg.RecipientGroups = rawGroups

	return &runtimeConfig{
		raw:       cfg,
		groups:    groups,
		rateLimit: time.Duration(cfg.RateLimitInterval) * time.Second,
	}, nil
}

func validateSMTP(name string, cfg SMTPConfig) error {
	if cfg.Server == "" || strings.ContainsAny(cfg.Server, " /\\\t\r\n") || sanitizeHeader(cfg.Server) != cfg.Server {
		return fmt.Errorf("invalid %s.server: expected hostname or IP address", name)
	}
	if strings.Contains(cfg.Server, ":") && net.ParseIP(cfg.Server) == nil {
		return fmt.Errorf("invalid %s.server: specify the port in the port field", name)
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return fmt.Errorf("invalid %s.port", name)
	}
	if sanitizeHeader(cfg.Email) != cfg.Email {
		return fmt.Errorf("invalid %s.email: control characters", name)
	}
	if _, err := mail.ParseAddress(cfg.Email); err != nil {
		return fmt.Errorf("invalid %s.email", name)
	}
	return nil
}
