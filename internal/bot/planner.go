package bot

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/markadodo/canvaslink/internal/canvas"
	"github.com/markadodo/canvaslink/internal/planner"
	"github.com/markadodo/canvaslink/internal/store"
)

func button(label, data string) tgbotapi.InlineKeyboardButton {
	return tgbotapi.NewInlineKeyboardButtonData(label, data)
}
func row(buttons ...tgbotapi.InlineKeyboardButton) []tgbotapi.InlineKeyboardButton {
	return tgbotapi.NewInlineKeyboardRow(buttons...)
}
func (b *Bot) plannerMessage(chat int64, text string, rows ...[]tgbotapi.InlineKeyboardButton) error {
	msg := tgbotapi.NewMessage(chat, text)
	if len(rows) > 0 {
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	}
	_, err := b.api.Send(msg)
	return err
}
func (b *Bot) handlePlannerCommand(ctx context.Context, msg *tgbotapi.Message, user int64, command string) bool {
	var action string
	switch command {
	case "today", "week", "upcoming":
		action = "list|" + command + "|0|0"
	case "completed":
		action = "list|done|0|0"
	case "add":
		action = "input|add"
	case "reminders":
		action = "rem|0"
	case "agenda":
		action = "agenda"
	case "cancel":
		if err := b.store.ClearPlannerInput(ctx, user); err != nil {
			b.reply(msg.Chat.ID, "Could not cancel. Please try again.")
		} else {
			b.reply(msg.Chat.ID, "Input cancelled.")
		}
		return true
	default:
		return false
	}
	account, err := b.store.GetTelegramAccount(ctx, user)
	if err != nil || account == nil {
		b.reply(msg.Chat.ID, "Send /start to set up CanvasLink first.")
		return true
	}
	release, ok, err := b.store.TryFeedSyncLock(ctx, user)
	if err != nil || !ok {
		b.reply(msg.Chat.ID, "A sync is finishing. Please try again in a moment.")
		return true
	}
	defer release()
	if err := b.plannerAction(ctx, msg.Chat.ID, user, strings.Split(action, "|")); err != nil {
		b.reply(msg.Chat.ID, "Could not finish: "+planner.Short(err.Error(), 300))
	}
	return true
}
func (b *Bot) handlePlannerCallback(ctx context.Context, cq *tgbotapi.CallbackQuery, user int64) {
	release, ok, err := b.store.TryFeedSyncLock(ctx, user)
	if err != nil || !ok {
		b.answerCallback(cq.ID, "A sync is finishing. Try again shortly.")
		return
	}
	defer release()
	b.answerCallback(cq.ID, "")
	if err := b.plannerAction(ctx, cq.Message.Chat.ID, user, strings.Split(strings.TrimPrefix(cq.Data, "pl|"), "|")); err != nil {
		b.reply(cq.Message.Chat.ID, "Could not finish: "+planner.Short(err.Error(), 300))
	}
}
func (b *Bot) plannerAction(ctx context.Context, chat, user int64, parts []string) error {
	if len(parts) == 0 {
		return fmt.Errorf("invalid action")
	}
	account, err := b.store.GetTelegramAccount(ctx, user)
	if err != nil {
		return err
	}
	if account == nil {
		return fmt.Errorf("send /start first")
	}
	loc := planner.Location(account.Timezone)
	prefs, err := b.store.PlannerPreferences(ctx, user)
	if err != nil {
		return err
	}
	id := int64(0)
	if len(parts) > 1 {
		id, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	switch parts[0] {
	case "list":
		if len(parts) != 4 {
			return fmt.Errorf("invalid list")
		}
		page, e := strconv.Atoi(parts[2])
		if e != nil || page < 0 || page > 10000 {
			return fmt.Errorf("invalid page")
		}
		filter, e := strconv.ParseInt(parts[3], 10, 64)
		if e != nil {
			return e
		}
		course := ""
		if filter != 0 {
			setting, e := b.store.GetCourseSettingByID(ctx, user, filter)
			if e != nil || setting == nil {
				return fmt.Errorf("course no longer available")
			}
			course = setting.CourseID
		}
		return b.showPlannerList(ctx, chat, user, parts[1], course, filter, page, loc)
	case "filters":
		courses, e := b.store.ListUserCourses(ctx, user)
		if e != nil {
			return e
		}
		rows := [][]tgbotapi.InlineKeyboardButton{row(button("All courses", "pl|list|upcoming|0|0"))}
		for _, c := range courses {
			rows = append(rows, row(button(c.CourseID, fmt.Sprintf("pl|list|upcoming|0|%d", c.ID))))
		}
		return b.plannerMessage(chat, "Filter upcoming items", rows...)
	case "task":
		return b.showPlannerTask(ctx, chat, user, id, loc)
	case "done", "undo", "snooze", "delete":
		t, e := b.store.PlannerTask(ctx, user, id)
		if e != nil {
			return fmt.Errorf("item no longer available")
		}
		if parts[0] == "delete" {
			if !t.Manual {
				return fmt.Errorf("only personal tasks can be deleted")
			}
			if e := b.store.ChangePlannerTask(ctx, user, id, "delete", nil); e != nil {
				return e
			}
			return b.plannerMessage(chat, "Personal task deleted.", row(button("Upcoming", "pl|list|upcoming|0|0")))
		}
		var value *time.Time
		if parts[0] == "snooze" {
			v := time.Now().Add(time.Hour)
			value = &v
		}
		if e := b.store.ChangePlannerTask(ctx, user, id, parts[0], value); e != nil {
			return e
		}
		return b.showPlannerTask(ctx, chat, user, id, loc)
	case "targetclear":
		if err := b.store.ChangePlannerTask(ctx, user, id, "target", nil); err != nil {
			return err
		}
		return b.showPlannerTask(ctx, chat, user, id, loc)
	case "input":
		if len(parts) != 2 {
			return fmt.Errorf("invalid input")
		}
		prompt, e := b.plannerInputPrompt(ctx, user, parts[1], loc)
		if e != nil {
			return e
		}
		if e := b.store.SetPlannerInput(ctx, user, parts[1]); e != nil {
			return e
		}
		return b.plannerMessage(chat, prompt+"\n\nSend /cancel to cancel. This input expires in 15 minutes.")
	case "rem":
		return b.showReminders(ctx, chat, user, id, prefs)
	case "remcourses":
		courses, e := b.store.ListUserCourses(ctx, user)
		if e != nil {
			return e
		}
		rows := [][]tgbotapi.InlineKeyboardButton{}
		for _, c := range courses {
			rows = append(rows, row(button(c.CourseID, fmt.Sprintf("pl|rem|-%d", c.ID))))
		}
		rows = append(rows, row(button("Overall default", "pl|rem|0")))
		return b.plannerMessage(chat, "Choose a course to override its reminders. Each course can also have different offsets per assignment type.", rows...)
	case "remtypes":
		setting, e := b.store.GetCourseSettingByID(ctx, user, id)
		if e != nil || setting == nil {
			return fmt.Errorf("course no longer available")
		}
		settings, e := b.store.ListCourseSettings(ctx, user, setting.CourseID)
		if e != nil {
			return e
		}
		rows := [][]tgbotapi.InlineKeyboardButton{}
		for _, s := range settings {
			rows = append(rows, row(button(typeLabel(s.AssignmentType), fmt.Sprintf("pl|rem|%d", s.ID))))
		}
		return b.plannerMessage(chat, "Reminder overrides for "+setting.CourseID, rows...)
	case "offset":
		if len(parts) != 3 {
			return fmt.Errorf("invalid offset")
		}
		scope, e := b.reminderScope(ctx, user, id)
		if e != nil {
			return e
		}
		if parts[2] == "inherit" {
			if id != 0 {
				delete(prefs.Overrides, scope)
			}
		} else {
			v, e := planner.ParseOffsets(parts[2])
			if e != nil {
				return e
			}
			if id == 0 {
				prefs.Reminders = v
			} else {
				prefs.Overrides[scope] = v
			}
		}
		if e := b.store.SavePlannerPreferences(ctx, user, prefs); e != nil {
			return e
		}
		return b.showReminders(ctx, chat, user, id, prefs)
	case "agenda":
		return b.showAgenda(chat, prefs, loc)
	case "digest":
		if len(parts) != 2 {
			return fmt.Errorf("invalid digest")
		}
		if parts[1] == "daily" {
			prefs.Daily = !prefs.Daily
		} else if parts[1] == "weekly" {
			prefs.Weekly = !prefs.Weekly
		} else {
			return fmt.Errorf("invalid digest")
		}
		if e := b.store.SavePlannerPreferences(ctx, user, prefs); e != nil {
			return e
		}
		return b.showAgenda(chat, prefs, loc)
	case "quietoff":
		prefs.QuietStart = 0
		prefs.QuietEnd = 0
		if e := b.store.SavePlannerPreferences(ctx, user, prefs); e != nil {
			return e
		}
		return b.showReminders(ctx, chat, user, 0, prefs)
	case "quick":
		if len(parts) != 2 || account.OnboardingStatus != OnboardingCourseSetup {
			return fmt.Errorf("this setup step has expired; use /settings to change modes")
		}
		if parts[1] == "custom" {
			b.startCourseSetup(ctx, chat, user)
			return nil
		}
		mode := parts[1]
		if mode == store.ModeAuto {
			connected, e := b.oauthServer.IsConnected(ctx, user)
			if e != nil || !connected {
				return fmt.Errorf("connect Google first or choose Active")
			}
		}
		if e := b.store.SetAllCourseModes(ctx, user, mode); e != nil {
			return e
		}
		prefs.DefaultMode = mode
		if e := b.store.SavePlannerPreferences(ctx, user, prefs); e != nil {
			return e
		}
		b.finishOnboarding(ctx, chat, user)
		return nil
	case "calendar":
		return b.showCalendar(ctx, chat, user, prefs)
	case "calchoose":
		if len(parts) != 2 {
			return fmt.Errorf("invalid calendar")
		}
		choices, e := b.google.WritableCalendars(ctx, user)
		if e != nil {
			return fmt.Errorf("reconnect with /connect_google to grant calendar permissions")
		}
		for _, c := range choices {
			if calendarChoiceKey(c.ID) == parts[1] {
				prefs.CalendarID = c.ID
				prefs.CalendarName = c.Name
				if e := b.store.SavePlannerPreferences(ctx, user, prefs); e != nil {
					return e
				}
				return b.showCalendar(ctx, chat, user, prefs)
			}
		}
		return fmt.Errorf("calendar unavailable; reopen Calendar settings")
	case "caldefault":
		prefs.CalendarID = ""
		prefs.CalendarName = ""
		if e := b.store.SavePlannerPreferences(ctx, user, prefs); e != nil {
			return e
		}
		if _, e := b.google.EnsureDestination(ctx, user); e != nil {
			return fmt.Errorf("could not prepare CanvasLink calendar; reconnect Google or try again")
		}
		prefs, e := b.store.PlannerPreferences(ctx, user)
		if e != nil {
			return e
		}
		return b.showCalendar(ctx, chat, user, prefs)
	case "title":
		if prefs.TitleStyle == "course" {
			prefs.TitleStyle = "plain"
		} else {
			prefs.TitleStyle = "course"
		}
		if e := b.store.SavePlannerPreferences(ctx, user, prefs); e != nil {
			return e
		}
		return b.showCalendar(ctx, chat, user, prefs)
	case "colors":
		courses, e := b.store.ListUserCourses(ctx, user)
		if e != nil {
			return e
		}
		rows := [][]tgbotapi.InlineKeyboardButton{}
		for _, c := range courses {
			rows = append(rows, row(button(c.CourseID, fmt.Sprintf("pl|color|%d", c.ID))))
		}
		return b.plannerMessage(chat, "Choose a course color. Changes apply on the next calendar create/update.", rows...)
	case "color":
		setting, e := b.store.GetCourseSettingByID(ctx, user, id)
		if e != nil || setting == nil {
			return fmt.Errorf("course no longer available")
		}
		if len(parts) == 3 {
			v, e := strconv.Atoi(parts[2])
			if e != nil || v < 0 || v > 11 {
				return fmt.Errorf("invalid color")
			}
			if v == 0 {
				delete(prefs.Colors, setting.CourseID)
			} else {
				prefs.Colors[setting.CourseID] = parts[2]
			}
			if e := b.store.SavePlannerPreferences(ctx, user, prefs); e != nil {
				return e
			}
		}
		rows := [][]tgbotapi.InlineKeyboardButton{}
		names := []string{"Calendar default", "Lavender", "Sage", "Grape", "Flamingo", "Banana", "Tangerine", "Peacock", "Graphite", "Blueberry", "Basil", "Tomato"}
		for i, name := range names {
			rows = append(rows, row(button(name, fmt.Sprintf("pl|color|%d|%d", id, i))))
		}
		return b.plannerMessage(chat, setting.CourseID+" color: "+prefs.Colors[setting.CourseID]+"\nChoose a color for future creates/updates.", rows...)
	}
	return fmt.Errorf("unknown action")
}

func (b *Bot) showPlannerList(ctx context.Context, chat, user int64, view, course string, filter int64, page int, loc *time.Location) error {
	if view != "today" && view != "week" && view != "upcoming" && view != "done" {
		return fmt.Errorf("invalid view")
	}
	allowed, err := b.store.VisiblePlannerTasks(ctx, user)
	if err != nil {
		return err
	}
	selected := planner.Select(allowed, view, course, time.Now().In(loc))
	const size = 6
	pages := (len(selected) + size - 1) / size
	if pages == 0 {
		pages = 1
	}
	if page >= pages {
		page = pages - 1
	}
	start := page * size
	end := start + size
	if end > len(selected) {
		end = len(selected)
	}
	title := fmt.Sprintf("📋 %s · page %d/%d", strings.ToUpper(view), page+1, pages)
	if course != "" {
		title += " · " + course
	}
	body := planner.Summary(title, selected[start:end], loc, size) + "\n\nTimes: " + loc.String() + ". Feed changes appear after the next successful check."
	if status, e := b.store.PlannerSyncStatus(ctx, user, loc); e == nil {
		body += "\n" + status
	}
	rows := [][]tgbotapi.InlineKeyboardButton{}
	for _, t := range selected[start:end] {
		rows = append(rows, row(button(planner.Short(t.Title, 45), fmt.Sprintf("pl|task|%d", t.ID))))
	}
	nav := []tgbotapi.InlineKeyboardButton{}
	if page > 0 {
		nav = append(nav, button("←", fmt.Sprintf("pl|list|%s|%d|%d", view, page-1, filter)))
	}
	if end < len(selected) {
		nav = append(nav, button("→", fmt.Sprintf("pl|list|%s|%d|%d", view, page+1, filter)))
	}
	if len(nav) > 0 {
		rows = append(rows, row(nav...))
	}
	rows = append(rows, row(button("Today", "pl|list|today|0|0"), button("7 days", "pl|list|week|0|0"), button("All upcoming", "pl|list|upcoming|0|0")), row(button("Filter course", "pl|filters"), button("Completed", "pl|list|done|0|0"), button("Add task", "pl|input|add")))
	return b.plannerMessage(chat, body, rows...)
}
func (b *Bot) showPlannerTask(ctx context.Context, chat, user, id int64, loc *time.Location) error {
	t, err := b.store.PlannerTask(ctx, user, id)
	if err != nil {
		return fmt.Errorf("item no longer available")
	}
	body := fmt.Sprintf("📌 %s\n%s\nOfficial deadline: %s", planner.Short(t.Title, 200), t.CourseID, store.PlannerDate(t.DueAt, t.AllDay, loc))
	if t.Manual {
		body = fmt.Sprintf("📌 Personal task: %s\nDue: %s", planner.Short(t.Title, 200), store.PlannerDate(t.DueAt, false, loc))
	}
	if t.TargetAt != nil {
		body += "\nPersonal target: " + store.PlannerDate(*t.TargetAt, false, loc)
	}
	if t.SnoozedUntil != nil {
		body += "\nSnoozed until: " + store.PlannerDate(*t.SnoozedUntil, false, loc)
	}
	label, action := "✅ Mark done", "done"
	if t.Done {
		body += "\n✅ Done in CanvasLink"
		label, action = "↩ Undo done", "undo"
	}
	if !t.Present {
		body += "\nNot in the latest feed; reminders paused."
	}
	if t.Manual {
		body += "\nThis personal task lives in Telegram; it is not synced to Google Calendar."
	}
	body += "\n\nDone is local to CanvasLink: it does not submit work or remove calendar events. Personal targets change reminder timing, not the official deadline."
	rows := [][]tgbotapi.InlineKeyboardButton{row(button(label, fmt.Sprintf("pl|%s|%d", action, id)), button("💤 Snooze 1h", fmt.Sprintf("pl|snooze|%d", id))), row(button("Personal target", fmt.Sprintf("pl|input|target:%d", id)), button("Clear target", fmt.Sprintf("pl|targetclear|%d", id)))}
	if t.Manual {
		rows = append(rows, row(button("Edit due date", fmt.Sprintf("pl|input|due:%d", id)), button("Delete task", fmt.Sprintf("pl|delete|%d", id))))
	}
	if u, e := url.Parse(t.URL); e == nil && u.Scheme == "https" && u.Host != "" {
		rows = append(rows, row(tgbotapi.NewInlineKeyboardButtonURL("Open in Canvas", t.URL)))
	}
	rows = append(rows, row(button("Upcoming", "pl|list|upcoming|0|0")))
	return b.plannerMessage(chat, body, rows...)
}
func (b *Bot) reminderScope(ctx context.Context, user, id int64) (string, error) {
	if id == 0 {
		return "", nil
	}
	actual := id
	if actual < 0 {
		actual = -actual
	}
	s, err := b.store.GetCourseSettingByID(ctx, user, actual)
	if err != nil || s == nil {
		return "", fmt.Errorf("course no longer available")
	}
	kind := s.AssignmentType
	if id < 0 {
		kind = ""
	}
	return store.ReminderKey(s.CourseID, kind), nil
}
func (b *Bot) showReminders(ctx context.Context, chat, user, id int64, p store.PlannerPreferences) error {
	scope, err := b.reminderScope(ctx, user, id)
	if err != nil {
		return err
	}
	label := "Overall default"
	offsets := p.Reminders
	if id != 0 {
		split := strings.Split(scope, "\x1f")
		label = split[0]
		if split[1] != "" {
			label += " · " + typeLabel(split[1])
		}
		offsets = p.Offsets(split[0], split[1])
		if _, ok := p.Overrides[scope]; !ok {
			label += " (inherited)"
		}
	}
	text := fmt.Sprintf("⏰ %s\n%s\n\nReminders cover all non-ignored items, even if you haven’t added them to Google. Done stops them. A personal target replaces the reminder reference date.\n\nQuiet hours: %02d:00–%02d:00 (same hour = off). Reminders wait until quiet hours end. For all-day items, offsets count back from 09:00 on the due date.\n\nToggle or change these anytime in /settings.", label, planner.OffsetLabel(offsets), p.QuietStart, p.QuietEnd)
	rows := [][]tgbotapi.InlineKeyboardButton{row(button("1 hour", fmt.Sprintf("pl|offset|%d|1h", id)), button("1 day", fmt.Sprintf("pl|offset|%d|1d", id)), button("1 week", fmt.Sprintf("pl|offset|%d|1w", id))), row(button("Off", fmt.Sprintf("pl|offset|%d|off", id)), button("Custom / multiple", fmt.Sprintf("pl|input|offset:%d", id)))}
	if id != 0 {
		rows = append(rows, row(button("Use inherited default", fmt.Sprintf("pl|offset|%d|inherit", id))))
	}
	if id < 0 {
		rows = append(rows, row(button("Per assignment type", fmt.Sprintf("pl|remtypes|%d", -id))))
	}
	rows = append(rows, row(button("Per course", "pl|remcourses"), button("Quiet hours", "pl|input|quiet"), button("Quiet hours off", "pl|quietoff")))
	return b.plannerMessage(chat, text, rows...)
}
func (b *Bot) showAgenda(chat int64, p store.PlannerPreferences, loc *time.Location) error {
	state := func(on bool) string {
		if on {
			return "ON"
		}
		return "OFF"
	}
	body := fmt.Sprintf("🗓 Scheduled agendas\nDaily: %s at %s\nWeekly: %s on %s at %s\nTimezone: %s\n\nDaily covers today; weekly covers the next 7 days. Completed and ignored items are omitted. Quiet hours also apply. /today and /week always work, even with scheduled agendas off.", state(p.Daily), p.DailyTime, state(p.Weekly), time.Weekday(p.Weekday), p.WeeklyTime, loc)
	return b.plannerMessage(chat, body, row(button("Toggle daily", "pl|digest|daily"), button("Set daily time", "pl|input|daily")), row(button("Toggle weekly", "pl|digest|weekly"), button("Set weekly day/time", "pl|input|weekly")))
}
func (b *Bot) plannerInputPrompt(ctx context.Context, user int64, action string, loc *time.Location) (string, error) {
	if action == "add" {
		return "Send: YYYY-MM-DD HH:MM | task title\nExample: 2026-10-15 18:00 | Prepare presentation slides\nTimezone: " + loc.String(), nil
	}
	if action == "daily" {
		return "Send the daily agenda time in 24-hour HH:MM format (e.g. 08:00). This enables the daily agenda. Timezone: " + loc.String(), nil
	}
	if action == "weekly" {
		return "Send weekday and time, e.g. Sun 18:00. This enables the weekly agenda. Timezone: " + loc.String(), nil
	}
	if action == "quiet" {
		return "Send quiet hours as HH-HH, e.g. 22-08. Use 00-00 to disable. Timezone: " + loc.String(), nil
	}
	split := strings.Split(action, ":")
	if len(split) != 2 {
		return "", fmt.Errorf("invalid input")
	}
	id, err := strconv.ParseInt(split[1], 10, 64)
	if err != nil {
		return "", err
	}
	if split[0] == "offset" {
		if _, err := b.reminderScope(ctx, user, id); err != nil {
			return "", err
		}
		return "Send up to 5 offsets separated by commas: 1h, 1d, 1w\nUnits: m minutes, h hours, d days, w weeks. Maximum 30 days. Send off to disable.", nil
	}
	t, err := b.store.PlannerTask(ctx, user, id)
	if err != nil {
		return "", fmt.Errorf("item no longer available")
	}
	if split[0] == "due" && !t.Manual {
		return "", fmt.Errorf("only personal task dates can be edited")
	}
	if split[0] != "target" && split[0] != "due" {
		return "", fmt.Errorf("invalid input")
	}
	return "Send YYYY-MM-DD HH:MM for " + planner.Short(t.Title, 100) + ". Timezone: " + loc.String() + "\nA personal target must be before the official deadline.", nil
}
func (b *Bot) handlePlannerText(ctx context.Context, msg *tgbotapi.Message, user int64, text string) bool {
	action, err := b.store.PlannerInput(ctx, user)
	if err != nil {
		b.reply(msg.Chat.ID, "Could not load your input. Try again.")
		return true
	}
	if action == "" {
		return false
	}
	release, ok, err := b.store.TryFeedSyncLock(ctx, user)
	if err != nil || !ok {
		b.reply(msg.Chat.ID, "A sync is finishing. Send your answer again in a moment.")
		return true
	}
	defer release()
	// Re-read after acquiring the user lock so expired/reset prompts cannot apply.
	action, err = b.store.PlannerInput(ctx, user)
	if err != nil || action == "" {
		b.reply(msg.Chat.ID, "This input expired. Reopen its setting.")
		return true
	}
	err = b.applyPlannerInput(ctx, user, action, text)
	if err != nil {
		b.reply(msg.Chat.ID, err.Error()+"\nTry again, or /cancel.")
		return true
	}
	if err := b.store.ClearPlannerInput(ctx, user); err != nil {
		b.reply(msg.Chat.ID, "Saved. Please send /cancel to close this input.")
		return true
	}
	b.plannerMessage(msg.Chat.ID, "✅ Saved. You can change or disable this anytime in /settings.", row(button("Upcoming", "pl|list|upcoming|0|0"), button("Reminders", "pl|rem|0"), button("Agendas", "pl|agenda")))
	return true
}
func (b *Bot) applyPlannerInput(ctx context.Context, user int64, action, text string) error {
	account, err := b.store.GetTelegramAccount(ctx, user)
	if err != nil {
		return err
	}
	if account == nil {
		return fmt.Errorf("send /start first")
	}
	loc := planner.Location(account.Timezone)
	p, err := b.store.PlannerPreferences(ctx, user)
	if err != nil {
		return err
	}
	switch action {
	case "daily":
		if !planner.ParseClock(text) {
			return fmt.Errorf("use HH:MM, for example 08:00")
		}
		p.Daily = true
		p.DailyTime = text
	case "weekly":
		fields := strings.Fields(text)
		if len(fields) != 2 || !planner.ParseClock(fields[1]) {
			return fmt.Errorf("use Sun 18:00, or another weekday and time")
		}
		days := map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
		day, ok := days[strings.ToLower(fields[0])]
		if !ok {
			return fmt.Errorf("use Mon, Tue, Wed, Thu, Fri, Sat or Sun")
		}
		p.Weekly = true
		p.Weekday = day
		p.WeeklyTime = fields[1]
	case "quiet":
		fields := strings.Split(text, "-")
		if len(fields) != 2 {
			return fmt.Errorf("use HH-HH, e.g. 22-08")
		}
		start, e1 := strconv.Atoi(fields[0])
		end, e2 := strconv.Atoi(fields[1])
		if e1 != nil || e2 != nil || start < 0 || start > 23 || end < 0 || end > 23 {
			return fmt.Errorf("hours must be 00–23")
		}
		p.QuietStart = start
		p.QuietEnd = end
	case "add":
		fields := strings.SplitN(text, "|", 2)
		if len(fields) != 2 {
			return fmt.Errorf("use YYYY-MM-DD HH:MM | title")
		}
		due, e := parsePlannerDate(strings.TrimSpace(fields[0]), loc)
		if e != nil {
			return e
		}
		title := strings.TrimSpace(fields[1])
		if len([]rune(title)) < 1 || len([]rune(title)) > 200 {
			return fmt.Errorf("title must be 1–200 characters")
		}
		_, e = b.store.AddManualTask(ctx, user, title, due)
		return e
	default:
		fields := strings.Split(action, ":")
		if len(fields) != 2 {
			return fmt.Errorf("invalid input")
		}
		id, e := strconv.ParseInt(fields[1], 10, 64)
		if e != nil {
			return e
		}
		if fields[0] == "offset" {
			v, e := planner.ParseOffsets(text)
			if e != nil {
				return e
			}
			scope, e := b.reminderScope(ctx, user, id)
			if e != nil {
				return e
			}
			if id == 0 {
				p.Reminders = v
			} else {
				p.Overrides[scope] = v
			}
		} else {
			due, e := parsePlannerDate(text, loc)
			if e != nil {
				return e
			}
			t, e := b.store.PlannerTask(ctx, user, id)
			if e != nil {
				return fmt.Errorf("item no longer available")
			}
			if fields[0] == "target" && !due.Before(t.EffectiveDue(loc)) {
				return fmt.Errorf("personal target must be before the official deadline")
			}
			if fields[0] == "due" && !t.Manual {
				return fmt.Errorf("only personal task dates can be edited")
			}
			return b.store.ChangePlannerTask(ctx, user, id, fields[0], &due)
		}
	}
	return b.store.SavePlannerPreferences(ctx, user, p)
}
func parsePlannerDate(text string, loc *time.Location) (time.Time, error) {
	d, err := time.ParseInLocation("2006-01-02 15:04", text, loc)
	if err != nil || d.Format("2006-01-02 15:04") != text {
		return time.Time{}, fmt.Errorf("use a valid local date/time: YYYY-MM-DD HH:MM")
	}
	if !d.After(time.Now()) {
		return time.Time{}, fmt.Errorf("choose a future date/time")
	}
	return d, nil
}
func (b *Bot) sendQuickSetup(ctx context.Context, chat, user int64, google bool) {
	if err := b.store.SetOnboardingStatus(ctx, user, OnboardingCourseSetup); err != nil {
		b.reply(chat, "Could not save setup. Try /start again.")
		return
	}
	rows := [][]tgbotapi.InlineKeyboardButton{}
	if google {
		rows = append(rows, row(button("🚀 Auto for all courses", "pl|quick|auto")))
	}
	rows = append(rows, row(button("🔔 Active for all courses", "pl|quick|active")), row(button("🚫 Ignore all for now", "pl|quick|ignore")), row(button("Customize each type now", "pl|quick|custom")))
	b.plannerMessage(chat, "Choose one default for all courses, including newly detected courses.\n\nYou can toggle ANY course or assignment type anytime in Settings → Module Sync Modes. You can also customize individual types now.\n\nReminders start at 1 day before for non-ignored items. Toggle them off or choose different offsets in Settings → Reminders.", rows...)
}
func calendarChoiceKey(id string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(id)))[:16] }
func (b *Bot) showCalendar(ctx context.Context, chat, user int64, p store.PlannerPreferences) error {
	choices, err := b.google.WritableCalendars(ctx, user)
	if err != nil {
		return fmt.Errorf("connect Google using /connect_google; calendar setup needs the additional calendar permissions")
	}
	name := p.CalendarName
	if name == "" {
		name = "CanvasLink (created automatically on first sync)"
	}
	body := "🎨 Calendar settings\nDestination: " + name + "\nTitle style: " + p.TitleStyle + "\n\nNew events use this calendar. Existing tracked events stay in their original calendar. Colors and title style apply on the next create/update."
	rows := [][]tgbotapi.InlineKeyboardButton{row(button("Use dedicated CanvasLink calendar", "pl|caldefault")), row(button("Toggle course prefix", "pl|title"), button("Course colors", "pl|colors"))}
	for _, c := range choices {
		rows = append(rows, row(button(planner.Short(c.Name, 45), "pl|calchoose|"+calendarChoiceKey(c.ID))))
	}
	return b.plannerMessage(chat, body, rows...)
}

func plannerReminderSummary(p store.PlannerPreferences) string {
	return fmt.Sprintf("Default reminders: %s, independently of calendar approval. Quiet hours: %02d:00–%02d:00 (same hour = off).", planner.OffsetLabel(p.Reminders), p.QuietStart, p.QuietEnd)
}
func onboardingPreview(events []canvas.Event, zone string, now time.Time) string {
	loc := planner.Location(zone)
	var tasks []store.PlannerTask
	seen := map[string]bool{}
	for _, e := range events {
		if e.UID == "" || seen[e.UID] || e.DueAt == nil || e.Cancelled || e.Course == "" {
			continue
		}
		seen[e.UID] = true
		tasks = append(tasks, store.PlannerTask{Title: e.Title, CourseID: e.Course, DueAt: *e.DueAt, AllDay: e.AllDay, Present: true})
	}
	selected := planner.Select(tasks, "upcoming", "", now.In(loc))
	if len(selected) == 0 {
		return "Your feed is connected. No upcoming recognizable deadlines yet; I’ll keep checking as courses publish their work."
	}
	return planner.Summary("Here’s a preview of your upcoming work:", selected, loc, 3) + "\n\nNext, choose a default sync mode. You can toggle every course/type later in /settings."
}
