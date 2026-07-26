package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/markadodo/canvaslink/internal/store"
	"golang.org/x/oauth2"
	googlecalendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

func TestCalendarEventPreservesDueTimeOffset(t *testing.T) {
	t.Parallel()

	location := time.FixedZone("SGT", 8*60*60)
	dueAt := time.Date(2026, 7, 25, 15, 30, 0, 0, location)
	event := calendarEvent("test-instance", "uid-1", "Quiz", dueAt, false, "Canvas event")

	if got, want := event.Start.DateTime, "2026-07-25T15:00:00+08:00"; got != want {
		t.Errorf("start = %q, want %q", got, want)
	}
	if got, want := event.End.DateTime, "2026-07-25T15:30:00+08:00"; got != want {
		t.Errorf("end = %q, want %q", got, want)
	}
	if event.Start.TimeZone != "" || event.End.TimeZone != "" {
		t.Errorf("calendarEvent() set a conflicting timezone: start=%q end=%q", event.Start.TimeZone, event.End.TimeZone)
	}
	if event.Start.Date != "" || event.End.Date != "" {
		t.Errorf("timed calendarEvent() unexpectedly set dates: start=%q end=%q", event.Start.Date, event.End.Date)
	}
	if !isOwnedEvent(event, "test-instance", "uid-1") {
		t.Error("timed event is missing ownership markers")
	}
}

func TestCalendarEventUsesExclusiveEndDateForAllDayEvent(t *testing.T) {
	t.Parallel()

	location := time.FixedZone("SGT", 8*60*60)
	dueAt := time.Date(2026, 7, 25, 0, 0, 0, 0, location)
	event := calendarEvent("test-instance", "uid-2", "Holiday", dueAt, true, "Canvas event")

	if got, want := event.Start.Date, "2026-07-25"; got != want {
		t.Errorf("start date = %q, want %q", got, want)
	}
	if got, want := event.End.Date, "2026-07-26"; got != want {
		t.Errorf("end date = %q, want %q", got, want)
	}
	if event.Start.DateTime != "" || event.End.DateTime != "" {
		t.Errorf("all-day calendarEvent() unexpectedly set times: start=%q end=%q", event.Start.DateTime, event.End.DateTime)
	}
	if !isOwnedEvent(event, "test-instance", "uid-2") {
		t.Error("all-day event is missing ownership markers")
	}
}

func TestDeterministicEventID(t *testing.T) {
	t.Parallel()

	first, err := DeterministicEventID("test-instance", 12345, "canvas-event-123")
	if err != nil {
		t.Fatalf("DeterministicEventID() error = %v", err)
	}
	if got, err := DeterministicEventID("test-instance", 12345, "canvas-event-123"); err != nil || got != first {
		t.Fatalf("same input produced different IDs: %q and %q", first, got)
	}

	validID := regexp.MustCompile(`^[0-9a-v]{52}$`)
	if !validID.MatchString(first) {
		t.Fatalf("event ID %q is not lowercase base32hex SHA-256", first)
	}

	otherUser, _ := DeterministicEventID("test-instance", 54321, "canvas-event-123")
	otherEvent, _ := DeterministicEventID("test-instance", 12345, "canvas-event-456")
	otherInstance, _ := DeterministicEventID("other-instance", 12345, "canvas-event-123")
	cases := []string{otherUser, otherEvent, otherInstance}
	for _, other := range cases {
		if other == first {
			t.Errorf("different input produced the same ID %q", first)
		}
	}
}

func TestIsEventNotFound(t *testing.T) {
	t.Parallel()

	if !IsEventNotFound(ErrEventNotFound) {
		t.Error("sentinel was not recognized")
	}
	if !IsEventNotFound(&googleapi.Error{Code: 404}) {
		t.Error("Google 404 was not recognized")
	}
	if !IsEventNotFound(&googleapi.Error{Code: 410}) {
		t.Error("Google 410 was not recognized")
	}
	if IsEventNotFound(&googleapi.Error{Code: 403}) {
		t.Error("Google 403 was incorrectly recognized")
	}

	wrapped := wrapEventOperationError("update", &googleapi.Error{Code: 404})
	if !errors.Is(wrapped, ErrEventNotFound) || !IsEventNotFound(wrapped) {
		t.Errorf("wrapped not-found error was not recognizable: %v", wrapped)
	}
}

