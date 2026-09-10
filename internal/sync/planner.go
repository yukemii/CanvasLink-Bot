package sync

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/markadodo/canvaslink/internal/canvas"
	"github.com/markadodo/canvaslink/internal/planner"
	"github.com/markadodo/canvaslink/internal/store"
)

func (w *Worker) recordPlannerSnapshot(ctx context.Context, user int64, events map[string]canvas.Event) error {
	account, err := w.store.GetTelegramAccount(ctx, user)
	if err != nil {
		return err
	}
	if account == nil {
		return nil
	}
	var tasks []store.PlannerTask
	for _, e := range events {
		if e.UID == "" || e.DueAt == nil || e.Course == "" || e.Cancelled {
			continue
		}
		tasks = append(tasks, store.PlannerTask{SourceUID: e.UID, Title: e.Title, CourseID: e.Course, AssignmentType: e.Type, DueAt: *e.DueAt, AllDay: e.AllDay, URL: e.URL})
	}
	return w.store.RecordPlannerSnapshot(ctx, user, tasks, planner.Location(account.Timezone))
}
func (w *Worker) runPlanner(ctx context.Context) {
	if err := w.store.PrunePlannerDeliveries(ctx); err != nil {
		log.Printf("prune planner deliveries: %v", err)
	}
	// New students get their first result within a minute, not a full sync interval.
	if feeds, err := w.store.ListEnabledFeeds(ctx); err == nil {
		for _, feed := range feeds {
			if feed.LastSyncedAt == nil {
				w.syncFeed(ctx, feed)
			}
		}
	}
	ids, err := w.store.PlannerUsers(ctx)
	if err != nil {
		log.Printf("planner users: %v", err)
		return
	}
	for _, user := range ids {
		if ctx.Err() != nil {
			return
		}
		release, ok, err := w.store.TryFeedSyncLock(ctx, user)
		if err != nil || !ok {
			continue
		}
		err = w.deliverPlannerUser(ctx, user, time.Now())
		if err != nil {
			log.Printf("planner user=%d: %v", user, err)
		}
		if err := release(); err != nil {
			log.Printf("planner release user=%d: %v", user, err)
		}
	}
}
func (w *Worker) deliverPlannerUser(ctx context.Context, user int64, now time.Time) error {
	account, err := w.store.GetTelegramAccount(ctx, user)
	if err != nil {
		return err
	}
	if account == nil || account.ChatID != user || account.OnboardingStatus != "" {
		return nil
	}
	loc := planner.Location(account.Timezone)
	now = now.In(loc)
	prefs, err := w.store.PlannerPreferences(ctx, user)
	if err != nil {
		return err
	}
	allowed, err := w.store.VisiblePlannerTasks(ctx, user)
	if err != nil {
		return err
	}
	byID := map[int64]store.PlannerTask{}
	for _, t := range allowed {
		byID[t.ID] = t
	}
	if !planner.Quiet(prefs, now) {
		for _, t := range allowed {
			if t.Done {
				continue
			}
			due := t.EffectiveDue(loc)
			if t.TargetAt != nil && t.TargetAt.Before(due) {
				due = *t.TargetAt
			}
			if !due.After(now) {
				continue
			}
			offsets := prefs.Offsets(t.CourseID, t.AssignmentType)
			if t.SnoozedUntil != nil {
				if now.Before(*t.SnoozedUntil) {
					continue
				}
				if len(offsets) > 0 {
					key := fmt.Sprintf("snooze:%d:%d:%d", t.ID, t.Revision, t.SnoozedUntil.Unix())
					if err := w.store.QueuePlannerDelivery(ctx, user, key, "reminder", &t, reminderBody(t, loc), due); err != nil {
						return err
					}
				}
				continue
			}
			// Only the most recently elapsed offset catches up after an outage/quiet hours.
			var chosen int
			found := false
			for _, offset := range offsets {
				at := t.ReminderAt(offset, loc)
				if !now.Before(at) && (!found || offset < chosen) {
					chosen = offset
					found = true
				}
			}
			if found {
				key := fmt.Sprintf("reminder:%d:%d:%d", t.ID, t.Revision, chosen)
				if err := w.store.QueuePlannerDelivery(ctx, user, key, "reminder", &t, reminderBody(t, loc), due); err != nil {
					return err
				}
			}
		}
		for _, weekly := range []bool{false, true} {
			ready, key := planner.DigestDue(prefs, weekly, now)
			if !ready {
				continue
			}
			view, title := "today", "☀️ Your daily agenda"
			if weekly {
				view, title = "week", "🗓 Your next 7 days"
			}
			end := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, loc)
			if err := w.store.QueuePlannerDelivery(ctx, user, key, "agenda", nil, planner.Summary(title, planner.Select(allowed, view, "", now), loc, 12), end); err != nil {
				return err
			}
		}
	}
	deliveries, err := w.store.PlannerDeliveries(ctx, user)
	if err != nil {
		return err
	}
	for _, d := range deliveries {
		if d.Kind != "health" && planner.Quiet(prefs, now) {
			continue
		}
		valid := true
		var task store.PlannerTask
		if d.TaskID != 0 {
			t, ok := byID[d.TaskID]
			task = t
			valid = ok && !t.Done && t.Revision == d.Revision
			if d.Kind == "reminder" {
				valid = valid && reminderDeliveryCurrent(d, t, prefs, now, loc)
			}
		}
		if d.Kind == "agenda" {
			if (strings.HasPrefix(d.Key, "daily:") && !prefs.Daily) || (strings.HasPrefix(d.Key, "weekly:") && !prefs.Weekly) {
				valid = false
			}
		}
		if !valid {
			if err := w.store.FinishPlannerDelivery(ctx, user, d.ID, true); err != nil {
				return err
			}
			continue
		}
		body := d.Body
		if d.Kind == "agenda" {
			weekly := strings.HasPrefix(d.Key, "weekly:")
			ready, key := planner.DigestDue(prefs, weekly, now)
			if !ready || key != d.Key {
				continue
			}
			view, title := "today", "☀️ Your daily agenda"
			if weekly {
				view, title = "week", "🗓 Your next 7 days"
			}
			body = planner.Summary(title, planner.Select(allowed, view, "", now), loc, 12)
			if status, err := w.store.PlannerSyncStatus(ctx, user, loc); err == nil {
				body += "\n\n" + status
			}
		}
		msg := tgbotapi.NewMessage(account.ChatID, body)
		if task.ID != 0 {
			msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("✅ Done", fmt.Sprintf("pl|done|%d", task.ID)),
				tgbotapi.NewInlineKeyboardButtonData("💤 Snooze 1h", fmt.Sprintf("pl|snooze|%d", task.ID)),
				tgbotapi.NewInlineKeyboardButtonData("Details", fmt.Sprintf("pl|task|%d", task.ID))))
		}
		_, sendErr := w.api.Send(msg)
		if sendErr == nil && strings.HasPrefix(d.Key, "snooze:") {
			if err := w.store.CompleteSnooze(ctx, user, d.ID, task, prefs.Offsets(task.CourseID, task.AssignmentType), now, loc); err != nil {
				return err
			}
			continue
		}
		if err := w.store.FinishPlannerDelivery(ctx, user, d.ID, sendErr == nil); err != nil {
			return err
		}
		if sendErr != nil {
			return fmt.Errorf("send notification: %w", sendErr)
		}
	}
	return nil
}
func reminderBody(t store.PlannerTask, loc *time.Location) string {
	body := fmt.Sprintf("⏰ Deadline reminder\n%s\n%s\nDue: %s", planner.Short(t.Title, 200), t.CourseID, store.PlannerDate(t.DueAt, t.AllDay, loc))
	if t.Manual {
		body = fmt.Sprintf("⏰ Personal task\n%s\nDue: %s", planner.Short(t.Title, 200), store.PlannerDate(t.DueAt, false, loc))
	}
	if t.TargetAt != nil {
		body += "\nPersonal target: " + store.PlannerDate(*t.TargetAt, false, loc)
	}
	return body + "\n\nDone stops reminders in CanvasLink; it does not submit work to Canvas."
}

func reminderDeliveryCurrent(d store.PlannerDelivery, t store.PlannerTask, p store.PlannerPreferences, now time.Time, loc *time.Location) bool {
	offsets := p.Offsets(t.CourseID, t.AssignmentType)
	if len(offsets) == 0 {
		return false
	}
	if t.SnoozedUntil != nil {
		return !now.Before(*t.SnoozedUntil) && d.Key == fmt.Sprintf("snooze:%d:%d:%d", t.ID, t.Revision, t.SnoozedUntil.Unix())
	}
	chosen := 0
	found := false
	for _, o := range offsets {
		if !now.Before(t.ReminderAt(o, loc)) && (!found || o < chosen) {
			chosen = o
			found = true
		}
	}
	return found && d.Key == fmt.Sprintf("reminder:%d:%d:%d", t.ID, t.Revision, chosen)
}
