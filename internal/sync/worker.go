package sync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/markadodo/canvaslink/internal/canvas"
	canvasGoogle "github.com/markadodo/canvaslink/internal/google"
	"github.com/markadodo/canvaslink/internal/store"
	telegramHTTP "github.com/markadodo/canvaslink/internal/telegram"
)

// Worker periodically syncs Canvas iCal feeds for all users.
// It detects new, changed, and removed events on each sync cycle.
type Worker struct {
	store               *store.Store
	interval            time.Duration
	calendarJobInterval time.Duration
	removalGracePeriod  time.Duration
	removalMisses       int
	workerID            string
	api                 *tgbotapi.BotAPI
	google              *canvasGoogle.CalendarClient
}

type WorkerOptions struct {
	SyncInterval        time.Duration
	CalendarJobInterval time.Duration
	RemovalGracePeriod  time.Duration
	RemovalMisses       int
}

const (
	calendarJobBatchSize               = 20
	calendarJobLease                   = 2 * time.Minute
	maxConcurrentFeeds                 = 4
	maxTelegramNotificationsPerFeedRun = 25
	maxCoursesInNotificationSummary    = 12
)

type telegramSendBudget struct {
	remaining int
}

func newTelegramSendBudget(limit int) telegramSendBudget {
	if limit < 0 {
		limit = 0
	}
	return telegramSendBudget{remaining: limit}
}

// take reserves one outbound Telegram attempt. Failed sends deliberately
// consume budget too, preventing a permanently rejected message from causing
// unbounded retries or starving other users during the same worker cycle.
func (b *telegramSendBudget) take() bool {
	if b == nil || b.remaining <= 0 {
		return false
	}
	b.remaining--
	return true
}

func NewWorker(db *store.Store, interval time.Duration, botToken string, googleClient *canvasGoogle.CalendarClient) (*Worker, error) {
	return NewWorkerWithOptions(db, botToken, googleClient, WorkerOptions{
		SyncInterval:        interval,
		CalendarJobInterval: 15 * time.Second,
		RemovalGracePeriod:  6 * time.Hour,
		RemovalMisses:       3,
	})
}

func NewWorkerWithOptions(db *store.Store, botToken string, googleClient *canvasGoogle.CalendarClient, options WorkerOptions) (*Worker, error) {
	if db == nil {
		return nil, errors.New("sync store is required")
	}
	if options.SyncInterval <= 0 {
		return nil, errors.New("sync interval must be positive")
	}
	if options.CalendarJobInterval <= 0 {
		return nil, errors.New("calendar job interval must be positive")
	}
	if options.RemovalGracePeriod <= 0 {
		return nil, errors.New("removal grace period must be positive")
	}
	if options.RemovalMisses < 2 {
		return nil, errors.New("removal misses must be at least 2")
	}
	if googleClient == nil {
		return nil, errors.New("google calendar client is required")
	}
	workerIDBytes := make([]byte, 12)
	if _, err := rand.Read(workerIDBytes); err != nil {
		return nil, fmt.Errorf("generate calendar worker ID: %w", err)
	}
	api, err := tgbotapi.NewBotAPIWithClient(botToken, tgbotapi.APIEndpoint, telegramHTTP.NewClient(botToken, 30*time.Second))
	if err != nil {
		return nil, err
	}
	return &Worker{
		store:               db,
		interval:            options.SyncInterval,
		calendarJobInterval: options.CalendarJobInterval,
		removalGracePeriod:  options.RemovalGracePeriod,
		removalMisses:       options.RemovalMisses,
		workerID:            hex.EncodeToString(workerIDBytes),
		api:                 api,
		google:              googleClient,
	}, nil
}

// Start begins the periodic sync loop. Runs immediately, then on the configured interval.
func (w *Worker) Start(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	var loops sync.WaitGroup
	loops.Add(2)

	go func() {
		defer loops.Done()
		w.runOnce(ctx)
		syncTicker := time.NewTicker(w.interval)
		defer syncTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-syncTicker.C:
				w.runOnce(ctx)
			}
		}
	}()

	go func() {
		defer loops.Done()
		w.runCalendarJobs(ctx)
		jobTicker := time.NewTicker(w.calendarJobInterval)
		defer jobTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-jobTicker.C:
				w.runCalendarJobs(ctx)
			}
		}
	}()

	loops.Wait()
}

// runOnce performs one full sync cycle for all enabled feeds.
func (w *Worker) runOnce(ctx context.Context) {
	feeds, err := w.store.ListEnabledFeeds(ctx)
	if err != nil {
		log.Printf("canvaslink sync: list feeds failed: %v", err)
		return
	}

	var group sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentFeeds)
	for _, feed := range feeds {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		group.Add(1)
		go func(feed store.Feed) {
			defer group.Done()
			defer func() { <-sem }()
			w.syncFeed(ctx, feed)
		}(feed)
	}
	group.Wait()
}

func (w *Worker) runCalendarJobs(ctx context.Context) {
	jobs, err := w.store.ClaimCalendarJobs(ctx, w.workerID, calendarJobBatchSize, calendarJobLease)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("canvaslink calendar jobs: claim failed: %v", err)
		}
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		w.processCalendarJob(ctx, job)
	}
}

