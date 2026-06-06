package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/markadodo/canvaslink/internal/store"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	googlecalendar "google.golang.org/api/calendar/v3"
)

// OnOAuthCompleteFn is called when a user successfully completes Google OAuth.
// The parameter is the Telegram user ID. Used to notify the bot to continue onboarding.
type OnOAuthCompleteFn func(telegramUserID int64)

// Server handles the Google OAuth flow for CanvasLink.
// It provides:
//   - Auth URL generation (with PKCE)
//   - OAuth callback handling (code exchange + token storage)
//   - Token status check
type Server struct {
	store            *store.Store
	oauthConfig      *oauth2.Config
	configured       bool
	onOAuthCompleted OnOAuthCompleteFn
}

// NewServer creates a new OAuth server. If clientID or clientSecret is empty,
// the server will be in "not configured" mode and return errors for all operations.
func NewServer(db *store.Store, clientID, clientSecret, redirectURL string) *Server {
	if clientID == "" || clientSecret == "" {
		return &Server{store: db, configured: false}
	}
	return &Server{
		store: db,
		oauthConfig: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Scopes:       []string{googlecalendar.CalendarEventsScope},
			Endpoint:     google.Endpoint,
		},
		configured: true,
	}
}

// SetOnOAuthComplete registers a callback to be called when a user completes OAuth.
func (s *Server) SetOnOAuthComplete(fn OnOAuthCompleteFn) {
	s.onOAuthCompleted = fn
}

// AuthURL generates a Google OAuth URL for a Telegram user.
// It creates a state token with PKCE challenge and stores it in the database.
func (s *Server) AuthURL(ctx context.Context, telegramUserID int64) (string, error) {
	if !s.configured {
		return "", errors.New("google oauth is not configured")
	}

	state, err := randomToken(16)
	if err != nil {
		return "", err
	}
	verifier, err := randomToken(32)
	if err != nil {
		return "", err
	}
	challenge := pkceChallenge(verifier)

	if err := s.store.CreateOAuthState(ctx, state, telegramUserID, verifier, time.Now().Add(10*time.Minute)); err != nil {
		return "", err
	}

	url := s.oauthConfig.AuthCodeURL(
		state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oauth2.SetAuthURLParam("prompt", "consent"),
	)
	return url, nil
}

// HandleCallback processes the OAuth callback from Google.
// It exchanges the authorization code for tokens and stores them.
func (s *Server) HandleCallback(ctx context.Context, state, code string) error {
	if !s.configured {
		return errors.New("google oauth is not configured")
	}
	if strings.TrimSpace(state) == "" || strings.TrimSpace(code) == "" {
		return errors.New("missing state or code")
	}

	telegramUserID, verifier, err := s.store.ConsumeOAuthState(ctx, state)
	if err != nil {
		return err
	}
	if telegramUserID == 0 || verifier == "" {
		return errors.New("invalid or expired oauth state")
	}

	token, err := s.oauthConfig.Exchange(ctx, code, oauth2.SetAuthURLParam("code_verifier", verifier))
	if err != nil {
		return err
	}

	scopes := ""
	if rawScope := token.Extra("scope"); rawScope != nil {
		if scopeString, ok := rawScope.(string); ok {
			scopes = scopeString
		}
	}

	return s.store.UpsertGoogleToken(
		ctx,
		telegramUserID,
		token.AccessToken,
		token.RefreshToken,
		token.TokenType,
		scopes,
		token.Expiry,
	)
}

// IsConnected checks if a Telegram user has a stored Google token.
func (s *Server) IsConnected(ctx context.Context, telegramUserID int64) (bool, error) {
	if !s.configured {
		return false, nil
	}
	return s.store.HasGoogleToken(ctx, telegramUserID)
}

// Disconnect removes a Telegram user's stored Google token.
func (s *Server) Disconnect(ctx context.Context, telegramUserID int64) error {
	if !s.configured {
		return nil
	}
	return s.store.DeleteGoogleToken(ctx, telegramUserID)
}

