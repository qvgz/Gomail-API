package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validTestConfig() Config {
	return Config{PrimarySMTP: SMTPConfig{Server: "smtp.example.com", Port: 465, Email: "robot@example.com"},
		RecipientGroups: map[string][]string{"it": {"admin@example.com"}}, WorkerCount: 2, QueueCapacity: 10}
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
		"sender":           func(c *Config) { c.PrimarySMTP.Email = "invalid" },
		"sender injection": func(c *Config) { c.PrimarySMTP.Email = "robot@example.com\r\nBcc: other@example.com" },
		"HTTP port":        func(c *Config) { c.HTTPPort = 65536 },
		"workers":          func(c *Config) { c.WorkerCount = -1 },
		"queue":            func(c *Config) { c.QueueCapacity = 100001 },
		"rate":             func(c *Config) { c.RateLimitInterval = 86401 },
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
			if _, err := buildRuntimeConfig(&cfg); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	cfg := validTestConfig()
	cfg.WorkerCount = 0
	cfg.QueueCapacity = 0
	cfg.RecipientGroups["it"] = []string{"admin@example.com", "Admin <admin@example.com>"}
	runtime, err := buildRuntimeConfig(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.raw.HTTPPort != 8080 || runtime.raw.QueueCapacity != 1000 || runtime.raw.WorkerCount < 1 {
		t.Fatal("defaults not applied")
	}
	if len(runtime.groups["it"].parsed) != 1 {
		t.Fatal("recipient not deduplicated")
	}
	cfg.RecipientGroups["it"][0] = "changed@example.com"
	if runtime.raw.RecipientGroups["it"][0] != "admin@example.com" || cfg.QueueCapacity != 0 {
		t.Fatal("configuration not isolated")
	}
}

func TestConfigEnvironmentAndStrictJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := validTestConfig()
	cfg.PrimarySMTP.Password = "${GOMAIL_TEST_PASSWORD}"
	secret := "test-only-\"quoted\\password$literal"
	t.Setenv("GOMAIL_TEST_PASSWORD", secret)
	writeTestConfig(t, path, cfg)
	loaded, err := loadAndValidateConfig(path)
	if err != nil || loaded.raw.PrimarySMTP.Password != secret {
		t.Fatalf("environment expansion failed: %v", err)
	}
	cfg.PrimarySMTP.Password = "${GOMAIL_MISSING_TEST_VARIABLE}"
	t.Setenv("GOMAIL_MISSING_TEST_VARIABLE", "")
	if err := os.Unsetenv("GOMAIL_MISSING_TEST_VARIABLE"); err != nil {
		t.Fatal(err)
	}
	writeTestConfig(t, path, cfg)
	if _, err := loadAndValidateConfig(path); err == nil {
		t.Fatal("missing environment variable accepted")
	}
	for _, body := range []string{`{"typo":1}`, `{} {}`, `null`, `{"http_port":1.5}`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAndValidateConfig(path); err == nil {
			t.Fatalf("invalid JSON config accepted: %s", body)
		}
	}
	if _, err := loadAndValidateConfig("config-example.json"); err != nil {
		t.Fatalf("example config: %v", err)
	}
}

func TestReloadIsAtomicAndRejectsRestartFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := validTestConfig()
	original, err := buildRuntimeConfig(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	app := testApp(original)
	app.configPath = path
	cfg.HTTPPort = 9090
	writeTestConfig(t, path, cfg)
	app.reloadConfig()
	if app.cfg.Load() != original {
		t.Fatal("restart-only change was partially applied")
	}
	cfg.HTTPPort = 0
	cfg.RateLimitInterval = 2
	writeTestConfig(t, path, cfg)
	app.reloadConfig()
	if app.cfg.Load() == original || app.cfg.Load().raw.RateLimitInterval != 2 {
		t.Fatal("valid reload failed")
	}
	current := app.cfg.Load()
	cfg.PrimarySMTP.Email = "invalid"
	writeTestConfig(t, path, cfg)
	app.reloadConfig()
	if app.cfg.Load() != current {
		t.Fatal("invalid reload replaced working configuration")
	}
}

func TestPIDLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gomail.pid")
	first, err := lockPIDFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := lockPIDFile(path); err == nil {
		second.Close()
		t.Fatal("second instance acquired PID lock")
	}
	first.Close()
	second, err := lockPIDFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if file, err := lockPIDFile(link); err == nil {
		file.Close()
		t.Fatal("PID symlink accepted")
	}
}

func TestNormalizeBody(t *testing.T) {
	for input, want := range map[string]string{"": "", "a": "a", "a\nb\rc\r\nd": "a\r\nb\r\nc\r\nd"} {
		if got := normalizeBody(input); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	}
	if got := sanitizeHeader("abc\r\n\x00def"); strings.ContainsAny(got, "\r\n\x00") {
		t.Fatal("control character retained")
	}
}
