package bot

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/markadodo/canvaslink/internal/canvas"
	"github.com/markadodo/canvaslink/internal/store"
)

func TestOAuthCompletionAccountRequiresCurrentConnection(t *testing.T) {
	t.Parallel()

	const userID int64 = 42
	account := &store.TelegramAccount{
		TelegramUserID:   userID,
		ChatID:           userID,
		OnboardingStatus: OnboardingGooglePrompt,
	}
	loadAccount := func(context.Context, int64) (*store.TelegramAccount, error) {
		return account, nil
	}

	t.Run("disconnected completion is suppressed", func(t *testing.T) {
		t.Parallel()

		got, err := oauthCompletionAccount(
			context.Background(),
			userID,
			loadAccount,
			func(context.Context, int64) (bool, error) { return false, nil },
		)
		if err != nil {
			t.Fatalf("oauthCompletionAccount() error = %v", err)
		}
		if got != nil {
			t.Fatalf("oauthCompletionAccount() = %#v, want nil", got)
		}
	})

	t.Run("currently connected completion continues", func(t *testing.T) {
		t.Parallel()

		got, err := oauthCompletionAccount(
			context.Background(),
			userID,
			loadAccount,
			func(context.Context, int64) (bool, error) { return true, nil },
		)
		if err != nil {
			t.Fatalf("oauthCompletionAccount() error = %v", err)
		}
		if got != account {
			t.Fatalf("oauthCompletionAccount() = %#v, want account", got)
		}
	})
}

func TestNextMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		current         string
		googleConnected bool
		want            string
	}{
		{name: "auto to active", current: store.ModeAuto, googleConnected: true, want: store.ModeActive},
		{name: "legacy quiet to active", current: store.ModeQuiet, googleConnected: true, want: store.ModeActive},
		{name: "active to ignore", current: store.ModeActive, googleConnected: true, want: store.ModeIgnore},
		{name: "review to ignore", current: store.ModeReview, googleConnected: true, want: store.ModeIgnore},
		{name: "ignore to auto with google", current: store.ModeIgnore, googleConnected: true, want: store.ModeAuto},
		{name: "ignore to active without google", current: store.ModeIgnore, googleConnected: false, want: store.ModeActive},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := nextMode(test.current, test.googleConnected); got != test.want {
				t.Errorf("nextMode(%q, %t) = %q, want %q", test.current, test.googleConnected, got, test.want)
			}
		})
	}
}

func TestCourseSettingsKeyboardSubmitsNextMode(t *testing.T) {
	t.Parallel()

	settings := []store.CourseSetting{{
		ID:             42,
		CourseID:       "CS2040S",
		AssignmentType: "quiz",
		Mode:           store.ModeActive,
	}}
	keyboard := courseSettingsKeyboard("CS2040S", settings, true)
	if len(keyboard.InlineKeyboard) == 0 || len(keyboard.InlineKeyboard[0]) == 0 {
		t.Fatal("courseSettingsKeyboard() returned no setting button")
	}
	data := keyboard.InlineKeyboard[0][0].CallbackData
	if data == nil || *data != "mode|42|ignore" {
		t.Fatalf("callback data = %v, want mode|42|ignore", data)
	}
}

func TestModuleCallbacksUseShortStableIDs(t *testing.T) {
	t.Parallel()

	keyboard := modulesKeyboard([]store.Course{{
		ID:         987,
		CourseID:   strings.Repeat("very-long-course-name", 5),
		CourseName: strings.Repeat("Very Long Course Name", 5),
	}})
	data := keyboard.InlineKeyboard[0][0].CallbackData
	if data == nil || *data != "course|987" {
		t.Fatalf("callback data = %v, want course|987", data)
	}
	if len(*data) > 64 {
		t.Fatalf("callback data is %d bytes", len(*data))
	}
}

