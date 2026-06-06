package sync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/markadodo/canvaslink/internal/canvas"
	canvasGoogle "github.com/markadodo/canvaslink/internal/google"
	"github.com/markadodo/canvaslink/internal/store"
)

// Worker periodically syncs Canvas iCal feeds for all users.
// It detects new, changed, and removed events on each sync cycle.
type Worker struct {
	store    *store.Store
	interval time.Duration
	api      *tgbotapi.BotAPI
	google   *canvasGoogle.CalendarClient
}

func NewWorker(db *store.Store, interval time.Duration, botToken string, googleClient *canvasGoogle.CalendarClient) (*Worker, error) {
	api, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		return nil, err
	}
	return &Worker{store: db, interval: interval, api: api, google: googleClient}, nil
}

// Start begins the periodic sync loop. Runs immediately, then on the configured interval.
func (w *Worker) Start(ctx context.Context) {
	w.runOnce(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

// runOnce performs one full sync cycle for all enabled feeds.
func (w *Worker) runOnce(ctx context.Context) {
	feeds, err := w.store.ListEnabledFeeds(ctx)
	if err != nil {
		log.Printf("canvaslink sync: list feeds failed: %v", err)
		return
	}

	for _, feed := range feeds {
		w.syncFeed(ctx, feed)
	}
}

// syncFeed syncs a single user's feed: fetches, diffs, and processes changes.
func (w *Worker) syncFeed(ctx context.Context, feed store.Feed) {
	events, seeds, err := canvas.FetchAndDetect(feed.ICalURL)
	if err != nil {
		log.Printf("canvaslink sync: fetch failed user=%d err=%v", feed.UserID, err)
		return
	}

	// Get existing course IDs BEFORE seeding to detect new courses
	existingCourses, _ := w.store.ListUserCourseIDs(ctx, feed.UserID)
	existingSet := make(map[string]bool, len(existingCourses))
	for _, cid := range existingCourses {
		existingSet[cid] = true
	}

	// Auto-seed any new courses that weren't in the settings before
	if err := w.store.SeedDefaultSettings(ctx, feed.UserID, seeds); err != nil {
		log.Printf("canvaslink sync: seed settings failed user=%d err=%v", feed.UserID, err)
	}

	// Detect new courses that were just seeded
	newCourseIDs := make(map[string]bool)
	for _, seed := range seeds {
		if !existingSet[seed.CourseID] {
			newCourseIDs[seed.CourseID] = true
		}
	}

	// Notify user about new courses and send configuration prompts
	if len(newCourseIDs) > 0 {
		w.notifyNewCourses(ctx, feed.UserID, newCourseIDs)
	}

	// Get previously synced events for this user
	prevSynced, err := w.store.ListSyncedEvents(ctx, feed.UserID)
	if err != nil {
		log.Printf("canvaslink sync: list synced failed user=%d err=%v", feed.UserID, err)
		return
	}

	now := time.Now()

	// Build a set of current event UIDs for diffing, skipping past events
	currentUIDs := make(map[string]canvas.Event, len(events))
	for _, ev := range events {
		if ev.UID == "" || ev.Course == "" || ev.DueAt == nil {
			continue
		}
		// Skip events that have already passed
		if ev.DueAt.Before(now) {
			continue
		}
		currentUIDs[ev.UID] = ev
	}

	// Process each current event: new or updated
	for uid, ev := range currentUIDs {
		prev, exists := prevSynced[uid]
		if !exists {
			// New event — process according to its mode
			w.processEvent(ctx, feed.UserID, ev, nil)
		} else {
			// Existing event — check if it changed
			if eventChanged(&ev, &prev) {
				log.Printf("canvaslink sync: event changed user=%d uid=%s title=%q", feed.UserID, uid, ev.Title)
				w.processEvent(ctx, feed.UserID, ev, &prev)
			}
		}
	}

	// Detect removed events: previously synced but no longer in the feed
	for uid, prev := range prevSynced {
		if _, stillExists := currentUIDs[uid]; !stillExists {
			log.Printf("canvaslink sync: event removed user=%d uid=%s title=%q", feed.UserID, uid, prev.CanvasTitle)
			w.handleRemovedEvent(ctx, feed.UserID, prev)
		}
	}

	if err := w.store.TouchFeedSync(ctx, feed.UserID); err != nil {
		log.Printf("canvaslink sync: touch sync failed user=%d err=%v", feed.UserID, err)
	}
}

// eventChanged checks if a Canvas event has been modified since last sync
// by comparing DTStamp and Sequence.
func eventChanged(ev *canvas.Event, prev *store.SyncedEvent) bool {
	// If the event has a DTStamp, use it for change detection
	if ev.DTStamp != nil && prev.CanvasDTStamp != nil {
		if !ev.DTStamp.Equal(*prev.CanvasDTStamp) {
			return true
		}
	}
	// Check sequence number
	if ev.Sequence > prev.CanvasSequence {
		return true
	}
	// Check if title or due date changed
	if ev.Title != prev.CanvasTitle {
		return true
	}
	if ev.DueAt != nil && !ev.DueAt.Equal(prev.CanvasDueAt) {
		return true
	}
	return false
}

// processEvent handles a single event according to its sync mode.
// If prev is nil, it's a new event. If prev is non-nil, it's an update.
func (w *Worker) processEvent(ctx context.Context, userID int64, ev canvas.Event, prev *store.SyncedEvent) {
	mode, err := w.store.GetCourseTypeMode(ctx, userID, ev.Course, ev.Type)
	if err != nil {
		log.Printf("canvaslink sync: mode lookup failed user=%d err=%v", userID, err)
		return
	}

	switch mode {
	case store.ModeIgnore:
		// If it was previously synced and now ignored, remove from calendar
		if prev != nil && prev.GoogleEventID != "" {
			w.handleRemovedEvent(ctx, userID, *prev)
		}
		return

	case store.ModeQuiet:
		// Quiet sync: push to Google Calendar silently, no Telegram notification
		w.syncQuiet(ctx, userID, ev, prev)

	case store.ModeNotify:
		// Notify & sync: push to Google Calendar + send Telegram notification
		w.syncNotify(ctx, userID, ev, prev)

	case store.ModeReview:
		// Review: send confirmation card (user must approve before adding to calendar)
		w.syncReview(ctx, userID, ev, prev)

	default:
		return
	}
}

// syncQuiet handles Quiet mode: push to Google Calendar silently, no Telegram notification.
func (w *Worker) syncQuiet(ctx context.Context, userID int64, ev canvas.Event, prev *store.SyncedEvent) {
	// If already synced and unchanged, skip
	if prev != nil && prev.GoogleEventID != "" {
		return
	}

	calendarID, eventID, err := w.google.CreateEventForTelegramUser(
		ctx,
		userID,
		ev.Title,
		*ev.DueAt,
		fmt.Sprintf("CanvasLink quiet-sync\nCourse: %s\nType: %s", ev.Course, ev.Type),
	)
	if errors.Is(err, canvasGoogle.ErrGoogleNotConfigured) || errors.Is(err, canvasGoogle.ErrGoogleNotConnected) {
		return
	}
	if err != nil {
		log.Printf("canvaslink sync: quiet google sync failed user=%d err=%v", userID, err)
		return
	}

	_, err = w.store.UpsertSyncedEvent(ctx, store.SyncedEventInput{
		TelegramUserID:   userID,
		CourseID:         ev.Course,
		AssignmentType:   ev.Type,
		CanvasEventUID:   ev.UID,
		CanvasTitle:      ev.Title,
		CanvasDueAt:      *ev.DueAt,
		GoogleCalendarID: calendarID,
		GoogleEventID:    eventID,
		CanvasDTStamp:    ev.DTStamp,
		CanvasSequence:   ev.Sequence,
	})
	if err != nil {
		log.Printf("canvaslink sync: quiet sync record failed user=%d err=%v", userID, err)
	}
	// No Telegram notification sent — quiet mode
}

// syncNotify handles Notify mode: push to Google Calendar + send Telegram notification.
func (w *Worker) syncNotify(ctx context.Context, userID int64, ev canvas.Event, prev *store.SyncedEvent) {
	// If already synced and unchanged, skip
	if prev != nil && prev.GoogleEventID != "" {
		return
	}

	calendarID, eventID, err := w.google.CreateEventForTelegramUser(
		ctx,
		userID,
		ev.Title,
		*ev.DueAt,
		fmt.Sprintf("CanvasLink notify-sync\nCourse: %s\nType: %s", ev.Course, ev.Type),
	)
	if errors.Is(err, canvasGoogle.ErrGoogleNotConfigured) || errors.Is(err, canvasGoogle.ErrGoogleNotConnected) {
		return
	}
	if err != nil {
		log.Printf("canvaslink sync: notify google sync failed user=%d err=%v", userID, err)
		return
	}

	inserted, err := w.store.UpsertSyncedEvent(ctx, store.SyncedEventInput{
		TelegramUserID:   userID,
		CourseID:         ev.Course,
		AssignmentType:   ev.Type,
		CanvasEventUID:   ev.UID,
		CanvasTitle:      ev.Title,
		CanvasDueAt:      *ev.DueAt,
		GoogleCalendarID: calendarID,
		GoogleEventID:    eventID,
		CanvasDTStamp:    ev.DTStamp,
		CanvasSequence:   ev.Sequence,
	})
	if err != nil {
		log.Printf("canvaslink sync: notify sync record failed user=%d err=%v", userID, err)
		return
	}

	// Send Telegram notification for new events
	if inserted {
		w.sendSyncedFeedback(ctx, userID, ev)
	}
}

// syncReview handles Review mode: send a confirmation card to the user.
func (w *Worker) syncReview(ctx context.Context, userID int64, ev canvas.Event, prev *store.SyncedEvent) {
	// If already handled (pending action exists), skip
	if prev != nil {
		return
	}

	account, err := w.store.GetTelegramAccount(ctx, userID)
	if err != nil || account == nil {
		return
	}

	pending, inserted, err := w.store.CreatePendingActionIfAbsent(ctx, store.PendingActionInput{
		TelegramUserID: userID,
		CourseID:       ev.Course,
		AssignmentType: ev.Type,
		CanvasEventUID: ev.UID,
		CanvasTitle:    ev.Title,
		CanvasDueAt:    *ev.DueAt,
		TelegramChatID: account.ChatID,
	})
	if err != nil || !inserted || pending.Status != store.PendingStatusPending {
		return
	}

	courseLabel := ev.CourseName
	if courseLabel == "" {
		courseLabel = ev.Course
	}

	dueStr := ev.DueAt.In(time.Local).Format("02 Jan, 15:04")
	if ev.AllDay {
		dueStr = ev.DueAt.In(time.Local).Format("02 Jan") + " (All day)"
	}

	text := fmt.Sprintf("🆕 %s\n📚 %s\n📅 Due: %s", ev.Title, courseLabel, dueStr)
	msg := tgbotapi.NewMessage(account.ChatID, text)
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Add to Cal", fmt.Sprintf("pending|add|%d", pending.ID)),
			tgbotapi.NewInlineKeyboardButtonData("🚫 Ignore", fmt.Sprintf("pending|ignore|%d", pending.ID)),
		),
	)
	sent, err := w.api.Send(msg)
	if err != nil {
		return
	}
	_ = w.store.SetPendingMessageID(ctx, userID, pending.ID, sent.MessageID)
}