// RegisterHTTPHandlers registers the OAuth callback and status HTTP handlers.
// The callback endpoint is mounted at callbackPath (e.g., "/oauth/callback").
// The status endpoint is mounted at statusPath (e.g., "/oauth/status").
func (s *Server) RegisterHTTPHandlers(mux *http.ServeMux, callbackPath, statusPath string) {
	mux.HandleFunc(callbackPath, s.handleCallbackHTTP)
	mux.HandleFunc(statusPath, s.handleStatusHTTP)
}

func (s *Server) handleCallbackHTTP(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")

	telegramUserID, err := s.HandleCallbackWithUserID(r.Context(), state, code)
	if err != nil {
		log.Printf("oauth callback error: %v", err)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)

		errMsg := err.Error()
		if strings.Contains(errMsg, "redirect_uri_mismatch") {
			fmt.Fprintf(w, `<html><body>
<h2>Google Auth Failed</h2>
<p><strong>redirect_uri_mismatch</strong></p>
<p>The redirect URI in your Google Cloud Console does not match the one configured in CanvasLink.</p>
<p>Please add this exact URL to your Google Cloud Console:</p>
<p><code>%s</code></p>
<p>Go to: APIs & Services → Credentials → OAuth 2.0 Client IDs → Edit → Authorized redirect URIs</p>
<p>You can close this window and try /connect_google again.</p>
</body></html>`, s.oauthConfig.RedirectURL)
		} else {
			fmt.Fprintf(w, "<html><body><h2>Google Auth Failed</h2><p>%s</p><p>You can close this window and try /connect_google again.</p></body></html>", errMsg)
		}
		return
	}

	// Notify the bot that OAuth completed for this user
	if s.onOAuthCompleted != nil {
		s.onOAuthCompleted(telegramUserID)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "<html><body><h2>Google Calendar Connected!</h2><p>You can close this window and return to Telegram.</p></body></html>")
}

// HandleCallbackWithUserID processes the OAuth callback and returns the Telegram user ID on success.
func (s *Server) HandleCallbackWithUserID(ctx context.Context, state, code string) (int64, error) {
	if !s.configured {
		return 0, errors.New("google oauth is not configured")
	}
	if strings.TrimSpace(state) == "" || strings.TrimSpace(code) == "" {
		return 0, errors.New("missing state or code")
	}

	telegramUserID, verifier, err := s.store.ConsumeOAuthState(ctx, state)
	if err != nil {
		return 0, err
	}
	if telegramUserID == 0 || verifier == "" {
		return 0, errors.New("invalid or expired oauth state")
	}

	token, err := s.oauthConfig.Exchange(ctx, code, oauth2.SetAuthURLParam("code_verifier", verifier))
	if err != nil {
		return 0, err
	}

	scopes := ""
	if rawScope := token.Extra("scope"); rawScope != nil {
		if scopeString, ok := rawScope.(string); ok {
			scopes = scopeString
		}
	}

	if err := s.store.UpsertGoogleToken(
		ctx,
		telegramUserID,
		token.AccessToken,
		token.RefreshToken,
		token.TokenType,
		scopes,
		token.Expiry,
	); err != nil {
		return 0, err
	}

	return telegramUserID, nil
}

func (s *Server) handleStatusHTTP(w http.ResponseWriter, r *http.Request) {
	telegramUserIDStr := r.URL.Query().Get("telegram_user_id")
	if telegramUserIDStr == "" {
		http.Error(w, "telegram_user_id query parameter is required", http.StatusBadRequest)
		return
	}

	var telegramUserID int64
	if _, err := fmt.Sscanf(telegramUserIDStr, "%d", &telegramUserID); err != nil || telegramUserID <= 0 {
		http.Error(w, "valid telegram_user_id query parameter is required", http.StatusBadRequest)
		return
	}

	connected, err := s.IsConnected(r.Context(), telegramUserID)
	if err != nil {
		http.Error(w, "could not check google status", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"connected":%t}`, connected)
}

// OAuthConfig returns the underlying oauth2.Config for use by the calendar client.
func (s *Server) OAuthConfig() *oauth2.Config {
	return s.oauthConfig
}

// IsConfigured returns whether the OAuth server has valid credentials.
func (s *Server) IsConfigured() bool {
	return s.configured
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
