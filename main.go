package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/markadodo/canvaslink/internal/bot"
	"github.com/markadodo/canvaslink/internal/canvas"
	"github.com/markadodo/canvaslink/internal/config"
	canvasGoogle "github.com/markadodo/canvaslink/internal/google"
	"github.com/markadodo/canvaslink/internal/oauth"
	"github.com/markadodo/canvaslink/internal/store"
	canvasSync "github.com/markadodo/canvaslink/internal/sync"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	// Initialize canvas parser with optional custom course code regex
	canvas.Init(cfg.CanvasCourseRegex)
	canvas.ConfigureFeedSecurity(canvas.FeedSecurityOptions{
		AllowHTTP:            cfg.AllowInsecureFeeds,
		AllowPrivateNetworks: cfg.AllowPrivateFeeds,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := store.ConnectEncrypted(cfg.DatabaseURL, store.EncryptionConfig{
		PrimaryKeyID: cfg.EncryptionKeyID,
		PrimaryKey:   cfg.EncryptionKey,
		PreviousKeys: cfg.PreviousKeys,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Printf("close database failed: %v", err)
		}
	}()

	if err := db.InitSchema(ctx); err != nil {
		return err
	}

	// Create the OAuth server (handles auth URL generation, callback, token storage)
	oauthServer := oauth.NewServer(db, cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.OAuthRedirectURL)

	// Create the Google Calendar client (uses OAuth config for token refresh)
	googleClient := canvasGoogle.NewCalendarClient(db, oauthServer.OAuthConfig(), cfg.InstanceID)

	// Construct the bot before exposing the OAuth callback so no completion can
	// arrive before the notification hook is installed.
	tgBot, err := bot.New(cfg.TelegramBotToken, db, oauthServer, googleClient, cfg.DefaultTimezone)
	if err != nil {
		return err
	}
	oauthServer.SetOnOAuthComplete(tgBot.NotifyOAuthComplete)

	worker, err := canvasSync.NewWorkerWithOptions(
		db,
		cfg.TelegramBotToken,
		googleClient,
		canvasSync.WorkerOptions{
			SyncInterval:        cfg.SyncInterval,
			CalendarJobInterval: cfg.CalendarJobInterval,
			RemovalGracePeriod:  cfg.RemovalGracePeriod,
			RemovalMisses:       cfg.RemovalMisses,
		},
	)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	oauthServer.RegisterHTTPHandlers(mux, cfg.OAuthCallbackPath)
	oauthServerHTTP := &http.Server{
		Addr:              cfg.OAuthListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	serverErrCh := make(chan error, 1)
	go func() {
		log.Printf("OAuth callback server listening on %s", cfg.OAuthListenAddr)
		if err := oauthServerHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErrCh <- err
		}
	}()

	var background sync.WaitGroup
	background.Add(2)
	go func() {
		defer background.Done()
		worker.Start(ctx)
	}()

	botErrCh := make(chan error, 1)
	go func() {
		defer background.Done()
		botErrCh <- tgBot.Start(ctx)
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serverErrCh:
		runErr = fmt.Errorf("oauth server: %w", err)
	case err := <-botErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			runErr = fmt.Errorf("telegram bot: %w", err)
		}
	}

	stop()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := oauthServerHTTP.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = fmt.Errorf("shutdown oauth server: %w", err)
	}

	backgroundDone := make(chan struct{})
	go func() {
		background.Wait()
		close(backgroundDone)
	}()
	select {
	case <-backgroundDone:
	case <-shutdownCtx.Done():
		if runErr == nil {
			runErr = errors.New("timed out waiting for background services to stop")
		}
	}

	return runErr
}
