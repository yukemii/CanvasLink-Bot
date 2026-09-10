package google

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/markadodo/canvaslink/internal/store"
	"golang.org/x/oauth2"
	googlecalendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

var (
	ErrGoogleNotConfigured = errors.New("google oauth not configured")
	ErrGoogleNotConnected  = errors.New("google calendar not connected")
	// ErrGoogleAuthorizationInvalid indicates that the stored grant must be
	// refreshed through the user-facing OAuth flow before work can continue.
	ErrGoogleAuthorizationInvalid = errors.New("google calendar authorization is no longer valid")
	// ErrEventNotFound lets callers distinguish a safely recoverable missing
	// event from authentication, permission, and transport failures.
	ErrEventNotFound = errors.New("google calendar event not found")
	// ErrEventOwnershipUnverified prevents deletion when the target cannot be
	// proven to have been created by this CanvasLink instance.
	ErrEventOwnershipUnverified = errors.New("google calendar event ownership could not be verified")
	// ErrEventChangedDuringDeletion indicates that Google's ETag precondition
	// rejected a delete because the event changed after ownership inspection.
	ErrEventChangedDuringDeletion = errors.New("google calendar event changed during safe deletion")
	// ErrEventChangedDuringUpdate prevents an update from overwriting a user
	// edit made after CanvasLink inspected ownership and attendees.
	ErrEventChangedDuringUpdate = errors.New("google calendar event changed during safe update")
)

const calendarOperationTimeout = 30 * time.Second

// CalendarClient creates Google Calendar events for CanvasLink users.
// It uses the OAuth2 config from the OAuth server for token refresh.
type CalendarClient struct {
	store                  *store.Store
	oauth                  *oauth2.Config
	configured             bool
	instanceID             string
	serviceForUserOverride func(context.Context, int64) (*googlecalendar.Service, error)
}

// NewCalendarClient creates a new CalendarClient.
// If oauthConfig is nil, the client will be in "not configured" mode.
func NewCalendarClient(db *store.Store, oauthConfig *oauth2.Config, instanceID string) *CalendarClient {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		instanceID = "canvaslink"
	}
	if oauthConfig == nil {
		return &CalendarClient{store: db, configured: false, instanceID: instanceID}
	}
	return &CalendarClient{
		store:      db,
		oauth:      oauthConfig,
		configured: true,
		instanceID: instanceID,
	}
}

func (c *CalendarClient) IsConfigured() bool {
	return c != nil && c.configured
}

func (c *CalendarClient) DeterministicEventID(telegramUserID int64, canvasUID string) (string, error) {
	if c == nil {
		return "", errors.New("google calendar client is nil")
	}
	return DeterministicEventID(c.instanceID, telegramUserID, canvasUID)
}

// CreateEventForTelegramUser creates or replaces the Google Calendar event for
// a Canvas event. The deterministic event ID makes retries idempotent, including
// retries after a process exits between creating the event and recording it.
func (c *CalendarClient) CreateEventForTelegramUser(ctx context.Context, telegramUserID int64, canvasUID, title string, dueAt time.Time, allDay bool, description string) (string, string, error) {
	destination, err := c.EnsureDestination(ctx, telegramUserID)
	if err != nil {
		return "", "", err
	}
	return c.CreateEventInCalendar(ctx, telegramUserID, destination, canvasUID, title, dueAt, allDay, description)
}

