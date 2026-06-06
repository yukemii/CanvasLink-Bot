package config

import (
	"log"
	"os"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	TelegramBotToken   string
	DatabaseURL        string
	SyncInterval       time.Duration
	GoogleClientID     string
	GoogleClientSecret string
	OAuthRedirectURL   string
	OAuthListenAddr    string
	CanvasCourseRegex  string
}

func Load() Config {
	// Try loading .env from multiple locations
	_ = godotenv.Load(".env")
	_ = godotenv.Load("internal/.env")

	cfg := Config{
		TelegramBotToken:   os.Getenv("CANVASLINK_TELEGRAM_BOT_TOKEN"),
		DatabaseURL:        os.Getenv("CANVASLINK_DATABASE_URL"),
		SyncInterval:       parseDurationEnv("CANVASLINK_SYNC_INTERVAL", time.Hour),
		GoogleClientID:     os.Getenv("CANVASLINK_GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("CANVASLINK_GOOGLE_CLIENT_SECRET"),
		OAuthRedirectURL:   getEnv("CANVASLINK_OAUTH_REDIRECT_URL", "http://localhost:9090/oauth/callback"),
		OAuthListenAddr:    getEnv("CANVASLINK_OAUTH_LISTEN_ADDR", ":9090"),
		CanvasCourseRegex:  os.Getenv("CANVASLINK_COURSE_REGEX"),
	}

	if cfg.TelegramBotToken == "" {
		log.Fatal("CANVASLINK_TELEGRAM_BOT_TOKEN is required")
	}
	if cfg.DatabaseURL == "" {
		log.Fatal("CANVASLINK_DATABASE_URL is required")
	}
	if cfg.GoogleClientID == "" || cfg.GoogleClientSecret == "" {
		log.Println("CANVASLINK_GOOGLE_CLIENT_ID/CANVASLINK_GOOGLE_CLIENT_SECRET missing; Google Calendar sync will be disabled")
	}

	return cfg
}

func getEnv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func parseDurationEnv(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		log.Printf("invalid %s=%q, using %s", key, value, fallback)
		return fallback
	}
	return d
}
