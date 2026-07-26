package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	TelegramBotToken    string
	DatabaseURL         string
	SyncInterval        time.Duration
	GoogleClientID      string
	GoogleClientSecret  string
	OAuthRedirectURL    string
	OAuthCallbackPath   string
	OAuthListenAddr     string
	CanvasCourseRegex   string
	InstanceID          string
	AllowInsecureFeeds  bool
	AllowPrivateFeeds   bool
	DefaultTimezone     string
	EncryptionKeyID     string
	EncryptionKey       []byte
	PreviousKeys        map[string][]byte
	RemovalGracePeriod  time.Duration
	RemovalMisses       int
	CalendarJobInterval time.Duration
}

func Load() (Config, error) {
	// Try loading .env from multiple locations
	_ = godotenv.Load(".env")
	_ = godotenv.Load("internal/.env")

	cfg := Config{
		TelegramBotToken:    os.Getenv("CANVASLINK_TELEGRAM_BOT_TOKEN"),
		DatabaseURL:         os.Getenv("CANVASLINK_DATABASE_URL"),
		SyncInterval:        parseDurationEnv("CANVASLINK_SYNC_INTERVAL", time.Hour),
		GoogleClientID:      os.Getenv("CANVASLINK_GOOGLE_CLIENT_ID"),
		GoogleClientSecret:  os.Getenv("CANVASLINK_GOOGLE_CLIENT_SECRET"),
		OAuthRedirectURL:    getEnv("CANVASLINK_OAUTH_REDIRECT_URL", "http://localhost:9090/oauth/callback"),
		OAuthListenAddr:     getEnv("CANVASLINK_OAUTH_LISTEN_ADDR", "127.0.0.1:9090"),
		CanvasCourseRegex:   os.Getenv("CANVASLINK_COURSE_REGEX"),
		InstanceID:          getEnv("CANVASLINK_INSTANCE_ID", "canvaslink"),
		DefaultTimezone:     getEnv("CANVASLINK_DEFAULT_TIMEZONE", "Asia/Singapore"),
		EncryptionKeyID:     getEnv("CANVASLINK_ENCRYPTION_KEY_ID", "primary"),
		RemovalGracePeriod:  parseDurationEnvWithMinimum("CANVASLINK_REMOVAL_GRACE_PERIOD", 6*time.Hour, time.Hour),
		RemovalMisses:       parseIntEnvWithMinimum("CANVASLINK_REMOVAL_MISSES", 3, 2),
		CalendarJobInterval: parseDurationEnvWithMinimum("CANVASLINK_CALENDAR_JOB_INTERVAL", 15*time.Second, time.Second),
	}
	cfg.TelegramBotToken = strings.TrimSpace(cfg.TelegramBotToken)
	cfg.DatabaseURL = strings.TrimSpace(cfg.DatabaseURL)
	cfg.GoogleClientID = strings.TrimSpace(cfg.GoogleClientID)
	cfg.GoogleClientSecret = strings.TrimSpace(cfg.GoogleClientSecret)
	cfg.OAuthRedirectURL = strings.TrimSpace(cfg.OAuthRedirectURL)
	cfg.OAuthListenAddr = strings.TrimSpace(cfg.OAuthListenAddr)
	cfg.DefaultTimezone = strings.TrimSpace(cfg.DefaultTimezone)
	cfg.InstanceID = strings.TrimSpace(cfg.InstanceID)
	cfg.EncryptionKeyID = strings.TrimSpace(cfg.EncryptionKeyID)

	var err error
	cfg.AllowInsecureFeeds, err = parseBoolEnv("CANVASLINK_ALLOW_INSECURE_FEEDS", false)
	if err != nil {
		return Config{}, err
	}
	cfg.AllowPrivateFeeds, err = parseBoolEnv("CANVASLINK_ALLOW_PRIVATE_FEEDS", false)
	if err != nil {
		return Config{}, err
	}

	callbackPath, err := parseOAuthCallbackPath(cfg.OAuthRedirectURL)
	if err != nil {
		return Config{}, err
	}
	cfg.OAuthCallbackPath = callbackPath
	if err := validateListenAddr(cfg.OAuthListenAddr); err != nil {
		return Config{}, err
	}

	if cfg.TelegramBotToken == "" {
		return Config{}, errors.New("CANVASLINK_TELEGRAM_BOT_TOKEN is required")
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("CANVASLINK_DATABASE_URL is required")
	}
	if (cfg.GoogleClientID == "") != (cfg.GoogleClientSecret == "") {
		return Config{}, errors.New("CANVASLINK_GOOGLE_CLIENT_ID and CANVASLINK_GOOGLE_CLIENT_SECRET must be set together")
	}
	if cfg.GoogleClientID == "" {
		log.Println("Google OAuth credentials missing; Google Calendar sync will be disabled")
	}
	if _, err := time.LoadLocation(cfg.DefaultTimezone); err != nil {
		return Config{}, fmt.Errorf("CANVASLINK_DEFAULT_TIMEZONE must be a valid IANA timezone: %w", err)
	}
	if !isValidIdentifier(cfg.InstanceID) {
		return Config{}, errors.New("CANVASLINK_INSTANCE_ID must be 1-64 characters using letters, numbers, '.', '_' or '-'")
	}
	if !isValidIdentifier(cfg.EncryptionKeyID) {
		return Config{}, errors.New("CANVASLINK_ENCRYPTION_KEY_ID must be 1-64 characters using letters, numbers, '.', '_' or '-'")
	}
	cfg.EncryptionKey, err = parseEncryptionKey("CANVASLINK_ENCRYPTION_KEY", os.Getenv("CANVASLINK_ENCRYPTION_KEY"))
	if err != nil {
		return Config{}, err
	}
	cfg.PreviousKeys, err = parsePreviousEncryptionKeys(os.Getenv("CANVASLINK_PREVIOUS_ENCRYPTION_KEYS"), cfg.EncryptionKeyID)
	if err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func isValidIdentifier(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') ||
			char == '.' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func parseOAuthCallbackPath(rawURL string) (string, error) {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" || parsed.Fragment != "" ||
		parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("CANVASLINK_OAUTH_REDIRECT_URL must be an absolute HTTP(S) URL")
	}
	if parsed.Scheme == "http" && !isLoopbackHostname(parsed.Hostname()) {
		return "", errors.New("CANVASLINK_OAUTH_REDIRECT_URL must use HTTPS except for localhost or loopback development URLs")
	}
	if parsed.Path == "" || parsed.Path == "/" {
		return "", fmt.Errorf("CANVASLINK_OAUTH_REDIRECT_URL must include a callback path")
	}
	return parsed.Path, nil
}

func isLoopbackHostname(hostname string) bool {
	if strings.EqualFold(strings.TrimSpace(hostname), "localhost") {
		return true
	}
	address := net.ParseIP(hostname)
	return address != nil && address.IsLoopback()
}

func validateListenAddr(rawAddr string) error {
	_, port, err := net.SplitHostPort(rawAddr)
	if err != nil {
		return fmt.Errorf("CANVASLINK_OAUTH_LISTEN_ADDR must be a host:port address: %w", err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("CANVASLINK_OAUTH_LISTEN_ADDR must use a port from 1 to 65535")
	}
	return nil
}

func getEnv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func parseBoolEnv(key string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", key)
	}
	return parsed, nil
}

func parseDurationEnv(key string, fallback time.Duration) time.Duration {
	return parseDurationEnvWithMinimum(key, fallback, time.Minute)
}

func parseDurationEnvWithMinimum(key string, fallback, minimum time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		log.Printf("invalid %s=%q, using %s", key, value, fallback)
		return fallback
	}
	if d < minimum {
		log.Printf("%s=%q is below the %s minimum, using %s", key, value, minimum, fallback)
		return fallback
	}
	return d
}