func TestTokenChanged(t *testing.T) {
	t.Parallel()

	expiry := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	stored := &store.GoogleToken{
		AccessToken:  "access",
		RefreshToken: "refresh",
		TokenType:    "Bearer",
		Expiry:       expiry,
	}
	same := &oauth2.Token{
		AccessToken:  "access",
		RefreshToken: "",
		TokenType:    "Bearer",
		Expiry:       expiry,
	}
	if tokenChanged(stored, same) {
		t.Error("tokenChanged() reported an unchanged token")
	}

	changed := *same
	changed.AccessToken = "new-access"
	if !tokenChanged(stored, &changed) {
		t.Error("tokenChanged() missed an access-token change")
	}
}

func TestInspectOwnedEventVerifiesPrivateMarkers(t *testing.T) {
	const (
		instanceID = "test-instance"
		canvasUID  = "canvas-uid-ownership"
		eventID    = "owned-event-id"
	)

	tests := []struct {
		name      string
		mutate    func(*googlecalendar.Event)
		wantError bool
	}{
		{name: "matching markers"},
		{
			name: "missing extended properties",
			mutate: func(event *googlecalendar.Event) {
				event.ExtendedProperties = nil
			},
			wantError: true,
		},
		{
			name: "wrong instance",
			mutate: func(event *googlecalendar.Event) {
				event.ExtendedProperties.Private["canvaslink_instance"] = "other-instance"
			},
			wantError: true,
		},
		{
			name: "wrong source hash",
			mutate: func(event *googlecalendar.Event) {
				event.ExtendedProperties.Private["canvaslink_source_sha"] = sourceUIDHash("other-uid")
			},
			wantError: true,
		},
		{
			name: "wrong marker version",
			mutate: func(event *googlecalendar.Event) {
				event.ExtendedProperties.Private["canvaslink_version"] = "2"
			},
			wantError: true,
		},
		{
			name: "wrong response event ID",
			mutate: func(event *googlecalendar.Event) {
				event.Id = "unexpected-event-id"
			},
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := ownedTestEvent(instanceID, canvasUID, eventID, `"etag-1"`)
			if test.mutate != nil {
				test.mutate(event)
			}
			service := newMockCalendarService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method = %s, want GET", r.Method)
				}
				writeCalendarJSON(t, w, event)
			}))

			got, err := inspectOwnedEvent(
				context.Background(),
				service,
				"primary",
				eventID,
				instanceID,
				canvasUID,
			)
			if test.wantError {
				if !errors.Is(err, ErrEventOwnershipUnverified) {
					t.Fatalf("inspectOwnedEvent() error = %v, want ErrEventOwnershipUnverified", err)
				}
				if got != nil {
					t.Fatalf("inspectOwnedEvent() event = %#v, want nil", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("inspectOwnedEvent() error = %v", err)
			}
			if got == nil || got.Id != eventID {
				t.Fatalf("inspectOwnedEvent() event = %#v, want ID %q", got, eventID)
			}
		})
	}
}

func TestInspectOwnedEventRefusesAttendees(t *testing.T) {
	const (
		instanceID = "test-instance"
		canvasUID  = "canvas-uid-attendees"
		eventID    = "owned-event-with-attendees"
	)
	event := ownedTestEvent(instanceID, canvasUID, eventID, `"etag-1"`)
	event.Attendees = []*googlecalendar.EventAttendee{{Email: "classmate@example.com"}}

	service := newMockCalendarService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCalendarJSON(t, w, event)
	}))
	got, err := inspectOwnedEvent(
		context.Background(),
		service,
		"primary",
		eventID,
		instanceID,
		canvasUID,
	)
	if !errors.Is(err, ErrEventOwnershipUnverified) {
		t.Fatalf("inspectOwnedEvent() error = %v, want ErrEventOwnershipUnverified", err)
	}
	if got != nil {
		t.Fatalf("inspectOwnedEvent() event = %#v, want nil", got)
	}
}

