package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestParseDurationEnvRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "valid", value: "15m", want: 15 * time.Minute},
		{name: "zero", value: "0", want: time.Hour},
		{name: "negative", value: "-1m", want: time.Hour},
		{name: "too frequent", value: "30s", want: time.Hour},
		{name: "invalid", value: "often", want: time.Hour},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CANVASLINK_TEST_INTERVAL", test.value)
			if got := parseDurationEnv("CANVASLINK_TEST_INTERVAL", time.Hour); got != test.want {
				t.Errorf("parseDurationEnv() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestParseOAuthCallbackPath(t *testing.T) {
	t.Parallel()

	path, err := parseOAuthCallbackPath("https://bot.example.com/custom/google/callback")
	if err != nil {
		t.Fatalf("parseOAuthCallbackPath() error = %v", err)
	}
	if path != "/custom/google/callback" {
		t.Errorf("path = %q", path)
	}

	for _, rawURL := range []string{
		"",
		"localhost:9090/oauth/callback",
		"https://example.com/",
		"http://example.com/oauth/callback",
		"https://user@example.com/oauth/callback",
	} {
		if _, err := parseOAuthCallbackPath(rawURL); err == nil {
			t.Errorf("parseOAuthCallbackPath(%q) returned nil error", rawURL)
		}
	}
	for _, rawURL := range []string{
		"http://localhost:9090/oauth/callback",
		"http://127.0.0.1:9090/oauth/callback",
		"http://[::1]:9090/oauth/callback",
	} {
		if _, err := parseOAuthCallbackPath(rawURL); err != nil {
			t.Errorf("parseOAuthCallbackPath(%q) error = %v", rawURL, err)
		}
	}
}

func TestValidateListenAddr(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{":9090", "127.0.0.1:8080", "[::1]:443"} {
		if err := validateListenAddr(addr); err != nil {
			t.Errorf("validateListenAddr(%q) error = %v", addr, err)
		}
	}
	for _, addr := range []string{"", "9090", ":0", ":70000", ":port"} {
		if err := validateListenAddr(addr); err == nil {
			t.Errorf("validateListenAddr(%q) returned nil error", addr)
		}
	}
}

func TestLoadReturnsValidationErrors(t *testing.T) {
	t.Setenv("CANVASLINK_TELEGRAM_BOT_TOKEN", "")
	t.Setenv("CANVASLINK_DATABASE_URL", "postgres://example")
	t.Setenv("CANVASLINK_GOOGLE_CLIENT_ID", "")
	t.Setenv("CANVASLINK_GOOGLE_CLIENT_SECRET", "")
	t.Setenv("CANVASLINK_OAUTH_REDIRECT_URL", "http://localhost:9090/oauth/callback")
	t.Setenv("CANVASLINK_OAUTH_LISTEN_ADDR", ":9090")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "TELEGRAM_BOT_TOKEN") {
		t.Fatalf("Load() error = %v, want missing bot token", err)
	}
}

func TestLoadSecurityConfiguration(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	t.Setenv("CANVASLINK_TELEGRAM_BOT_TOKEN", "123:test")
	t.Setenv("CANVASLINK_DATABASE_URL", "postgres://example")
	t.Setenv("CANVASLINK_GOOGLE_CLIENT_ID", "")
	t.Setenv("CANVASLINK_GOOGLE_CLIENT_SECRET", "")
	t.Setenv("CANVASLINK_OAUTH_REDIRECT_URL", "http://localhost:9090/oauth/callback")
	t.Setenv("CANVASLINK_OAUTH_LISTEN_ADDR", ":9090")
	t.Setenv("CANVASLINK_DEFAULT_TIMEZONE", "Asia/Singapore")
	t.Setenv("CANVASLINK_INSTANCE_ID", "test-instance")
	t.Setenv("CANVASLINK_ENCRYPTION_KEY", key)
	t.Setenv("CANVASLINK_ENCRYPTION_KEY_ID", "test-key")
	t.Setenv("CANVASLINK_PREVIOUS_ENCRYPTION_KEYS", "")
	t.Setenv("CANVASLINK_ALLOW_INSECURE_FEEDS", "true")
	t.Setenv("CANVASLINK_ALLOW_PRIVATE_FEEDS", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.AllowInsecureFeeds || cfg.AllowPrivateFeeds {
		t.Errorf("feed policy = insecure:%t private:%t", cfg.AllowInsecureFeeds, cfg.AllowPrivateFeeds)
	}
	if len(cfg.EncryptionKey) != 32 || cfg.EncryptionKeyID != "test-key" {
		t.Errorf("encryption configuration was not decoded")
	}
}

func TestParsePreviousEncryptionKeys(t *testing.T) {
	t.Parallel()

	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	keys, err := parsePreviousEncryptionKeys("old:"+key, "current")
	if err != nil {
		t.Fatalf("parsePreviousEncryptionKeys() error = %v", err)
	}
	if len(keys["old"]) != 32 {
		t.Errorf("decoded previous key length = %d", len(keys["old"]))
	}
	if _, err := parsePreviousEncryptionKeys("current:"+key, "current"); err == nil {
		t.Fatal("duplicate primary key ID was accepted")
	}
	if _, err := parsePreviousEncryptionKeys("bad key:"+key, "current"); err == nil {
		t.Fatal("invalid previous key ID was accepted")
	}
}

func TestIsValidIdentifier(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"canvaslink", "prod.sg-1", "bot_instance"} {
		if !isValidIdentifier(value) {
			t.Errorf("isValidIdentifier(%q) = false", value)
		}
	}
	for _, value := range []string{"", "contains space", "bad:delimiter", strings.Repeat("x", 65)} {
		if isValidIdentifier(value) {
			t.Errorf("isValidIdentifier(%q) = true", value)
		}
	}
}
