package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func ptr(v int) *int { return &v }

// 测试默认不限速，避免限速影响时序断言。
func validTestConfig() Config {
	return Config{PrimarySMTP: SMTPConfig{Server: "smtp.example.com", Port: 465, Email: "robot@example.com"},
		RecipientGroups: map[string][]string{"it": {"admin@example.com"}}, WorkerCount: ptr(2), QueueCapacity: ptr(10),
		RateLimitInterval: ptr(0)}
}

func testSettings(t *testing.T, mutate func(*Config)) *settings {
	t.Helper()
	cfg := validTestConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := newSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func writeTestConfig(t *testing.T, path string, cfg Config) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestConfigValidation(t *testing.T) {
	tests := map[string]func(*Config){
		"SMTP port":        func(c *Config) { c.PrimarySMTP.Port = 65536 },
		"SMTP host":        func(c *Config) { c.PrimarySMTP.Server = "smtp.example.com/path" },
		"SMTP host:port":   func(c *Config) { c.PrimarySMTP.Server = "smtp.example.com:25" },
		"sender":           func(c *Config) { c.PrimarySMTP.Email = "invalid" },
		"sender injection": func(c *Config) { c.PrimarySMTP.Email = "robot@example.com\r\nBcc: other@example.com" },
		"HTTP port":        func(c *Config) { c.HTTPPort = ptr(65536) },
		"HTTP port zero":   func(c *Config) { c.HTTPPort = ptr(0) },
		"workers":          func(c *Config) { c.WorkerCount = ptr(65) },
		"workers zero":     func(c *Config) { c.WorkerCount = ptr(0) },
		"queue negative":   func(c *Config) { c.QueueCapacity = ptr(-1) },
		"queue":            func(c *Config) { c.QueueCapacity = ptr(100001) },
		"rate":             func(c *Config) { c.RateLimitInterval = ptr(86401) },
		"rate negative":    func(c *Config) { c.RateLimitInterval = ptr(-1) },
		"retries":          func(c *Config) { c.MaxRetries = ptr(11) },
		"retries negative": func(c *Config) { c.MaxRetries = ptr(-1) },
		"timeout zero":     func(c *Config) { c.SendTimeout = ptr(0) },
		"timeout":          func(c *Config) { c.SendTimeout = ptr(301) },
		"groups":           func(c *Config) { c.RecipientGroups = nil },
		"empty group":      func(c *Config) { c.RecipientGroups["it"] = nil },
		"group name":       func(c *Config) { c.RecipientGroups["\n"] = []string{"admin@example.com"} },
		"recipient":        func(c *Config) { c.RecipientGroups["it"] = []string{"invalid"} },
		"backup":           func(c *Config) { c.BackupSMTP = &SMTPConfig{} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := validTestConfig()
			mutate(&cfg)
			if _, err := newSettings(cfg); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestConfigDefaultsAndIsolation(t *testing.T) {
	cfg := validTestConfig()
	cfg.WorkerCount, cfg.QueueCapacity, cfg.RateLimitInterval = nil, nil, nil
	cfg.RecipientGroups["it"] = []string{"admin@example.com", "Admin <admin@example.com>", "ops@example.com"}
	backup := cfg.PrimarySMTP
	cfg.BackupSMTP = &backup
	s, err := newSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.httpPort != 8080 || s.queueCapacity != 1000 || s.workers != 4 || s.maxRetries != 2 ||
		s.sendInterval != time.Second || s.sendTimeout != 10*time.Second {
		t.Fatalf("defaults not applied: %+v", s)
	}
	// 显式 0 按字面生效：不限速、不重试。
	cfg.RateLimitInterval, cfg.MaxRetries = ptr(0), ptr(0)
	if s, err := newSettings(cfg); err != nil || s.sendInterval != 0 || s.maxRetries != 0 {
		t.Fatalf("explicit zero not honored: %v", err)
	}
	if got := s.groups["it"]; len(got) != 2 || got[0].Name != "" || got[1].Address != "ops@example.com" {
		t.Fatalf("recipients not deduplicated in order: %v", got)
	}
	backup.Server = "changed.example.com"
	if s.backup.Server != "smtp.example.com" {
		t.Fatal("settings share state with input config")
	}
}

func TestConfigEnvironmentAndStrictJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := validTestConfig()
	cfg.PrimarySMTP.Password = "${GOMAIL_TEST_PASSWORD}"
	cfg.RecipientGroups["it"] = []string{"$GOMAIL_TEST_RCPT"}
	secret := "test-only-\"quoted\\password$literal"
	t.Setenv("GOMAIL_TEST_PASSWORD", secret)
	t.Setenv("GOMAIL_TEST_RCPT", "ops@example.com")
	writeTestConfig(t, path, cfg)
	loaded, err := loadConfig(path)
	if err != nil || loaded.primary.Password != secret || loaded.groups["it"][0].Address != "ops@example.com" {
		t.Fatalf("environment expansion failed: %v", err)
	}

	cfg.PrimarySMTP.Password = "${GOMAIL_MISSING_TEST_VARIABLE}"
	t.Setenv("GOMAIL_MISSING_TEST_VARIABLE", "")
	if err := os.Unsetenv("GOMAIL_MISSING_TEST_VARIABLE"); err != nil {
		t.Fatal(err)
	}
	writeTestConfig(t, path, cfg)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("missing environment variable accepted")
	}

	for _, body := range []string{`{"typo":1}`, `{} {}`, `null`, `{"http_port":1.5}`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil {
			t.Fatalf("invalid JSON config accepted: %s", body)
		}
	}
	if _, err := loadConfig("config-example.json"); err != nil {
		t.Fatalf("example config: %v", err)
	}
}