// notifyNewCourses sends a Telegram notification about newly detected courses
// and prompts the user to configure each type, one by one.
func (w *Worker) notifyNewCourses(ctx context.Context, userID int64, newCourseIDs map[string]bool) {
	account, err := w.store.GetTelegramAccount(ctx, userID)
	if err != nil || account == nil {
		log.Printf("canvaslink sync: no telegram account for user=%d", userID)
		return
	}
	chatID := account.ChatID

	// Collect course names for the notification
	courseNames := make([]string, 0, len(newCourseIDs))
	for cid := range newCourseIDs {
		courseNames = append(courseNames, cid)
	}

	// Send notification
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("📚 *New Course%s Detected!*\n\nI found %d new course%s in your Canvas feed:\n%s\n\nLet's configure them!",
		map[bool]string{true: "", false: "s"}[len(courseNames) == 1],
		len(courseNames),
		map[bool]string{true: "", false: "s"}[len(courseNames) == 1],
		strings.Join(courseNames, "\n"),
	))
	msg.ParseMode = "Markdown"
	if _, err := w.api.Send(msg); err != nil {
		log.Printf("canvaslink sync: send new course notification failed user=%d err=%v", userID, err)
	}

	// Send configuration prompts for each new course's types
	for _, cid := range courseNames {
		w.sendNewCoursePrompts(ctx, chatID, userID, cid)
	}
}