func TestDeleteOwnedEventRejectsNonDeterministicIDBeforeHTTP(t *testing.T) {
	const (
		instanceID = "test-instance"
		canvasUID  = "canvas-uid-deterministic"
		userID     = int64(12345)
	)

	serviceCalls := 0
	client := &CalendarClient{
		configured: true,
		instanceID: instanceID,
		serviceForUserOverride: func(context.Context, int64) (*googlecalendar.Service, error) {
			serviceCalls++
			return nil, errors.New("service should not be requested")
		},
	}

	otherUIDEventID, err := DeterministicEventID(instanceID, userID, "different-canvas-uid")
	if err != nil {
		t.Fatal(err)
	}
	for _, eventID := range []string{"not-deterministic", otherUIDEventID} {
		err := client.DeleteOwnedEvent(context.Background(), userID, "primary", eventID, canvasUID)
		if !errors.Is(err, ErrEventOwnershipUnverified) {
			t.Errorf("DeleteOwnedEvent(%q) error = %v, want ErrEventOwnershipUnverified", eventID, err)
		}
	}
	if serviceCalls != 0 {
		t.Fatalf("service was requested %d times for invalid IDs", serviceCalls)
	}
}

func TestDeleteOwnedEventUsesInspectedETag(t *testing.T) {
	const (
		instanceID = "test-instance"
		canvasUID  = "canvas-uid-etag"
		userID     = int64(12345)
		etag       = `"etag-safe-delete"`
	)
	eventID := mustDeterministicEventID(t, instanceID, userID, canvasUID)

	var (
		mu      sync.Mutex
		methods []string
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()

		switch r.Method {
		case http.MethodGet:
			writeCalendarJSON(t, w, ownedTestEvent(instanceID, canvasUID, eventID, etag))
		case http.MethodDelete:
			if got := r.Header.Get("If-Match"); got != etag {
				t.Errorf("If-Match = %q, want %q", got, etag)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	client := mockCalendarClient(t, instanceID, handler)

	if err := client.DeleteOwnedEvent(context.Background(), userID, "primary", eventID, canvasUID); err != nil {
		t.Fatalf("DeleteOwnedEvent() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := strings.Join(methods, ","), "GET,DELETE"; got != want {
		t.Fatalf("methods = %s, want %s", got, want)
	}
}

func TestUpdateOwnedEventUsesInspectedETag(t *testing.T) {
	const (
		instanceID = "test-instance"
		canvasUID  = "canvas-uid-update-etag"
		userID     = int64(12345)
		etag       = `"etag-safe-update"`
	)
	eventID := mustDeterministicEventID(t, instanceID, userID, canvasUID)
	dueAt := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	var patchCalls int
	client := mockCalendarClient(t, instanceID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeCalendarJSON(t, w, ownedTestEvent(instanceID, canvasUID, eventID, etag))
		case http.MethodPatch:
			patchCalls++
			if got := r.Header.Get("If-Match"); got != etag {
				t.Errorf("If-Match = %q, want %q", got, etag)
			}
			writeCalendarJSON(t, w, ownedTestEvent(instanceID, canvasUID, eventID, `"etag-updated"`))
		default:
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	if err := client.UpdateEvent(
		context.Background(),
		userID,
		"primary",
		eventID,
		canvasUID,
		"Updated assignment",
		dueAt,
		false,
		"Updated by CanvasLink",
	); err != nil {
		t.Fatalf("UpdateEvent() error = %v", err)
	}
	if patchCalls != 1 {
		t.Fatalf("PATCH called %d times, want 1", patchCalls)
	}
}

func TestUpdateOwnedEventReportsETagConflict(t *testing.T) {
	const (
		instanceID = "test-instance"
		canvasUID  = "canvas-uid-update-conflict"
		userID     = int64(12345)
		etag       = `"etag-update-conflict"`
	)
	eventID := mustDeterministicEventID(t, instanceID, userID, canvasUID)
	client := mockCalendarClient(t, instanceID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeCalendarJSON(t, w, ownedTestEvent(instanceID, canvasUID, eventID, etag))
		case http.MethodPatch:
			writeCalendarError(w, http.StatusPreconditionFailed)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	err := client.UpdateEvent(
		context.Background(),
		userID,
		"primary",
		eventID,
		canvasUID,
		"Updated assignment",
		time.Now().Add(time.Hour),
		false,
		"",
	)
	if !errors.Is(err, ErrEventChangedDuringUpdate) {
		t.Fatalf("UpdateEvent() error = %v, want ErrEventChangedDuringUpdate", err)
	}
}

func TestUpdateOwnedEventRejectsUnverifiedResponse(t *testing.T) {
	const (
		instanceID = "test-instance"
		canvasUID  = "canvas-uid-update-response"
		userID     = int64(12345)
		etag       = `"etag-update-response"`
	)
	eventID := mustDeterministicEventID(t, instanceID, userID, canvasUID)
	client := mockCalendarClient(t, instanceID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeCalendarJSON(t, w, ownedTestEvent(instanceID, canvasUID, eventID, etag))
		case http.MethodPatch:
			updated := ownedTestEvent(instanceID, canvasUID, "unexpected-event-id", `"etag-updated"`)
			writeCalendarJSON(t, w, updated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	err := client.UpdateEvent(
		context.Background(),
		userID,
		"primary",
		eventID,
		canvasUID,
		"Updated assignment",
		time.Now().UTC().Add(time.Hour),
		false,
		"",
	)
	if !errors.Is(err, ErrEventOwnershipUnverified) {
		t.Fatalf("UpdateEvent() error = %v, want ErrEventOwnershipUnverified", err)
	}
}

func TestDeleteOwnedEventTreats404And410AsSuccess(t *testing.T) {
	const (
		instanceID = "test-instance"
		canvasUID  = "canvas-uid-not-found"
		userID     = int64(12345)
		etag       = `"etag-not-found"`
	)
	eventID := mustDeterministicEventID(t, instanceID, userID, canvasUID)

	for _, stage := range []string{"inspect", "delete"} {
		for _, status := range []int{http.StatusNotFound, http.StatusGone} {
			t.Run(fmt.Sprintf("%s_%d", stage, status), func(t *testing.T) {
				deleteCalls := 0
				client := mockCalendarClient(t, instanceID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.Method {
					case http.MethodGet:
						if stage == "inspect" {
							writeCalendarError(w, status)
							return
						}
						writeCalendarJSON(t, w, ownedTestEvent(instanceID, canvasUID, eventID, etag))
					case http.MethodDelete:
						deleteCalls++
						writeCalendarError(w, status)
					default:
						w.WriteHeader(http.StatusMethodNotAllowed)
					}
				}))

				if err := client.DeleteOwnedEvent(context.Background(), userID, "primary", eventID, canvasUID); err != nil {
					t.Fatalf("DeleteOwnedEvent() error = %v, want nil", err)
				}
				if stage == "inspect" && deleteCalls != 0 {
					t.Fatalf("DELETE called %d times after failed inspection", deleteCalls)
				}
				if stage == "delete" && deleteCalls != 1 {
					t.Fatalf("DELETE called %d times, want 1", deleteCalls)
				}
			})
		}
	}
}

func TestDeleteOwnedEventFailsClosedWithoutETag(t *testing.T) {
	const (
		instanceID = "test-instance"
		canvasUID  = "canvas-uid-no-etag"
		userID     = int64(12345)
	)
	eventID := mustDeterministicEventID(t, instanceID, userID, canvasUID)
	deleteCalls := 0
	client := mockCalendarClient(t, instanceID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeCalendarJSON(t, w, ownedTestEvent(instanceID, canvasUID, eventID, ""))
		case http.MethodDelete:
			deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		}
	}))

	err := client.DeleteOwnedEvent(context.Background(), userID, "primary", eventID, canvasUID)
	if !errors.Is(err, ErrEventOwnershipUnverified) {
		t.Fatalf("DeleteOwnedEvent() error = %v, want ErrEventOwnershipUnverified", err)
	}
	if deleteCalls != 0 {
		t.Fatalf("DELETE called %d times without an ETag", deleteCalls)
	}
}

func TestDeleteOwnedEventReportsETagConflict(t *testing.T) {
	const (
		instanceID = "test-instance"
		canvasUID  = "canvas-uid-etag-conflict"
		userID     = int64(12345)
		etag       = `"etag-conflict"`
	)
	eventID := mustDeterministicEventID(t, instanceID, userID, canvasUID)
	client := mockCalendarClient(t, instanceID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeCalendarJSON(t, w, ownedTestEvent(instanceID, canvasUID, eventID, etag))
		case http.MethodDelete:
			writeCalendarError(w, http.StatusPreconditionFailed)
		}
	}))

	err := client.DeleteOwnedEvent(context.Background(), userID, "primary", eventID, canvasUID)
	if !errors.Is(err, ErrEventChangedDuringDeletion) {
		t.Fatalf("DeleteOwnedEvent() error = %v, want ErrEventChangedDuringDeletion", err)
	}
}