func parseIntEnvWithMinimum(key string, fallback, minimum int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum {
		log.Printf("%s=%q must be at least %d, using %d", key, value, minimum, fallback)
		return fallback
	}
	return parsed
}

func parseEncryptionKey(key, raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%s is required; provide a base64-encoded 32-byte key", key)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("%s must be a base64-encoded 32-byte key", key)
	}
	return decoded, nil
}

func parsePreviousEncryptionKeys(raw, primaryKeyID string) (map[string][]byte, error) {
	result := make(map[string][]byte)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return result, nil
	}
	for _, entry := range strings.Split(raw, ",") {
		keyID, encoded, ok := strings.Cut(strings.TrimSpace(entry), ":")
		keyID = strings.TrimSpace(keyID)
		if !ok || !isValidIdentifier(keyID) {
			return nil, errors.New("CANVASLINK_PREVIOUS_ENCRYPTION_KEYS must use key_id:base64_key entries")
		}
		if keyID == primaryKeyID {
			return nil, fmt.Errorf("previous encryption key ID %q duplicates the primary key", keyID)
		}
		if _, exists := result[keyID]; exists {
			return nil, fmt.Errorf("duplicate previous encryption key ID %q", keyID)
		}
		decoded, err := parseEncryptionKey("previous encryption key "+keyID, encoded)
		if err != nil {
			return nil, err
		}
		result[keyID] = decoded
	}
	return result, nil
}