// sendNewCoursePrompts sends configuration prompts for each type in a new course.
// Each prompt is sent as a separate message with inline buttons.
// The callback data encodes the full state so no in-memory map is needed.
func (w *Worker) sendNewCoursePrompts(ctx context.Context, chatID int64, userID int64, courseID string) {
	types, err := w.store.ListCourseTypes(ctx, userID, courseID)
	if err != nil || len(types) == 0 {
		log.Printf("canvaslink sync: no types for new course=%s user=%d", courseID, userID)
		return
	}

	googleConnected, _ := w.store.HasGoogleToken(ctx, userID)

	for i, assignmentType := range types {
		// Build the prompt message
		text := fmt.Sprintf("📚 *%s* — Step %d/%d\nHow should I handle *%s*?",
			courseID, i+1, len(types), typeLabel(assignmentType))

		msg := tgbotapi.NewMessage(chatID, text)
		msg.ParseMode = "Markdown"

		// Build callback data: new_course_mode|COURSE_ID|TYPE|TYPE_INDEX|TOTAL_TYPES|MODE
		// We encode the index and total so the bot can send the next prompt without in-memory state.
		baseData := fmt.Sprintf("new_course_mode|%s|%s|%d|%d|", courseID, assignmentType, i, len(types))

		var row []tgbotapi.InlineKeyboardButton
		if googleConnected {
			row = append(row, tgbotapi.NewInlineKeyboardButtonData("🔇 Quiet", baseData+"quiet"))
			row = append(row, tgbotapi.NewInlineKeyboardButtonData("🔔 Notify", baseData+"notify"))
		}
		row = append(row,
			tgbotapi.NewInlineKeyboardButtonData("📋 Review", baseData+"review"),
			tgbotapi.NewInlineKeyboardButtonData("🚫 Ignore", baseData+"ignore"),
		)

		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(row...),
		)

		if _, err := w.api.Send(msg); err != nil {
			log.Printf("canvaslink sync: send new course prompt failed user=%d course=%s type=%s err=%v", userID, courseID, assignmentType, err)
		}
	}
}

