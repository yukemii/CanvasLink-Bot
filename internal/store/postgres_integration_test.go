package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPostgresSafetyLifecycle(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("CANVASLINK_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("CANVASLINK_TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	databaseURL = isolatedPostgresURL(t, ctx, databaseURL)
	encryption := EncryptionConfig{
		PrimaryKeyID: "integration",
		PrimaryKey:   bytes.Repeat([]byte{0x5a}, 32),
	}
	primary, err := ConnectContextEncrypted(ctx, databaseURL, encryption)
	if err != nil {
		t.Fatalf("connect primary store: %v", err)
	}
	defer primary.Close()
	if err := primary.InitSchema(ctx); err != nil {
		t.Fatalf("initialize schema: %v", err)
	}

	secondary, err := ConnectContextEncrypted(ctx, databaseURL, encryption)
	if err != nil {
		t.Fatalf("connect secondary store: %v", err)
	}
	defer secondary.Close()

	userID := time.Now().UnixNano() & 0x3fffffffffffffff
	if userID == 0 {
		userID = 1
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if err := primary.ResetUser(cleanupCtx, userID); err != nil {
			t.Logf("cleanup user: %v", err)
		}
	}()

	if err := primary.UpsertTelegramAccountWithTimezone(ctx, userID, userID, "integration", "UTC"); err != nil {
		t.Fatalf("upsert private Telegram account: %v", err)
	}
	accountBeforeRevisionChange, err := primary.GetTelegramAccount(ctx, userID)
	if err != nil || accountBeforeRevisionChange == nil {
		t.Fatalf("load account revision: %#v, %v", accountBeforeRevisionChange, err)
	}
	if err := primary.SetUserTimezone(ctx, userID, "UTC"); err != nil {
		t.Fatalf("advance account revision: %v", err)
	}
	const staleOAuthState = "0123456789abcdef0123456789abcdef"
	err = primary.CreateOAuthState(
		ctx,
		staleOAuthState,
		userID,
		"integration-verifier",
		time.Now().Add(time.Minute),
		accountBeforeRevisionChange.StateRevision,
	)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale OAuth state creation error = %v, want ErrNotFound", err)
	}
	if resolvedUserID, err := primary.GetOAuthStateUserID(ctx, staleOAuthState); err != nil || resolvedUserID != 0 {
		t.Fatalf("stale OAuth state resolved user = %d, %v; want 0", resolvedUserID, err)
	}
	const feedURL = "https://canvas.example/feeds/private-token.ics"
	if err := primary.UpsertFeed(ctx, userID, feedURL); err != nil {
		t.Fatalf("store encrypted feed: %v", err)
	}
	feed, err := primary.GetFeed(ctx, userID)
	if err != nil || feed == nil || feed.ICalURL != feedURL {
		t.Fatalf("feed round trip = %#v, %v", feed, err)
	}
	var rawFeed string
	if err := primary.db.QueryRowContext(
		ctx,
		`SELECT ical_url FROM canvaslink_feeds WHERE telegram_user_id = $1`,
		userID,
	).Scan(&rawFeed); err != nil {
		t.Fatalf("read raw feed: %v", err)
	}
	if !strings.HasPrefix(rawFeed, encryptedFieldPrefix+"integration:") || strings.Contains(rawFeed, "private-token") {
		t.Fatalf("feed was not encrypted at rest")
	}

	if err := primary.SaveGoogleAuthorization(
		ctx,
		userID,
		"access-secret",
		"refresh-secret",
		"Bearer",
		"calendar.events",
		time.Now().Add(time.Hour),
	); err != nil {
		t.Fatalf("save encrypted Google grant: %v", err)
	}
	token, err := primary.GetGoogleToken(ctx, userID)
	if err != nil || token == nil || token.AccessToken != "access-secret" || token.RefreshToken != "refresh-secret" {
		t.Fatalf("token round trip = %#v, %v", token, err)
	}

	dueAt := time.Now().UTC().Add(48 * time.Hour)
	const sourceUID = "integration-source"
	const googleEventID = "integrationeventid"
	inserted, err := primary.UpsertSyncedEvent(ctx, SyncedEventInput{
		TelegramUserID:   userID,
		CourseID:         "COURSE",
		AssignmentType:   "assignment",
		CanvasEventUID:   sourceUID,
		CanvasTitle:      "Integration assignment",
		CanvasDueAt:      dueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    googleEventID,
		RemovalEligible:  true,
		GoogleConfirmed:  true,
	})
	if err != nil || !inserted {
		t.Fatalf("insert tracked source: inserted=%v err=%v", inserted, err)
	}
	job, _, err := primary.EnqueueCalendarJob(ctx, CalendarJobInput{
		TelegramUserID:   userID,
		DedupeKey:        "event:" + googleEventID,
		Action:           CalendarActionCreate,
		SourceUID:        sourceUID,
		CourseID:         "COURSE",
		AssignmentType:   "assignment",
		Title:            "Integration assignment",
		DueAt:            &dueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    googleEventID,
	})
	if err != nil {
		t.Fatalf("enqueue create: %v", err)
	}
	claimed, err := primary.ClaimCalendarJobs(ctx, "integration-worker", 10, time.Minute)
	if err != nil {
		t.Fatalf("claim calendar job: %v", err)
	}
	var leased *CalendarJob
	for i := range claimed {
		if claimed[i].ID == job.ID {
			leased = &claimed[i]
			break
		}
	}
	if leased == nil {
		t.Fatalf("enqueued job %d was not claimed: %#v", job.ID, claimed)
	}
	allowed, err := primary.CanRunCalendarJob(ctx, leased.ID, "integration-worker", leased.LeaseVersion)
	if err != nil || !allowed {
		t.Fatalf("active create preflight = %v, %v", allowed, err)
	}
	if err := primary.CompleteCalendarJob(
		ctx,
		leased.ID,
		"integration-worker",
		leased.LeaseVersion,
		googleEventID,
	); err != nil {
		t.Fatalf("complete create: %v", err)
	}
	tracked, err := primary.GetSyncedEvent(ctx, userID, sourceUID)
	if err != nil || tracked == nil || !tracked.GoogleConfirmed {
		t.Fatalf("completed tracking = %#v, %v", tracked, err)
	}

	staleUpdate, _, err := primary.EnqueueCalendarJob(ctx, CalendarJobInput{
		TelegramUserID:   userID,
		DedupeKey:        "event:" + googleEventID,
		Action:           CalendarActionUpdate,
		SourceUID:        sourceUID,
		CourseID:         "COURSE",
		AssignmentType:   "assignment",
		Title:            "Unclassifiable integration assignment",
		DueAt:            &dueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    googleEventID,
	})
	if err != nil {
		t.Fatalf("enqueue stale-source update: %v", err)
	}
	claimed, err = primary.ClaimCalendarJobs(ctx, "integration-stale-worker", 10, time.Minute)
	if err != nil {
		t.Fatalf("claim stale-source update: %v", err)
	}
	leased = findClaimedJob(claimed, staleUpdate.ID)
	if leased == nil {
		t.Fatalf("stale-source update %d was not claimed: %#v", staleUpdate.ID, claimed)
	}
	if err := primary.ObserveSyncedEvent(ctx, SyncedEventObservation{
		TelegramUserID:  userID,
		CanvasEventUID:  sourceUID,
		RemovalEligible: false,
	}); err != nil {
		t.Fatalf("make source non-actionable: %v", err)
	}
	allowed, err = primary.CanRunCalendarJob(ctx, leased.ID, "integration-stale-worker", leased.LeaseVersion)
	if err != nil || allowed {
		t.Fatalf("non-actionable update preflight = %v, %v; want false", allowed, err)
	}
	if err := primary.CompleteCalendarJob(
		ctx,
		leased.ID,
		"integration-stale-worker",
		leased.LeaseVersion,
		googleEventID,
	); !errors.Is(err, ErrStaleCalendarJob) {
		t.Fatalf("non-actionable update completion error = %v, want ErrStaleCalendarJob", err)
	}
	if err := primary.ObserveSyncedEvent(ctx, SyncedEventObservation{
		TelegramUserID:  userID,
		CanvasEventUID:  sourceUID,
		DueAt:           &dueAt,
		RemovalEligible: true,
	}); err != nil {
		t.Fatalf("restore actionable source: %v", err)
	}

	pastUpdate, _, err := primary.EnqueueCalendarJob(ctx, CalendarJobInput{
		TelegramUserID:   userID,
		DedupeKey:        "event:" + googleEventID,
		Action:           CalendarActionUpdate,
		SourceUID:        sourceUID,
		CourseID:         "COURSE",
		AssignmentType:   "assignment",
		Title:            "Past integration assignment",
		DueAt:            &dueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    googleEventID,
	})
	if err != nil {
		t.Fatalf("enqueue past-source update: %v", err)
	}
	claimed, err = primary.ClaimCalendarJobs(ctx, "integration-past-worker", 10, time.Minute)
	if err != nil {
		t.Fatalf("claim past-source update: %v", err)
	}
	leased = findClaimedJob(claimed, pastUpdate.ID)
	if leased == nil {
		t.Fatalf("past-source update %d was not claimed: %#v", pastUpdate.ID, claimed)
	}
	pastDueAt := time.Now().UTC().Add(-time.Hour)
	if err := primary.ObserveSyncedEvent(ctx, SyncedEventObservation{
		TelegramUserID:  userID,
		CanvasEventUID:  sourceUID,
		DueAt:           &pastDueAt,
		RemovalEligible: true,
	}); err != nil {
		t.Fatalf("make source historical: %v", err)
	}
	allowed, err = primary.CanRunCalendarJob(ctx, leased.ID, "integration-past-worker", leased.LeaseVersion)
	if err != nil || allowed {
		t.Fatalf("historical-source update preflight = %v, %v; want false", allowed, err)
	}
	if err := primary.CompleteCalendarJob(
		ctx,
		leased.ID,
		"integration-past-worker",
		leased.LeaseVersion,
		googleEventID,
	); !errors.Is(err, ErrStaleCalendarJob) {
		t.Fatalf("historical-source update completion error = %v, want ErrStaleCalendarJob", err)
	}
	if err := primary.ObserveSyncedEvent(ctx, SyncedEventObservation{
		TelegramUserID:  userID,
		CanvasEventUID:  sourceUID,
		DueAt:           &dueAt,
		RemovalEligible: true,
	}); err != nil {
		t.Fatalf("restore future source: %v", err)
	}

	const removedSourceUID = "integration-removed-source"
	const removedGoogleEventID = "integrationremovedeventid"
	inserted, err = primary.UpsertSyncedEvent(ctx, SyncedEventInput{
		TelegramUserID:   userID,
		CourseID:         "COURSE",
		AssignmentType:   "assignment",
		CanvasEventUID:   removedSourceUID,
		CanvasTitle:      "Removed integration assignment",
		CanvasDueAt:      dueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    removedGoogleEventID,
		RemovalEligible:  true,
		GoogleConfirmed:  true,
	})
	if err != nil || !inserted {
		t.Fatalf("insert removable source: inserted=%v err=%v", inserted, err)
	}
	if err := primary.ObserveSyncedEvent(ctx, SyncedEventObservation{
		TelegramUserID:  userID,
		CanvasEventUID:  removedSourceUID,
		DueAt:           &dueAt,
		CanvasCancelled: true,
		RemovalEligible: true,
	}); err != nil {
		t.Fatalf("mark source explicitly cancelled: %v", err)
	}
	missing, err := primary.MarkSyncedEventMissing(ctx, userID, removedSourceUID)
	if err != nil || missing == nil || missing.MissingSince == nil {
		t.Fatalf("mark source missing = %#v, %v", missing, err)
	}
	deleteNotBefore := *missing.MissingSince
	deleteJob, _, err := primary.EnqueueCalendarJob(ctx, CalendarJobInput{
		TelegramUserID:       userID,
		DedupeKey:            "event:" + removedGoogleEventID,
		Action:               CalendarActionDelete,
		SourceUID:            removedSourceUID,
		CourseID:             "COURSE",
		AssignmentType:       "assignment",
		Title:                "Removed integration assignment",
		GoogleCalendarID:     "primary",
		GoogleEventID:        removedGoogleEventID,
		CanvasCancelled:      true,
		ExpectedMissingSince: missing.MissingSince,
		ExpectedMissingCount: missing.MissingCount,
		DeleteNotBefore:      &deleteNotBefore,
	})
	if err != nil {
		t.Fatalf("enqueue guarded delete: %v", err)
	}
	claimed, err = primary.ClaimCalendarJobs(ctx, "integration-delete-worker", 10, time.Minute)
	if err != nil {
		t.Fatalf("claim guarded delete: %v", err)
	}
	leasedDelete := findClaimedJob(claimed, deleteJob.ID)
	if leasedDelete == nil {
		t.Fatalf("guarded delete %d was not claimed: %#v", deleteJob.ID, claimed)
	}
	allowed, err = primary.CanRunCalendarDeleteJob(
		ctx,
		leasedDelete.ID,
		"integration-delete-worker",
		leasedDelete.LeaseVersion,
	)
	if err != nil || !allowed {
		t.Fatalf("guarded delete preflight = %v, %v", allowed, err)
	}
	if err := primary.ReactivateSyncedEventPresence(ctx, userID, removedSourceUID); err != nil {
		t.Fatalf("reactivate source: %v", err)
	}
	allowed, err = primary.CanRunCalendarDeleteJob(
		ctx,
		leasedDelete.ID,
		"integration-delete-worker",
		leasedDelete.LeaseVersion,
	)
	if err != nil || allowed {
		t.Fatalf("reappeared delete preflight = %v, %v; want false", allowed, err)
	}
	if err := primary.CancelCalendarJob(
		ctx,
		leasedDelete.ID,
		"integration-delete-worker",
		leasedDelete.LeaseVersion,
		"source reappeared",
	); err != nil {
		t.Fatalf("cancel stale delete: %v", err)
	}

	missing, err = primary.MarkSyncedEventMissing(ctx, userID, removedSourceUID)
	if err != nil || missing == nil || missing.MissingSince == nil {
		t.Fatalf("mark source missing again = %#v, %v", missing, err)
	}
	deleteNotBefore = time.Now().UTC().Add(time.Hour)
	deleteJob, _, err = primary.EnqueueCalendarJob(ctx, CalendarJobInput{
		TelegramUserID:       userID,
		DedupeKey:            "event:" + removedGoogleEventID,
		Action:               CalendarActionDelete,
		SourceUID:            removedSourceUID,
		CourseID:             "COURSE",
		AssignmentType:       "assignment",
		Title:                "Removed integration assignment",
		GoogleCalendarID:     "primary",
		GoogleEventID:        removedGoogleEventID,
		ExpectedMissingSince: missing.MissingSince,
		ExpectedMissingCount: missing.MissingCount,
		DeleteNotBefore:      &deleteNotBefore,
	})
	if err != nil {
		t.Fatalf("enqueue future-threshold delete: %v", err)
	}
	claimed, err = primary.ClaimCalendarJobs(ctx, "integration-delete-worker", 10, time.Minute)
	if err != nil {
		t.Fatalf("claim before delete threshold: %v", err)
	}
	if leasedDelete = findClaimedJob(claimed, deleteJob.ID); leasedDelete != nil {
		t.Fatalf("delete job was claimed before delete_not_before: %#v", leasedDelete)
	}
	if err := primary.CancelPendingCalendarDelete(ctx, userID, removedSourceUID); err != nil {
		t.Fatalf("cancel future-threshold delete: %v", err)
	}

	deleteNotBefore = missing.MissingSince.Add(time.Millisecond)
	deleteJob, _, err = primary.EnqueueCalendarJob(ctx, CalendarJobInput{
		TelegramUserID:       userID,
		DedupeKey:            "event:" + removedGoogleEventID,
		Action:               CalendarActionDelete,
		SourceUID:            removedSourceUID,
		CourseID:             "COURSE",
		AssignmentType:       "assignment",
		Title:                "Removed integration assignment",
		GoogleCalendarID:     "primary",
		GoogleEventID:        removedGoogleEventID,
		ExpectedMissingSince: missing.MissingSince,
		ExpectedMissingCount: missing.MissingCount,
		DeleteNotBefore:      &deleteNotBefore,
	})
	if err != nil {
		t.Fatalf("enqueue exact-state delete: %v", err)
	}
	claimed, err = primary.ClaimCalendarJobs(ctx, "integration-delete-worker", 10, time.Minute)
	if err != nil {
		t.Fatalf("claim exact-state delete: %v", err)
	}
	leasedDelete = findClaimedJob(claimed, deleteJob.ID)
	if leasedDelete == nil {
		t.Fatalf("exact-state delete %d was not claimed: %#v", deleteJob.ID, claimed)
	}

	missing, err = primary.MarkSyncedEventMissing(ctx, userID, removedSourceUID)
	if err != nil || missing == nil {
		t.Fatalf("advance missing observation = %#v, %v", missing, err)
	}
	allowed, err = primary.CanRunCalendarDeleteJob(
		ctx,
		leasedDelete.ID,
		"integration-delete-worker",
		leasedDelete.LeaseVersion,
	)
	if err != nil || allowed {
		t.Fatalf("changed-count delete preflight = %v, %v; want false", allowed, err)
	}
	if err := primary.CancelCalendarJob(
		ctx,
		leasedDelete.ID,
		"integration-delete-worker",
		leasedDelete.LeaseVersion,
		"missing observation changed",
	); err != nil {
		t.Fatalf("cancel changed-count delete: %v", err)
	}

	deleteNotBefore = missing.MissingSince.Add(time.Millisecond)
	deleteJob, _, err = primary.EnqueueCalendarJob(ctx, CalendarJobInput{
		TelegramUserID:       userID,
		DedupeKey:            "event:" + removedGoogleEventID,
		Action:               CalendarActionDelete,
		SourceUID:            removedSourceUID,
		CourseID:             "COURSE",
		AssignmentType:       "assignment",
		Title:                "Removed integration assignment",
		GoogleCalendarID:     "primary",
		GoogleEventID:        removedGoogleEventID,
		ExpectedMissingSince: missing.MissingSince,
		ExpectedMissingCount: missing.MissingCount,
		DeleteNotBefore:      &deleteNotBefore,
	})
	if err != nil {
		t.Fatalf("enqueue latest exact-state delete: %v", err)
	}
	claimed, err = primary.ClaimCalendarJobs(ctx, "integration-delete-worker", 10, time.Minute)
	if err != nil {
		t.Fatalf("claim latest exact-state delete: %v", err)
	}
	leasedDelete = findClaimedJob(claimed, deleteJob.ID)
	if leasedDelete == nil {
		t.Fatalf("latest exact-state delete %d was not claimed: %#v", deleteJob.ID, claimed)
	}
	if err := primary.CompleteCalendarJob(
		ctx,
		leasedDelete.ID,
		"integration-delete-worker",
		leasedDelete.LeaseVersion,
		"",
	); err != nil {
		t.Fatalf("complete guarded delete: %v", err)
	}
	removed, err := primary.GetSyncedEvent(ctx, userID, removedSourceUID)
	if err != nil || removed != nil {
		t.Fatalf("completed delete tracking = %#v, %v; want nil", removed, err)
	}

	updateJob, _, err := primary.EnqueueCalendarJob(ctx, CalendarJobInput{
		TelegramUserID:   userID,
		DedupeKey:        "event:" + googleEventID,
		Action:           CalendarActionUpdate,
		SourceUID:        sourceUID,
		CourseID:         "COURSE",
		AssignmentType:   "assignment",
		Title:            "Updated integration assignment",
		DueAt:            &dueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    googleEventID,
	})
	if err != nil {
		t.Fatalf("enqueue update before disconnect: %v", err)
	}
	claimed, err = primary.ClaimCalendarJobs(ctx, "integration-worker", 10, time.Minute)
	if err != nil {
		t.Fatalf("claim update before disconnect: %v", err)
	}
	leased = findClaimedJob(claimed, updateJob.ID)
	if leased == nil {
		t.Fatalf("update %d was not claimed: %#v", updateJob.ID, claimed)
	}

	releasePrimary, acquired, err := primary.TryFeedSyncLock(ctx, userID)
	if err != nil || !acquired {
		t.Fatalf("acquire primary advisory lock = %v, %v", acquired, err)
	}
	if _, acquired, err := secondary.TryFeedSyncLock(ctx, userID); err != nil || acquired {
		t.Fatalf("second advisory lock = %v, %v; want unavailable", acquired, err)
	}
	if err := releasePrimary(); err != nil {
		t.Fatalf("release primary advisory lock: %v", err)
	}
	releaseSecondary, acquired, err := secondary.TryFeedSyncLock(ctx, userID)
	if err != nil || !acquired {
		t.Fatalf("acquire secondary advisory lock after release = %v, %v", acquired, err)
	}
	if err := releaseSecondary(); err != nil {
		t.Fatalf("release secondary advisory lock: %v", err)
	}

	nonce, err := primary.CreateDestructiveConfirmation(ctx, userID, "reset", time.Minute)
	if err != nil {
		t.Fatalf("create one-time confirmation: %v", err)
	}
	if valid, err := primary.ConsumeDestructiveConfirmation(ctx, userID, "reset", nonce); err != nil || !valid {
		t.Fatalf("first confirmation consume = %v, %v", valid, err)
	}
	if valid, err := primary.ConsumeDestructiveConfirmation(ctx, userID, "reset", nonce); err != nil || valid {
		t.Fatalf("second confirmation consume = %v, %v; want false", valid, err)
	}

	if err := primary.DisconnectCanvas(ctx, userID); err != nil {
		t.Fatalf("disconnect Canvas: %v", err)
	}
	if _, err := primary.CanRunCalendarJob(ctx, leased.ID, "integration-worker", leased.LeaseVersion); !errors.Is(err, ErrCalendarJobLease) {
		t.Fatalf("cancelled create preflight error = %v, want ErrCalendarJobLease", err)
	}
	tracked, err = primary.GetSyncedEvent(ctx, userID, sourceUID)
	if err != nil || tracked == nil || !tracked.Detached || tracked.RemovalEligible {
		t.Fatalf("detached tracking = %#v, %v", tracked, err)
	}
	missing, err = primary.MarkSyncedEventMissing(ctx, userID, sourceUID)
	if err != nil || missing != nil {
		t.Fatalf("detached missing observation = %#v, %v; want nil", missing, err)
	}
	candidates, err := primary.ListCalendarOwnershipCandidates(ctx, userID)
	if err != nil {
		t.Fatalf("list wipe candidates: %v", err)
	}
	foundCandidate := false
	for _, candidate := range candidates {
		if candidate.CanvasEventUID == sourceUID && candidate.GoogleEventID == googleEventID {
			foundCandidate = true
			break
		}
	}
	if !foundCandidate {
		t.Fatalf("detached/outbox ownership target was lost: %#v", candidates)
	}
}