// CreateEventInCalendar pins retries to the destination saved in the durable job.
func (c *CalendarClient) CreateEventInCalendar(ctx context.Context, telegramUserID int64, destination, canvasUID, title string, dueAt time.Time, allDay bool, description string) (string, string, error) {
	eventID, err := DeterministicEventID(c.instanceID, telegramUserID, canvasUID)
	if err != nil {
		return "", "", err
	}

	opCtx, cancel := context.WithTimeout(ctx, calendarOperationTimeout)
	defer cancel()

	service, err := c.serviceForUser(opCtx, telegramUserID)
	if err != nil {
		return "", "", err
	}

	event, err := c.styledEvent(ctx, telegramUserID, canvasUID, title, calendarEvent(c.instanceID, canvasUID, title, dueAt, allDay, description), description)
	if err != nil {
		return "", "", err
	}
	event.Id = eventID
	created, err := service.Events.Insert(destination, event).Context(opCtx).Do()
	if isGoogleAPIStatus(err, 409) {
		existing, inspectErr := inspectOwnedEvent(opCtx, service, destination, eventID, c.instanceID, canvasUID)
		if inspectErr != nil {
			return "", "", inspectErr
		}
		if strings.TrimSpace(existing.Etag) == "" {
			return "", "", ErrEventOwnershipUnverified
		}
		event.Id = "" // ID belongs in the URL, not in a patch body.
		updateCall := service.Events.Patch(
			destination,
			eventID,
			event,
		).Context(opCtx)
		updateCall.Header().Set("If-Match", existing.Etag)
		updated, updateErr := updateCall.Do()
		if updateErr != nil {
			return "", "", wrapEventOperationError("update existing google calendar event", updateErr)
		}
		if updated == nil || updated.Id != eventID || !isOwnedEvent(updated, c.instanceID, canvasUID) {
			return "", "", ErrEventOwnershipUnverified
		}
		return destination, eventID, nil
	}
	if err != nil {
		return "", "", wrapEventOperationError("create google calendar event", err)
	}
	if created == nil || created.Id != eventID || !isOwnedEvent(created, c.instanceID, canvasUID) {
		return "", "", ErrEventOwnershipUnverified
	}
	return destination, eventID, nil
}

