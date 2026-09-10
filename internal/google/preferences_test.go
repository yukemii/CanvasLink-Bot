package google

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/markadodo/canvaslink/internal/testsupport"
	googlecalendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

func TestDedicatedCalendarRecoveryAndPinnedEventDestination(t *testing.T) {
	s := testsupport.Store(t)
	ctx := context.Background()
	const user int64 = 99
	if err := s.UpsertTelegramAccountWithTimezone(ctx, user, user, "test", "UTC"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var marker string
	var creates int
	var targets []string
	var latest googlecalendar.Event
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/me/calendarList"):
			items := []*googlecalendar.CalendarListEntry{{Id: "primary", Summary: "Personal", Primary: true, AccessRole: "owner"}}
			if marker != "" {
				items = append(items, &googlecalendar.CalendarListEntry{Id: "dedicated", Summary: "CanvasLink", Description: marker, AccessRole: "owner"})
			}
			json.NewEncoder(w).Encode(googlecalendar.CalendarList{Items: items})
		case r.URL.Path == "/calendars" && r.Method == "POST":
			var c googlecalendar.Calendar
			json.NewDecoder(r.Body).Decode(&c)
			marker = c.Description
			creates++
			c.Id = "dedicated"
			json.NewEncoder(w).Encode(c)
		case strings.HasSuffix(r.URL.Path, "/events") && r.Method == "POST":
			targets = append(targets, r.URL.Path)
			json.NewDecoder(r.Body).Decode(&latest)
			json.NewEncoder(w).Encode(latest)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()
	service, err := googlecalendar.NewService(ctx, option.WithHTTPClient(server.Client()), option.WithEndpoint(server.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	client := &CalendarClient{store: s, configured: true, instanceID: "test", serviceForUserOverride: func(context.Context, int64) (*googlecalendar.Service, error) { return service, nil }}
	id, err := client.EnsureDestination(ctx, user)
	if err != nil || id != "dedicated" {
		t.Fatalf("destination=%q %v", id, err)
	}
	p, err := s.PlannerPreferences(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	p.CalendarID = ""
	p.CalendarName = ""
	p.Colors["CS"] = "9"
	if err = s.SavePlannerPreferences(ctx, user, p); err != nil {
		t.Fatal(err)
	}
	// Simulate losing the preference write after creation: discover by marker.
	id, err = client.EnsureDestination(ctx, user)
	if err != nil || id != "dedicated" {
		t.Fatal(err)
	}
	p, _ = s.PlannerPreferences(ctx, user)
	p.CalendarID = "another"
	if err = s.SavePlannerPreferences(ctx, user, p); err != nil {
		t.Fatal(err)
	}
	_, _, err = client.CreateEventInCalendar(ctx, user, id, "uid", "Quiz", time.Now().Add(time.Hour), false, "Course: CS\nType: quiz")
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if creates != 1 {
		t.Fatalf("created %d calendars", creates)
	}
	if len(targets) != 1 || !strings.Contains(targets[0], "/calendars/dedicated/") {
		t.Fatalf("job migrated unexpectedly: %v", targets)
	}
	if latest.Summary != "[CS] Quiz" || latest.ColorId != "9" {
		t.Fatalf("style missing: %s", fmt.Sprint(latest.Summary, latest.ColorId))
	}
	if !isOwnedEvent(&latest, "test", "uid") {
		t.Fatal("ownership markers lost")
	}
}
