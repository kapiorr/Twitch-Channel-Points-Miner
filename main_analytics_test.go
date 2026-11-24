package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnalyticsConfigDefaultsAndEnvOverride(t *testing.T) {
	t.Setenv("TCPM_ANALYTICS_ADDR", "")
	cfg, err := loadOrCreateConfig(filepath.Join(t.TempDir(), "cfg.json"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !cfg.Analytics.Enabled || cfg.Analytics.Host != "0.0.0.0" || cfg.Analytics.Port != 5000 {
		t.Fatalf("unexpected analytics defaults %+v", cfg.Analytics)
	}
	if got := analyticsAddr(cfg.Analytics); got != "0.0.0.0:5000" {
		t.Fatalf("addr got %q", got)
	}
	if got := analyticsAddr(analyticsConfig{}); got != "0.0.0.0:5000" {
		t.Fatalf("empty config addr got %q", got)
	}
	t.Setenv("TCPM_ANALYTICS_ADDR", "127.0.0.1:8080")
	if got := analyticsAddr(cfg.Analytics); got != "127.0.0.1:8080" {
		t.Fatalf("env override got %q", got)
	}
}

func TestAutoUpdateDisabledByEnv(t *testing.T) {
	for value, want := range map[string]bool{"": false, "0": false, "1": true, "true": true} {
		t.Setenv("TCPM_DISABLE_AUTO_UPDATE", value)
		if got := autoUpdateDisabledByEnv(); got != want {
			t.Fatalf("TCPM_DISABLE_AUTO_UPDATE=%q got %v, want %v", value, got, want)
		}
	}
}

func TestTelegramConfigChatIDAcceptsNumberOrString(t *testing.T) {
	for raw, want := range map[string]string{`-1001234567890`: "-1001234567890", `"@mychannel"`: "@mychannel", `" 42 "`: "42", `null`: ""} {
		var cfg telegramConfig
		if err := json.Unmarshal([]byte(`{"chat_id":`+raw+`,"token":"t"}`), &cfg); err != nil {
			t.Fatalf("chat_id %s: %v", raw, err)
		}
		if got := cfg.settings().ChatID; got != want {
			t.Fatalf("chat_id %s got %q, want %q", raw, got, want)
		}
	}
}

func TestLoadOrCreateConfigFillsAnalyticsAndTelegramKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"username":"viewer","analytics":{"host":"127.0.0.1"},"telegram":{"chat_id":123}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := loadOrCreateConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !cfg.Analytics.Enabled || cfg.Analytics.Host != "127.0.0.1" || cfg.Analytics.Port != 5000 {
		t.Fatalf("partial analytics section should keep host and get defaults, got %+v", cfg.Analytics)
	}
	if string(cfg.Telegram.ChatID) != "123" || cfg.Telegram.Token != "" || cfg.Telegram.DisableNotification {
		t.Fatalf("partial telegram section should keep chat_id and get defaults, got %+v", cfg.Telegram)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	for _, key := range []string{`"enabled"`, `"port"`, `"token"`, `"disable_notification"`} {
		if !strings.Contains(string(written), key) {
			t.Fatalf("missing %s in the rewritten config", key)
		}
	}
}