func (w *Worker) processCalendarJob(ctx context.Context, job store.CalendarJob) {
	release, acquired, err := w.store.TryFeedSyncLock(ctx, job.TelegramUserID)
	if err != nil {
		w.retryCalendarJob(ctx, job, fmt.Errorf("acquire user operation lock: %w", err))
		return
	}
	if !acquired {
		w.blockCalendarJob(ctx, job, 5*time.Second, "another user operation is in progress")
		return
	}
	defer func() {
		if err := release(); err != nil {
			log.Printf(
				"canvaslink calendar jobs: release user operation lock failed user=%d job=%d err=%v",
				job.TelegramUserID,
				job.ID,
				err,
			)
		}
	}()

	allowed, err := w.store.CanRunCalendarJob(ctx, job.ID, w.workerID, job.LeaseVersion)
	if errors.Is(err, store.ErrCalendarJobLease) {
		return
	}
	if err != nil {
		w.retryCalendarJob(ctx, job, fmt.Errorf("revalidate calendar job: %w", err))
		return
	}
	if !allowed {
		if err := w.store.CancelCalendarJob(
			ctx,
			job.ID,
			w.workerID,
			job.LeaseVersion,
			"account connection or source state changed before execution",
		); err != nil && !errors.Is(err, store.ErrCalendarJobLease) {
			log.Printf("canvaslink calendar jobs: cancel stale job=%d failed: %v", job.ID, err)
		}
		return
	}

	eventID := job.GoogleEventID
	switch job.Action {
	case store.CalendarActionCreate:
		if job.DueAt == nil {
			w.failCalendarJob(ctx, job, errors.New("create job has no due date"), true)
			return
		}
		_, createdEventID, createErr := w.google.CreateEventForTelegramUser(
			ctx,
			job.TelegramUserID,
			job.SourceUID,
			job.Title,
			*job.DueAt,
			job.AllDay,
			job.Description,
		)
		if createErr != nil {
			w.handleCalendarJobError(ctx, job, createErr)
			return
		}
		eventID = createdEventID

	case store.CalendarActionUpdate:
		if job.DueAt == nil {
			w.failCalendarJob(ctx, job, errors.New("update job has no due date"), true)
			return
		}
		updateErr := w.google.UpdateEvent(
			ctx,
			job.TelegramUserID,
			job.GoogleCalendarID,
			job.GoogleEventID,
			job.SourceUID,
			job.Title,
			*job.DueAt,
			job.AllDay,
			job.Description,
		)
		if canvasGoogle.IsEventNotFound(updateErr) {
			// A user may have manually removed the old event. Recreate it with
			// the stable CanvasLink ID and update local tracking atomically.
			_, createdEventID, createErr := w.google.CreateEventForTelegramUser(
				ctx,
				job.TelegramUserID,
				job.SourceUID,
				job.Title,
				*job.DueAt,
				job.AllDay,
				job.Description,
			)
			updateErr = createErr
			eventID = createdEventID
		}
		if updateErr != nil {
			w.handleCalendarJobError(ctx, job, updateErr)
			return
		}

	case store.CalendarActionDelete:
		if err := w.google.DeleteOwnedEvent(
			ctx,
			job.TelegramUserID,
			job.GoogleCalendarID,
			job.GoogleEventID,
			job.SourceUID,
		); err != nil {
			w.handleCalendarJobError(ctx, job, err)
			return
		}

	default:
		w.failCalendarJob(ctx, job, fmt.Errorf("unsupported calendar action %q", job.Action), true)
		return
	}

	if err := w.store.CompleteCalendarJob(ctx, job.ID, w.workerID, job.LeaseVersion, eventID); err != nil {
		if !errors.Is(err, store.ErrCalendarJobLease) && !errors.Is(err, store.ErrStaleCalendarJob) {
			log.Printf("canvaslink calendar jobs: complete job=%d failed: %v", job.ID, err)
		}
		return
	}
	w.sendCalendarJobFeedback(ctx, job)
}

func (w *Worker) handleCalendarJobError(ctx context.Context, job store.CalendarJob, operationErr error) {
	if ctx.Err() != nil {
		return
	}
	switch {
	case errors.Is(operationErr, canvasGoogle.ErrGoogleNotConfigured):
		w.blockCalendarJob(ctx, job, 24*time.Hour, operationErr.Error())
	case errors.Is(operationErr, canvasGoogle.ErrGoogleNotConnected):
		w.blockCalendarJob(ctx, job, time.Hour, operationErr.Error())
	case errors.Is(operationErr, canvasGoogle.ErrGoogleAuthorizationInvalid):
		if job.LastError == "" {
			w.notifyCalendarReconnect(ctx, job.TelegramUserID)
		}
		w.blockCalendarJob(ctx, job, 6*time.Hour, operationErr.Error())
	case errors.Is(operationErr, canvasGoogle.ErrEventOwnershipUnverified):
		w.failCalendarJob(ctx, job, operationErr, false)
		w.notifyUnverifiedCalendarEvent(ctx, job)
	default:
		w.retryCalendarJob(ctx, job, operationErr)
	}
}

func (w *Worker) retryCalendarJob(ctx context.Context, job store.CalendarJob, operationErr error) {
	if ctx.Err() != nil {
		return
	}
	if job.Attempts >= job.MaxAttempts {
		w.failCalendarJob(ctx, job, operationErr, true)
		return
	}
	delay := calendarRetryDelay(job.ID, job.Attempts)
	err := w.store.RetryCalendarJob(
		ctx,
		job.ID,
		w.workerID,
		job.LeaseVersion,
		time.Now().UTC().Add(delay),
		operationErr.Error(),
	)
	if err != nil && !errors.Is(err, store.ErrCalendarJobLease) {
		log.Printf("canvaslink calendar jobs: retry job=%d failed: %v", job.ID, err)
	}
}

func (w *Worker) blockCalendarJob(ctx context.Context, job store.CalendarJob, delay time.Duration, reason string) {
	if ctx.Err() != nil {
		return
	}
	err := w.store.BlockCalendarJob(
		ctx,
		job.ID,
		w.workerID,
		job.LeaseVersion,
		time.Now().UTC().Add(delay),
		reason,
	)
	if err != nil && !errors.Is(err, store.ErrCalendarJobLease) {
		log.Printf("canvaslink calendar jobs: block job=%d failed: %v", job.ID, err)
	}
}

func (w *Worker) failCalendarJob(ctx context.Context, job store.CalendarJob, operationErr error, notify bool) {
	err := w.store.FailCalendarJob(ctx, job.ID, w.workerID, job.LeaseVersion, operationErr.Error())
	if err != nil {
		if !errors.Is(err, store.ErrCalendarJobLease) {
			log.Printf("canvaslink calendar jobs: fail job=%d failed: %v", job.ID, err)
		}
		return
	}
	if notify {
		w.notifyCalendarJobFailure(ctx, job)
	}
}

func calendarRetryDelay(jobID int64, attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	exponent := attempts - 1
	if exponent > 9 {
		exponent = 9
	}
	delay := 30 * time.Second * time.Duration(1<<exponent)
	if delay > 6*time.Hour {
		delay = 6 * time.Hour
	}
	// Stable positive jitter avoids synchronized retries across instances
	// without relying on mutable global pseudo-random state.
	jitterBasis := jobID % 1000
	if jitterBasis < 0 {
		jitterBasis = -jitterBasis
	}
	return delay + time.Duration(jitterBasis)*delay/10000
}

