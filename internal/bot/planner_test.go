package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/markadodo/canvaslink/internal/store"
	"github.com/markadodo/canvaslink/internal/testsupport"
)

func TestPlannerInputSettingsAndTaskOwnership(t *testing.T) {
	s := testsupport.Store(t)
	b := &Bot{store: s}
	ctx := context.Background()
	const user int64 = 24
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.UpsertTelegramAccountWithTimezone(ctx, user, user, "test", "Asia/Singapore"))
	must(s.UpsertTelegramAccountWithTimezone(ctx, 25, 25, "other", "UTC"))
	must(b.applyPlannerInput(ctx, user, "daily", "09:30"))
	must(b.applyPlannerInput(ctx, user, "weekly", "Sun 18:00"))
	must(b.applyPlannerInput(ctx, user, "quiet", "23-07"))
	must(b.applyPlannerInput(ctx, user, "offset:0", "1d,1w"))
	p, err := s.PlannerPreferences(ctx, user)
	must(err)
	if !p.Daily || !p.Weekly || p.Weekday != 0 || p.QuietStart != 23 || len(p.Reminders) != 2 {
		t.Fatalf("settings %#v", p)
	}
	for action, text := range map[string]string{"daily": "25:00", "weekly": "Someday 18:00", "quiet": "30-40", "offset:0": "0d", "add": "not a date | Task"} {
		if b.applyPlannerInput(ctx, user, action, text) == nil {
			t.Errorf("accepted %s %s", action, text)
		}
	}
	date := time.Now().Add(72 * time.Hour).In(time.FixedZone("SGT", 8*3600)).Format("2006-01-02 15:04")
	must(b.applyPlannerInput(ctx, user, "add", date+" | Write slides"))
	tasks, err := s.PlannerTasks(ctx, user)
	must(err)
	if len(tasks) != 1 || !tasks[0].Manual || tasks[0].Title != "Write slides" {
		t.Fatal("manual task not created")
	}
	if _, err := s.PlannerTask(ctx, 25, tasks[0].ID); err == nil {
		t.Fatal("cross-user read")
	}
	if _, err := b.plannerInputPrompt(ctx, user, "nonsense", time.UTC); err == nil {
		t.Fatal("unknown prompt accepted")
	}
	if _, err := parsePlannerDate("2026-03-08 02:30", mustLocation(t, "America/New_York")); err == nil {
		t.Fatal("nonexistent local time accepted")
	}
}
func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
func TestTaskURLAndCallbackBudget(t *testing.T) {
	// IDs, rather than course names or URLs, keep every new callback below 64 bytes.
	key := calendarChoiceKey(strings.Repeat("calendar", 100))
	if len("pl|calchoose|"+key) > 64 {
		t.Fatal("oversized callback")
	}
	if store.DefaultPlannerPreferences().Reminders[0] != 1440 {
		t.Fatal("reminders not on one day before")
	}
}