// UpdateEvent updates a previously created CanvasLink event.
func (c *CalendarClient) UpdateEvent(ctx context.Context, telegramUserID int64, calendarID, eventID, canvasUID, title string, dueAt time.Time, allDay bool, description string) error {
	if eventID == "" {
		return errors.New("google event ID is required")
	}
	if strings.TrimSpace(canvasUID) == "" {
		return errors.New("canvas event UID is required")
	}
	if calendarID == "" {
		calendarID = "primary"
	}
	expectedEventID, err := c.DeterministicEventID(telegramUserID, canvasUID)
	if err != nil || eventID != expectedEventID {
		return ErrEventOwnershipUnverified
	}

	opCtx, cancel := context.WithTimeout(ctx, calendarOperationTimeout)
	defer cancel()

	service, err := c.serviceForUser(opCtx, telegramUserID)
	if err != nil {
		return err
	}
	existing, err := inspectOwnedEvent(opCtx, service, calendarID, eventID, c.instanceID, canvasUID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(existing.Etag) == "" {
		return ErrEventOwnershipUnverified
	}
	event, err := c.styledEvent(ctx, telegramUserID, canvasUID, title, calendarEvent(c.instanceID, canvasUID, title, dueAt, allDay, description), description)
	if err != nil {
		return err
	}
	updateCall := service.Events.Patch(
		calendarID,
		eventID,
		event,
	).Context(opCtx)
	updateCall.Header().Set("If-Match", existing.Etag)
	updated, err := updateCall.Do()
	if isGoogleAPIStatus(err, http.StatusPreconditionFailed) {
		return fmt.Errorf("update owned google calendar event: %w", errors.Join(ErrEventChangedDuringUpdate, err))
	}
	if err != nil {
		return wrapEventOperationError("update google calendar event", err)
	}
	if updated == nil || updated.Id != eventID || !isOwnedEvent(updated, c.instanceID, canvasUID) {
		return ErrEventOwnershipUnverified
	}
	return nil
}

// DeleteOwnedEvent deletes an event only after verifying the private
// CanvasLink ownership marker and source UID hash. Events with attendees are
// quarantined for manual review so CanvasLink cannot unexpectedly cancel an
// invitation for other people.
func (c *CalendarClient) DeleteOwnedEvent(ctx context.Context, telegramUserID int64, calendarID, eventID, canvasUID string) error {
	if eventID == "" {
		return nil
	}
	if strings.TrimSpace(canvasUID) == "" {
		return errors.New("canvas event UID is required")
	}
	if calendarID == "" {
		calendarID = "primary"
	}
	expectedEventID, err := c.DeterministicEventID(telegramUserID, canvasUID)
	if err != nil || eventID != expectedEventID {
		return ErrEventOwnershipUnverified
	}

	opCtx, cancel := context.WithTimeout(ctx, calendarOperationTimeout)
	defer cancel()
	service, err := c.serviceForUser(opCtx, telegramUserID)
	if err != nil {
		return err
	}

	event, err := inspectOwnedEvent(opCtx, service, calendarID, eventID, c.instanceID, canvasUID)
	if isGoogleAPIStatus(err, 404, 410) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(event.Etag) == "" {
		return ErrEventOwnershipUnverified
	}

	deleteCall := service.Events.Delete(calendarID, eventID).Context(opCtx)
	deleteCall.Header().Set("If-Match", event.Etag)
	err = deleteCall.Do()
	if isGoogleAPIStatus(err, 404, 410) {
		return nil
	}
	if isGoogleAPIStatus(err, http.StatusPreconditionFailed) {
		return fmt.Errorf("delete owned google calendar event: %w", errors.Join(ErrEventChangedDuringDeletion, err))
	}
	if err != nil {
		return wrapEventOperationError("delete owned google calendar event", err)
	}
	return nil
}

func (c *CalendarClient) serviceForUser(ctx context.Context, telegramUserID int64) (*googlecalendar.Service, error) {
	if c != nil && c.serviceForUserOverride != nil {
		return c.serviceForUserOverride(ctx, telegramUserID)
	}
	if !c.configured {
		return nil, ErrGoogleNotConfigured
	}

	tokenRow, err := c.store.GetGoogleToken(ctx, telegramUserID)
	if err != nil {
		return nil, err
	}
	if tokenRow == nil {
		return nil, ErrGoogleNotConnected
	}

	baseToken := &oauth2.Token{
		AccessToken:  tokenRow.AccessToken,
		RefreshToken: tokenRow.RefreshToken,
		TokenType:    tokenRow.TokenType,
		Expiry:       tokenRow.Expiry,
	}
	freshToken, err := c.oauth.TokenSource(ctx, baseToken).Token()
	if err != nil {
		if isOAuthAuthorizationError(err) {
			return nil, fmt.Errorf("refresh google token: %w", errors.Join(ErrGoogleAuthorizationInvalid, err))
		}
		return nil, fmt.Errorf("refresh google token: %w", err)
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
			return nil, fmt.Errorf("persist refreshed token: %w", err)
		}
	}

	httpClient := oauth2.NewClient(ctx, oauth2.StaticTokenSource(freshToken))
	httpClient.Timeout = calendarOperationTimeout
	service, err := googlecalendar.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("create google calendar service: %w", err)
	}
	return service, nil
}

// IsEventNotFound reports whether a Google Calendar operation failed because
// the target event was already absent.
func IsEventNotFound(err error) bool {
	if errors.Is(err, ErrEventNotFound) {
		return true
	}
	return isGoogleAPIStatus(err, 404, 410)
}