func (w *Worker) sendCalendarJobFeedback(ctx context.Context, job store.CalendarJob) {
	switch job.Action {
	case store.CalendarActionDelete:
		w.sendRemovedFeedback(ctx, job)
	case store.CalendarActionCreate, store.CalendarActionUpdate:
		mode, err := w.store.GetCourseTypeMode(ctx, job.TelegramUserID, job.CourseID, job.AssignmentType)
		if err != nil || mode != store.ModeNotify {
			return
		}
		event := calendarJobEvent(job)
		if job.Action == store.CalendarActionCreate {
			w.sendSyncedFeedback(ctx, job.TelegramUserID, event)
		} else {
			w.sendUpdatedFeedback(ctx, job.TelegramUserID, event)
		}
	}
}

func (w *Worker) sendRemovedFeedback(ctx context.Context, job store.CalendarJob) {
	account, err := w.store.GetTelegramAccount(ctx, job.TelegramUserID)
	if err != nil || account == nil {
		return
	}
	reason := "it was no longer present in Canvas after several successful checks"
	if job.CanvasCancelled {
		reason = "Canvas marked it as cancelled"
	}
	msg := tgbotapi.NewMessage(
		account.ChatID,
		fmt.Sprintf("🗑 Removed from Google Calendar: %s\nCanvasLink removed only its verified event because %s.", job.Title, reason),
	)
	if _, err := w.api.Send(msg); err != nil {
		log.Printf("canvaslink calendar jobs: send removal feedback user=%d failed: %v", job.TelegramUserID, err)
	}
}

func (w *Worker) notifyUnverifiedCalendarEvent(ctx context.Context, job store.CalendarJob) {
	account, err := w.store.GetTelegramAccount(ctx, job.TelegramUserID)
	if err != nil || account == nil {
		return
	}
	msg := tgbotapi.NewMessage(
		account.ChatID,
		fmt.Sprintf("⚠️ I left this calendar event untouched because its CanvasLink ownership could not be safely verified: %s", job.Title),
	)
	if _, err := w.api.Send(msg); err != nil {
		log.Printf("canvaslink calendar jobs: send ownership warning user=%d failed: %v", job.TelegramUserID, err)
	}
}

func (w *Worker) notifyCalendarReconnect(ctx context.Context, userID int64) {
	account, err := w.store.GetTelegramAccount(ctx, userID)
	if err != nil || account == nil {
		return
	}
	msg := tgbotapi.NewMessage(account.ChatID, "⚠️ Google Calendar authorization expired. Reconnect it with /connect_google; queued calendar changes will remain safe until then.")
	if _, err := w.api.Send(msg); err != nil {
		log.Printf("canvaslink calendar jobs: send reconnect warning user=%d failed: %v", userID, err)
	}
}

func (w *Worker) notifyCalendarJobFailure(ctx context.Context, job store.CalendarJob) {
	account, err := w.store.GetTelegramAccount(ctx, job.TelegramUserID)
	if err != nil || account == nil {
		return
	}
	msg := tgbotapi.NewMessage(
		account.ChatID,
		fmt.Sprintf("⚠️ I could not finish the calendar change for %s after several retries. Your CanvasLink data was kept; reconnect Google or try again later.", job.Title),
	)
	if _, err := w.api.Send(msg); err != nil {
		log.Printf("canvaslink calendar jobs: send failure warning user=%d failed: %v", job.TelegramUserID, err)
	}
}

func calendarJobEvent(job store.CalendarJob) canvas.Event {
	event := canvas.Event{
		UID:       job.SourceUID,
		Title:     job.Title,
		Course:    job.CourseID,
		Type:      job.AssignmentType,
		AllDay:    job.AllDay,
		DTStamp:   job.CanvasDTStamp,
		Sequence:  job.CanvasSequence,
		Status:    job.CanvasStatus,
		Cancelled: job.CanvasCancelled,
	}
	if job.DueAt != nil {
		dueAt := *job.DueAt
		event.DueAt = &dueAt
	}
	return event
}

