package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostgresPlannerLifecycle(t *testing.T) {
	base := os.Getenv("CANVASLINK_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires PostgreSQL")
	}
	ctx := context.Background()
	s, err := Connect(isolatedPostgresURL(t, ctx, base))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.InitSchema(ctx))
	must(s.InitSchema(ctx))
	const user int64 = 81
	must(s.UpsertTelegramAccountWithTimezone(ctx, user, user, "planner", "Asia/Singapore"))
	p, err := s.PlannerPreferences(ctx, user)
	must(err)
	if len(p.Reminders) != 1 || p.Reminders[0] != 1440 || p.Daily || p.Weekly {
		t.Fatal("wrong defaults")
	}
	p.DefaultMode = ModeAuto
	must(s.SavePlannerPreferences(ctx, user, p))
	must(s.SeedDefaultSettings(ctx, user, []CourseTypeSeed{{CourseID: "CS", AssignmentType: "quiz"}}))
	mode, err := s.GetCourseTypeMode(ctx, user, "CS", "quiz")
	must(err)
	if mode != ModeQuiet {
		t.Fatal("new courses did not inherit auto")
	}
	due := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	task := PlannerTask{SourceUID: "quiz", Title: "Quiz 1", CourseID: "CS", AssignmentType: "quiz", DueAt: due}
	must(s.RecordPlannerSnapshot(ctx, user, []PlannerTask{task}, time.UTC))
	tasks, err := s.PlannerTasks(ctx, user)
	must(err)
	id := tasks[0].ID
	must(s.ChangePlannerTask(ctx, user, id, "done", nil))
	target := due.Add(-12 * time.Hour)
	must(s.ChangePlannerTask(ctx, user, id, "target", &target))
	task.DueAt = due.Add(24 * time.Hour)
	must(s.RecordPlannerSnapshot(ctx, user, []PlannerTask{task}, time.UTC))
	got, err := s.PlannerTask(ctx, user, id)
	must(err)
	if !got.Done || got.TargetAt == nil || !got.TargetAt.Equal(target) {
		t.Fatal("feed overwrite lost local state")
	}
	deliveries, err := s.PlannerDeliveries(ctx, user)
	must(err)
	if len(deliveries) != 1 || !strings.Contains(deliveries[0].Body, "Previously:") || !strings.Contains(deliveries[0].Body, "Now:") {
		t.Fatal("missing durable change diff")
	}
	must(s.RecordPlannerSnapshot(ctx, user, []PlannerTask{task}, time.UTC))
	deliveries, err = s.PlannerDeliveries(ctx, user)
	must(err)
	if len(deliveries) != 1 {
		t.Fatal("duplicate change")
	}
	must(s.RecordPlannerSnapshot(ctx, user, nil, time.UTC))
	got, err = s.PlannerTask(ctx, user, id)
	must(err)
	if got.Present {
		t.Fatal("missing item still active")
	}
	must(s.RecordPlannerSnapshot(ctx, user, []PlannerTask{task}, time.UTC))
	got, err = s.PlannerTask(ctx, user, id)
	must(err)
	if !got.Present {
		t.Fatal("reappearance not restored")
	}
	if err := s.ChangePlannerTask(ctx, 999, id, "done", nil); err == nil {
		t.Fatal("cross-user write allowed")
	}
	manual, err := s.AddManualTask(ctx, user, "Prepare slides", due)
	must(err)
	must(s.SetPlannerInput(ctx, user, "quiet"))
	action, err := s.PlannerInput(ctx, user)
	must(err)
	if action != "quiet" {
		t.Fatal("input not persisted")
	}
	for i := 0; i < 3; i++ {
		must(s.RecordHealth(ctx, user, "Canvas", false, 3, "Disconnected"))
	}
	deliveries, err = s.PlannerDeliveries(ctx, user)
	must(err)
	var healthID int64
	for _, d := range deliveries {
		if d.Kind == "health" {
			healthID = d.ID
		}
	}
	if healthID == 0 {
		t.Fatal("no failure alert")
	}
	must(s.RecordHealth(ctx, user, "Canvas", false, 3, "Disconnected"))
	must(s.FinishPlannerDelivery(ctx, user, healthID, true))
	must(s.RecordHealth(ctx, user, "Canvas", true, 3, ""))
	deliveries, err = s.PlannerDeliveries(ctx, user)
	must(err)
	recoveries := 0
	for _, d := range deliveries {
		if d.Kind == "health" {
			recoveries++
		}
	}
	if recoveries != 1 {
		t.Fatal("expected exactly one recovery")
	}
	must(s.QueueFirstPlannerSummary(ctx, user, "first"))
	must(s.QueueFirstPlannerSummary(ctx, user, "duplicate"))
	var summaries int
	must(s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM canvaslink_deliveries WHERE kind='summary'`).Scan(&summaries))
	if summaries != 1 {
		t.Fatal("first summary repeated")
	}
	must(s.DisconnectCanvas(ctx, user))
	if _, err := s.PlannerTask(ctx, user, id); err == nil {
		t.Fatal("disconnected Canvas reminder retained")
	}
	if _, err := s.PlannerTask(ctx, user, manual); err != nil {
		t.Fatal("manual task lost on Canvas disconnect")
	}
	must(s.ResetUser(ctx, user))
	for _, table := range []string{"canvaslink_tasks", "canvaslink_preferences", "canvaslink_deliveries", "canvaslink_connection_health", "canvaslink_planner_inputs"} {
		var n int
		must(s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n))
		if n != 0 {
			t.Fatalf("reset retained %s", table)
		}
	}
}