func TestCreateEventRecoversOwnedCancelledConflict(t *testing.T) {
	const (
		instanceID = "test-instance"
		canvasUID  = "canvas-uid-cancelled"
		userID     = int64(12345)
		etag       = `"etag-cancelled"`
	)
	eventID := mustDeterministicEventID(t, instanceID, userID, canvasUID)
	dueAt := time.Date(2026, 7, 26, 17, 0, 0, 0, time.UTC)

	var (
		mu      sync.Mutex
		methods []string
	)
	client := mockCalendarClient(t, instanceID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()

		switch r.Method {
		case http.MethodPost:
			var inserted googlecalendar.Event
			if err := json.NewDecoder(r.Body).Decode(&inserted); err != nil {
				t.Errorf("decode inserted event: %v", err)
			}
			if inserted.Id != eventID {
				t.Errorf("inserted ID = %q, want %q", inserted.Id, eventID)
			}
			writeCalendarError(w, http.StatusConflict)
		case http.MethodGet:
			cancelled := ownedTestEvent(instanceID, canvasUID, eventID, etag)
			cancelled.Status = "cancelled"
			writeCalendarJSON(t, w, cancelled)
		case http.MethodPatch:
			if got := r.Header.Get("If-Match"); got != etag {
				t.Errorf("If-Match = %q, want %q", got, etag)
			}
			var patched googlecalendar.Event
			if err := json.NewDecoder(r.Body).Decode(&patched); err != nil {
				t.Errorf("decode patched event: %v", err)
			}
			if patched.Status != "confirmed" {
				t.Errorf("patched status = %q, want confirmed", patched.Status)
			}
			if !isOwnedEvent(&patched, instanceID, canvasUID) {
				t.Error("patched event is missing ownership markers")
			}
			patched.Id = eventID
			writeCalendarJSON(t, w, &patched)
		default:
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	calendarID, gotEventID, err := client.CreateEventForTelegramUser(
		context.Background(),
		userID,
		canvasUID,
		"Recovered assignment",
		dueAt,
		false,
		"CanvasLink event",
	)
	if err != nil {
		t.Fatalf("CreateEventForTelegramUser() error = %v", err)
	}
	if calendarID != "primary" || gotEventID != eventID {
		t.Fatalf("created target = %q/%q, want primary/%q", calendarID, gotEventID, eventID)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := strings.Join(methods, ","), "POST,GET,PATCH"; got != want {
		t.Fatalf("methods = %s, want %s", got, want)
	}
}

func TestCreateEventConflictRefusesUnownedTarget(t *testing.T) {
	const (
		instanceID = "test-instance"
		canvasUID  = "canvas-uid-unowned-conflict"
		userID     = int64(12345)
	)
	eventID := mustDeterministicEventID(t, instanceID, userID, canvasUID)
	patchCalls := 0
	client := mockCalendarClient(t, instanceID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeCalendarError(w, http.StatusConflict)
		case http.MethodGet:
			event := ownedTestEvent("other-instance", canvasUID, eventID, `"etag-1"`)
			writeCalendarJSON(t, w, event)
		case http.MethodPatch:
			patchCalls++
			w.WriteHeader(http.StatusNoContent)
		}
	}))

	_, _, err := client.CreateEventForTelegramUser(
		context.Background(),
		userID,
		canvasUID,
		"Assignment",
		time.Now().UTC().Add(time.Hour),
		false,
		"CanvasLink event",
	)
	if !errors.Is(err, ErrEventOwnershipUnverified) {
		t.Fatalf("CreateEventForTelegramUser() error = %v, want ErrEventOwnershipUnverified", err)
	}
	if patchCalls != 0 {
		t.Fatalf("PATCH called %d times for an unowned target", patchCalls)
	}
}

