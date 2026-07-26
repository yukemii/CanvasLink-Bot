package sync

import (
	"testing"
	"time"

	"github.com/markadodo/canvaslink/internal/canvas"
	"github.com/markadodo/canvaslink/internal/store"
)

func TestEventChanged(t *testing.T) {
	t.Parallel()

	dueAt := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	stamp := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	event := canvas.Event{
		UID:      "event-1",
		Title:    "Quiz",
		Course:   "CS2040S",
		Type:     "quiz",
		DueAt:    &dueAt,
		DTStamp:  &stamp,
		Sequence: 1,
	}
	previous := store.SyncedEvent{
		CanvasEventUID: "event-1",
		CanvasTitle:    "Quiz",
		CourseID:       "CS2040S",
		AssignmentType: "quiz",
		CanvasDueAt:    dueAt,
		CanvasDTStamp:  &stamp,
		CanvasSequence: 1,
	}

	if eventChanged(&event, &previous) {
		t.Fatal("eventChanged() = true for equal events")
	}

	changed := event
	changed.Course = "CS2106"
	if !eventChanged(&changed, &previous) {
		t.Error("eventChanged() missed a course change")
	}

	changed = event
	changed.DTStamp = nil
	if !eventChanged(&changed, &previous) {
		t.Error("eventChanged() missed removal of DTSTAMP")
	}

	changed = event
	changed.Sequence = 0
	if !eventChanged(&changed, &previous) {
		t.Error("eventChanged() missed a decreased sequence")
	}
}

func TestNewWorkerRejectsInvalidIntervalBeforeTelegramSetup(t *testing.T) {
	t.Parallel()

	if _, err := NewWorker(&store.Store{}, 0, "unused", nil); err == nil {
		t.Fatal("NewWorker() accepted a zero interval")
	}
}

func TestEventHasPassedKeepsAllDayEventForEntireDate(t *testing.T) {
	t.Parallel()

	location := time.FixedZone("SGT", 8*60*60)
	dueAt := time.Date(2026, 7, 25, 0, 0, 0, 0, location)
	event := canvas.Event{DueAt: &dueAt, AllDay: true}

	if eventHasPassed(event, time.Date(2026, 7, 25, 23, 59, 0, 0, location)) {
		t.Error("eventHasPassed() expired an all-day event before the day ended")
	}
	if !eventHasPassed(event, time.Date(2026, 7, 26, 0, 0, 0, 0, location)) {
		t.Error("eventHasPassed() kept an all-day event after the day ended")
	}
}

func TestMassRemovalGuardClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		futureTracked int
		missing       int
		wantGuard     bool
	}{
		{name: "nothing missing", futureTracked: 3, missing: 0, wantGuard: false},
		{name: "single removed event", futureTracked: 1, missing: 1, wantGuard: false},
		{name: "one isolated loss", futureTracked: 5, missing: 1, wantGuard: false},
		{name: "two losses use small-feed grace", futureTracked: 2, missing: 2, wantGuard: false},
		{name: "large majority loss", futureTracked: 5, missing: 3, wantGuard: true},
		{name: "large non-majority loss", futureTracked: 6, missing: 3, wantGuard: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := requiresMassRemovalGuard(test.futureTracked, test.missing); got != test.wantGuard {
				t.Fatalf("requiresMassRemovalGuard(%d, %d) = %v, want %v",
					test.futureTracked, test.missing, got, test.wantGuard)
			}
		})
	}
}

func TestRemovalThresholdRequiresCountAndElapsedGrace(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-5 * time.Hour)
	old := now.Add(-7 * time.Hour)

	if removalThresholdMet(&store.SyncedEvent{MissingSince: &old, MissingCount: 2}, now, 3, 6*time.Hour) {
		t.Fatal("removal threshold passed without enough healthy observations")
	}
	if removalThresholdMet(&store.SyncedEvent{MissingSince: &recent, MissingCount: 3}, now, 3, 6*time.Hour) {
		t.Fatal("removal threshold passed before elapsed grace")
	}
	if !removalThresholdMet(&store.SyncedEvent{MissingSince: &old, MissingCount: 3}, now, 3, 6*time.Hour) {
		t.Fatal("removal threshold did not pass after both independent guards")
	}
}

func TestCompleteSmallFeedLossGetsExtendedGrace(t *testing.T) {
	t.Parallel()

	worker := &Worker{removalMisses: 3, removalGracePeriod: 6 * time.Hour}
	misses, grace := worker.removalThresholds(1, 1)
	if misses != 6 || grace != 24*time.Hour {
		t.Fatalf("small-feed thresholds = %d, %s; want 6, 24h", misses, grace)
	}
	misses, grace = worker.removalThresholds(5, 1)
	if misses != 3 || grace != 6*time.Hour {
		t.Fatalf("ordinary thresholds = %d, %s; want 3, 6h", misses, grace)
	}
	misses, grace = worker.removalThresholds(5, 3)
	if misses != 12 || grace != 7*24*time.Hour {
		t.Fatalf("mass-removal thresholds = %d, %s; want 12, 168h", misses, grace)
	}
}

func TestDetachedAndPastEventsAreNeverDeleteEligible(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	past := store.SyncedEvent{
		CanvasDueAt:     now.Add(-time.Minute),
		RemovalEligible: true,
	}
	if !syncedEventHasPassed(past, now) {
		t.Fatal("past timed event was not recognized as historical")
	}
	detached := store.SyncedEvent{
		CanvasDueAt:     now.Add(time.Hour),
		Detached:        true,
		RemovalEligible: false,
	}
	if syncedEventHasPassed(detached, now) {
		t.Fatal("future detached event was treated as past")
	}
	if !detached.Detached || detached.RemovalEligible {
		t.Fatal("test fixture did not represent a fail-closed detached ownership row")
	}
}

func TestTelegramSendBudgetCountsAttemptsAndNeverUnderflows(t *testing.T) {
	t.Parallel()

	budget := newTelegramSendBudget(2)
	if !budget.take() || !budget.take() {
		t.Fatal("budget rejected an allowed send attempt")
	}
	if budget.take() {
		t.Fatal("budget allowed a third send attempt")
	}
	if budget.remaining != 0 {
		t.Fatalf("remaining budget = %d, want 0", budget.remaining)
	}

	disabled := newTelegramSendBudget(-1)
	if disabled.take() || disabled.remaining != 0 {
		t.Fatalf("negative budget was not clamped: %#v", disabled)
	}
}