// syncFeed syncs a single user's feed: fetches, diffs, and processes changes.
func (w *Worker) syncFeed(ctx context.Context, feed store.Feed) {
	release, acquired, err := w.store.TryFeedSyncLock(ctx, feed.UserID)
	if err != nil {
		log.Printf("canvaslink sync: acquire feed lock failed user=%d err=%v", feed.UserID, err)
		return
	}
	if !acquired {
		return
	}
	defer func() {
		if err := release(); err != nil {
			log.Printf("canvaslink sync: release feed lock failed user=%d err=%v", feed.UserID, err)
		}
	}()

	events, seeds, err := canvas.FetchAndDetectContext(ctx, feed.ICalURL)
	if err != nil {
		log.Printf("canvaslink sync: fetch failed user=%d err=%v", feed.UserID, err)
		return
	}

	// Get existing course IDs BEFORE seeding to detect new courses
	existingCourses, err := w.store.ListUserCourseIDs(ctx, feed.UserID)
	canDetectNewCourses := err == nil
	if err != nil {
		log.Printf("canvaslink sync: list course IDs failed user=%d err=%v", feed.UserID, err)
	}
	existingSet := make(map[string]bool, len(existingCourses))
	for _, cid := range existingCourses {
		existingSet[cid] = true
	}

	// Auto-seed any new courses that weren't in the settings before
	if err := w.store.SeedDefaultSettings(ctx, feed.UserID, seeds); err != nil {
		log.Printf("canvaslink sync: seed settings failed user=%d err=%v", feed.UserID, err)
		return
	}

	// Detect new courses that were just seeded
	newCourseIDs := make(map[string]bool)
	for _, seed := range seeds {
		if !existingSet[seed.CourseID] {
			newCourseIDs[seed.CourseID] = true
		}
	}

	notificationBudget := newTelegramSendBudget(maxTelegramNotificationsPerFeedRun)

	// Notify user about new courses and send configuration prompts
	if canDetectNewCourses && len(newCourseIDs) > 0 {
		w.notifyNewCourses(ctx, feed.UserID, newCourseIDs, &notificationBudget)
	}

	// Get previously synced events for this user
	prevSynced, err := w.store.ListSyncedEvents(ctx, feed.UserID)
	if err != nil {
		log.Printf("canvaslink sync: list synced failed user=%d err=%v", feed.UserID, err)
		return
	}

	now := time.Now()

	// Preserve every VEVENT UID for removal detection, even when an event is
	// past, unclassified, or temporarily missing a due date.
	observedUIDs := make(map[string]canvas.Event, len(events))
	ambiguousUIDs := make(map[string]struct{})
	for _, ev := range events {
		if ev.UID == "" {
			continue
		}
		if _, duplicated := observedUIDs[ev.UID]; duplicated {
			// Recurrence instances and malformed feeds can repeat a UID with
			// contradictory metadata. Count it as present, but do not let an
			// arbitrary duplicate authorize an update or deletion.
			ambiguousUIDs[ev.UID] = struct{}{}
			observedUIDs[ev.UID] = canvas.Event{UID: ev.UID}
			continue
		}
		if _, alreadyAmbiguous := ambiguousUIDs[ev.UID]; !alreadyAmbiguous {
			observedUIDs[ev.UID] = ev
		}
	}

	for uid, ev := range observedUIDs {
		previous, tracked := prevSynced[uid]
		removalEligible := ev.Course != "" && ev.DueAt != nil
		nonActionable := !removalEligible || eventHasPassed(ev, now)
		if tracked {
			if ev.Cancelled && previous.RemovalEligible {
				// Cancellation tombstones often contain only UID + STATUS.
				// Previously validated metadata remains sufficient because the
				// Google target is still independently ownership-verified.
				removalEligible = true
			}
			var observeErr error
			if ev.Cancelled || nonActionable {
				// Past/unclassifiable observations update local safety metadata
				// so a stale future timestamp can never authorize deletion.
				observeErr = w.store.ObserveSyncedEvent(ctx, store.SyncedEventObservation{
					TelegramUserID:  feed.UserID,
					CanvasEventUID:  uid,
					DueAt:           ev.DueAt,
					AllDay:          ev.AllDay,
					CanvasStatus:    ev.Status,
					CanvasCancelled: ev.Cancelled,
					RemovalEligible: removalEligible,
				})
			} else {
				// Preserve confirmed metadata, but replace any unconfirmed
				// staged intent with this newest complete observation. That
				// gives job preflight an independent exact-state fence.
				observeErr = w.store.ObserveActionableSyncedEvent(ctx, store.SyncedEventInput{
					TelegramUserID:  feed.UserID,
					CourseID:        ev.Course,
					AssignmentType:  ev.Type,
					CanvasEventUID:  uid,
					CanvasTitle:     ev.Title,
					CanvasDueAt:     *ev.DueAt,
					AllDay:          ev.AllDay,
					CanvasDTStamp:   ev.DTStamp,
					CanvasSequence:  ev.Sequence,
					CanvasStatus:    ev.Status,
					RemovalEligible: true,
				})
			}
			if observeErr != nil && !errors.Is(observeErr, store.ErrNotFound) {
				log.Printf("canvaslink sync: observe tracked event failed user=%d err=%v", feed.UserID, observeErr)
				continue
			}
		}

		if ev.Cancelled {
			w.cancelPendingReview(ctx, feed.UserID, uid, &notificationBudget)
			if err := w.store.CancelOpenCalendarSyncJobsBySource(
				ctx,
				feed.UserID,
				uid,
				"Canvas explicitly cancelled the source event",
			); err != nil {
				log.Printf("canvaslink sync: cancel source work failed user=%d err=%v", feed.UserID, err)
			}
			if tracked && !syncedEventHasPassed(previous, now) {
				guarded, err := w.store.MarkSyncedEventMissing(ctx, feed.UserID, uid)
				if err != nil {
					log.Printf("canvaslink sync: guard cancelled event failed user=%d err=%v", feed.UserID, err)
					continue
				}
				if guarded == nil || guarded.MissingSince == nil {
					continue
				}
				w.enqueueCalendarDelete(
					ctx,
					*guarded,
					guarded.MissingSince,
					guarded.MissingCount,
					*guarded.MissingSince,
				)
			}
			continue
		}
		if nonActionable {
			if err := w.store.CancelOpenCalendarSyncJobsBySource(
				ctx,
				feed.UserID,
				uid,
				"source is no longer actionable in Canvas",
			); err != nil {
				log.Printf("canvaslink sync: fence non-actionable source work failed user=%d err=%v", feed.UserID, err)
			}
		}
		if tracked {
			if err := w.store.CancelPendingCalendarDelete(ctx, feed.UserID, uid); err != nil {
				log.Printf("canvaslink sync: cancel stale delete failed user=%d err=%v", feed.UserID, err)
			}
		}
	}

	// Build a set of actionable current events, skipping cancelled and past
	// events only after the full observation set has been recorded.
	currentUIDs := make(map[string]canvas.Event, len(observedUIDs))
	for _, ev := range observedUIDs {
		if ev.UID == "" || ev.Course == "" || ev.DueAt == nil || ev.Cancelled {
			continue
		}
		if eventHasPassed(ev, now) {
			continue
		}
		currentUIDs[ev.UID] = ev
	}

	// Process events in due-date order so notifications are deterministic and
	// the most urgent work reaches users first.
	currentEventIDs := make([]string, 0, len(currentUIDs))
	for uid := range currentUIDs {
		currentEventIDs = append(currentEventIDs, uid)
	}
	sort.Slice(currentEventIDs, func(i, j int) bool {
		left := currentUIDs[currentEventIDs[i]]
		right := currentUIDs[currentEventIDs[j]]
		if left.DueAt.Equal(*right.DueAt) {
			return currentEventIDs[i] < currentEventIDs[j]
		}
		return left.DueAt.Before(*right.DueAt)
	})

	// Fence any queued source operation that no longer matches this complete,
	// successful Canvas observation. This runs even when Canvas reverted to the
	// last Google-confirmed value (and eventChanged would otherwise be false).
	for _, uid := range currentEventIDs {
		ev := currentUIDs[uid]
		var previous *store.SyncedEvent
		if value, exists := prevSynced[uid]; exists {
			value := value
			previous = &value
		}
		if err := w.reconcileOpenCalendarSyncJobs(ctx, feed.UserID, ev, previous); err != nil {
			log.Printf("canvaslink sync: reconcile queued source failed user=%d err=%v", feed.UserID, err)
			// Do not enqueue, update missing counters, or authorize deletion
			// after a failed stale-work fence.
			return
		}
	}

	// Process each current event: new or updated.
	for _, uid := range currentEventIDs {
		ev := currentUIDs[uid]
		prev, exists := prevSynced[uid]
		if !exists {
			// New event — process according to its mode
			w.processEvent(ctx, feed.UserID, ev, nil, &notificationBudget)
		} else {
			// Existing event — check if it changed
			if prev.Detached || !prev.GoogleConfirmed || prev.GoogleEventID == "" || eventChanged(&ev, &prev) {
				w.processEvent(ctx, feed.UserID, ev, &prev, &notificationBudget)
			}
		}
	}

	w.handleMissingEvents(ctx, feed.UserID, observedUIDs, prevSynced, now, &notificationBudget)

	if err := w.store.TouchFeedSync(ctx, feed.UserID); err != nil {
		log.Printf("canvaslink sync: touch sync failed user=%d err=%v", feed.UserID, err)
	}
}