func TestCreateEventRejectsUnverifiedResponse(t *testing.T) {
	const (
		instanceID = "test-instance"
		canvasUID  = "canvas-uid-create-response"
		userID     = int64(12345)
	)
	eventID := mustDeterministicEventID(t, instanceID, userID, canvasUID)
	client := mockCalendarClient(t, instanceID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		created := ownedTestEvent(instanceID, canvasUID, "unexpected-event-id", `"etag-created"`)
		writeCalendarJSON(t, w, created)
	}))

	_, _, err := client.CreateEventForTelegramUser(
		context.Background(),
		userID,
		canvasUID,
		"Assignment",
		time.Now().UTC().Add(time.Hour),
		false,
		"CanvasLink event",
	)
	if !errors.Is(err, ErrEventOwnershipUnverified) {
		t.Fatalf("CreateEventForTelegramUser() error = %v, want ErrEventOwnershipUnverified (expected ID %q)", err, eventID)
	}
}

func mockCalendarClient(t *testing.T, instanceID string, handler http.Handler) *CalendarClient {
	t.Helper()
	service := newMockCalendarService(t, handler)
	return &CalendarClient{
		configured: true,
		instanceID: instanceID,
		serviceForUserOverride: func(context.Context, int64) (*googlecalendar.Service, error) {
			return service, nil
		},
	}
}

func newMockCalendarService(t *testing.T, handler http.Handler) *googlecalendar.Service {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	service, err := googlecalendar.NewService(
		context.Background(),
		option.WithoutAuthentication(),
		option.WithEndpoint(server.URL+"/"),
	)
	if err != nil {
		t.Fatalf("new mock calendar service: %v", err)
	}
	return service
}

func ownedTestEvent(instanceID, canvasUID, eventID, etag string) *googlecalendar.Event {
	event := calendarEvent(
		instanceID,
		canvasUID,
		"Owned Canvas event",
		time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
		false,
		"CanvasLink event",
	)
	event.Id = eventID
	event.Etag = etag
	return event
}

func mustDeterministicEventID(t *testing.T, instanceID string, userID int64, canvasUID string) string {
	t.Helper()
	eventID, err := DeterministicEventID(instanceID, userID, canvasUID)
	if err != nil {
		t.Fatalf("DeterministicEventID() error = %v", err)
	}
	return eventID
}

func writeCalendarJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode calendar response: %v", err)
	}
}

func writeCalendarError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(
		w,
		`{"error":{"code":%d,"message":%q,"status":"TEST_ERROR"}}`,
		status,
		http.StatusText(status),
	)
}
