package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"

	"github.com/markadodo/canvaslink/internal/bot"
	"github.com/markadodo/canvaslink/internal/canvas"
	"github.com/markadodo/canvaslink/internal/config"
	canvasGoogle "github.com/markadodo/canvaslink/internal/google"
	"github.com/markadodo/canvaslink/internal/oauth"
	"github.com/markadodo/canvaslink/internal/store"
	canvasSync "github.com/markadodo/canvaslink/internal/sync"
)

func main() {
	cfg := config.Load()

	// Initialize canvas parser with optional custom course code regex
	canvas.Init(cfg.CanvasCourseRegex)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := store.Connect(cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if err := db.InitSchema(ctx); err != nil {
		log.Fatal(err)
	}

	// Create the OAuth server (handles auth URL generation, callback, token storage)
	oauthServer := oauth.NewServer(db, cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.OAuthRedirectURL)

	// Create the Google Calendar client (uses OAuth config for token refresh)
	googleClient := canvasGoogle.NewCalendarClient(db, oauthServer.OAuthConfig())

	// Start the OAuth HTTP server for Google callback
	mux := http.NewServeMux()
	oauthServer.RegisterHTTPHandlers(mux, "/oauth/callback", "/oauth/status")
	oauthServerHTTP := &http.Server{
		Addr:    cfg.OAuthListenAddr,
		Handler: mux,
	}
	go func() {
		log.Printf("OAuth callback server listening on %s", cfg.OAuthListenAddr)
		if err := oauthServerHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("oauth server error: %v", err)
		}
	}()

	// Start the sync worker
	worker, err := canvasSync.NewWorker(db, cfg.SyncInterval, cfg.TelegramBotToken, googleClient)
	if err != nil {
		log.Fatal(err)
	}
	go worker.Start(ctx)

	// Start the Telegram bot
	tgBot, err := bot.New(cfg.TelegramBotToken, db, oauthServer, googleClient)
	if err != nil {
		log.Fatal(err)
	}

	// Wire up OAuth completion notification so the bot can continue onboarding
	oauthServer.SetOnOAuthComplete(tgBot.NotifyOAuthComplete)

	if err := tgBot.Start(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