func (w *Worker) reconcileOpenCalendarSyncJobs(
	ctx context.Context,
	userID int64,
	ev canvas.Event,
	previous *store.SyncedEvent,
) error {
	if ev.DueAt == nil {
		return w.store.CancelOpenCalendarSyncJobsBySource(
			ctx,
			userID,
			ev.UID,
			"latest Canvas source has no actionable due date",
		)
	}
	mode, err := w.store.GetCourseTypeMode(ctx, userID, ev.Course, ev.Type)
	if err != nil {
		return err
	}
	if mode == store.ModeIgnore || !w.google.IsConfigured() {
		return w.store.CancelOpenCalendarSyncJobsBySource(
			ctx,
			userID,
			ev.UID,
			"current sync mode does not authorize calendar work",
		)
	}

	// Review mode permits automatic updates only after the event was already
	// confirmed in Google, or while a user-approved durable add is pending.
	if mode == store.ModeReview && (previous == nil || !previous.GoogleConfirmed) {
		approved := false
		if previous != nil && previous.GoogleEventID != "" {
			pending, err := w.store.GetPendingActionByUID(ctx, userID, ev.UID)
			if err != nil {
				return err
			}
			approved = pending != nil && pending.Status == store.PendingStatusAdded
		}
		if !approved {
			return w.store.CancelOpenCalendarSyncJobsBySource(
				ctx,
				userID,
				ev.UID,
				"Review mode requires a current user approval",
			)
		}
	}

	calendarID := "primary"
	eventID, err := w.google.DeterministicEventID(userID, ev.UID)
	if err != nil {
		return err
	}
	action := store.CalendarActionCreate
	if previous != nil && previous.GoogleEventID != "" {
		calendarID = previous.GoogleCalendarID
		eventID = previous.GoogleEventID
		if previous.GoogleConfirmed {
			action = store.CalendarActionUpdate
		}
	}
	dueAt := *ev.DueAt
	_, err = w.store.CancelMismatchedOpenCalendarSyncJobs(ctx, store.CalendarJobInput{
		TelegramUserID:   userID,
		DedupeKey:        "event:" + eventID,
		Action:           action,
		SourceUID:        ev.UID,
		CourseID:         ev.Course,
		AssignmentType:   ev.Type,
		Title:            ev.Title,
		DueAt:            &dueAt,
		AllDay:           ev.AllDay,
		GoogleCalendarID: calendarID,
		GoogleEventID:    eventID,
		CanvasDTStamp:    ev.DTStamp,
		CanvasSequence:   ev.Sequence,
		CanvasStatus:     ev.Status,
		CanvasCancelled:  ev.Cancelled,
		MaxAttempts:      12,
	}, "queued calendar change no longer matches the latest Canvas event")
	return err
}

func (w *Worker) handleMissingEvents(
	ctx context.Context,
	userID int64,
	observed map[string]canvas.Event,
	previous map[string]store.SyncedEvent,
	now time.Time,
	notificationBudget *telegramSendBudget,
) {
	futureTracked := 0
	missing := make([]store.SyncedEvent, 0)
	for uid, event := range previous {
		if event.Detached || !event.RemovalEligible || syncedEventHasPassed(event, now) {
			continue
		}
		futureTracked++
		if _, exists := observed[uid]; !exists {
			missing = append(missing, event)
		}
	}
	if len(missing) == 0 {
		return
	}

	// Empty or sharply reduced feeds can be legitimate when instructors remove
	// work, but they are also the riskiest deletion signal. They still converge
	// eventually, using substantially longer thresholds below.
	if requiresMassRemovalGuard(futureTracked, len(missing)) {
		log.Printf(
			"canvaslink sync: extended mass-removal guard user=%d observed=%d candidates=%d future_tracked=%d",
			userID,
			len(observed),
			len(missing),
			futureTracked,
		)
	}

	sort.Slice(missing, func(i, j int) bool {
		return missing[i].CanvasEventUID < missing[j].CanvasEventUID
	})
	for _, event := range missing {
		updated, err := w.store.MarkSyncedEventMissing(ctx, userID, event.CanvasEventUID)
		if err != nil {
			log.Printf("canvaslink sync: mark event missing failed user=%d err=%v", userID, err)
			continue
		}
		requiredMisses, requiredGrace := w.removalThresholds(futureTracked, len(missing))
		if !removalThresholdMet(updated, now, requiredMisses, requiredGrace) {
			continue
		}
		if err := w.store.CancelOpenCalendarSyncJobsBySource(
			ctx,
			userID,
			event.CanvasEventUID,
			"source absent after conservative removal grace",
		); err != nil {
			log.Printf("canvaslink sync: fence missing source work failed user=%d err=%v", userID, err)
			continue
		}
		w.cancelPendingReview(ctx, userID, event.CanvasEventUID, notificationBudget)
		deleteNotBefore := updated.MissingSince.Add(requiredGrace)
		w.enqueueCalendarDelete(
			ctx,
			*updated,
			updated.MissingSince,
			updated.MissingCount,
			deleteNotBefore,
		)
	}
}

func requiresMassRemovalGuard(futureTracked, missingCount int) bool {
	// Three or more simultaneous losses representing a majority of the
	// upcoming tracked feed are much more likely to be a partial feed than an
	// isolated instructor removal.
	return missingCount >= 3 && futureTracked > 0 && missingCount*2 > futureTracked
}

