package planner

import (
	"reflect"
	"testing"
	"time"

	"github.com/markadodo/canvaslink/internal/store"
)

func TestOffsetsValidationAndInheritance(t *testing.T) {
	got, err := ParseOffsets("1w, 1d, 1h, 1d")
	if err != nil || !reflect.DeepEqual(got, []int{60, 1440, 10080}) {
		t.Fatalf("offsets=%v %v", got, err)
	}
	for _, bad := range []string{"", "0d", "-1h", "31d", "1x", "999999999999999999999w", "1m,2m,3m,4m,5m,6m"} {
		if _, err := ParseOffsets(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	p := store.DefaultPlannerPreferences()
	p.Overrides[store.ReminderKey("CS", "")] = []int{10080}
	p.Overrides[store.ReminderKey("CS", "quiz")] = []int{}
	if len(p.Offsets("CS", "quiz")) != 0 || p.Offsets("CS", "assignment")[0] != 10080 || p.Offsets("OTHER", "quiz")[0] != 1440 {
		t.Fatal("inheritance failed")
	}
}
func TestLocalAgendaAndQuietHours(t *testing.T) {
	loc := Location("America/New_York")
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, loc)
	start, end := Bounds("today", now)
	if end.Sub(start) != 23*time.Hour {
		t.Fatal("DST day must be 23h")
	}
	tasks := []store.PlannerTask{
		{ID: 1, Present: true, DueAt: time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC), AllDay: true},
		{ID: 2, Present: true, DueAt: end},
		{ID: 3, Present: true, Done: true, DueAt: now.Add(time.Hour)},
		{ID: 4, Present: false, DueAt: now.Add(time.Hour)},
	}
	selected := Select(tasks, "today", "", now)
	if len(selected) != 1 || selected[0].ID != 1 {
		t.Fatalf("wrong local day filtering: %#v", selected)
	}
	p := store.DefaultPlannerPreferences()
	p.Daily = true
	p.DailyTime = "08:00"
	ready, key := DigestDue(p, false, now)
	if !ready || key != "daily:2026-03-08" {
		t.Fatal("daily schedule")
	}
	if !Quiet(p, time.Date(2026, 3, 8, 23, 0, 0, 0, loc)) || Quiet(p, now) {
		t.Fatal("overnight quiet hours")
	}
	p.QuietStart = 8
	p.QuietEnd = 17
	if !Quiet(p, now) {
		t.Fatal("daytime quiet hours")
	}
	p.QuietStart = 0
	p.QuietEnd = 0
	if Quiet(p, now) {
		t.Fatal("disabled quiet hours")
	}
}
func TestAllDayReminderPreservesLocalWallTimeAcrossDST(t *testing.T) {
	loc := Location("America/New_York")
	task := store.PlannerTask{DueAt: time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC), AllDay: true}
	want := time.Date(2026, 3, 7, 9, 0, 0, 0, loc)
	if got := task.ReminderAt(1440, loc); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	target := time.Date(2026, 3, 6, 15, 0, 0, 0, loc)
	task.TargetAt = &target
	if !task.ReminderAt(60, loc).Equal(target.Add(-time.Hour)) {
		t.Fatal("target ignored")
	}
	target = task.EffectiveDue(loc).Add(time.Hour)
	if !task.ReminderAt(1440, loc).Equal(want) {
		t.Fatal("invalid later target displaced official deadline")
	}
}