func calendarEvent(instanceID, canvasUID, title string, dueAt time.Time, allDay bool, description string) *googlecalendar.Event {
	privateProperties := map[string]string{
		"canvaslink_instance":   instanceID,
		"canvaslink_source_sha": sourceUIDHash(canvasUID),
		"canvaslink_version":    "1",
	}
	if allDay {
		return &googlecalendar.Event{
			Summary: title,
			Status:  "confirmed",
			Start: &googlecalendar.EventDateTime{
				Date: dueAt.Format(time.DateOnly),
			},
			End: &googlecalendar.EventDateTime{
				Date: dueAt.AddDate(0, 0, 1).Format(time.DateOnly),
			},
			Description: description,
			ExtendedProperties: &googlecalendar.EventExtendedProperties{
				Private: privateProperties,
			},
		}
	}

	start := dueAt.Add(-30 * time.Minute)
	return &googlecalendar.Event{
		Summary: title,
		Status:  "confirmed",
		Start: &googlecalendar.EventDateTime{
			DateTime: start.Format(time.RFC3339),
		},
		End: &googlecalendar.EventDateTime{
			DateTime: dueAt.Format(time.RFC3339),
		},
		Description: description,
		ExtendedProperties: &googlecalendar.EventExtendedProperties{
			Private: privateProperties,
		},
	}
}

// DeterministicEventID returns the stable Google Calendar event ID used for a
// Canvas event. Exposing the ID lets the durable outbox record the exact
// external target before attempting the Google API call.
func DeterministicEventID(instanceID string, telegramUserID int64, canvasUID string) (string, error) {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return "", errors.New("CanvasLink instance ID is required")
	}
	if telegramUserID <= 0 {
		return "", errors.New("telegram user ID must be positive")
	}
	if strings.TrimSpace(canvasUID) == "" {
		return "", errors.New("canvas event UID is required")
	}
	input := instanceID + "\x00" + strconv.FormatInt(telegramUserID, 10) + "\x00" + canvasUID
	digest := sha256.Sum256([]byte(input))
	return strings.ToLower(base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:])), nil
}

func sourceUIDHash(canvasUID string) string {
	digest := sha256.Sum256([]byte(canvasUID))
	return hex.EncodeToString(digest[:])
}

func isOwnedEvent(event *googlecalendar.Event, instanceID, canvasUID string) bool {
	if event == nil || event.ExtendedProperties == nil {
		return false
	}
	private := event.ExtendedProperties.Private
	return private["canvaslink_version"] == "1" &&
		private["canvaslink_instance"] == instanceID &&
		private["canvaslink_source_sha"] == sourceUIDHash(canvasUID)
}

func inspectOwnedEvent(
	ctx context.Context,
	service *googlecalendar.Service,
	calendarID,
	eventID,
	instanceID,
	canvasUID string,
) (*googlecalendar.Event, error) {
	event, err := service.Events.Get(calendarID, eventID).Context(ctx).Do()
	if err != nil {
		return nil, wrapEventOperationError("inspect google calendar event ownership", err)
	}
	if event == nil || event.Id != eventID ||
		!isOwnedEvent(event, instanceID, canvasUID) || len(event.Attendees) > 0 {
		return nil, ErrEventOwnershipUnverified
	}
	return event, nil
}

func wrapEventOperationError(operation string, err error) error {
	if isGoogleAPIStatus(err, 404, 410) {
		return fmt.Errorf("%s: %w", operation, errors.Join(ErrEventNotFound, err))
	}
	if isGoogleAPIStatus(err, 401) {
		return fmt.Errorf("%s: %w", operation, errors.Join(ErrGoogleAuthorizationInvalid, err))
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func isOAuthAuthorizationError(err error) bool {
	var retrieveErr *oauth2.RetrieveError
	if !errors.As(err, &retrieveErr) {
		return false
	}
	if retrieveErr.ErrorCode == "invalid_grant" || retrieveErr.ErrorCode == "invalid_token" {
		return true
	}
	return retrieveErr.Response != nil &&
		(retrieveErr.Response.StatusCode == 400 || retrieveErr.Response.StatusCode == 401)
}

func isGoogleAPIStatus(err error, statuses ...int) bool {
	if err == nil {
		return false
	}
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	for _, status := range statuses {
		if apiErr.Code == status {
			return true
		}
	}
	return false
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