// typeLabel returns a human-readable label for an assignment type.
func typeLabel(t string) string {
	switch t {
	case "assignment":
		return "📝 Assignment"
	case "quiz":
		return "📋 Quiz"
	case "discussion":
		return "💬 Discussion"
	case "exam":
		return "📄 Exam"
	case "project":
		return "📁 Project"
	case "test":
		return "📄 Test"
	case "midterm":
		return "📄 Midterm"
	case "final":
		return "📄 Final"
	case "presentation":
		return "📊 Presentation"
	case "essay":
		return "📝 Essay"
	case "homework":
		return "📚 Homework"
	case "lab":
		return "🔬 Lab"
	case "reading":
		return "📖 Reading"
	case "other":
		return "📌 Other"
	default:
		return t
	}
}

// handleRemovedEvent handles an event that was removed from the Canvas feed.
func (w *Worker) handleRemovedEvent(ctx context.Context, userID int64, prev store.SyncedEvent) {
	// Delete from synced events
	if err := w.store.DeleteSyncedEvent(ctx, userID, prev.CanvasEventUID); err != nil {
		log.Printf("canvaslink sync: delete synced event failed user=%d uid=%s err=%v", userID, prev.CanvasEventUID, err)
	}
}

// sendSyncedFeedback sends a brief "Synced" notification for auto-synced events.
func (w *Worker) sendSyncedFeedback(ctx context.Context, userID int64, ev canvas.Event) {
	account, err := w.store.GetTelegramAccount(ctx, userID)
	if err != nil || account == nil {
		return
	}
	msg := tgbotapi.NewMessage(account.ChatID, fmt.Sprintf("✅ Synced: %s", ev.Title))
	if _, err := w.api.Send(msg); err != nil {
		log.Printf("canvaslink sync: send synced feedback failed user=%d err=%v", userID, err)
	}
}