func TestPostgresPendingCalendarAddLifecycle(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("CANVASLINK_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("CANVASLINK_TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	databaseURL = isolatedPostgresURL(t, ctx, databaseURL)
	storage, err := ConnectContextEncrypted(ctx, databaseURL, EncryptionConfig{
		PrimaryKeyID: "integration",
		PrimaryKey:   bytes.Repeat([]byte{0x5a}, 32),
	})
	if err != nil {
		t.Fatalf("connect store: %v", err)
	}
	defer storage.Close()
	if err := storage.InitSchema(ctx); err != nil {
		t.Fatalf("initialize schema: %v", err)
	}

	userID := time.Now().UnixNano() & 0x3fffffffffffffff
	if userID == 0 {
		userID = 1
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if err := storage.ResetUser(cleanupCtx, userID); err != nil {
			t.Logf("cleanup user: %v", err)
		}
	}()

	if err := storage.UpsertTelegramAccountWithTimezone(ctx, userID, userID, "pending-integration", "UTC"); err != nil {
		t.Fatalf("upsert Telegram account: %v", err)
	}
	if err := storage.UpsertFeed(ctx, userID, "https://canvas.example/feeds/pending-private-token.ics"); err != nil {
		t.Fatalf("upsert feed: %v", err)
	}

	dueAt := time.Now().UTC().Add(48 * time.Hour)
	const sourceUID = "pending-add-source"
	const googleEventID = "pendingdeterministicid"
	inserted, err := storage.UpsertSyncedEvent(ctx, SyncedEventInput{
		TelegramUserID:   userID,
		CourseID:         "COURSE",
		AssignmentType:   "assignment",
		CanvasEventUID:   sourceUID,
		CanvasTitle:      "Pending assignment",
		CanvasDueAt:      dueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    "",
		RemovalEligible:  true,
		GoogleConfirmed:  false,
	})
	if err != nil || !inserted {
		t.Fatalf("insert pending placeholder: inserted=%v err=%v", inserted, err)
	}
	pending, inserted, err := storage.CreatePendingActionIfAbsent(ctx, PendingActionInput{
		TelegramUserID: userID,
		CourseID:       "COURSE",
		AssignmentType: "assignment",
		CanvasEventUID: sourceUID,
		CanvasTitle:    "Pending assignment",
		CanvasDueAt:    dueAt,
		TelegramChatID: userID,
	})
	if err != nil || !inserted {
		t.Fatalf("create pending action: inserted=%v action=%#v err=%v", inserted, pending, err)
	}

	job, err := storage.QueuePendingCalendarAdd(ctx, PendingCalendarAddInput{
		TelegramUserID:   userID,
		PendingActionID:  pending.ID,
		GoogleCalendarID: "primary",
		GoogleEventID:    googleEventID,
		Description:      "manual add integration test",
		MaxAttempts:      12,
	})
	if err != nil {
		t.Fatalf("queue pending calendar add: %v", err)
	}
	if job == nil || job.Action != CalendarActionCreate || job.GoogleEventID != googleEventID {
		t.Fatalf("queued job = %#v", job)
	}
	tracked, err := storage.GetSyncedEvent(ctx, userID, sourceUID)
	if err != nil || tracked == nil {
		t.Fatalf("get targeted placeholder: %#v, %v", tracked, err)
	}
	if tracked.GoogleEventID != googleEventID || tracked.GoogleConfirmed {
		t.Fatalf("targeted placeholder = %#v; want deterministic ID and unconfirmed", tracked)
	}
	pending, err = storage.GetPendingAction(ctx, userID, pending.ID)
	if err != nil || pending == nil || pending.Status != PendingStatusAdded {
		t.Fatalf("consumed pending action = %#v, %v", pending, err)
	}
	if _, err := storage.QueuePendingCalendarAdd(ctx, PendingCalendarAddInput{
		TelegramUserID:   userID,
		PendingActionID:  pending.ID,
		GoogleCalendarID: "primary",
		GoogleEventID:    googleEventID,
	}); !errors.Is(err, ErrPendingAlreadyHandled) {
		t.Fatalf("replayed pending add error = %v, want ErrPendingAlreadyHandled", err)
	}

	cancelled, err := storage.CancelPendingActionByUID(ctx, userID, sourceUID)
	if err != nil || cancelled == nil || cancelled.Status != PendingStatusSourceRemoved {
		t.Fatalf("source-cancelled added action = %#v, %v", cancelled, err)
	}

	const reviewUID = "pending-reappearance-source"
	inserted, err = storage.UpsertSyncedEvent(ctx, SyncedEventInput{
		TelegramUserID:   userID,
		CourseID:         "COURSE",
		AssignmentType:   "quiz",
		CanvasEventUID:   reviewUID,
		CanvasTitle:      "Reappearing quiz",
		CanvasDueAt:      dueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    "",
		RemovalEligible:  true,
		GoogleConfirmed:  false,
	})
	if err != nil || !inserted {
		t.Fatalf("insert reappearance placeholder: inserted=%v err=%v", inserted, err)
	}
	review, inserted, err := storage.CreatePendingActionIfAbsent(ctx, PendingActionInput{
		TelegramUserID: userID,
		CourseID:       "COURSE",
		AssignmentType: "quiz",
		CanvasEventUID: reviewUID,
		CanvasTitle:    "Reappearing quiz",
		CanvasDueAt:    dueAt,
		TelegramChatID: userID,
	})
	if err != nil || !inserted {
		t.Fatalf("create reappearance action: inserted=%v action=%#v err=%v", inserted, review, err)
	}
	if err := storage.SetPendingMessageID(ctx, userID, review.ID, 777); err != nil {
		t.Fatalf("set pending message ID: %v", err)
	}
	cancelled, err = storage.CancelPendingActionByUID(ctx, userID, reviewUID)
	if err != nil || cancelled == nil || cancelled.Status != PendingStatusSourceRemoved {
		t.Fatalf("cancel pending source action = %#v, %v", cancelled, err)
	}
	reopened, inserted, err := storage.CreatePendingActionIfAbsent(ctx, PendingActionInput{
		TelegramUserID: userID,
		CourseID:       "COURSE",
		AssignmentType: "quiz",
		CanvasEventUID: reviewUID,
		CanvasTitle:    "Reappearing quiz, updated",
		CanvasDueAt:    dueAt.Add(time.Hour),
		TelegramChatID: userID,
	})
	if err != nil || inserted {
		t.Fatalf("reopen source-cancelled action: inserted=%v action=%#v err=%v", inserted, reopened, err)
	}
	if reopened.Status != PendingStatusPending || reopened.TelegramMessageID.Valid {
		t.Fatalf("reopened action = %#v; want pending with a fresh message", reopened)
	}
	if err := storage.MarkPendingStatus(ctx, userID, reopened.ID, PendingStatusIgnored); err != nil {
		t.Fatalf("user-ignore reopened action: %v", err)
	}
	cancelled, err = storage.CancelPendingActionByUID(ctx, userID, reviewUID)
	if err != nil || cancelled != nil {
		t.Fatalf("source cancellation changed user-ignored action = %#v, %v", cancelled, err)
	}
	ignored, inserted, err := storage.CreatePendingActionIfAbsent(ctx, PendingActionInput{
		TelegramUserID: userID,
		CourseID:       "COURSE",
		AssignmentType: "quiz",
		CanvasEventUID: reviewUID,
		CanvasTitle:    "Reappearing quiz again",
		CanvasDueAt:    dueAt.Add(2 * time.Hour),
		TelegramChatID: userID,
	})
	if err != nil || inserted || ignored.Status != PendingStatusIgnored {
		t.Fatalf("user-ignored action was reopened: inserted=%v action=%#v err=%v", inserted, ignored, err)
	}

	const changedCardUID = "pending-changed-card"
	inserted, err = storage.UpsertSyncedEvent(ctx, SyncedEventInput{
		TelegramUserID:   userID,
		CourseID:         "COURSE",
		AssignmentType:   "assignment",
		CanvasEventUID:   changedCardUID,
		CanvasTitle:      "Original card title",
		CanvasDueAt:      dueAt,
		GoogleCalendarID: "primary",
		RemovalEligible:  true,
		GoogleConfirmed:  false,
	})
	if err != nil || !inserted {
		t.Fatalf("insert changed-card placeholder: inserted=%v err=%v", inserted, err)
	}
	changedCard, inserted, err := storage.CreatePendingActionIfAbsent(ctx, PendingActionInput{
		TelegramUserID: userID,
		CourseID:       "COURSE",
		AssignmentType: "assignment",
		CanvasEventUID: changedCardUID,
		CanvasTitle:    "Original card title",
		CanvasDueAt:    dueAt,
		TelegramChatID: userID,
	})
	if err != nil || !inserted {
		t.Fatalf("create changed-card action: inserted=%v action=%#v err=%v", inserted, changedCard, err)
	}
	if err := storage.SetPendingMessageID(ctx, userID, changedCard.ID, 888); err != nil {
		t.Fatalf("set changed-card message ID: %v", err)
	}
	changedCard, inserted, err = storage.CreatePendingActionIfAbsent(ctx, PendingActionInput{
		TelegramUserID: userID,
		CourseID:       "COURSE",
		AssignmentType: "assignment",
		CanvasEventUID: changedCardUID,
		CanvasTitle:    "Updated card title",
		CanvasDueAt:    dueAt.Add(time.Hour),
		TelegramChatID: userID,
	})
	if err != nil || inserted {
		t.Fatalf("update changed-card action: inserted=%v action=%#v err=%v", inserted, changedCard, err)
	}
	if changedCard.TelegramMessageID.Valid ||
		changedCard.CanvasTitle != "Updated card title" ||
		absDuration(changedCard.CanvasDueAt.Sub(dueAt.Add(time.Hour))) >= time.Microsecond {
		t.Fatalf("changed card did not require a fresh Telegram message: %#v", changedCard)
	}

	if err := storage.SeedDefaultSettings(ctx, userID, []CourseTypeSeed{
		{CourseID: "REVIEW", CourseName: "REVIEW", AssignmentType: "assignment"},
		{CourseID: "IGNORED", CourseName: "IGNORED", AssignmentType: "quiz"},
	}); err != nil {
		t.Fatalf("seed stale-card settings: %v", err)
	}
	if err := storage.SetCourseTypeMode(ctx, userID, "IGNORED", "quiz", ModeIgnore); err != nil {
		t.Fatalf("set stale-card target mode: %v", err)
	}
	const staleCardUID = "stale-review-card-source"
	inserted, err = storage.UpsertSyncedEvent(ctx, SyncedEventInput{
		TelegramUserID:   userID,
		CourseID:         "REVIEW",
		AssignmentType:   "assignment",
		CanvasEventUID:   staleCardUID,
		CanvasTitle:      "Old review card",
		CanvasDueAt:      dueAt,
		GoogleCalendarID: "primary",
		RemovalEligible:  true,
		GoogleConfirmed:  false,
	})
	if err != nil || !inserted {
		t.Fatalf("insert stale-card source: inserted=%v err=%v", inserted, err)
	}
	staleCard, inserted, err := storage.CreatePendingActionIfAbsent(ctx, PendingActionInput{
		TelegramUserID: userID,
		CourseID:       "REVIEW",
		AssignmentType: "assignment",
		CanvasEventUID: staleCardUID,
		CanvasTitle:    "Old review card",
		CanvasDueAt:    dueAt,
		TelegramChatID: userID,
	})
	if err != nil || !inserted {
		t.Fatalf("create stale review card: inserted=%v action=%#v err=%v", inserted, staleCard, err)
	}
	currentIgnoredDueAt := dueAt.Add(3 * time.Hour)
	if err := storage.ObserveActionableSyncedEvent(ctx, SyncedEventInput{
		TelegramUserID:  userID,
		CourseID:        "IGNORED",
		AssignmentType:  "quiz",
		CanvasEventUID:  staleCardUID,
		CanvasTitle:     "Current ignored source",
		CanvasDueAt:     currentIgnoredDueAt,
		RemovalEligible: true,
	}); err != nil {
		t.Fatalf("observe changed stale-card source: %v", err)
	}
	_, err = storage.QueuePendingCalendarAdd(ctx, PendingCalendarAddInput{
		TelegramUserID:   userID,
		PendingActionID:  staleCard.ID,
		GoogleCalendarID: "primary",
		GoogleEventID:    "stalereviewcardeventid",
		MaxAttempts:      12,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale review approval error = %v, want ErrNotFound", err)
	}
	tracked, err = storage.GetSyncedEvent(ctx, userID, staleCardUID)
	if err != nil || tracked == nil ||
		tracked.CourseID != "IGNORED" ||
		tracked.AssignmentType != "quiz" ||
		tracked.CanvasTitle != "Current ignored source" ||
		absDuration(tracked.CanvasDueAt.Sub(currentIgnoredDueAt)) >= time.Microsecond ||
		tracked.GoogleEventID != "" {
		t.Fatalf("stale approval changed the current source: %#v, %v", tracked, err)
	}
	var staleCardJobs int
	if err := storage.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM canvaslink_calendar_jobs
		 WHERE telegram_user_id = $1 AND source_uid = $2`,
		userID,
		staleCardUID,
	).Scan(&staleCardJobs); err != nil {
		t.Fatalf("count stale-card jobs: %v", err)
	}
	if staleCardJobs != 0 {
		t.Fatalf("stale review approval enqueued %d jobs", staleCardJobs)
	}

	if err := storage.SeedDefaultSettings(ctx, userID, []CourseTypeSeed{{
		CourseID:       "AUTO",
		CourseName:     "AUTO",
		AssignmentType: "assignment",
	}}); err != nil {
		t.Fatalf("seed Auto setting: %v", err)
	}
	if err := storage.SetCourseTypeMode(ctx, userID, "AUTO", "assignment", ModeAuto); err != nil {
		t.Fatalf("set Auto mode: %v", err)
	}
	if err := storage.SaveGoogleAuthorization(
		ctx,
		userID,
		"pending-access",
		"pending-refresh",
		"Bearer",
		"calendar.events",
		time.Now().Add(time.Hour),
	); err != nil {
		t.Fatalf("save pending-test Google token: %v", err)
	}

	const autoUID = "review-placeholder-to-auto"
	const autoEventID = "reviewplaceholderautoid"
	inserted, err = storage.UpsertSyncedEvent(ctx, SyncedEventInput{
		TelegramUserID:   userID,
		CourseID:         "AUTO",
		AssignmentType:   "assignment",
		CanvasEventUID:   autoUID,
		CanvasTitle:      "Review placeholder",
		CanvasDueAt:      dueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    "",
		RemovalEligible:  true,
		GoogleConfirmed:  false,
	})
	if err != nil || !inserted {
		t.Fatalf("insert Auto placeholder: inserted=%v err=%v", inserted, err)
	}
	autoDueAt := dueAt.Add(2 * time.Hour)
	autoJob, err := storage.QueueCalendarSync(ctx, SyncedEventInput{
		TelegramUserID:   userID,
		CourseID:         "AUTO",
		AssignmentType:   "assignment",
		CanvasEventUID:   autoUID,
		CanvasTitle:      "Review placeholder, updated",
		CanvasDueAt:      autoDueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    autoEventID,
		RemovalEligible:  true,
		GoogleConfirmed:  false,
	}, CalendarJobInput{
		TelegramUserID:   userID,
		DedupeKey:        "event:" + autoEventID,
		Action:           CalendarActionCreate,
		SourceUID:        autoUID,
		CourseID:         "AUTO",
		AssignmentType:   "assignment",
		Title:            "Review placeholder, updated",
		DueAt:            &autoDueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    autoEventID,
		MaxAttempts:      12,
	})
	if err != nil {
		t.Fatalf("queue Review-to-Auto source: %v", err)
	}
	tracked, err = storage.GetSyncedEvent(ctx, userID, autoUID)
	if err != nil || tracked == nil {
		t.Fatalf("load bound Auto placeholder: %#v, %v", tracked, err)
	}
	if tracked.GoogleEventID != autoEventID ||
		tracked.GoogleConfirmed ||
		tracked.CanvasTitle != "Review placeholder, updated" ||
		absDuration(tracked.CanvasDueAt.Sub(autoDueAt)) >= time.Microsecond {
		t.Fatalf("bound Auto placeholder = %#v", tracked)
	}
	claimed, err := storage.ClaimCalendarJobs(ctx, "pending-auto-worker", 100, time.Minute)
	if err != nil {
		t.Fatalf("claim Review-to-Auto job: %v", err)
	}
	leased := findClaimedJob(claimed, autoJob.ID)
	if leased == nil {
		t.Fatalf("Review-to-Auto job %d was not claimed: %#v", autoJob.ID, claimed)
	}
	allowed, err := storage.CanRunCalendarJob(ctx, leased.ID, "pending-auto-worker", leased.LeaseVersion)
	if err != nil || !allowed {
		t.Fatalf("Review-to-Auto preflight = %v, %v", allowed, err)
	}
	if err := storage.CancelCalendarJob(
		ctx,
		leased.ID,
		"pending-auto-worker",
		leased.LeaseVersion,
		"integration test complete",
	); err != nil {
		t.Fatalf("cancel Review-to-Auto test job: %v", err)
	}

	const revertUID = "queued-update-reverted"
	const revertEventID = "queuedupdaterevertedid"
	revertStamp := time.Now().UTC().Add(-time.Hour)
	inserted, err = storage.UpsertSyncedEvent(ctx, SyncedEventInput{
		TelegramUserID:   userID,
		CourseID:         "AUTO",
		AssignmentType:   "assignment",
		CanvasEventUID:   revertUID,
		CanvasTitle:      "Confirmed A",
		CanvasDueAt:      dueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    revertEventID,
		CanvasDTStamp:    &revertStamp,
		CanvasSequence:   1,
		RemovalEligible:  true,
		GoogleConfirmed:  true,
	})
	if err != nil || !inserted {
		t.Fatalf("insert reverted-update source: inserted=%v err=%v", inserted, err)
	}
	queuedBStamp := revertStamp.Add(time.Minute)
	queuedBDueAt := dueAt.Add(time.Hour)
	queuedB, _, err := storage.EnqueueCalendarJob(ctx, CalendarJobInput{
		TelegramUserID:   userID,
		DedupeKey:        "event:" + revertEventID,
		Action:           CalendarActionUpdate,
		SourceUID:        revertUID,
		CourseID:         "AUTO",
		AssignmentType:   "assignment",
		Title:            "Queued B",
		DueAt:            &queuedBDueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    revertEventID,
		CanvasDTStamp:    &queuedBStamp,
		CanvasSequence:   2,
		MaxAttempts:      12,
	})
	if err != nil {
		t.Fatalf("enqueue stale B update: %v", err)
	}
	cancelledCount, err := storage.CancelMismatchedOpenCalendarSyncJobs(ctx, CalendarJobInput{
		TelegramUserID:   userID,
		DedupeKey:        "event:" + revertEventID,
		Action:           CalendarActionUpdate,
		SourceUID:        revertUID,
		CourseID:         "AUTO",
		AssignmentType:   "assignment",
		Title:            "Confirmed A",
		DueAt:            &dueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    revertEventID,
		CanvasDTStamp:    &revertStamp,
		CanvasSequence:   1,
		MaxAttempts:      12,
	}, "Canvas reverted to A")
	if err != nil || cancelledCount != 1 {
		t.Fatalf("cancel stale B update = %d, %v; want 1", cancelledCount, err)
	}
	queuedB, err = storage.GetCalendarJob(ctx, userID, queuedB.ID)
	if err != nil || queuedB == nil || queuedB.Status != CalendarJobStatusCancelled {
		t.Fatalf("stale B job after reconciliation = %#v, %v", queuedB, err)
	}

	matchingA, _, err := storage.EnqueueCalendarJob(ctx, CalendarJobInput{
		TelegramUserID:   userID,
		DedupeKey:        "event:" + revertEventID,
		Action:           CalendarActionUpdate,
		SourceUID:        revertUID,
		CourseID:         "AUTO",
		AssignmentType:   "assignment",
		Title:            "Confirmed A",
		DueAt:            &dueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    revertEventID,
		CanvasDTStamp:    &revertStamp,
		CanvasSequence:   1,
		MaxAttempts:      12,
	})
	if err != nil {
		t.Fatalf("enqueue matching A update: %v", err)
	}
	cancelledCount, err = storage.CancelMismatchedOpenCalendarSyncJobs(ctx, CalendarJobInput{
		TelegramUserID:   userID,
		DedupeKey:        "event:" + revertEventID,
		Action:           CalendarActionUpdate,
		SourceUID:        revertUID,
		CourseID:         "AUTO",
		AssignmentType:   "assignment",
		Title:            "Confirmed A",
		DueAt:            &dueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    revertEventID,
		CanvasDTStamp:    &revertStamp,
		CanvasSequence:   1,
		MaxAttempts:      12,
	}, "exact state should remain")
	if err != nil || cancelledCount != 0 {
		t.Fatalf("exact A reconciliation = %d, %v; want 0", cancelledCount, err)
	}
	matchingA, err = storage.GetCalendarJob(ctx, userID, matchingA.ID)
	if err != nil || matchingA == nil || matchingA.Status != CalendarJobStatusPending {
		t.Fatalf("matching A job after reconciliation = %#v, %v", matchingA, err)
	}

	const stagedUID = "staged-update-observation-fence"
	const stagedEventID = "stagedupdateobservationid"
	inserted, err = storage.UpsertSyncedEvent(ctx, SyncedEventInput{
		TelegramUserID:   userID,
		CourseID:         "AUTO",
		AssignmentType:   "assignment",
		CanvasEventUID:   stagedUID,
		CanvasTitle:      "Observed A",
		CanvasDueAt:      dueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    stagedEventID,
		CanvasDTStamp:    &revertStamp,
		CanvasSequence:   1,
		RemovalEligible:  true,
		GoogleConfirmed:  true,
	})
	if err != nil || !inserted {
		t.Fatalf("insert staged-observation source: inserted=%v err=%v", inserted, err)
	}
	stagedBDueAt := dueAt.Add(4 * time.Hour)
	stagedBStamp := revertStamp.Add(2 * time.Minute)
	stagedJob, err := storage.QueueCalendarSync(ctx, SyncedEventInput{
		TelegramUserID:   userID,
		CourseID:         "AUTO",
		AssignmentType:   "assignment",
		CanvasEventUID:   stagedUID,
		CanvasTitle:      "Staged B",
		CanvasDueAt:      stagedBDueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    stagedEventID,
		CanvasDTStamp:    &stagedBStamp,
		CanvasSequence:   2,
		RemovalEligible:  true,
		GoogleConfirmed:  false,
	}, CalendarJobInput{
		TelegramUserID:   userID,
		DedupeKey:        "event:" + stagedEventID,
		Action:           CalendarActionUpdate,
		SourceUID:        stagedUID,
		CourseID:         "AUTO",
		AssignmentType:   "assignment",
		Title:            "Staged B",
		DueAt:            &stagedBDueAt,
		GoogleCalendarID: "primary",
		GoogleEventID:    stagedEventID,
		CanvasDTStamp:    &stagedBStamp,
		CanvasSequence:   2,
		MaxAttempts:      12,
	})
	if err != nil {
		t.Fatalf("stage B update: %v", err)
	}
	if err := storage.ObserveActionableSyncedEvent(ctx, SyncedEventInput{
		TelegramUserID:  userID,
		CourseID:        "AUTO",
		AssignmentType:  "assignment",
		CanvasEventUID:  stagedUID,
		CanvasTitle:     "Observed A",
		CanvasDueAt:     dueAt,
		CanvasDTStamp:   &revertStamp,
		CanvasSequence:  1,
		RemovalEligible: true,
	}); err != nil {
		t.Fatalf("observe A after staged B: %v", err)
	}
	claimed, err = storage.ClaimCalendarJobs(ctx, "staged-observation-worker", 100, time.Minute)
	if err != nil {
		t.Fatalf("claim staged B update: %v", err)
	}
	stagedLease := findClaimedJob(claimed, stagedJob.ID)
	if stagedLease == nil {
		t.Fatalf("staged B job %d was not claimed: %#v", stagedJob.ID, claimed)
	}
	allowed, err = storage.CanRunCalendarJob(
		ctx,
		stagedLease.ID,
		"staged-observation-worker",
		stagedLease.LeaseVersion,
	)
	if err != nil || allowed {
		t.Fatalf("stale staged B preflight = %v, %v; want false", allowed, err)
	}
	if err := storage.CompleteCalendarJob(
		ctx,
		stagedLease.ID,
		"staged-observation-worker",
		stagedLease.LeaseVersion,
		stagedEventID,
	); !errors.Is(err, ErrStaleCalendarJob) {
		t.Fatalf("stale staged B completion error = %v, want ErrStaleCalendarJob", err)
	}
	tracked, err = storage.GetSyncedEvent(ctx, userID, stagedUID)
	if err != nil || tracked == nil ||
		tracked.CanvasTitle != "Observed A" ||
		tracked.GoogleConfirmed {
		t.Fatalf("stale B completion changed observed A: %#v, %v", tracked, err)
	}

	if err := storage.SeedDefaultSettings(ctx, userID, []CourseTypeSeed{{
		CourseID:       "MODEGUARD",
		CourseName:     "MODEGUARD",
		AssignmentType: "assignment",
	}}); err != nil {
		t.Fatalf("seed mode-guard setting: %v", err)
	}
	if err := storage.SetCourseTypeMode(ctx, userID, "MODEGUARD", "assignment", ModeAuto); err != nil {
		t.Fatalf("set initial mode-guard Auto mode: %v", err)
	}
	const modeGuardUID = "mode-transition-create-guard"
	const modeGuardEventID = "modetransitioncreateid"
	queueModeGuard := func() *CalendarJob {
		t.Helper()
		job, err := storage.QueueCalendarSync(ctx, SyncedEventInput{
			TelegramUserID:   userID,
			CourseID:         "MODEGUARD",
			AssignmentType:   "assignment",
			CanvasEventUID:   modeGuardUID,
			CanvasTitle:      "Mode transition source",
			CanvasDueAt:      dueAt,
			GoogleCalendarID: "primary",
			GoogleEventID:    modeGuardEventID,
			RemovalEligible:  true,
			GoogleConfirmed:  false,
		}, CalendarJobInput{
			TelegramUserID:   userID,
			DedupeKey:        "event:" + modeGuardEventID,
			Action:           CalendarActionCreate,
			SourceUID:        modeGuardUID,
			CourseID:         "MODEGUARD",
			AssignmentType:   "assignment",
			Title:            "Mode transition source",
			DueAt:            &dueAt,
			GoogleCalendarID: "primary",
			GoogleEventID:    modeGuardEventID,
			MaxAttempts:      12,
		})
		if err != nil {
			t.Fatalf("queue mode-guard create: %v", err)
		}
		return job
	}
	for index, blockedMode := range []string{ModeActive, ModeIgnore} {
		if err := storage.SetCourseTypeMode(ctx, userID, "MODEGUARD", "assignment", ModeAuto); err != nil {
			t.Fatalf("restore mode-guard Auto mode: %v", err)
		}
		modeJob := queueModeGuard()
		workerID := "mode-guard-worker-" + strconv.Itoa(index)
		claimed, err = storage.ClaimCalendarJobs(ctx, workerID, 100, time.Minute)
		if err != nil {
			t.Fatalf("claim mode-guard create: %v", err)
		}
		modeLease := findClaimedJob(claimed, modeJob.ID)
		if modeLease == nil {
			t.Fatalf("mode-guard job %d was not claimed: %#v", modeJob.ID, claimed)
		}
		if err := storage.SetCourseTypeMode(ctx, userID, "MODEGUARD", "assignment", blockedMode); err != nil {
			t.Fatalf("set blocked mode %q: %v", blockedMode, err)
		}
		allowed, err = storage.CanRunCalendarJob(ctx, modeLease.ID, workerID, modeLease.LeaseVersion)
		if err != nil || allowed {
			t.Fatalf("create preflight after mode %q = %v, %v; want false", blockedMode, allowed, err)
		}
		if err := storage.CompleteCalendarJob(
			ctx,
			modeLease.ID,
			workerID,
			modeLease.LeaseVersion,
			modeGuardEventID,
		); !errors.Is(err, ErrStaleCalendarJob) {
			t.Fatalf("create completion after mode %q = %v, want ErrStaleCalendarJob", blockedMode, err)
		}
	}
}

func isolatedPostgresURL(t *testing.T, ctx context.Context, databaseURL string) string {
	t.Helper()

	parsed, err := url.Parse(databaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		t.Fatalf("CANVASLINK_TEST_DATABASE_URL must be a PostgreSQL URL: %v", err)
	}
	schema := "canvaslink_test_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	admin, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open integration database for schema isolation: %v", err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close integration schema connection: %v", err)
		}
	})
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create isolated integration schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(cleanupCtx, `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop isolated integration schema: %v", err)
		}
	})

	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func findClaimedJob(jobs []CalendarJob, id int64) *CalendarJob {
	for i := range jobs {
		if jobs[i].ID == id {
			return &jobs[i]
		}
	}
	return nil
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}
