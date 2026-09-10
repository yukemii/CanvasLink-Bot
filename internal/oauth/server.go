package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/markadodo/canvaslink/internal/store"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	googlecalendar "google.golang.org/api/calendar/v3"
)

// OnOAuthCompleteFn is called when a user successfully completes Google OAuth.
// The parameter is the Telegram user ID. Used to notify the bot to continue onboarding.
type OnOAuthCompleteFn func(telegramUserID int64)

const (
	oauthCallbackTimeout   = 30 * time.Second
	oauthDisconnectTimeout = 10 * time.Second
	oauthRevokeTimeout     = 5 * time.Second
	googleRevokeURL        = "https://oauth2.googleapis.com/revoke"
	maxOAuthCodeLength     = 4096
)

// Server handles the Google OAuth flow for CanvasLink.
// It provides:
//   - Auth URL generation (with PKCE)
//   - OAuth callback handling (code exchange + token storage)
//   - Token status check
type Server struct {
	store            *store.Store
	oauthConfig      *oauth2.Config
	configured       bool
	callbackMu       sync.RWMutex
	onOAuthCompleted OnOAuthCompleteFn
}

type oauthStateStore interface {
	GetTelegramAccount(context.Context, int64) (*store.TelegramAccount, error)
	TryFeedSyncLock(context.Context, int64) (func() error, bool, error)
	CreateOAuthState(context.Context, string, int64, string, time.Time, int64) error
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
			Scopes:       []string{googlecalendar.CalendarEventsScope, googlecalendar.CalendarCalendarsScope, googlecalendar.CalendarCalendarlistReadonlyScope},
			Endpoint:     google.Endpoint,
		},
		configured: true,
	}
}

// SetOnOAuthComplete registers a callback to be called when a user completes OAuth.
func (s *Server) SetOnOAuthComplete(fn OnOAuthCompleteFn) {
	s.callbackMu.Lock()
	defer s.callbackMu.Unlock()
	s.onOAuthCompleted = fn
}