func (w *Worker) removalThresholds(futureTracked, missingCount int) (int, time.Duration) {
	misses := w.removalMisses
	grace := w.removalGracePeriod
	// Losing the entire small upcoming set is especially hard to distinguish
	// from a partial Canvas export, so require a longer healthy observation
	// window while still allowing a genuine professor removal to converge.
	if futureTracked > 0 && futureTracked <= 2 && missingCount == futureTracked {
		if misses < 6 {
			misses = 6
		}
		if grace < 24*time.Hour {
			grace = 24 * time.Hour
		}
	}
	// A large majority disappearance is allowed to converge only after many
	// independent complete fetches spanning a full week. This avoids permanent
	// retention while making partial/export outages extremely unlikely to
	// authorize a mass delete.
	if requiresMassRemovalGuard(futureTracked, missingCount) {
		if misses < 12 {
			misses = 12
		}
		if grace < 7*24*time.Hour {
			grace = 7 * 24 * time.Hour
		}
	}
	return misses, grace
}

func removalThresholdMet(event *store.SyncedEvent, now time.Time, requiredMisses int, grace time.Duration) bool {
	if event == nil || event.MissingSince == nil || requiredMisses < 2 || grace <= 0 {
		return false
	}
	if event.MissingCount < requiredMisses {
		return false
	}
	return !event.MissingSince.After(now.Add(-grace))
}

func (w *Worker) enqueueCalendarDelete(
	ctx context.Context,
	event store.SyncedEvent,
	expectedSince *time.Time,
	expectedCount int,
	deleteNotBefore time.Time,
) {
	if event.GoogleEventID == "" || event.Detached || !event.RemovalEligible || syncedEventHasPassed(event, time.Now()) {
		return
	}
	_, _, err := w.store.EnqueueCalendarJob(ctx, store.CalendarJobInput{
		TelegramUserID:       event.TelegramUserID,
		DedupeKey:            "event:" + event.GoogleEventID,
		Action:               store.CalendarActionDelete,
		SourceUID:            event.CanvasEventUID,
		CourseID:             event.CourseID,
		AssignmentType:       event.AssignmentType,
		Title:                event.CanvasTitle,
		AllDay:               event.AllDay,
		GoogleCalendarID:     event.GoogleCalendarID,
		GoogleEventID:        event.GoogleEventID,
		CanvasDTStamp:        event.CanvasDTStamp,
		CanvasSequence:       event.CanvasSequence,
		CanvasStatus:         event.CanvasStatus,
		CanvasCancelled:      event.CanvasCancelled,
		ExpectedMissingSince: expectedSince,
		ExpectedMissingCount: expectedCount,
		DeleteNotBefore:      &deleteNotBefore,
		MaxAttempts:          12,
	})
	if err != nil {
		log.Printf("canvaslink sync: enqueue calendar delete failed user=%d err=%v", event.TelegramUserID, err)
	}
}

func (w *Worker) cancelPendingReview(
	ctx context.Context,
	userID int64,
	sourceUID string,
	notificationBudget *telegramSendBudget,
) {
	pending, err := w.store.CancelPendingActionByUID(ctx, userID, sourceUID)
	if err != nil {
		log.Printf("canvaslink sync: cancel pending review failed user=%d err=%v", userID, err)
		return
	}
	if pending == nil || !pending.TelegramMessageID.Valid {
		return
	}
	if !notificationBudget.take() {
		return
	}
	chatID, err := strconv.ParseInt(pending.TelegramChatID, 10, 64)
	if err != nil {
		log.Printf("canvaslink sync: invalid pending chat ID user=%d", userID)
		return
	}
	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, int(pending.TelegramMessageID.Int64), tgbotapi.InlineKeyboardMarkup{})
	if _, err := w.api.Send(edit); err != nil {
		log.Printf("canvaslink sync: clear cancelled review card failed user=%d err=%v", userID, err)
	}
}