func TestBuildSettingsMatrixHandlesLegacyAndCurrentModes(t *testing.T) {
	t.Parallel()

	text := BuildSettingsMatrixMessage("CS2040S", []store.CourseSetting{
		{AssignmentType: "assignment", Mode: store.ModeQuiet},
		{AssignmentType: "discussion", Mode: store.ModeNotify},
		{AssignmentType: "quiz", Mode: store.ModeReview},
		{AssignmentType: "exam", Mode: store.ModeIgnore},
	})
	for _, label := range []string{"Auto", "Auto + Notify", "Active", "Ignore"} {
		if !strings.Contains(text, label) {
			t.Errorf("settings text %q does not contain %q", text, label)
		}
	}
}

func TestLegacyNotifyModeDisplayPreservesItsBehavior(t *testing.T) {
	t.Parallel()

	_, label := displayMode(store.ModeNotify)
	if label != "Auto + Notify" {
		t.Fatalf("displayMode(ModeNotify) label = %q, want %q", label, "Auto + Notify")
	}
	if got := nextMode(store.ModeNotify, true); got != store.ModeActive {
		t.Fatalf("nextMode(ModeNotify, true) = %q, want %q", got, store.ModeActive)
	}
}

func TestUpcomingEventCount(t *testing.T) {
	t.Parallel()

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	events := []canvas.Event{
		{DueAt: &past},
		{DueAt: &future},
		{DueAt: &today, AllDay: true},
		{},
	}
	if got := upcomingEventCount(events); got != 2 {
		t.Errorf("upcomingEventCount() = %d, want 2", got)
	}
}

func TestSyncedEventConfirmedInGoogleRequiresConfirmation(t *testing.T) {
	t.Parallel()

	if syncedEventConfirmedInGoogle(nil) {
		t.Fatal("nil event was reported as confirmed")
	}
	if syncedEventConfirmedInGoogle(&store.SyncedEvent{
		GoogleEventID:   "deterministic-target",
		GoogleConfirmed: false,
	}) {
		t.Fatal("durably targeted but unconfirmed event was reported as already added")
	}
	if syncedEventConfirmedInGoogle(&store.SyncedEvent{
		GoogleEventID:   "",
		GoogleConfirmed: true,
	}) {
		t.Fatal("confirmed event without a Google target was reported as already added")
	}
	if !syncedEventConfirmedInGoogle(&store.SyncedEvent{
		GoogleEventID:   "deterministic-target",
		GoogleConfirmed: true,
	}) {
		t.Fatal("confirmed Google event was not recognized")
	}
}

func TestPendingActionMustExactlyMatchCurrentActiveSource(t *testing.T) {
	t.Parallel()

	dueAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	pending := &store.PendingAction{
		TelegramUserID:    42,
		CourseID:          "CS2040S",
		AssignmentType:    "quiz",
		CanvasEventUID:    "source-1",
		CanvasTitle:       "Quiz 1",
		CanvasDueAt:       dueAt,
		CanvasAllDay:      false,
		TelegramChatID:    "42",
		TelegramMessageID: sql.NullInt64{Int64: 100, Valid: true},
		Status:            store.PendingStatusPending,
	}
	event := &store.SyncedEvent{
		TelegramUserID:  42,
		CourseID:        "CS2040S",
		AssignmentType:  "quiz",
		CanvasEventUID:  "source-1",
		CanvasTitle:     "Quiz 1",
		CanvasDueAt:     dueAt,
		RemovalEligible: true,
		GoogleConfirmed: false,
	}
	if !pendingActionMatchesSyncedEvent(pending, event) {
		t.Fatal("exact pending action did not match its active source")
	}

	changed := *event
	changed.CourseID = "CS2106"
	if pendingActionMatchesSyncedEvent(pending, &changed) {
		t.Fatal("pending action matched a source whose course changed")
	}
	changed = *event
	changed.CanvasDueAt = dueAt.Add(time.Hour)
	if pendingActionMatchesSyncedEvent(pending, &changed) {
		t.Fatal("pending action matched a source whose due date changed")
	}
	changed = *event
	changed.Detached = true
	if pendingActionMatchesSyncedEvent(pending, &changed) {
		t.Fatal("pending action matched a detached source")
	}
}