// AuthURL generates a Google OAuth URL for a Telegram user.
// It creates a state token with PKCE challenge and stores it in the database.
func (s *Server) AuthURL(ctx context.Context, telegramUserID int64) (string, error) {
	if !s.configured {
		return "", errors.New("google oauth is not configured")
	}
	if telegramUserID <= 0 {
		return "", errors.New("invalid telegram user ID")
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

	if err := createOAuthStateWithLock(
		ctx,
		s.store,
		state,
		telegramUserID,
		verifier,
		time.Now().Add(10*time.Minute),
	); err != nil {
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

func createOAuthStateWithLock(
	ctx context.Context,
	storage oauthStateStore,
	state string,
	telegramUserID int64,
	verifier string,
	expiresAt time.Time,
) (err error) {
	account, err := storage.GetTelegramAccount(ctx, telegramUserID)
	if err != nil {
		return fmt.Errorf("load account before Google authorization link: %w", err)
	}
	if account == nil {
		return errors.New("Telegram account is not configured for Google authorization")
	}
	expectedStateRevision := account.StateRevision

	release, acquired, err := storage.TryFeedSyncLock(ctx, telegramUserID)
	if err != nil {
		return fmt.Errorf("lock Google authorization link: %w", err)
	}
	if !acquired {
		return errors.New("another account operation is in progress; retry connecting Google")
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release Google authorization-link lock: %w", releaseErr))
		}
	}()
	return storage.CreateOAuthState(
		ctx,
		state,
		telegramUserID,
		verifier,
		expiresAt,
		expectedStateRevision,
	)
}

// HandleCallback processes the OAuth callback from Google.
// It exchanges the authorization code for tokens and stores them.
func (s *Server) HandleCallback(ctx context.Context, state, code string) error {
	_, err := s.HandleCallbackWithUserID(ctx, state, code)
	return err
}

// IsConnected checks if a Telegram user has a stored Google token.
func (s *Server) IsConnected(ctx context.Context, telegramUserID int64) (bool, error) {
	if !s.configured {
		return false, nil
	}
	return s.store.HasGoogleToken(ctx, telegramUserID)
}

type googleDisconnectStore interface {
	GetGoogleToken(context.Context, int64) (*store.GoogleToken, error)
	DisconnectGoogle(context.Context, int64) error
}

type googleResetStore interface {
	GetGoogleToken(context.Context, int64) (*store.GoogleToken, error)
	ResetUser(context.Context, int64) error
}

type googleTokenRevoker func(context.Context, string) error

// Disconnect removes a Telegram user's stored Google token. Provider
// revocation is best-effort; local deletion remains authoritative even if the
// stored token cannot be read or decrypted.
func (s *Server) Disconnect(ctx context.Context, telegramUserID int64) error {
	return disconnectGoogle(ctx, s.store, telegramUserID, revokeGoogleToken)
}

// ResetUser removes all local CanvasLink data, then best-effort revokes the
// readable Google grant. Callers must delete verified remote calendar events
// first and hold the user's distributed operation lock.
func (s *Server) ResetUser(ctx context.Context, telegramUserID int64) error {
	return resetUserAndRevoke(ctx, s.store, telegramUserID, revokeGoogleToken)
}

func disconnectGoogle(
	ctx context.Context,
	storage googleDisconnectStore,
	telegramUserID int64,
	revoke googleTokenRevoker,
) error {
	token, tokenErr := storage.GetGoogleToken(ctx, telegramUserID)

	// Use a fresh, bounded context so a failed or cancelled token read cannot
	// prevent the local security boundary from being applied.
	disconnectCtx, cancelDisconnect := context.WithTimeout(context.WithoutCancel(ctx), oauthDisconnectTimeout)
	defer cancelDisconnect()
	if err := storage.DisconnectGoogle(disconnectCtx, telegramUserID); err != nil {
		return err
	}

	revokeReadableGoogleToken(ctx, telegramUserID, token, tokenErr, revoke)
	return nil
}

func resetUserAndRevoke(
	ctx context.Context,
	storage googleResetStore,
	telegramUserID int64,
	revoke googleTokenRevoker,
) error {
	token, tokenErr := storage.GetGoogleToken(ctx, telegramUserID)

	resetCtx, cancelReset := context.WithTimeout(context.WithoutCancel(ctx), oauthDisconnectTimeout)
	defer cancelReset()
	if err := storage.ResetUser(resetCtx, telegramUserID); err != nil {
		return err
	}

	revokeReadableGoogleToken(ctx, telegramUserID, token, tokenErr, revoke)
	return nil
}

func revokeReadableGoogleToken(
	ctx context.Context,
	telegramUserID int64,
	token *store.GoogleToken,
	tokenErr error,
	revoke googleTokenRevoker,
) {
	if tokenErr != nil {
		log.Printf("Google token unavailable for best-effort revocation after local removal user=%d: %v", telegramUserID, tokenErr)
		return
	}
	if token == nil {
		return
	}

	tokenValue := token.RefreshToken
	if tokenValue == "" {
		tokenValue = token.AccessToken
	}
	if strings.TrimSpace(tokenValue) == "" {
		return
	}

	revokeCtx, cancelRevoke := context.WithTimeout(context.WithoutCancel(ctx), oauthRevokeTimeout)
	defer cancelRevoke()
	if err := revoke(revokeCtx, tokenValue); err != nil {
		log.Printf("best-effort Google grant revocation failed user=%d: %v", telegramUserID, err)
	}
}

// RegisterHTTPHandlers registers the OAuth callback handler.
// The callback endpoint is mounted at callbackPath (e.g., "/oauth/callback").
func (s *Server) RegisterHTTPHandlers(mux *http.ServeMux, callbackPath string) {
	mux.HandleFunc(callbackPath, s.handleCallbackHTTP)
}

func (s *Server) handleCallbackHTTP(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if providerError := strings.TrimSpace(r.URL.Query().Get("error")); providerError != "" {
		log.Printf("oauth callback denied: %q", providerError)
		writeHTML(w, http.StatusBadRequest, "<h2>Google Auth Cancelled</h2><p>No Google connection was saved. You can close this window and try again from Telegram.</p>")
		return
	}

	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")

	ctx, cancel := context.WithTimeout(r.Context(), oauthCallbackTimeout)
	defer cancel()
	telegramUserID, err := s.HandleCallbackWithUserID(ctx, state, code)
	if err != nil {
		log.Printf("oauth callback error: %v", err)

		errMsg := err.Error()
		if strings.Contains(errMsg, "redirect_uri_mismatch") {
			writeHTML(w, http.StatusBadRequest, fmt.Sprintf(`
<h2>Google Auth Failed</h2>
<p><strong>redirect_uri_mismatch</strong></p>
<p>The redirect URI in your Google Cloud Console does not match the one configured in CanvasLink.</p>
<p>Please add this exact URL to your Google Cloud Console:</p>
<p><code>%s</code></p>
<p>Go to: APIs &amp; Services → Credentials → OAuth 2.0 Client IDs → Edit → Authorized redirect URIs</p>
<p>You can close this window and try /connect_google again.</p>
`, html.EscapeString(s.oauthConfig.RedirectURL)))
		} else {
			writeHTML(w, http.StatusBadRequest, "<h2>Google Auth Failed</h2><p>The authorization could not be completed.</p><p>You can close this window and try /connect_google again.</p>")
		}
		return
	}

	// Notify the bot that OAuth completed for this user
	s.callbackMu.RLock()
	onOAuthCompleted := s.onOAuthCompleted
	s.callbackMu.RUnlock()
	if onOAuthCompleted != nil {
		onOAuthCompleted(telegramUserID)
	}

	writeHTML(w, http.StatusOK, "<h2>Google Calendar Connected!</h2><p>You can close this window and return to Telegram.</p>")
}

// HandleCallbackWithUserID processes the OAuth callback and returns the Telegram user ID on success.
func (s *Server) HandleCallbackWithUserID(ctx context.Context, state, code string) (int64, error) {
	if !s.configured {
		return 0, errors.New("google oauth is not configured")
	}
	state = strings.TrimSpace(state)
	code = strings.TrimSpace(code)
	if !validOAuthState(state) || code == "" || len(code) > maxOAuthCodeLength ||
		strings.ContainsAny(code, "\x00\r\n") {
		return 0, errors.New("missing state or code")
	}

	stateUserID, err := s.store.GetOAuthStateUserID(ctx, state)
	if err != nil {
		return 0, err
	}
	if stateUserID == 0 {
		return 0, errors.New("invalid or expired oauth state")
	}
	release, acquired, err := s.store.TryFeedSyncLock(ctx, stateUserID)
	if err != nil {
		return 0, fmt.Errorf("lock Google authorization: %w", err)
	}
	if !acquired {
		return 0, errors.New("another account operation is in progress; retry the Google callback")
	}
	defer func() {
		if err := release(); err != nil {
			log.Printf("release Google authorization lock failed user=%d: %v", stateUserID, err)
		}
	}()

	telegramUserID, verifier, err := s.store.ConsumeOAuthState(ctx, state)
	if err != nil {
		return 0, err
	}
	if telegramUserID == 0 || telegramUserID != stateUserID || verifier == "" {
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

	if err := s.store.SaveGoogleAuthorization(
		ctx,
		telegramUserID,
		token.AccessToken,
		token.RefreshToken,
		token.TokenType,
		scopes,
		token.Expiry,
	); err != nil {
		tokenValue := token.RefreshToken
		if tokenValue == "" {
			tokenValue = token.AccessToken
		}
		revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthRevokeTimeout)
		defer cancel()
		if revokeErr := revokeGoogleToken(revokeCtx, tokenValue); revokeErr != nil {
			log.Printf("best-effort cleanup of unsaved Google grant failed user=%d: %v", telegramUserID, revokeErr)
		}
		return 0, err
	}

	return telegramUserID, nil
}

func validOAuthState(state string) bool {
	if len(state) != 32 {
		return false
	}
	for _, character := range state {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func setNoStoreHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func writeHTML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<!doctype html><html><body>%s</body></html>", body)
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

func revokeGoogleToken(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	body := url.Values{"token": {token}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, googleRevokeURL, strings.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := (&http.Client{Timeout: oauthRevokeTimeout}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusBadRequest {
		return fmt.Errorf("Google revocation returned status %d", response.StatusCode)
	}
	return nil
}
