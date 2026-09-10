package sync

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/markadodo/canvaslink/internal/store"
	"github.com/markadodo/canvaslink/internal/testsupport"
)

func TestPlannerDeliveryRestartSnoozeAndCompletion(t *testing.T) {
	s := testsupport.Store(t)
	ctx := context.Background()
	const user int64 = 72
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.UpsertTelegramAccountWithTimezone(ctx, user, user, "test", "UTC"))
	prefs := store.DefaultPlannerPreferences()
	prefs.QuietStart = 0
	prefs.QuietEnd = 0
	must(s.SavePlannerPreferences(ctx, user, prefs))
	// All network traffic in this test stays on the mock Telegram server.
	var sent atomic.Int64
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"Test","username":"test_bot"}}`)
			return
		}
		r.ParseForm()
		if fail.Load() {
			fmt.Fprint(w, `{"ok":false,"error_code":500,"description":"temporary"}`)
			return
		}
		sent.Add(1)
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":72,"type":"private"},"text":"ok"}}`)
	}))
	defer server.Close()
	api, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	must(err)
	worker := &Worker{store: s, api: api}
	now := time.Now().UTC()
	id, err := s.AddManualTask(ctx, user, "Write report", now.Add(20*time.Hour))
	must(err)
	must(worker.deliverPlannerUser(ctx, user, now))
	if sent.Load() != 1 {
		t.Fatalf("first reminder count=%d", sent.Load())
	}
	worker = &Worker{store: s, api: api}
	must(worker.deliverPlannerUser(ctx, user, now))
	if sent.Load() != 1 {
		t.Fatal("restart duplicated reminder")
	}
	snooze := now.Add(time.Hour)
	must(s.ChangePlannerTask(ctx, user, id, "snooze", &snooze))
	must(worker.deliverPlannerUser(ctx, user, now))
	if sent.Load() != 1 {
		t.Fatal("snooze ignored")
	}
	must(worker.deliverPlannerUser(ctx, user, snooze.Add(time.Minute)))
	if sent.Load() != 2 {
		t.Fatal("snooze missing")
	}
	must(worker.deliverPlannerUser(ctx, user, snooze.Add(2*time.Minute)))
	if sent.Load() != 2 {
		t.Fatal("snooze immediately repeated")
	}
	must(s.ChangePlannerTask(ctx, user, id, "done", nil))
	target := now.Add(10 * time.Hour)
	must(s.ChangePlannerTask(ctx, user, id, "target", &target))
	must(worker.deliverPlannerUser(ctx, user, now))
	if sent.Load() != 2 {
		t.Fatal("done task reminder sent")
	}
	must(s.ChangePlannerTask(ctx, user, id, "undo", nil))
	must(worker.deliverPlannerUser(ctx, user, now))
	if sent.Load() != 3 {
		t.Fatal("undo did not restore reminders")
	}
	// Failed sends remain pending; settings changed before retry invalidate them.
	fail.Store(true)
	_, err = s.AddManualTask(ctx, user, "Second task", now.Add(10*time.Hour))
	must(err)
	if err := worker.deliverPlannerUser(ctx, user, now); err == nil {
		t.Fatal("expected simulated failure")
	}
	pending, err := s.PlannerDeliveries(ctx, user)
	must(err)
	if len(pending) != 0 {
		t.Fatal("retry was not deferred")
	}
	fail.Store(false)
	prefs.Reminders = []int{}
	must(s.SavePlannerPreferences(ctx, user, prefs))
	must(worker.deliverPlannerUser(ctx, user, now))
	if sent.Load() != 3 {
		t.Fatal("disabled reminders sent")
	}
}
func TestReminderDeliveryCurrent(t *testing.T) {
	now := time.Now().UTC()
	task := store.PlannerTask{ID: 1, Revision: 3, DueAt: now.Add(30 * time.Minute)}
	prefs := store.DefaultPlannerPreferences()
	prefs.Reminders = []int{60, 1440}
	stale := store.PlannerDelivery{Key: "reminder:1:3:1440"}
	if reminderDeliveryCurrent(stale, task, prefs, now, time.UTC) {
		t.Fatal("stale offset survives newer elapsed offset")
	}
	current := store.PlannerDelivery{Key: "reminder:1:3:60"}
	if !reminderDeliveryCurrent(current, task, prefs, now, time.UTC) {
		t.Fatal("current reminder rejected")
	}
	prefs.Reminders = []int{10080}
	if reminderDeliveryCurrent(current, task, prefs, now, time.UTC) {
		t.Fatal("removed offset still sends")
	}
}
