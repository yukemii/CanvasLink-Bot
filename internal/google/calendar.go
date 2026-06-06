package google

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/markadodo/canvaslink/internal/store"
	"golang.org/x/oauth2"
	googlecalendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

var (
	ErrGoogleNotConfigured = errors.New("google oauth not configured")
	ErrGoogleNotConnected  = errors.New("google calendar not connected")
)

// CalendarClient creates Google Calendar events for CanvasLink users.
// It uses the OAuth2 config from the OAuth server for token refresh.
type CalendarClient struct {
	store      *store.Store
	oauth      *oauth2.Config
	configured bool
}

// NewCalendarClient creates a new CalendarClient.
// If oauthConfig is nil, the client will be in "not configured" mode.
func NewCalendarClient(db *store.Store, oauthConfig *oauth2.Config) *CalendarClient {
	if oauthConfig == nil {
		return &CalendarClient{store: db, configured: false}
	}
	return &CalendarClient{
		store:      db,
		oauth:      oauthConfig,
		configured: true,
	}
}

// CreateEventForTelegramUser creates a Google Calendar event for a Telegram user.
// It looks up the user's stored Google token, refreshes if needed, and creates the event.
func (c *CalendarClient) CreateEventForTelegramUser(ctx context.Context, telegramUserID int64, title string, dueAt time.Time, description string) (string, string, error) {
	if !c.configured {
		return "", "", ErrGoogleNotConfigured
	}

	tokenRow, err := c.store.GetGoogleToken(ctx, telegramUserID)
	if err != nil {
		return "", "", err
	}
	if tokenRow == nil {
		return "", "", ErrGoogleNotConnected
	}

	baseToken := &oauth2.Token{
		AccessToken:  tokenRow.AccessToken,
		RefreshToken: tokenRow.RefreshToken,
		TokenType:    tokenRow.TokenType,
		Expiry:       tokenRow.Expiry,
	}
	tokenSource := c.oauth.TokenSource(ctx, baseToken)
	freshToken, err := tokenSource.Token()
	if err != nil {
		return "", "", err
	}
	if tokenChanged(tokenRow, freshToken) {
		if err := c.store.UpsertGoogleToken(
			ctx,
			telegramUserID,
			freshToken.AccessToken,
			freshToken.RefreshToken,
			freshToken.TokenType,
			tokenRow.Scopes,
			freshToken.Expiry,
		); err != nil {
			return "", "", fmt.Errorf("persist refreshed token: %w", err)
		}
	}

	httpClient := oauth2.NewClient(ctx, oauth2.StaticTokenSource(freshToken))
	service, err := googlecalendar.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return "", "", err
	}

	start := dueAt.Add(-30 * time.Minute)
	event := &googlecalendar.Event{
		Summary: title,
		Start: &googlecalendar.EventDateTime{
			DateTime: start.Format(time.RFC3339),
			TimeZone: "Asia/Singapore",
		},
		End: &googlecalendar.EventDateTime{
			DateTime: dueAt.Format(time.RFC3339),
			TimeZone: "Asia/Singapore",
		},
		Description: description,
	}
	created, err := service.Events.Insert("primary", event).Do()
	if err != nil {
		return "", "", err
	}
	if created == nil || created.Id == "" {
		return "", "", errors.New("google calendar did not return event id")
	}
	return "primary", created.Id, nil
}

// DeleteEvent removes a Google Calendar event for a Telegram user.
// Used during reset to clean up CanvasLink-created events.
func (c *CalendarClient) DeleteEvent(ctx context.Context, telegramUserID int64, calendarID, eventID string) error {
	if !c.configured {
		return ErrGoogleNotConfigured
	}

	tokenRow, err := c.store.GetGoogleToken(ctx, telegramUserID)
	if err != nil {
		return err
	}
	if tokenRow == nil {
		return ErrGoogleNotConnected
	}

	baseToken := &oauth2.Token{
		AccessToken:  tokenRow.AccessToken,
		RefreshToken: tokenRow.RefreshToken,
		TokenType:    tokenRow.TokenType,
		Expiry:       tokenRow.Expiry,
	}
	tokenSource := c.oauth.TokenSource(ctx, baseToken)
	freshToken, err := tokenSource.Token()
	if err != nil {
		return err
	}
	if tokenChanged(tokenRow, freshToken) {
		if err := c.store.UpsertGoogleToken(
			ctx,
			telegramUserID,
			freshToken.AccessToken,
			freshToken.RefreshToken,
			freshToken.TokenType,
			tokenRow.Scopes,
			freshToken.Expiry,
		); err != nil {
			return fmt.Errorf("persist refreshed token: %w", err)
		}
	}

	httpClient := oauth2.NewClient(ctx, oauth2.StaticTokenSource(freshToken))
	service, err := googlecalendar.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return err
	}

	return service.Events.Delete(calendarID, eventID).Do()
}

func tokenChanged(stored *store.GoogleToken, fresh *oauth2.Token) bool {
	if stored == nil || fresh == nil {
		return false
	}
	if stored.AccessToken != fresh.AccessToken {
		return true
	}
	if fresh.RefreshToken != "" && stored.RefreshToken != fresh.RefreshToken {
		return true
	}
	if stored.TokenType != fresh.TokenType {
		return true
	}
	return !stored.Expiry.Equal(fresh.Expiry)
}