func syncedEventHasPassed(event store.SyncedEvent, now time.Time) bool {
	return eventHasPassed(canvas.Event{DueAt: &event.CanvasDueAt, AllDay: event.AllDay}, now)
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
	if (ev.DTStamp == nil) != (prev.CanvasDTStamp == nil) {
		return true
	}
	// Check sequence number
	if ev.Sequence != prev.CanvasSequence {
		return true
	}
	if ev.Course != prev.CourseID || ev.Type != prev.AssignmentType {
		return true
	}
	if ev.AllDay != prev.AllDay || ev.Status != prev.CanvasStatus || ev.Cancelled != prev.CanvasCancelled {
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
func (w *Worker) processEvent(
	ctx context.Context,
	userID int64,
	ev canvas.Event,
	prev *store.SyncedEvent,
	notificationBudget *telegramSendBudget,
) {
	mode, err := w.store.GetCourseTypeMode(ctx, userID, ev.Course, ev.Type)
	if err != nil {
		log.Printf("canvaslink sync: mode lookup failed user=%d err=%v", userID, err)
		return
	}

	switch mode {
	case store.ModeIgnore:
		// Calendar deletion is a user-facing policy decision. Retain any existing
		// tracking row so switching modes cannot create a duplicate later.
		w.cancelPendingReview(ctx, userID, ev.UID, notificationBudget)
		return

	case store.ModeQuiet:
		// Quiet sync: push to Google Calendar silently, no Telegram notification
		w.syncQuiet(ctx, userID, ev, prev, notificationBudget)

	case store.ModeNotify:
		// Notify & sync: push to Google Calendar + send Telegram notification
		w.syncNotify(ctx, userID, ev, prev, notificationBudget)

	case store.ModeReview:
		// Review: send confirmation card (user must approve before adding to calendar)
		w.syncReview(ctx, userID, ev, prev, notificationBudget)

	default:
		log.Printf("canvaslink sync: unsupported mode user=%d mode=%q", userID, mode)
		return
	}
}

// syncQuiet handles Quiet mode: push to Google Calendar silently, no Telegram notification.
func (w *Worker) syncQuiet(
	ctx context.Context,
	userID int64,
	ev canvas.Event,
	prev *store.SyncedEvent,
	notificationBudget *telegramSendBudget,
) {
	w.enqueueCalendarSync(
		ctx,
		userID,
		ev,
		prev,
		fmt.Sprintf("CanvasLink quiet-sync\nCourse: %s\nType: %s", ev.Course, ev.Type),
		notificationBudget,
	)
}

// syncNotify handles Notify mode: push to Google Calendar + send Telegram notification.
func (w *Worker) syncNotify(
	ctx context.Context,
	userID int64,
	ev canvas.Event,
	prev *store.SyncedEvent,
	notificationBudget *telegramSendBudget,
) {
	w.enqueueCalendarSync(
		ctx,
		userID,
		ev,
		prev,
		fmt.Sprintf("CanvasLink notify-sync\nCourse: %s\nType: %s", ev.Course, ev.Type),
		notificationBudget,
	)
}

func (w *Worker) enqueueCalendarSync(
	ctx context.Context,
	userID int64,
	ev canvas.Event,
	prev *store.SyncedEvent,
	description string,
	notificationBudget *telegramSendBudget,
) {
	if !w.google.IsConfigured() || ev.DueAt == nil {
		return
	}
	connected, err := w.store.HasGoogleToken(ctx, userID)
	if err != nil {
		log.Printf("canvaslink sync: check Google connection failed user=%d err=%v", userID, err)
		return
	}
	if !connected {
		return
	}

	action := store.CalendarActionCreate
	calendarID := "primary"
	eventID, err := w.google.DeterministicEventID(userID, ev.UID)
	if err != nil {
		log.Printf("canvaslink sync: deterministic event ID failed user=%d err=%v", userID, err)
		return
	}
	if prev != nil && prev.GoogleEventID != "" {
		calendarID = prev.GoogleCalendarID
		eventID = prev.GoogleEventID
		if prev.GoogleConfirmed {
			action = store.CalendarActionUpdate
		}
	}
	dueAt := *ev.DueAt
	jobInput := store.CalendarJobInput{
		TelegramUserID:   userID,
		DedupeKey:        "event:" + eventID,
		Action:           action,
		SourceUID:        ev.UID,
		CourseID:         ev.Course,
		AssignmentType:   ev.Type,
		Title:            ev.Title,
		Description:      description,
		DueAt:            &dueAt,
		AllDay:           ev.AllDay,
		GoogleCalendarID: calendarID,
		GoogleEventID:    eventID,
		CanvasDTStamp:    ev.DTStamp,
		CanvasSequence:   ev.Sequence,
		CanvasStatus:     ev.Status,
		CanvasCancelled:  ev.Cancelled,
		MaxAttempts:      12,
	}
	_, err = w.store.QueueCalendarSync(ctx, store.SyncedEventInput{
		TelegramUserID:   userID,
		CourseID:         ev.Course,
		AssignmentType:   ev.Type,
		CanvasEventUID:   ev.UID,
		CanvasTitle:      ev.Title,
		CanvasDueAt:      dueAt,
		AllDay:           ev.AllDay,
		GoogleCalendarID: calendarID,
		GoogleEventID:    eventID,
		CanvasDTStamp:    ev.DTStamp,
		CanvasSequence:   ev.Sequence,
		CanvasStatus:     ev.Status,
		CanvasCancelled:  ev.Cancelled,
		RemovalEligible:  true,
		GoogleConfirmed:  false,
	}, jobInput)
	if err != nil {
		log.Printf("canvaslink sync: stage calendar %s failed user=%d err=%v", action, userID, err)
		return
	}
	if prev != nil && !prev.GoogleConfirmed && prev.GoogleEventID == "" {
		// A Review placeholder is now governed by Auto/Notify. Make its old
		// approval card inert after the target and job are durably staged.
		w.cancelPendingReview(ctx, userID, ev.UID, notificationBudget)
	}
}

// syncReview handles Review mode: send a confirmation card to the user.
func (w *Worker) syncReview(
	ctx context.Context,
	userID int64,
	ev canvas.Event,
	prev *store.SyncedEvent,
	notificationBudget *telegramSendBudget,
) {
	if prev != nil {
		if prev.GoogleConfirmed && prev.GoogleEventID != "" {
			w.enqueueCalendarSync(
				ctx,
				userID,
				ev,
				prev,
				fmt.Sprintf("CanvasLink manual add\nCourse: %s\nType: %s", ev.Course, ev.Type),
				notificationBudget,
			)
			return
		}
		if prev.GoogleEventID != "" {
			pending, err := w.store.GetPendingActionByUID(ctx, userID, ev.UID)
			if err != nil {
				log.Printf("canvaslink sync: load review approval failed user=%d err=%v", userID, err)
				return
			}
			if pending != nil && pending.Status == store.PendingStatusAdded {
				w.enqueueCalendarSync(
					ctx,
					userID,
					ev,
					prev,
					fmt.Sprintf("CanvasLink manual add\nCourse: %s\nType: %s", ev.Course, ev.Type),
					notificationBudget,
				)
				return
			}
			if err := w.store.CancelOpenCalendarSyncJobsBySource(
				ctx,
				userID,
				ev.UID,
				"Review mode requires a current user approval",
			); err != nil {
				log.Printf("canvaslink sync: fence unapproved review job failed user=%d err=%v", userID, err)
				return
			}
		}
	}

	account, err := w.store.GetTelegramAccount(ctx, userID)
	if err != nil || account == nil {
		return
	}
	if prev == nil {
		_, err = w.store.UpsertSyncedEvent(ctx, store.SyncedEventInput{
			TelegramUserID:   userID,
			CourseID:         ev.Course,
			AssignmentType:   ev.Type,
			CanvasEventUID:   ev.UID,
			CanvasTitle:      ev.Title,
			CanvasDueAt:      *ev.DueAt,
			AllDay:           ev.AllDay,
			GoogleCalendarID: "primary",
			GoogleEventID:    "",
			CanvasDTStamp:    ev.DTStamp,
			CanvasSequence:   ev.Sequence,
			CanvasStatus:     ev.Status,
			CanvasCancelled:  false,
			RemovalEligible:  true,
			GoogleConfirmed:  false,
		})
		if err != nil {
			log.Printf("canvaslink sync: persist review source failed user=%d err=%v", userID, err)
			return
		}
	}

	pending, inserted, err := w.store.CreatePendingActionIfAbsent(ctx, store.PendingActionInput{
		TelegramUserID: userID,
		CourseID:       ev.Course,
		AssignmentType: ev.Type,
		CanvasEventUID: ev.UID,
		CanvasTitle:    ev.Title,
		CanvasDueAt:    *ev.DueAt,
		AllDay:         ev.AllDay,
		TelegramChatID: account.ChatID,
	})
	if err != nil {
		log.Printf("canvaslink sync: pending action upsert failed user=%d err=%v", userID, err)
		return
	}
	if pending.Status != store.PendingStatusPending {
		return
	}
	if !inserted && pending.TelegramMessageID.Valid {
		return
	}
	if !notificationBudget.take() {
		return
	}

	courseLabel := ev.CourseName
	if courseLabel == "" {
		courseLabel = ev.Course
	}

	userLocation, err := time.LoadLocation(account.Timezone)
	if err != nil {
		log.Printf("canvaslink sync: invalid stored timezone user=%d timezone=%q err=%v", userID, account.Timezone, err)
		userLocation = time.UTC
	}
	dueStr := ev.DueAt.In(userLocation).Format("02 Jan, 15:04 MST")
	if ev.AllDay {
		dueStr = ev.DueAt.Format("02 Jan") + " (All day)"
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
		log.Printf("canvaslink sync: send review card failed user=%d err=%v", userID, err)
		return
	}
	if err := w.store.SetPendingMessageID(ctx, userID, pending.ID, sent.MessageID); err != nil {
		log.Printf("canvaslink sync: store review message ID failed user=%d err=%v", userID, err)
		// A card is actionable only after its exact Telegram message ID is
		// durably bound. Clear the keyboard on an unregistered send; callbacks
		// also fail closed against the stored message ID.
		edit := tgbotapi.NewEditMessageReplyMarkup(
			account.ChatID,
			sent.MessageID,
			tgbotapi.InlineKeyboardMarkup{},
		)
		if _, clearErr := w.api.Send(edit); clearErr != nil {
			log.Printf("canvaslink sync: clear unregistered review card failed user=%d err=%v", userID, clearErr)
		}
	}
}

// notifyNewCourses sends a Telegram notification about newly detected courses
// and prompts the user to configure each type, one by one.
func (w *Worker) notifyNewCourses(
	ctx context.Context,
	userID int64,
	newCourseIDs map[string]bool,
	notificationBudget *telegramSendBudget,
) {
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
	sort.Strings(courseNames)

	if !notificationBudget.take() {
		log.Printf("canvaslink sync: Telegram notification budget exhausted before new-course summary user=%d", userID)
		return
	}
	summaryNames := courseNames
	if len(summaryNames) > maxCoursesInNotificationSummary {
		summaryNames = summaryNames[:maxCoursesInNotificationSummary]
	}
	summary := strings.Join(summaryNames, "\n")
	if omitted := len(courseNames) - len(summaryNames); omitted > 0 {
		summary += fmt.Sprintf("\n… and %d more. Use /settings to configure every module.", omitted)
	}

	// Send notification
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("📚 New Course%s Detected!\n\nI found %d new course%s in your Canvas feed:\n%s\n\nLet's configure them!",
		map[bool]string{true: "", false: "s"}[len(courseNames) == 1],
		len(courseNames),
		map[bool]string{true: "", false: "s"}[len(courseNames) == 1],
		summary,
	))
	if _, err := w.api.Send(msg); err != nil {
		log.Printf("canvaslink sync: send new course notification failed user=%d err=%v", userID, err)
	}

	// Send configuration prompts for each new course's types
	for _, cid := range courseNames {
		if notificationBudget.remaining <= 0 {
			break
		}
		w.sendNewCoursePrompts(ctx, chatID, userID, cid, notificationBudget)
	}
}

// sendNewCoursePrompts sends configuration prompts for each type in a new course.
// Each prompt is sent as a separate message with inline buttons.
// The callback data encodes the full state so no in-memory map is needed.
func (w *Worker) sendNewCoursePrompts(
	ctx context.Context,
	chatID int64,
	userID int64,
	courseID string,
	notificationBudget *telegramSendBudget,
) {
	settings, err := w.store.ListCourseSettings(ctx, userID, courseID)
	if err != nil || len(settings) == 0 {
		log.Printf("canvaslink sync: no types for newly detected course user=%d", userID)
		return
	}

	googleConnected := false
	if w.google.IsConfigured() {
		var err error
		googleConnected, err = w.store.HasGoogleToken(ctx, userID)
		if err != nil {
			log.Printf("canvaslink sync: google status failed user=%d err=%v", userID, err)
		}
	}

	for i, setting := range settings {
		if !notificationBudget.take() {
			return
		}
		// Build the prompt message
		text := fmt.Sprintf("📚 %s — Step %d/%d\nHow should I handle %s?",
			courseID, i+1, len(settings), typeLabel(setting.AssignmentType))

		msg := tgbotapi.NewMessage(chatID, text)

		baseData := fmt.Sprintf("mode|%d|", setting.ID)

		var row []tgbotapi.InlineKeyboardButton
		if googleConnected {
			row = append(row, tgbotapi.NewInlineKeyboardButtonData("🚀 Auto", baseData+store.ModeAuto))
		}
		row = append(row,
			tgbotapi.NewInlineKeyboardButtonData("🔔 Active", baseData+store.ModeActive),
			tgbotapi.NewInlineKeyboardButtonData("🚫 Ignore", baseData+store.ModeIgnore),
		)

		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(row...),
		)

		if _, err := w.api.Send(msg); err != nil {
			log.Printf("canvaslink sync: send new course prompt failed user=%d err=%v", userID, err)
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

func (w *Worker) sendUpdatedFeedback(ctx context.Context, userID int64, ev canvas.Event) {
	account, err := w.store.GetTelegramAccount(ctx, userID)
	if err != nil || account == nil {
		return
	}
	msg := tgbotapi.NewMessage(account.ChatID, fmt.Sprintf("✅ Updated: %s", ev.Title))
	if _, err := w.api.Send(msg); err != nil {
		log.Printf("canvaslink sync: send updated feedback failed user=%d err=%v", userID, err)
	}
}

func eventHasPassed(event canvas.Event, now time.Time) bool {
	if event.DueAt == nil {
		return false
	}
	if event.AllDay {
		return !now.Before(event.DueAt.AddDate(0, 0, 1))
	}
	return event.DueAt.Before(now)
}
