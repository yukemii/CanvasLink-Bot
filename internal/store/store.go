package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Store struct {
	db     *sql.DB
	lockDB *sql.DB
	fields *fieldCipher
}

var (
	ErrNotFound              = errors.New("store record not found")
	ErrInvalidMode           = errors.New("invalid sync mode")
	ErrInvalidPendingStatus  = errors.New("invalid pending status")
	ErrPendingAlreadyHandled = errors.New("pending action already handled")
	ErrInvalidTimezone       = errors.New("invalid timezone")
	ErrStaleOnboardingStep   = errors.New("stale onboarding step")
	ErrEncryptionRequired    = errors.New("encrypted database value requires a configured encryption key")
	ErrUnknownEncryptionKey  = errors.New("database value uses an unknown encryption key")
	ErrInvalidCalendarJob    = errors.New("invalid calendar job")
	ErrCalendarJobLease      = errors.New("calendar job is not leased by this worker")
	ErrStaleCalendarJob      = errors.New("calendar job was cancelled because its source state changed")
	ErrUnsafeChatDestination = errors.New("telegram account is not bound to a private chat")
)

func Connect(databaseURL string) (*Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return ConnectContext(ctx, databaseURL)
}

func ConnectContext(ctx context.Context, databaseURL string) (*Store, error) {
	return connectContext(ctx, databaseURL, nil)
}

// EncryptionConfig configures application-level encryption for sensitive
// database values. PrimaryKey must contain 16, 24, or 32 random bytes. Previous
// keys remain read-only and allow InitSchema to rotate existing ciphertext
// after older application writers have been drained.
type EncryptionConfig struct {
	PrimaryKeyID string
	PrimaryKey   []byte
	PreviousKeys map[string][]byte
}

// ConnectEncrypted is Connect with application-level field encryption enabled.
func ConnectEncrypted(databaseURL string, encryption EncryptionConfig) (*Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return ConnectContextEncrypted(ctx, databaseURL, encryption)
}

// ConnectContextEncrypted is ConnectContext with application-level field
// encryption enabled.
func ConnectContextEncrypted(ctx context.Context, databaseURL string, encryption EncryptionConfig) (*Store, error) {
	fields, err := newFieldCipher(encryption)
	if err != nil {
		return nil, err
	}
	return connectContext(ctx, databaseURL, fields)
}

func connectContext(ctx context.Context, databaseURL string, fields *fieldCipher) (*Store, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	lockDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open database lock pool: %w", err)
	}
	lockDB.SetMaxOpenConns(5)
	lockDB.SetMaxIdleConns(2)
	lockDB.SetConnMaxLifetime(30 * time.Minute)
	if err := lockDB.PingContext(ctx); err != nil {
		_ = lockDB.Close()
		_ = db.Close()
		return nil, fmt.Errorf("ping database lock pool: %w", err)
	}

	return &Store{db: db, lockDB: lockDB, fields: fields}, nil
}

func (s *Store) Close() error {
	var lockErr error
	if s.lockDB != nil {
		lockErr = s.lockDB.Close()
	}
	var dbErr error
	if s.db != nil {
		dbErr = s.db.Close()
	}
	return errors.Join(lockErr, dbErr)
}

func (s *Store) InitSchema(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema transaction: %w", err)
	}
	defer tx.Rollback()

	// Serialize schema initialization across concurrently starting instances.
	const schemaLockID int64 = 0x43414E564153
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaLockID); err != nil {
		return fmt.Errorf("lock schema initialization: %w", err)
	}

	statements := []string{
		canvaslinkTelegramAccountsTable,
		canvaslinkFeedsTable,
		canvaslinkCourseSettingsTable,
		canvaslinkSyncedEventsTable,
		canvaslinkPendingActionsTable,
		canvaslinkOAuthStatesTable,
		canvaslinkGoogleTokensTable,
		canvaslinkCalendarJobsTable,
		canvaslinkDestructiveConfirmationsTable,
	}

	for i, q := range statements {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("create schema object %d: %w", i+1, err)
		}
	}

	// Run migrations for existing tables that may have been created with an older schema
	migrations := []string{
		// Add canvas_dtstamp column to synced_events if missing
		`ALTER TABLE canvaslink_synced_events ADD COLUMN IF NOT EXISTS canvas_dtstamp TIMESTAMPTZ`,
		// Add canvas_sequence column to synced_events if missing
		`ALTER TABLE canvaslink_synced_events ADD COLUMN IF NOT EXISTS canvas_sequence INT NOT NULL DEFAULT 0`,
		// Add updated_at column to synced_events if missing
		`ALTER TABLE canvaslink_synced_events ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`,
		// Add onboarding_status column to telegram_accounts if missing
		`ALTER TABLE canvaslink_telegram_accounts ADD COLUMN IF NOT EXISTS onboarding_status TEXT NOT NULL DEFAULT ''`,
		// Add sync_interval column to feeds if missing
		`ALTER TABLE canvaslink_feeds ADD COLUMN IF NOT EXISTS sync_interval TEXT NOT NULL DEFAULT '1h'`,
		// Persist the exact onboarding row so a restart resumes at the same step.
		`ALTER TABLE canvaslink_telegram_accounts ADD COLUMN IF NOT EXISTS onboarding_setting_id BIGINT`,
		// Store display and calendar times in the user's chosen IANA timezone.
		`ALTER TABLE canvaslink_telegram_accounts ADD COLUMN IF NOT EXISTS timezone TEXT NOT NULL DEFAULT 'UTC'`,
		`ALTER TABLE canvaslink_telegram_accounts ADD COLUMN IF NOT EXISTS state_revision BIGINT NOT NULL DEFAULT 0`,
		addOAuthStatePrivateChatBindingColumn,
		invalidateUnboundOAuthStates,
		// Preserve all-day semantics and cancellation/removal observations.
		`ALTER TABLE canvaslink_synced_events ADD COLUMN IF NOT EXISTS all_day BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE canvaslink_synced_events ADD COLUMN IF NOT EXISTS canvas_status TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE canvaslink_synced_events ADD COLUMN IF NOT EXISTS canvas_cancelled BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE canvaslink_synced_events ADD COLUMN IF NOT EXISTS missing_since TIMESTAMPTZ`,
		`ALTER TABLE canvaslink_synced_events ADD COLUMN IF NOT EXISTS missing_count INT NOT NULL DEFAULT 0`,
		`ALTER TABLE canvaslink_synced_events ADD COLUMN IF NOT EXISTS detached BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE canvaslink_synced_events ADD COLUMN IF NOT EXISTS removal_eligible BOOLEAN NOT NULL DEFAULT TRUE`,
		`ALTER TABLE canvaslink_synced_events ADD COLUMN IF NOT EXISTS google_confirmed BOOLEAN NOT NULL DEFAULT TRUE`,
		`ALTER TABLE canvaslink_pending_actions ADD COLUMN IF NOT EXISTS all_day BOOLEAN NOT NULL DEFAULT FALSE`,
		migratePendingActionStatusConstraint,
		`ALTER TABLE canvaslink_calendar_jobs ADD COLUMN IF NOT EXISTS lease_version BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE canvaslink_calendar_jobs ADD COLUMN IF NOT EXISTS expected_missing_since TIMESTAMPTZ`,
		`ALTER TABLE canvaslink_calendar_jobs ADD COLUMN IF NOT EXISTS expected_missing_count INT NOT NULL DEFAULT 0`,
		`ALTER TABLE canvaslink_calendar_jobs ADD COLUMN IF NOT EXISTS delete_not_before TIMESTAMPTZ`,
		`ALTER TABLE canvaslink_calendar_jobs
			DROP CONSTRAINT IF EXISTS canvaslink_calendar_jobs_telegram_user_id_dedupe_key_key`,
		// Update the mode constraint once when upgrading from the legacy three-mode schema.
		`DO $migration$
		BEGIN
			IF NOT EXISTS (
				SELECT 1
				FROM pg_constraint
				WHERE conrelid = 'canvaslink_course_type_settings'::regclass
					AND conname = 'canvaslink_course_type_settings_mode_check'
					AND POSITION('quiet' IN pg_get_constraintdef(oid)) > 0
					AND POSITION('notify' IN pg_get_constraintdef(oid)) > 0
					AND POSITION('review' IN pg_get_constraintdef(oid)) > 0
					AND POSITION('ignore' IN pg_get_constraintdef(oid)) > 0
					AND POSITION('auto' IN pg_get_constraintdef(oid)) > 0
					AND POSITION('active' IN pg_get_constraintdef(oid)) > 0
			) THEN
				ALTER TABLE canvaslink_course_type_settings
					DROP CONSTRAINT IF EXISTS canvaslink_course_type_settings_mode_check;
				ALTER TABLE canvaslink_course_type_settings
					ADD CONSTRAINT canvaslink_course_type_settings_mode_check
					CHECK (mode IN ('quiet', 'notify', 'review', 'ignore', 'auto', 'active'));
			END IF;
		END;
		$migration$`,
		`CREATE INDEX IF NOT EXISTS canvaslink_oauth_states_expires_at_idx ON canvaslink_oauth_states (expires_at)`,
		`CREATE INDEX IF NOT EXISTS canvaslink_synced_events_user_course_idx ON canvaslink_synced_events (telegram_user_id, course_id)`,
		`CREATE INDEX IF NOT EXISTS canvaslink_calendar_jobs_claim_idx
			ON canvaslink_calendar_jobs (status, available_at, lease_until, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS canvaslink_calendar_jobs_pending_dedupe_idx
			ON canvaslink_calendar_jobs (telegram_user_id, dedupe_key)
			WHERE status = 'pending'`,
		`CREATE INDEX IF NOT EXISTS canvaslink_destructive_confirmations_expiry_idx
			ON canvaslink_destructive_confirmations (expires_at)`,
	}

	for i, q := range migrations {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("apply schema migration %d: %w", i+1, err)
		}
	}

	if s.fields != nil {
		if err := s.migrateSensitiveFields(ctx, tx); err != nil {
			return fmt.Errorf("migrate encrypted database fields: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema transaction: %w", err)
	}
	return nil
}

// --- Feeds ---

func (s *Store) UpsertFeed(ctx context.Context, userID int64, icalURL string) error {
	storedURL, err := s.encodeSensitive(icalURL, feedURLPurpose(userID))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(
		ctx,
		`
		INSERT INTO canvaslink_feeds (telegram_user_id, ical_url, sync_enabled)
		VALUES ($1, $2, TRUE)
		ON CONFLICT (telegram_user_id)
		DO UPDATE SET
			ical_url = EXCLUDED.ical_url,
			sync_enabled = TRUE,
			updated_at = NOW()
		`,
		userID,
		storedURL,
	)
	return err
}

// SaveFeedWithSettings atomically stores a validated feed and its detected
// course/type defaults.
func (s *Store) SaveFeedWithSettings(
	ctx context.Context,
	userID int64,
	icalURL,
	expectedOnboardingStatus,
	nextOnboardingStatus string,
	expectedStateRevision int64,
	rows []CourseTypeSeed,
) error {
	storedURL, err := s.encodeSensitive(icalURL, feedURLPurpose(userID))
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		return saveFeedWithSettingsTx(
			ctx,
			tx,
			userID,
			storedURL,
			expectedOnboardingStatus,
			nextOnboardingStatus,
			expectedStateRevision,
			rows,
		)
	})
}

func saveFeedWithSettingsTx(
	ctx context.Context,
	execer contextExecer,
	userID int64,
	storedURL,
	expectedOnboardingStatus,
	nextOnboardingStatus string,
	expectedStateRevision int64,
	rows []CourseTypeSeed,
) error {
	if _, err := execer.ExecContext(
		ctx,
		`
		INSERT INTO canvaslink_feeds (telegram_user_id, ical_url, sync_enabled)
		VALUES ($1, $2, TRUE)
		ON CONFLICT (telegram_user_id)
		DO UPDATE SET
			ical_url = EXCLUDED.ical_url,
			sync_enabled = TRUE,
			updated_at = NOW()
		`,
		userID,
		storedURL,
	); err != nil {
		return err
	}
	result, err := execer.ExecContext(
		ctx,
		`UPDATE canvaslink_telegram_accounts
		 SET onboarding_status = $2,
		     onboarding_setting_id = CASE WHEN $2 = 'course_setup' THEN onboarding_setting_id ELSE NULL END,
		     state_revision = state_revision + 1,
		     updated_at = NOW()
		 WHERE telegram_user_id = $1
		   AND onboarding_status = $3
		   AND state_revision = $4`,
		userID,
		nextOnboardingStatus,
		expectedOnboardingStatus,
		expectedStateRevision,
	)
	if err := requireAffected(result, err); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrStaleOnboardingStep
		}
		return err
	}
	return seedDefaultSettings(ctx, execer, userID, rows)
}

func (s *Store) ListEnabledFeeds(ctx context.Context) ([]Feed, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`
		SELECT telegram_user_id, ical_url, last_synced_at
		FROM canvaslink_feeds
		WHERE sync_enabled = TRUE
		`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var feeds []Feed
	for rows.Next() {
		var feed Feed
		if err := rows.Scan(&feed.UserID, &feed.ICalURL, &feed.LastSyncedAt); err != nil {
			return nil, err
		}
		feed.ICalURL, err = s.decodeSensitive(feed.ICalURL, feedURLPurpose(feed.UserID))
		if err != nil {
			return nil, fmt.Errorf("decrypt feed for user %d: %w", feed.UserID, err)
		}
		feeds = append(feeds, feed)
	}
	return feeds, rows.Err()
}

func (s *Store) TouchFeedSync(ctx context.Context, userID int64) error {
	result, err := s.db.ExecContext(
		ctx,
		`
		UPDATE canvaslink_feeds
		SET last_synced_at = NOW(), updated_at = NOW()
		WHERE telegram_user_id = $1
		`,
		userID,
	)
	return requireAffected(result, err)
}

// GetFeed returns the feed configuration for a user, including last_synced_at.
func (s *Store) GetFeed(ctx context.Context, userID int64) (*Feed, error) {
	var feed Feed
	err := s.db.QueryRowContext(
		ctx,
		`
		SELECT telegram_user_id, ical_url, last_synced_at
		FROM canvaslink_feeds
		WHERE telegram_user_id = $1
		`,
		userID,
	).Scan(&feed.UserID, &feed.ICalURL, &feed.LastSyncedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	feed.ICalURL, err = s.decodeSensitive(feed.ICalURL, feedURLPurpose(userID))
	if err != nil {
		return nil, fmt.Errorf("decrypt feed for user %d: %w", userID, err)
	}
	return &feed, nil
}

// UpdateSyncInterval stores the sync interval preference for a user.
func (s *Store) UpdateSyncInterval(ctx context.Context, userID int64, interval string) error {
	duration, err := time.ParseDuration(strings.TrimSpace(interval))
	if err != nil || duration <= 0 {
		return fmt.Errorf("invalid sync interval %q", interval)
	}

	result, err := s.db.ExecContext(
		ctx,
		`
		UPDATE canvaslink_feeds
		SET sync_interval = $2, updated_at = NOW()
		WHERE telegram_user_id = $1
		`,
		userID, interval,
	)
	return requireAffected(result, err)
}

// GetSyncInterval returns the sync interval preference for a user, defaulting to "1h".
func (s *Store) GetSyncInterval(ctx context.Context, userID int64) (string, error) {
	var interval *string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT sync_interval FROM canvaslink_feeds WHERE telegram_user_id = $1`,
		userID,
	).Scan(&interval)
	if errors.Is(err, sql.ErrNoRows) {
		return "1h", nil
	}
	if err != nil {
		return "1h", err
	}
	if interval == nil || *interval == "" {
		return "1h", nil
	}
	return *interval, nil
}

// HasFeed checks if a user has a feed configured.
func (s *Store) HasFeed(ctx context.Context, userID int64) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM canvaslink_feeds WHERE telegram_user_id = $1)`,
		userID,
	).Scan(&exists)
	return exists, err
}

// TryFeedSyncLock obtains a session-level PostgreSQL advisory lock on a
// dedicated connection. It prevents multiple bot instances from diffing and
// enqueueing the same feed concurrently. Call release on every acquired lock.
func (s *Store) TryFeedSyncLock(ctx context.Context, userID int64) (release func() error, acquired bool, err error) {
	lockPool := s.lockDB
	if lockPool == nil {
		lockPool = s.db
	}
	if lockPool == nil {
		return nil, false, errors.New("feed lock database is not configured")
	}
	conn, err := lockPool.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("open feed lock connection: %w", err)
	}

	const feedLockExpression = `hashtextextended('canvaslink:feed-sync:' || $1::bigint::text, 0)`
	if err := conn.QueryRowContext(
		ctx,
		`SELECT pg_try_advisory_lock(`+feedLockExpression+`)`,
		userID,
	).Scan(&acquired); err != nil {
		closeErr := closeAdvisoryLockConnection(conn, true)
		return nil, false, errors.Join(
			fmt.Errorf("acquire feed sync lock: %w", err),
			closeErr,
		)
	}
	if !acquired {
		if err := conn.Close(); err != nil {
			return nil, false, fmt.Errorf("close unacquired feed lock connection: %w", err)
		}
		return nil, false, nil
	}

	release = makeAdvisoryLockRelease(
		func() (bool, error) {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var unlocked bool
			err := conn.QueryRowContext(
				unlockCtx,
				`SELECT pg_advisory_unlock(`+feedLockExpression+`)`,
				userID,
			).Scan(&unlocked)
			return unlocked, err
		},
		func(discard bool) error {
			return closeAdvisoryLockConnection(conn, discard)
		},
	)
	return release, true, nil
}

func makeAdvisoryLockRelease(
	unlock func() (bool, error),
	closeConnection func(discard bool) error,
) func() error {
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			unlocked, err := unlock()
			discard := err != nil || !unlocked
			switch {
			case err != nil:
				releaseErr = fmt.Errorf("release feed sync lock: %w", err)
			case !unlocked:
				releaseErr = errors.New("release feed sync lock: database reported that the lock was not held")
			}
			if err := closeConnection(discard); err != nil {
				releaseErr = errors.Join(
					releaseErr,
					fmt.Errorf("close feed lock connection: %w", err),
				)
			}
		})
		return releaseErr
	}
}

// closeAdvisoryLockConnection must discard a session whenever lock ownership
// is uncertain. Returning driver.ErrBadConn from Raw tells database/sql not to
// put that physical connection (and any session-level lock) back in the pool.
func closeAdvisoryLockConnection(conn *sql.Conn, discard bool) error {
	if !discard {
		return conn.Close()
	}
	rawErr := conn.Raw(func(any) error {
		return driver.ErrBadConn
	})
	closeErr := conn.Close()
	if errors.Is(rawErr, driver.ErrBadConn) || errors.Is(rawErr, sql.ErrConnDone) {
		rawErr = nil
	}
	if errors.Is(closeErr, sql.ErrConnDone) {
		closeErr = nil
	}
	return errors.Join(rawErr, closeErr)
}

// DisconnectCanvas atomically removes the feed and queued work. Synced-event
// ownership records are intentionally retained so a later wipe can safely
// remove CanvasLink-created Google events and reconnecting cannot duplicate
// them. The Telegram account and Google token are also retained.
func (s *Store) DisconnectCanvas(ctx context.Context, userID int64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		queries := []string{
			`DELETE FROM canvaslink_course_type_settings WHERE telegram_user_id = $1`,
			`DELETE FROM canvaslink_pending_actions WHERE telegram_user_id = $1`,
			`UPDATE canvaslink_calendar_jobs
			 SET status = 'cancelled',
			     lease_owner = '',
			     lease_until = NULL,
			     last_error = 'Canvas disconnected',
			     updated_at = NOW()
			 WHERE telegram_user_id = $1 AND status IN ('pending', 'processing', 'failed')`,
			`UPDATE canvaslink_synced_events
			 SET missing_since = NULL,
			     missing_count = 0,
			     detached = TRUE,
			     removal_eligible = FALSE,
			     updated_at = NOW()
			 WHERE telegram_user_id = $1`,
			`DELETE FROM canvaslink_feeds WHERE telegram_user_id = $1`,
			`UPDATE canvaslink_telegram_accounts
			 SET onboarding_status = '',
			     onboarding_setting_id = NULL,
			     state_revision = state_revision + 1,
			     updated_at = NOW()
			 WHERE telegram_user_id = $1`,
		}
		for _, query := range queries {
			if _, err := tx.ExecContext(ctx, query, userID); err != nil {
				return err
			}
		}
		return nil
	})
}

// DisconnectGoogle atomically invalidates outstanding OAuth states and removes
// the stored Google token. It does not revoke the grant at Google.
func (s *Store) DisconnectGoogle(ctx context.Context, userID int64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM canvaslink_oauth_states WHERE telegram_user_id = $1`, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM canvaslink_google_tokens WHERE telegram_user_id = $1`, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE canvaslink_calendar_jobs
			 SET status = 'cancelled',
			     lease_owner = '',
			     lease_until = NULL,
			     last_error = 'Google Calendar disconnected',
			     updated_at = NOW()
			 WHERE telegram_user_id = $1 AND status IN ('pending', 'processing', 'failed')`,
			userID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE canvaslink_telegram_accounts
			 SET state_revision = state_revision + 1, updated_at = NOW()
			 WHERE telegram_user_id = $1`,
			userID,
		); err != nil {
			return err
		}
		return nil
	})
}

// ResetUser atomically removes all CanvasLink data for a Telegram user.
func (s *Store) ResetUser(ctx context.Context, userID int64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		queries := []string{
			`DELETE FROM canvaslink_course_type_settings WHERE telegram_user_id = $1`,
			`DELETE FROM canvaslink_synced_events WHERE telegram_user_id = $1`,
			`DELETE FROM canvaslink_pending_actions WHERE telegram_user_id = $1`,
			`DELETE FROM canvaslink_calendar_jobs WHERE telegram_user_id = $1`,
			`DELETE FROM canvaslink_feeds WHERE telegram_user_id = $1`,
			`DELETE FROM canvaslink_oauth_states WHERE telegram_user_id = $1`,
			`DELETE FROM canvaslink_google_tokens WHERE telegram_user_id = $1`,
			`DELETE FROM canvaslink_destructive_confirmations WHERE telegram_user_id = $1`,
			`DELETE FROM canvaslink_telegram_accounts WHERE telegram_user_id = $1`,
		}
		for _, query := range queries {
			if _, err := tx.ExecContext(ctx, query, userID); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListUserCourseIDs returns distinct course IDs for a user.
func (s *Store) ListUserCourseIDs(ctx context.Context, userID int64) ([]string, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT DISTINCT course_id FROM canvaslink_course_type_settings WHERE telegram_user_id = $1 ORDER BY course_id`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListCourseTypes returns distinct assignment types for a given course and user.
func (s *Store) ListCourseTypes(ctx context.Context, userID int64, courseID string) ([]string, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT assignment_type FROM canvaslink_course_type_settings WHERE telegram_user_id = $1 AND course_id = $2 ORDER BY assignment_type`,
		userID,
		courseID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var types []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		types = append(types, t)
	}
	return types, rows.Err()
}

// --- Telegram Accounts ---

func (s *Store) UpsertTelegramAccount(ctx context.Context, telegramUserID int64, chatID int64, username string) error {
	return s.UpsertTelegramAccountWithTimezone(ctx, telegramUserID, chatID, username, "UTC")
}

// UpsertTelegramAccountWithTimezone applies defaultTimezone only when the
// account is first created; later messages never overwrite the user's choice.
func (s *Store) UpsertTelegramAccountWithTimezone(ctx context.Context, telegramUserID int64, chatID int64, username, defaultTimezone string) error {
	if telegramUserID <= 0 || chatID != telegramUserID {
		return ErrUnsafeChatDestination
	}
	defaultTimezone = strings.TrimSpace(defaultTimezone)
	if err := ValidateTimezone(defaultTimezone); err != nil {
		return err
	}
	_, err := s.db.ExecContext(
		ctx,
		`
		INSERT INTO canvaslink_telegram_accounts (telegram_user_id, chat_id, username, timezone)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (telegram_user_id)
		DO UPDATE SET
			chat_id = EXCLUDED.chat_id,
			username = EXCLUDED.username,
			updated_at = NOW()
		`,
		telegramUserID,
		chatID,
		username,
		defaultTimezone,
	)
	return err
}

func (s *Store) GetTelegramAccount(ctx context.Context, userID int64) (*TelegramAccount, error) {
	var row TelegramAccount
	err := s.db.QueryRowContext(
		ctx,
		`
		SELECT telegram_user_id, chat_id, username, onboarding_status,
			onboarding_setting_id, timezone, state_revision
		FROM canvaslink_telegram_accounts
		WHERE telegram_user_id = $1
		`,
		userID,
	).Scan(
		&row.TelegramUserID,
		&row.ChatID,
		&row.Username,
		&row.OnboardingStatus,
		&row.OnboardingSettingID,
		&row.Timezone,
		&row.StateRevision,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if row.TelegramUserID <= 0 || row.ChatID != row.TelegramUserID {
		return nil, ErrUnsafeChatDestination
	}
	return &row, nil
}

func (s *Store) SetOnboardingStatus(ctx context.Context, userID int64, status string) error {
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE canvaslink_telegram_accounts
		 SET onboarding_status = $2,
		     onboarding_setting_id = CASE WHEN $2 = 'course_setup' THEN onboarding_setting_id ELSE NULL END,
		     state_revision = state_revision + 1,
		     updated_at = NOW()
		 WHERE telegram_user_id = $1`,
		userID,
		status,
	)
	return requireAffected(result, err)
}

// SetUserTimezone stores an IANA timezone name after validating that Go can
// load it. "UTC" is always accepted.
func (s *Store) SetUserTimezone(ctx context.Context, userID int64, timezone string) error {
	timezone = strings.TrimSpace(timezone)
	if err := ValidateTimezone(timezone); err != nil {
		return err
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE canvaslink_telegram_accounts
		 SET timezone = $2,
		     state_revision = state_revision + 1,
		     updated_at = NOW()
		 WHERE telegram_user_id = $1`,
		userID,
		timezone,
	)
	return requireAffected(result, err)
}

// GetUserTimezone returns UTC for an existing account with an empty legacy
// value and for callers that have not chosen a timezone yet.
func (s *Store) GetUserTimezone(ctx context.Context, userID int64) (string, error) {
	var timezone string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT timezone FROM canvaslink_telegram_accounts WHERE telegram_user_id = $1`,
		userID,
	).Scan(&timezone)
	if errors.Is(err, sql.ErrNoRows) {
		return "UTC", nil
	}
	if err != nil {
		return "", err
	}
	timezone = strings.TrimSpace(timezone)
	if timezone == "" {
		return "UTC", nil
	}
	if err := ValidateTimezone(timezone); err != nil {
		return "", fmt.Errorf("stored timezone: %w", err)
	}
	return timezone, nil
}

// BeginOnboardingCourseSetup selects and persists the first setting row in the
// deterministic onboarding order. It returns nil when the user has no settings.
func (s *Store) BeginOnboardingCourseSetup(ctx context.Context, userID int64) (*CourseSetting, error) {
	var first *CourseSetting
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := lockTelegramAccount(ctx, tx, userID); err != nil {
			return err
		}

		setting, err := firstCourseSetting(ctx, tx, userID)
		if err != nil {
			return err
		}
		first = setting

		var settingID any
		status := ""
		if setting != nil {
			settingID = setting.ID
			status = OnboardingStatusCourseSetup
		}
		result, err := tx.ExecContext(
			ctx,
			`UPDATE canvaslink_telegram_accounts
			 SET onboarding_status = $2,
			     onboarding_setting_id = $3,
			     state_revision = state_revision + 1,
			     updated_at = NOW()
			 WHERE telegram_user_id = $1`,
			userID,
			status,
			settingID,
		)
		return requireAffected(result, err)
	})
	return first, err
}

// AdvanceOnboardingCourseSetup atomically verifies the button refers to the
// currently persisted setting and advances to the next setting. A stale or
// replayed button returns ErrStaleOnboardingStep without changing progress.
func (s *Store) AdvanceOnboardingCourseSetup(ctx context.Context, userID, expectedSettingID int64) (*CourseSetting, bool, error) {
	return s.advanceOnboardingCourseSetup(ctx, userID, expectedSettingID, nil)
}

// SetModeAndAdvanceOnboardingCourseSetup updates the current setting and
// advances progress in one transaction, closing the replay window between two
// separate calls in multi-instance deployments.
func (s *Store) SetModeAndAdvanceOnboardingCourseSetup(ctx context.Context, userID, expectedSettingID int64, mode string) (*CourseSetting, bool, error) {
	if !IsValidMode(mode) {
		return nil, false, fmt.Errorf("%w: %q", ErrInvalidMode, mode)
	}
	return s.advanceOnboardingCourseSetup(ctx, userID, expectedSettingID, &mode)
}

func (s *Store) advanceOnboardingCourseSetup(ctx context.Context, userID, expectedSettingID int64, mode *string) (*CourseSetting, bool, error) {
	var next *CourseSetting
	var completed bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var status string
		var current sql.NullInt64
		err := tx.QueryRowContext(
			ctx,
			`SELECT onboarding_status, onboarding_setting_id
			 FROM canvaslink_telegram_accounts
			 WHERE telegram_user_id = $1
			 FOR UPDATE`,
			userID,
		).Scan(&status, &current)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != OnboardingStatusCourseSetup || !current.Valid || current.Int64 != expectedSettingID {
			return ErrStaleOnboardingStep
		}

		currentSetting, err := getCourseSettingByID(ctx, tx, userID, expectedSettingID)
		if err != nil {
			return err
		}
		if currentSetting == nil {
			return ErrStaleOnboardingStep
		}
		if mode != nil {
			result, err := tx.ExecContext(
				ctx,
				`UPDATE canvaslink_course_type_settings
				 SET mode = $3, updated_at = NOW()
				 WHERE telegram_user_id = $1 AND id = $2`,
				userID,
				expectedSettingID,
				*mode,
			)
			if err := requireAffected(result, err); err != nil {
				return ErrStaleOnboardingStep
			}
			currentSetting.Mode = *mode
		}
		next, err = nextCourseSetting(ctx, tx, userID, *currentSetting)
		if err != nil {
			return err
		}

		var nextID any
		nextStatus := OnboardingStatusCourseSetup
		if next == nil {
			nextStatus = ""
			completed = true
		} else {
			nextID = next.ID
		}
		result, err := tx.ExecContext(
			ctx,
			`UPDATE canvaslink_telegram_accounts
			 SET onboarding_status = $2,
			     onboarding_setting_id = $3,
			     state_revision = state_revision + 1,
			     updated_at = NOW()
			 WHERE telegram_user_id = $1`,
			userID,
			nextStatus,
			nextID,
		)
		return requireAffected(result, err)
	})
	return next, completed, err
}

// ClearOnboardingCourseSetup clears progress only if expectedSettingID is still
// the active setting, preventing an old button from cancelling a newer flow.
func (s *Store) ClearOnboardingCourseSetup(ctx context.Context, userID, expectedSettingID int64) error {
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE canvaslink_telegram_accounts
		 SET onboarding_status = '',
		     onboarding_setting_id = NULL,
		     state_revision = state_revision + 1,
		     updated_at = NOW()
		 WHERE telegram_user_id = $1
		   AND onboarding_status = 'course_setup'
		   AND onboarding_setting_id = $2`,
		userID,
		expectedSettingID,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrStaleOnboardingStep
	}
	return nil
}

// CreateDestructiveConfirmation creates a short-lived, single-use nonce bound
// to the user's current state revision. A later feed/account change makes the
// confirmation invalid even if an old Telegram message is clicked.
func (s *Store) CreateDestructiveConfirmation(ctx context.Context, userID int64, action string, ttl time.Duration) (string, error) {
	action = strings.TrimSpace(action)
	if !destructiveActionPattern.MatchString(action) {
		return "", fmt.Errorf("invalid destructive confirmation action %q", action)
	}
	if ttl <= 0 || ttl > time.Hour {
		return "", fmt.Errorf("destructive confirmation TTL must be between 1ns and 1h")
	}

	nonceBytes := make([]byte, 18)
	if _, err := io.ReadFull(rand.Reader, nonceBytes); err != nil {
		return "", fmt.Errorf("generate confirmation nonce: %w", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var revision int64
		err := tx.QueryRowContext(
			ctx,
			`SELECT state_revision
			 FROM canvaslink_telegram_accounts
			 WHERE telegram_user_id = $1
			 FOR UPDATE`,
			userID,
		).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`DELETE FROM canvaslink_destructive_confirmations
			 WHERE expires_at <= NOW()
			    OR (telegram_user_id = $1 AND action = $2)`,
			userID,
			action,
		); err != nil {
			return err
		}
		_, err = tx.ExecContext(
			ctx,
			`INSERT INTO canvaslink_destructive_confirmations
				(nonce, telegram_user_id, action, state_revision, expires_at)
			 VALUES ($1, $2, $3, $4, NOW() + ($5 * INTERVAL '1 millisecond'))`,
			nonce,
			userID,
			action,
			revision,
			max(ttl.Milliseconds(), 1),
		)
		return err
	})
	if err != nil {
		return "", err
	}
	return nonce, nil
}

// ConsumeDestructiveConfirmation validates and removes a nonce atomically.
func (s *Store) ConsumeDestructiveConfirmation(ctx context.Context, userID int64, action, nonce string) (bool, error) {
	action = strings.TrimSpace(action)
	nonce = strings.TrimSpace(nonce)
	if !destructiveActionPattern.MatchString(action) || nonce == "" {
		return false, nil
	}
	var consumed bool
	err := s.db.QueryRowContext(
		ctx,
		`WITH consumed AS (
			DELETE FROM canvaslink_destructive_confirmations AS confirmations
			USING canvaslink_telegram_accounts AS accounts
			WHERE confirmations.nonce = $1
			  AND confirmations.telegram_user_id = $2
			  AND confirmations.action = $3
			  AND confirmations.expires_at > NOW()
			  AND accounts.telegram_user_id = confirmations.telegram_user_id
			  AND accounts.state_revision = confirmations.state_revision
			RETURNING 1
		 )
		 SELECT EXISTS(SELECT 1 FROM consumed)`,
		nonce,
		userID,
		action,
	).Scan(&consumed)
	return consumed, err
}

// --- Course Settings ---

func (s *Store) SeedDefaultSettings(ctx context.Context, userID int64, rows []CourseTypeSeed) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin settings seed: %w", err)
	}
	defer tx.Rollback()

	if err := seedDefaultSettings(ctx, tx, userID, rows); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit settings seed: %w", err)
	}
	return nil
}

type contextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func seedDefaultSettings(ctx context.Context, execer contextExecer, userID int64, rows []CourseTypeSeed) error {
	for _, row := range rows {
		courseID := strings.TrimSpace(row.CourseID)
		if courseID == "" {
			continue
		}
		courseName := strings.TrimSpace(row.CourseName)
		if courseName == "" {
			courseName = courseID
		}
		assignmentType := strings.TrimSpace(row.AssignmentType)
		if assignmentType == "" {
			continue
		}
		_, err := execer.ExecContext(
			ctx,
			`
			INSERT INTO canvaslink_course_type_settings (telegram_user_id, course_id, course_name, assignment_type, mode)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (telegram_user_id, course_id, assignment_type)
			DO UPDATE SET
				course_name = EXCLUDED.course_name,
				updated_at = NOW()
			`,
			userID,
			courseID,
			courseName,
			assignmentType,
			ModeReview,
		)
		if err != nil {
			return fmt.Errorf("seed settings for course %q: %w", courseID, err)
		}
	}
	return nil
}

func (s *Store) ListUserCourses(ctx context.Context, userID int64) ([]Course, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`
		SELECT MIN(id), course_id,
			COALESCE(NULLIF(MAX(course_name), ''), course_id)
		FROM canvaslink_course_type_settings
		WHERE telegram_user_id = $1
		GROUP BY course_id
		ORDER BY COALESCE(NULLIF(MAX(course_name), ''), course_id) ASC, course_id ASC
		`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var courses []Course
	for rows.Next() {
		var c Course
		if err := rows.Scan(&c.ID, &c.CourseID, &c.CourseName); err != nil {
			return nil, err
		}
		if strings.TrimSpace(c.CourseName) == "" {
			c.CourseName = c.CourseID
		}
		courses = append(courses, c)
	}
	return courses, rows.Err()
}

func (s *Store) ListCourseSettings(ctx context.Context, userID int64, courseID string) ([]CourseSetting, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`
		SELECT id, course_id, course_name, assignment_type, mode
		FROM canvaslink_course_type_settings
		WHERE telegram_user_id = $1 AND course_id = $2
		ORDER BY assignment_type ASC
		`,
		userID,
		courseID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var settings []CourseSetting
	for rows.Next() {
		var r CourseSetting
		if err := rows.Scan(&r.ID, &r.CourseID, &r.CourseName, &r.AssignmentType, &r.Mode); err != nil {
			return nil, err
		}
		settings = append(settings, r)
	}
	return settings, rows.Err()
}

// ListAllCourseSettings returns all owned settings in the same deterministic
// order used by persistent onboarding.
func (s *Store) ListAllCourseSettings(ctx context.Context, userID int64) ([]CourseSetting, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, course_id, course_name, assignment_type, mode
		 FROM canvaslink_course_type_settings
		 WHERE telegram_user_id = $1
		 ORDER BY COALESCE(NULLIF(course_name, ''), course_id),
		          course_id, assignment_type, id`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var settings []CourseSetting
	for rows.Next() {
		var setting CourseSetting
		if err := rows.Scan(
			&setting.ID,
			&setting.CourseID,
			&setting.CourseName,
			&setting.AssignmentType,
			&setting.Mode,
		); err != nil {
			return nil, err
		}
		if strings.TrimSpace(setting.CourseName) == "" {
			setting.CourseName = setting.CourseID
		}
		settings = append(settings, setting)
	}
	return settings, rows.Err()
}

// GetCourseBySettingID resolves the course represented by a short setting ID
// and verifies that the row belongs to userID.
func (s *Store) GetCourseBySettingID(ctx context.Context, userID, settingID int64) (*Course, error) {
	var course Course
	err := s.db.QueryRowContext(
		ctx,
		`SELECT id, course_id, course_name
		 FROM canvaslink_course_type_settings
		 WHERE telegram_user_id = $1 AND id = $2`,
		userID,
		settingID,
	).Scan(&course.ID, &course.CourseID, &course.CourseName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(course.CourseName) == "" {
		course.CourseName = course.CourseID
	}
	return &course, nil
}

// GetCourseSettingByID resolves an owned course/type setting by its short ID.
func (s *Store) GetCourseSettingByID(ctx context.Context, userID, settingID int64) (*CourseSetting, error) {
	return getCourseSettingByID(ctx, s.db, userID, settingID)
}

// ListCourseSettingsByCourseSettingID lists a course's settings after resolving
// and ownership-checking a short representative setting ID.
func (s *Store) ListCourseSettingsByCourseSettingID(ctx context.Context, userID, settingID int64) ([]CourseSetting, error) {
	course, err := s.GetCourseBySettingID(ctx, userID, settingID)
	if err != nil || course == nil {
		return nil, err
	}
	return s.ListCourseSettings(ctx, userID, course.CourseID)
}

// SetCourseTypeModeByID changes an owned setting using compact callback data.
func (s *Store) SetCourseTypeModeByID(ctx context.Context, userID, settingID int64, mode string) error {
	if !IsValidMode(mode) {
		return fmt.Errorf("%w: %q", ErrInvalidMode, mode)
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE canvaslink_course_type_settings
		 SET mode = $3, updated_at = NOW()
		 WHERE telegram_user_id = $1 AND id = $2`,
		userID,
		settingID,
		mode,
	)
	return requireAffected(result, err)
}

type contextQueryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getCourseSettingByID(ctx context.Context, queryer contextQueryRower, userID, settingID int64) (*CourseSetting, error) {
	var setting CourseSetting
	err := queryer.QueryRowContext(
		ctx,
		`SELECT id, course_id, course_name, assignment_type, mode
		 FROM canvaslink_course_type_settings
		 WHERE telegram_user_id = $1 AND id = $2`,
		userID,
		settingID,
	).Scan(
		&setting.ID,
		&setting.CourseID,
		&setting.CourseName,
		&setting.AssignmentType,
		&setting.Mode,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(setting.CourseName) == "" {
		setting.CourseName = setting.CourseID
	}
	return &setting, nil
}

func firstCourseSetting(ctx context.Context, queryer contextQueryRower, userID int64) (*CourseSetting, error) {
	var setting CourseSetting
	err := queryer.QueryRowContext(
		ctx,
		`SELECT id, course_id, course_name, assignment_type, mode
		 FROM canvaslink_course_type_settings
		 WHERE telegram_user_id = $1
		 ORDER BY COALESCE(NULLIF(course_name, ''), course_id), course_id, assignment_type, id
		 LIMIT 1`,
		userID,
	).Scan(
		&setting.ID,
		&setting.CourseID,
		&setting.CourseName,
		&setting.AssignmentType,
		&setting.Mode,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(setting.CourseName) == "" {
		setting.CourseName = setting.CourseID
	}
	return &setting, nil
}

func nextCourseSetting(ctx context.Context, queryer contextQueryRower, userID int64, current CourseSetting) (*CourseSetting, error) {
	var setting CourseSetting
	err := queryer.QueryRowContext(
		ctx,
		`SELECT id, course_id, course_name, assignment_type, mode
		 FROM canvaslink_course_type_settings
		 WHERE telegram_user_id = $1
		   AND (COALESCE(NULLIF(course_name, ''), course_id), course_id, assignment_type, id) > ($2, $3, $4, $5)
		 ORDER BY COALESCE(NULLIF(course_name, ''), course_id), course_id, assignment_type, id
		 LIMIT 1`,
		userID,
		current.CourseName,
		current.CourseID,
		current.AssignmentType,
		current.ID,
	).Scan(
		&setting.ID,
		&setting.CourseID,
		&setting.CourseName,
		&setting.AssignmentType,
		&setting.Mode,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(setting.CourseName) == "" {
		setting.CourseName = setting.CourseID
	}
	return &setting, nil
}

func lockTelegramAccount(ctx context.Context, tx *sql.Tx, userID int64) error {
	var found int
	err := tx.QueryRowContext(
		ctx,
		`SELECT 1 FROM canvaslink_telegram_accounts
		 WHERE telegram_user_id = $1
		 FOR UPDATE`,
		userID,
	).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) SetCourseTypeMode(ctx context.Context, userID int64, courseID, assignmentType, mode string) error {
	if !IsValidMode(mode) {
		return fmt.Errorf("%w: %q", ErrInvalidMode, mode)
	}
	result, err := s.db.ExecContext(
		ctx,
		`
		UPDATE canvaslink_course_type_settings
		SET mode = $4, updated_at = NOW()
		WHERE telegram_user_id = $1
			AND course_id = $2
			AND assignment_type = $3
		`,
		userID,
		courseID,
		assignmentType,
		mode,
	)
	return requireAffected(result, err)
}

func (s *Store) GetCourseTypeMode(ctx context.Context, userID int64, courseID, assignmentType string) (string, error) {
	var mode string
	err := s.db.QueryRowContext(
		ctx,
		`
		SELECT mode
		FROM canvaslink_course_type_settings
		WHERE telegram_user_id = $1
			AND course_id = $2
			AND assignment_type = $3
		`,
		userID,
		courseID,
		assignmentType,
	).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return ModeReview, nil
	}
	if err != nil {
		return "", err
	}
	return CanonicalMode(mode), nil
}

// --- Synced Events ---

// SyncedEvent represents a row from canvaslink_synced_events.
type SyncedEvent struct {
	ID               int64
	TelegramUserID   int64
	CourseID         string
	AssignmentType   string
	CanvasEventUID   string
	CanvasTitle      string
	CanvasDueAt      time.Time
	AllDay           bool
	GoogleCalendarID string
	GoogleEventID    string
	CanvasDTStamp    *time.Time
	CanvasSequence   int
	CanvasStatus     string
	CanvasCancelled  bool
	MissingSince     *time.Time
	MissingCount     int
	Detached         bool
	RemovalEligible  bool
	GoogleConfirmed  bool
	SyncedAt         time.Time
}

func (s *Store) UpsertSyncedEvent(ctx context.Context, input SyncedEventInput) (bool, error) {
	var inserted bool
	err := s.db.QueryRowContext(
		ctx,
		`
		WITH inserted AS (
			INSERT INTO canvaslink_synced_events (
				telegram_user_id,
				course_id,
				assignment_type,
				canvas_event_uid,
				canvas_title,
				canvas_due_at,
				all_day,
				google_calendar_id,
				google_event_id,
				canvas_dtstamp,
				canvas_sequence,
				canvas_status,
				canvas_cancelled,
				detached,
				removal_eligible,
				google_confirmed
			)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,FALSE,$14,$15)
			ON CONFLICT (telegram_user_id, canvas_event_uid) DO NOTHING
			RETURNING 1
		)
		SELECT EXISTS(SELECT 1 FROM inserted)
		`,
		input.TelegramUserID,
		input.CourseID,
		input.AssignmentType,
		input.CanvasEventUID,
		input.CanvasTitle,
		input.CanvasDueAt,
		input.AllDay,
		defaultString(input.GoogleCalendarID, "primary"),
		input.GoogleEventID,
		input.CanvasDTStamp,
		input.CanvasSequence,
		input.CanvasStatus,
		input.CanvasCancelled,
		input.RemovalEligible,
		input.GoogleConfirmed,
	).Scan(&inserted)
	return inserted, err
}

// UpdateSyncedEvent updates an existing synced event's metadata (title, due date, dtstamp, sequence).
// Returns true if a row was updated.
func (s *Store) UpdateSyncedEvent(ctx context.Context, input SyncedEventInput) (bool, error) {
	result, err := s.db.ExecContext(
		ctx,
		`
		UPDATE canvaslink_synced_events
		SET
			course_id = $3,
			assignment_type = $4,
			canvas_title = $5,
			canvas_due_at = $6,
			all_day = $7,
			canvas_dtstamp = $8,
			canvas_sequence = $9,
			canvas_status = $10,
			canvas_cancelled = $11,
			detached = FALSE,
			removal_eligible = $12,
			google_confirmed = $13,
			missing_since = NULL,
			missing_count = 0,
			updated_at = NOW()
		WHERE telegram_user_id = $1 AND canvas_event_uid = $2
		`,
		input.TelegramUserID,
		input.CanvasEventUID,
		input.CourseID,
		input.AssignmentType,
		input.CanvasTitle,
		input.CanvasDueAt,
		input.AllDay,
		input.CanvasDTStamp,
		input.CanvasSequence,
		input.CanvasStatus,
		input.CanvasCancelled,
		input.RemovalEligible,
		input.GoogleConfirmed,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// GetSyncedEvent returns a tracked Canvas event for a user.
func (s *Store) GetSyncedEvent(ctx context.Context, userID int64, canvasEventUID string) (*SyncedEvent, error) {
	var event SyncedEvent
	row := s.db.QueryRowContext(
		ctx,
		`
		SELECT id, telegram_user_id, course_id, assignment_type, canvas_event_uid,
			canvas_title, canvas_due_at, all_day, google_calendar_id, google_event_id,
			canvas_dtstamp, canvas_sequence, canvas_status, canvas_cancelled,
			missing_since, missing_count, detached, removal_eligible,
			google_confirmed, synced_at
		FROM canvaslink_synced_events
		WHERE telegram_user_id = $1 AND canvas_event_uid = $2
		`,
		userID,
		canvasEventUID,
	)
	err := scanSyncedEvent(row, &event)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &event, nil
}

// MarkSyncedEventMissing records one complete successful feed observation in
// which a previously synced event was absent. Callers can use MissingCount and
// MissingSince to apply a conservative deletion grace period.
func (s *Store) MarkSyncedEventMissing(ctx context.Context, userID int64, canvasEventUID string) (*SyncedEvent, error) {
	var event SyncedEvent
	row := s.db.QueryRowContext(
		ctx,
		`UPDATE canvaslink_synced_events
		 SET missing_since = COALESCE(missing_since, NOW()),
		     missing_count = missing_count + 1,
		     updated_at = NOW()
		 WHERE telegram_user_id = $1
		   AND canvas_event_uid = $2
		   AND detached = FALSE
		 RETURNING id, telegram_user_id, course_id, assignment_type, canvas_event_uid,
			canvas_title, canvas_due_at, all_day, google_calendar_id, google_event_id,
			canvas_dtstamp, canvas_sequence, canvas_status, canvas_cancelled,
			missing_since, missing_count, detached, removal_eligible,
			google_confirmed, synced_at`,
		userID,
		canvasEventUID,
	)
	err := scanSyncedEvent(row, &event)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &event, nil
}

// ObserveSyncedEvent records conservative metadata for every recognized
// occurrence of an already tracked UID, including past or temporarily
// unclassifiable events. This prevents stale future dates from authorizing a
// later deletion and reactivates only ownership rows actually seen after a
// Canvas disconnect.
func (s *Store) ObserveSyncedEvent(ctx context.Context, observation SyncedEventObservation) error {
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE canvaslink_synced_events
		 SET canvas_due_at = COALESCE($3, canvas_due_at),
		     all_day = CASE WHEN $3 IS NULL THEN all_day ELSE $4 END,
		     canvas_status = CASE WHEN $6 THEN $5 ELSE canvas_status END,
		     canvas_cancelled = $6,
		     missing_since = NULL,
		     missing_count = 0,
		     detached = FALSE,
		     removal_eligible = $7,
		     updated_at = NOW()
		 WHERE telegram_user_id = $1 AND canvas_event_uid = $2`,
		observation.TelegramUserID,
		observation.CanvasEventUID,
		observation.DueAt,
		observation.AllDay,
		observation.CanvasStatus,
		observation.CanvasCancelled,
		observation.RemovalEligible,
	)
	return requireAffected(result, err)
}

// ReactivateSyncedEventPresence clears a prior missing/detached state without
// claiming that Google already reflects newly observed Canvas metadata.
func (s *Store) ReactivateSyncedEventPresence(ctx context.Context, userID int64, canvasEventUID string) error {
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE canvaslink_synced_events
		 SET missing_since = NULL,
		     missing_count = 0,
		     detached = FALSE,
		     removal_eligible = TRUE,
		     canvas_cancelled = FALSE,
		     updated_at = NOW()
		 WHERE telegram_user_id = $1 AND canvas_event_uid = $2`,
		userID,
		canvasEventUID,
	)
	return requireAffected(result, err)
}

// ObserveActionableSyncedEvent reactivates a present future source. If Google
// has not yet confirmed the staged state, it also replaces that staged
// metadata with the newest complete Canvas observation. This independently
// makes an older leased job fail exact-state preflight even if explicit job
// cancellation is interrupted.
func (s *Store) ObserveActionableSyncedEvent(ctx context.Context, input SyncedEventInput) error {
	if input.TelegramUserID <= 0 ||
		strings.TrimSpace(input.CanvasEventUID) == "" ||
		input.CanvasDueAt.IsZero() ||
		input.CanvasCancelled ||
		!input.RemovalEligible {
		return fmt.Errorf("%w: invalid actionable source observation", ErrInvalidCalendarJob)
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE canvaslink_synced_events
		 SET course_id = CASE WHEN google_confirmed THEN course_id ELSE $3 END,
		     assignment_type = CASE WHEN google_confirmed THEN assignment_type ELSE $4 END,
		     canvas_title = CASE WHEN google_confirmed THEN canvas_title ELSE $5 END,
		     canvas_due_at = CASE WHEN google_confirmed THEN canvas_due_at ELSE $6 END,
		     all_day = CASE WHEN google_confirmed THEN all_day ELSE $7 END,
		     canvas_dtstamp = CASE WHEN google_confirmed THEN canvas_dtstamp ELSE $8 END,
		     canvas_sequence = CASE WHEN google_confirmed THEN canvas_sequence ELSE $9 END,
		     canvas_status = CASE WHEN google_confirmed THEN canvas_status ELSE $10 END,
		     canvas_cancelled = FALSE,
		     missing_since = NULL,
		     missing_count = 0,
		     detached = FALSE,
		     removal_eligible = TRUE,
		     updated_at = NOW()
		 WHERE telegram_user_id = $1 AND canvas_event_uid = $2`,
		input.TelegramUserID,
		input.CanvasEventUID,
		input.CourseID,
		input.AssignmentType,
		input.CanvasTitle,
		input.CanvasDueAt,
		input.AllDay,
		input.CanvasDTStamp,
		input.CanvasSequence,
		input.CanvasStatus,
	)
	return requireAffected(result, err)
}

type rowScanner interface {
	Scan(...any) error
}

func scanSyncedEvent(scanner rowScanner, event *SyncedEvent) error {
	return scanner.Scan(
		&event.ID,
		&event.TelegramUserID,
		&event.CourseID,
		&event.AssignmentType,
		&event.CanvasEventUID,
		&event.CanvasTitle,
		&event.CanvasDueAt,
		&event.AllDay,
		&event.GoogleCalendarID,
		&event.GoogleEventID,
		&event.CanvasDTStamp,
		&event.CanvasSequence,
		&event.CanvasStatus,
		&event.CanvasCancelled,
		&event.MissingSince,
		&event.MissingCount,
		&event.Detached,
		&event.RemovalEligible,
		&event.GoogleConfirmed,
		&event.SyncedAt,
	)
}

// ListSyncedEventsByCourse returns all synced events for a specific course.
func (s *Store) ListSyncedEventsByCourse(ctx context.Context, userID int64, courseID string) ([]SyncedEvent, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`
		SELECT id, telegram_user_id, course_id, assignment_type, canvas_event_uid,
			canvas_title, canvas_due_at, all_day, google_calendar_id, google_event_id,
			canvas_dtstamp, canvas_sequence, canvas_status, canvas_cancelled,
			missing_since, missing_count, detached, removal_eligible,
			google_confirmed, synced_at
		FROM canvaslink_synced_events
		WHERE telegram_user_id = $1 AND course_id = $2
		`,
		userID, courseID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []SyncedEvent
	for rows.Next() {
		var ev SyncedEvent
		if err := scanSyncedEvent(rows, &ev); err != nil {
			return nil, err
		}
		result = append(result, ev)
	}
	return result, rows.Err()
}

// ListSyncedEvents returns all synced events for a user, keyed by canvas_event_uid.
func (s *Store) ListSyncedEvents(ctx context.Context, userID int64) (map[string]SyncedEvent, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`
		SELECT id, telegram_user_id, course_id, assignment_type, canvas_event_uid,
			canvas_title, canvas_due_at, all_day, google_calendar_id, google_event_id,
			canvas_dtstamp, canvas_sequence, canvas_status, canvas_cancelled,
			missing_since, missing_count, detached, removal_eligible,
			google_confirmed, synced_at
		FROM canvaslink_synced_events
		WHERE telegram_user_id = $1
		`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]SyncedEvent)
	for rows.Next() {
		var ev SyncedEvent
		if err := scanSyncedEvent(rows, &ev); err != nil {
			return nil, err
		}
		result[ev.CanvasEventUID] = ev
	}
	return result, rows.Err()
}

// --- Pending Actions ---

func (s *Store) CreatePendingActionIfAbsent(ctx context.Context, input PendingActionInput) (*PendingAction, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin pending action upsert: %w", err)
	}
	defer tx.Rollback()

	var action PendingAction
	err = tx.QueryRowContext(
		ctx,
		`
		INSERT INTO canvaslink_pending_actions (
			telegram_user_id,
			course_id,
			assignment_type,
			canvas_event_uid,
			canvas_title,
			canvas_due_at,
			all_day,
			telegram_chat_id
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (telegram_user_id, canvas_event_uid) DO NOTHING
		RETURNING id, telegram_user_id, course_id, assignment_type, canvas_event_uid,
			canvas_title, canvas_due_at, all_day, telegram_chat_id, telegram_message_id, status
		`,
		input.TelegramUserID,
		input.CourseID,
		input.AssignmentType,
		input.CanvasEventUID,
		input.CanvasTitle,
		input.CanvasDueAt,
		input.AllDay || input.CanvasAllDay,
		fmt.Sprintf("%d", input.TelegramChatID),
	).Scan(
		&action.ID,
		&action.TelegramUserID,
		&action.CourseID,
		&action.AssignmentType,
		&action.CanvasEventUID,
		&action.CanvasTitle,
		&action.CanvasDueAt,
		&action.CanvasAllDay,
		&action.TelegramChatID,
		&action.TelegramMessageID,
		&action.Status,
	)
	inserted := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}

	if !inserted {
		err = tx.QueryRowContext(
			ctx,
			`
			UPDATE canvaslink_pending_actions
			SET
				course_id = $2,
				assignment_type = $3,
				canvas_title = $5,
				canvas_due_at = $6,
				all_day = $7,
				telegram_chat_id = $8,
				telegram_message_id = CASE
					WHEN status = 'source_removed'
					  OR course_id IS DISTINCT FROM $2
					  OR assignment_type IS DISTINCT FROM $3
					  OR canvas_title IS DISTINCT FROM $5
					  OR canvas_due_at IS DISTINCT FROM $6
					  OR all_day IS DISTINCT FROM $7
					  OR telegram_chat_id IS DISTINCT FROM $8
					THEN NULL
					ELSE telegram_message_id
				END,
				status = CASE
					WHEN status = 'source_removed' THEN 'pending'
					ELSE status
				END,
				updated_at = NOW()
			WHERE telegram_user_id = $1 AND canvas_event_uid = $4
			RETURNING id, telegram_user_id, course_id, assignment_type, canvas_event_uid,
				canvas_title, canvas_due_at, all_day, telegram_chat_id, telegram_message_id, status
			`,
			input.TelegramUserID,
			input.CourseID,
			input.AssignmentType,
			input.CanvasEventUID,
			input.CanvasTitle,
			input.CanvasDueAt,
			input.AllDay || input.CanvasAllDay,
			fmt.Sprintf("%d", input.TelegramChatID),
		).Scan(
			&action.ID,
			&action.TelegramUserID,
			&action.CourseID,
			&action.AssignmentType,
			&action.CanvasEventUID,
			&action.CanvasTitle,
			&action.CanvasDueAt,
			&action.CanvasAllDay,
			&action.TelegramChatID,
			&action.TelegramMessageID,
			&action.Status,
		)
		if err != nil {
			return nil, false, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit pending action upsert: %w", err)
	}
	return &action, inserted, nil
}

func (s *Store) SetPendingMessageID(ctx context.Context, userID int64, pendingID int64, messageID int) error {
	result, err := s.db.ExecContext(
		ctx,
		`
		UPDATE canvaslink_pending_actions
		SET telegram_message_id = $3, updated_at = NOW()
		WHERE telegram_user_id = $1
		  AND id = $2
		  AND status = 'pending'
		  AND telegram_message_id IS NULL
		`,
		userID,
		pendingID,
		messageID,
	)
	return requireAffected(result, err)
}

func (s *Store) GetPendingAction(ctx context.Context, userID int64, pendingID int64) (*PendingAction, error) {
	var action PendingAction
	err := s.db.QueryRowContext(
		ctx,
		`
		SELECT id, telegram_user_id, course_id, assignment_type, canvas_event_uid,
			canvas_title, canvas_due_at, all_day, telegram_chat_id, telegram_message_id, status
		FROM canvaslink_pending_actions
		WHERE telegram_user_id = $1 AND id = $2
		`,
		userID,
		pendingID,
	).Scan(
		&action.ID,
		&action.TelegramUserID,
		&action.CourseID,
		&action.AssignmentType,
		&action.CanvasEventUID,
		&action.CanvasTitle,
		&action.CanvasDueAt,
		&action.CanvasAllDay,
		&action.TelegramChatID,
		&action.TelegramMessageID,
		&action.Status,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &action, nil
}

// GetPendingActionByUID returns the review decision associated with a Canvas
// source. It lets the sync worker distinguish a user-approved durable add from
// an automatic create that was queued before the mode changed to Review.
func (s *Store) GetPendingActionByUID(ctx context.Context, userID int64, sourceUID string) (*PendingAction, error) {
	var action PendingAction
	err := s.db.QueryRowContext(
		ctx,
		`
		SELECT id, telegram_user_id, course_id, assignment_type, canvas_event_uid,
			canvas_title, canvas_due_at, all_day, telegram_chat_id, telegram_message_id, status
		FROM canvaslink_pending_actions
		WHERE telegram_user_id = $1 AND canvas_event_uid = $2
		`,
		userID,
		sourceUID,
	).Scan(
		&action.ID,
		&action.TelegramUserID,
		&action.CourseID,
		&action.AssignmentType,
		&action.CanvasEventUID,
		&action.CanvasTitle,
		&action.CanvasDueAt,
		&action.CanvasAllDay,
		&action.TelegramChatID,
		&action.TelegramMessageID,
		&action.Status,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &action, nil
}

func (s *Store) MarkPendingStatus(ctx context.Context, userID int64, pendingID int64, status string) error {
	if status != PendingStatusAdded && status != PendingStatusIgnored {
		return fmt.Errorf("%w: %q", ErrInvalidPendingStatus, status)
	}

	result, err := s.db.ExecContext(
		ctx,
		`
		UPDATE canvaslink_pending_actions
		SET status = $3, updated_at = NOW()
		WHERE telegram_user_id = $1 AND id = $2 AND status = 'pending'
		`,
		userID,
		pendingID,
		status,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrPendingAlreadyHandled
	}
	return nil
}

// QueuePendingCalendarAdd atomically binds a Review/Active source to its
// deterministic Google target, enqueues the durable create, and consumes the
// user's pending approval. A failure leaves all three records unchanged.
func (s *Store) QueuePendingCalendarAdd(ctx context.Context, input PendingCalendarAddInput) (*CalendarJob, error) {
	input.GoogleCalendarID = defaultString(input.GoogleCalendarID, "primary")
	input.GoogleEventID = strings.TrimSpace(input.GoogleEventID)
	if input.MaxAttempts == 0 {
		input.MaxAttempts = 8
	}
	if input.TelegramUserID <= 0 || input.PendingActionID <= 0 || input.GoogleEventID == "" {
		return nil, fmt.Errorf("%w: invalid pending calendar add", ErrInvalidCalendarJob)
	}

	var queued *CalendarJob
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var pending PendingAction
		var syncedID int64
		var existingEventID string
		var canvasDTStamp *time.Time
		var canvasSequence int
		var canvasStatus string
		var canvasCancelled bool
		var missingSince *time.Time
		var detached bool
		var removalEligible bool
		var googleConfirmed bool
		err := tx.QueryRowContext(
			ctx,
			`SELECT
				pending.id, pending.telegram_user_id, pending.course_id,
				pending.assignment_type, pending.canvas_event_uid,
				pending.canvas_title, pending.canvas_due_at, pending.all_day,
				pending.telegram_chat_id, pending.telegram_message_id,
				pending.status,
				events.id, events.google_event_id, events.canvas_dtstamp,
				events.canvas_sequence, events.canvas_status,
				events.canvas_cancelled, events.missing_since,
				events.detached, events.removal_eligible,
				events.google_confirmed
			 FROM canvaslink_pending_actions AS pending
			 JOIN canvaslink_synced_events AS events
			   ON events.telegram_user_id = pending.telegram_user_id
			  AND events.canvas_event_uid = pending.canvas_event_uid
			  AND events.course_id = pending.course_id
			  AND events.assignment_type = pending.assignment_type
			  AND events.canvas_title = pending.canvas_title
			  AND events.canvas_due_at = pending.canvas_due_at
			  AND events.all_day = pending.all_day
			 WHERE pending.telegram_user_id = $1
			   AND pending.id = $2
			 FOR UPDATE OF pending, events`,
			input.TelegramUserID,
			input.PendingActionID,
		).Scan(
			&pending.ID,
			&pending.TelegramUserID,
			&pending.CourseID,
			&pending.AssignmentType,
			&pending.CanvasEventUID,
			&pending.CanvasTitle,
			&pending.CanvasDueAt,
			&pending.CanvasAllDay,
			&pending.TelegramChatID,
			&pending.TelegramMessageID,
			&pending.Status,
			&syncedID,
			&existingEventID,
			&canvasDTStamp,
			&canvasSequence,
			&canvasStatus,
			&canvasCancelled,
			&missingSince,
			&detached,
			&removalEligible,
			&googleConfirmed,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if pending.Status != PendingStatusPending {
			return ErrPendingAlreadyHandled
		}
		var currentMode string
		err = tx.QueryRowContext(
			ctx,
			`SELECT mode
			 FROM canvaslink_course_type_settings
			 WHERE telegram_user_id = $1
			   AND course_id = $2
			   AND assignment_type = $3
			 FOR SHARE`,
			input.TelegramUserID,
			pending.CourseID,
			pending.AssignmentType,
		).Scan(&currentMode)
		if errors.Is(err, sql.ErrNoRows) {
			currentMode = ModeReview
		} else if err != nil {
			return err
		}
		if CanonicalMode(currentMode) != ModeReview {
			return ErrPendingAlreadyHandled
		}
		if detached || missingSince != nil || canvasCancelled || !removalEligible {
			return ErrNotFound
		}
		if googleConfirmed && existingEventID != "" {
			return ErrPendingAlreadyHandled
		}
		if existingEventID != "" && existingEventID != input.GoogleEventID {
			return fmt.Errorf("%w: pending source has a different Google target", ErrInvalidCalendarJob)
		}

		dueAt := pending.CanvasDueAt
		jobInput := normalizeCalendarJobInput(CalendarJobInput{
			TelegramUserID:   input.TelegramUserID,
			DedupeKey:        "event:" + input.GoogleEventID,
			Action:           CalendarActionCreate,
			SourceUID:        pending.CanvasEventUID,
			CourseID:         pending.CourseID,
			AssignmentType:   pending.AssignmentType,
			Title:            pending.CanvasTitle,
			Description:      input.Description,
			DueAt:            &dueAt,
			AllDay:           pending.CanvasAllDay,
			GoogleCalendarID: input.GoogleCalendarID,
			GoogleEventID:    input.GoogleEventID,
			CanvasDTStamp:    canvasDTStamp,
			CanvasSequence:   canvasSequence,
			CanvasStatus:     canvasStatus,
			CanvasCancelled:  false,
			MaxAttempts:      input.MaxAttempts,
		})
		if err := validateCalendarJobInput(jobInput); err != nil {
			return err
		}

		result, err := tx.ExecContext(
			ctx,
			`UPDATE canvaslink_synced_events
			 SET course_id = $2,
			     assignment_type = $3,
			     canvas_title = $4,
			     canvas_due_at = $5,
			     all_day = $6,
			     google_calendar_id = $7,
			     google_event_id = $8,
			     google_confirmed = FALSE,
			     updated_at = NOW()
			 WHERE id = $1
			   AND detached = FALSE
			   AND missing_since IS NULL
			   AND canvas_cancelled = FALSE
			   AND removal_eligible = TRUE
			   AND google_confirmed = FALSE
			   AND (google_event_id = '' OR google_event_id = $8)
			   AND (
					($6 = TRUE AND $5::timestamptz + INTERVAL '1 day' > NOW())
					OR ($6 = FALSE AND $5::timestamptz > NOW())
			   )`,
			syncedID,
			pending.CourseID,
			pending.AssignmentType,
			pending.CanvasTitle,
			pending.CanvasDueAt,
			pending.CanvasAllDay,
			jobInput.GoogleCalendarID,
			jobInput.GoogleEventID,
		)
		if err := requireAffected(result, err); err != nil {
			return err
		}

		job, _, err := enqueueCalendarJob(ctx, tx, jobInput)
		if err != nil {
			return err
		}
		result, err = tx.ExecContext(
			ctx,
			`UPDATE canvaslink_pending_actions
			 SET status = 'added', updated_at = NOW()
			 WHERE telegram_user_id = $1
			   AND id = $2
			   AND status = 'pending'`,
			input.TelegramUserID,
			input.PendingActionID,
		)
		if err := requireAffected(result, err); err != nil {
			return err
		}
		queued = job
		return nil
	})
	if err != nil {
		return nil, err
	}
	return queued, nil
}

// CancelPendingActionByUID makes a Canvas-cancelled or removed review card
// inert and returns its Telegram identifiers so the caller can clear the old
// keyboard. User-ignored actions remain terminal, while this source-driven
// status can be reopened safely if the same UID becomes actionable again.
func (s *Store) CancelPendingActionByUID(ctx context.Context, userID int64, sourceUID string) (*PendingAction, error) {
	var action PendingAction
	err := s.db.QueryRowContext(
		ctx,
		`UPDATE canvaslink_pending_actions
		 SET status = 'source_removed', updated_at = NOW()
		 WHERE telegram_user_id = $1
		   AND canvas_event_uid = $2
		   AND status IN ('pending', 'added')
		 RETURNING id, telegram_user_id, course_id, assignment_type,
			canvas_event_uid, canvas_title, canvas_due_at, all_day,
			telegram_chat_id, telegram_message_id, status`,
		userID,
		sourceUID,
	).Scan(
		&action.ID,
		&action.TelegramUserID,
		&action.CourseID,
		&action.AssignmentType,
		&action.CanvasEventUID,
		&action.CanvasTitle,
		&action.CanvasDueAt,
		&action.CanvasAllDay,
		&action.TelegramChatID,
		&action.TelegramMessageID,
		&action.Status,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &action, nil
}

// --- Durable Google Calendar Jobs ---

// EnqueueCalendarJob inserts a durable calendar operation. A duplicate pending
// key is coalesced to its latest metadata. If the prior operation is already
// processing or terminal, a pending successor is inserted instead, so newer
// intent cannot be lost behind an in-flight claim.
func (s *Store) EnqueueCalendarJob(ctx context.Context, input CalendarJobInput) (*CalendarJob, bool, error) {
	input = normalizeCalendarJobInput(input)
	if err := validateCalendarJobInput(input); err != nil {
		return nil, false, err
	}
	return enqueueCalendarJob(ctx, s.db, input)
}

// QueueCalendarSync atomically stages the exact latest Canvas state and its
// Google target before making the durable create/update visible. The row is
// marked unconfirmed until the external operation completes, so a crash or
// later Canvas change is retried from explicit state rather than being mistaken
// for a completed sync.
func (s *Store) QueueCalendarSync(
	ctx context.Context,
	source SyncedEventInput,
	jobInput CalendarJobInput,
) (*CalendarJob, error) {
	jobInput = normalizeCalendarJobInput(jobInput)
	source.GoogleCalendarID = defaultString(source.GoogleCalendarID, "primary")
	source.GoogleEventID = strings.TrimSpace(source.GoogleEventID)
	if err := validateCalendarJobInput(jobInput); err != nil {
		return nil, err
	}
	if jobInput.Action != CalendarActionCreate && jobInput.Action != CalendarActionUpdate {
		return nil, fmt.Errorf("%w: source sync must create or update", ErrInvalidCalendarJob)
	}
	if source.TelegramUserID <= 0 ||
		strings.TrimSpace(source.CanvasEventUID) == "" ||
		source.GoogleEventID == "" ||
		source.CanvasCancelled ||
		!source.RemovalEligible ||
		source.GoogleConfirmed ||
		jobInput.TelegramUserID != source.TelegramUserID ||
		jobInput.SourceUID != source.CanvasEventUID ||
		jobInput.CourseID != source.CourseID ||
		jobInput.AssignmentType != source.AssignmentType ||
		jobInput.Title != source.CanvasTitle ||
		jobInput.DueAt == nil ||
		!jobInput.DueAt.Equal(source.CanvasDueAt) ||
		jobInput.AllDay != source.AllDay ||
		jobInput.GoogleCalendarID != source.GoogleCalendarID ||
		jobInput.GoogleEventID != source.GoogleEventID ||
		!optionalTimesEqual(jobInput.CanvasDTStamp, source.CanvasDTStamp) ||
		jobInput.CanvasSequence != source.CanvasSequence ||
		jobInput.CanvasStatus != source.CanvasStatus ||
		jobInput.CanvasCancelled != source.CanvasCancelled {
		return nil, fmt.Errorf("%w: calendar job does not exactly match its source", ErrInvalidCalendarJob)
	}

	var queued *CalendarJob
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var (
			existingID         int64
			existingCalendarID string
			existingEventID    string
			googleConfirmed    bool
			detached           bool
			removalEligible    bool
			canvasCancelled    bool
			missingSince       *time.Time
		)
		err := tx.QueryRowContext(
			ctx,
			`SELECT id, google_calendar_id, google_event_id, google_confirmed,
				detached, removal_eligible, canvas_cancelled, missing_since
			 FROM canvaslink_synced_events
			 WHERE telegram_user_id = $1 AND canvas_event_uid = $2
			 FOR UPDATE`,
			source.TelegramUserID,
			source.CanvasEventUID,
		).Scan(
			&existingID,
			&existingCalendarID,
			&existingEventID,
			&googleConfirmed,
			&detached,
			&removalEligible,
			&canvasCancelled,
			&missingSince,
		)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if jobInput.Action != CalendarActionCreate {
				return fmt.Errorf("%w: a new source must use a create job", ErrInvalidCalendarJob)
			}
			_, err = tx.ExecContext(
				ctx,
				`INSERT INTO canvaslink_synced_events (
					telegram_user_id, course_id, assignment_type,
					canvas_event_uid, canvas_title, canvas_due_at, all_day,
					google_calendar_id, google_event_id, canvas_dtstamp,
					canvas_sequence, canvas_status, canvas_cancelled,
					detached, removal_eligible, google_confirmed
				 )
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,FALSE,FALSE,TRUE,FALSE)`,
				source.TelegramUserID,
				source.CourseID,
				source.AssignmentType,
				source.CanvasEventUID,
				source.CanvasTitle,
				source.CanvasDueAt,
				source.AllDay,
				source.GoogleCalendarID,
				source.GoogleEventID,
				source.CanvasDTStamp,
				source.CanvasSequence,
				source.CanvasStatus,
			)
			if err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if detached || !removalEligible || canvasCancelled || missingSince != nil {
				return ErrNotFound
			}
			if googleConfirmed {
				if jobInput.Action != CalendarActionUpdate ||
					existingEventID == "" ||
					existingCalendarID != source.GoogleCalendarID ||
					existingEventID != source.GoogleEventID {
					return fmt.Errorf("%w: confirmed source has a different Google target", ErrInvalidCalendarJob)
				}
			} else if jobInput.Action != CalendarActionCreate ||
				(existingEventID != "" &&
					(existingCalendarID != source.GoogleCalendarID || existingEventID != source.GoogleEventID)) {
				return fmt.Errorf("%w: unconfirmed source has a different Google target", ErrInvalidCalendarJob)
			}
			result, err := tx.ExecContext(
				ctx,
				`UPDATE canvaslink_synced_events
				 SET course_id = $2,
				     assignment_type = $3,
				     canvas_title = $4,
				     canvas_due_at = $5,
				     all_day = $6,
				     google_calendar_id = $7,
				     google_event_id = $8,
				     canvas_dtstamp = $9,
				     canvas_sequence = $10,
				     canvas_status = $11,
				     canvas_cancelled = FALSE,
				     missing_since = NULL,
				     missing_count = 0,
				     detached = FALSE,
				     removal_eligible = TRUE,
				     google_confirmed = FALSE,
				     updated_at = NOW()
				 WHERE id = $1
				   AND detached = FALSE
				   AND removal_eligible = TRUE
				   AND canvas_cancelled = FALSE
				   AND missing_since IS NULL`,
				existingID,
				source.CourseID,
				source.AssignmentType,
				source.CanvasTitle,
				source.CanvasDueAt,
				source.AllDay,
				source.GoogleCalendarID,
				source.GoogleEventID,
				source.CanvasDTStamp,
				source.CanvasSequence,
				source.CanvasStatus,
			)
			if err := requireAffected(result, err); err != nil {
				return err
			}
		}

		job, _, err := enqueueCalendarJob(ctx, tx, jobInput)
		if err != nil {
			return err
		}
		queued = job
		return nil
	})
	if err != nil {
		return nil, err
	}
	return queued, nil
}

func enqueueCalendarJob(ctx context.Context, queryer contextQueryRower, input CalendarJobInput) (*CalendarJob, bool, error) {
	var job CalendarJob
	var created bool
	row := queryer.QueryRowContext(
		ctx,
		`INSERT INTO canvaslink_calendar_jobs (
			telegram_user_id, dedupe_key, action, source_uid, course_id,
			assignment_type, title, description, due_at, all_day,
			google_calendar_id, google_event_id, canvas_dtstamp,
			canvas_sequence, canvas_status, canvas_cancelled, max_attempts,
			expected_missing_since, expected_missing_count, delete_not_before
		 )
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		 ON CONFLICT (telegram_user_id, dedupe_key) WHERE status = 'pending'
		 DO UPDATE SET
			action = EXCLUDED.action,
			source_uid = EXCLUDED.source_uid,
			course_id = EXCLUDED.course_id,
			assignment_type = EXCLUDED.assignment_type,
			title = EXCLUDED.title,
			description = EXCLUDED.description,
			due_at = EXCLUDED.due_at,
			all_day = EXCLUDED.all_day,
			google_calendar_id = EXCLUDED.google_calendar_id,
			google_event_id = EXCLUDED.google_event_id,
			canvas_dtstamp = EXCLUDED.canvas_dtstamp,
			canvas_sequence = EXCLUDED.canvas_sequence,
			canvas_status = EXCLUDED.canvas_status,
			canvas_cancelled = EXCLUDED.canvas_cancelled,
			attempts = 0,
			max_attempts = EXCLUDED.max_attempts,
			available_at = NOW(),
			lease_owner = '',
			lease_until = NULL,
			last_error = '',
			expected_missing_since = EXCLUDED.expected_missing_since,
			expected_missing_count = EXCLUDED.expected_missing_count,
			delete_not_before = EXCLUDED.delete_not_before,
			updated_at = NOW()
		 RETURNING `+calendarJobColumns+`, (xmax = 0)`,
		input.TelegramUserID,
		input.DedupeKey,
		input.Action,
		input.SourceUID,
		input.CourseID,
		input.AssignmentType,
		input.Title,
		input.Description,
		input.DueAt,
		input.AllDay,
		input.GoogleCalendarID,
		input.GoogleEventID,
		input.CanvasDTStamp,
		input.CanvasSequence,
		input.CanvasStatus,
		input.CanvasCancelled,
		input.MaxAttempts,
		input.ExpectedMissingSince,
		input.ExpectedMissingCount,
		input.DeleteNotBefore,
	)
	destinations := calendarJobScanDestinations(&job)
	destinations = append(destinations, &created)
	if err := row.Scan(destinations...); err != nil {
		return nil, false, err
	}
	return &job, created, nil
}

// ClaimCalendarJobs leases ready work using SKIP LOCKED, allowing multiple bot
// instances to process jobs without double-claiming. Expired leases are safely
// reclaimed and increment the attempt counter.
func (s *Store) ClaimCalendarJobs(ctx context.Context, workerID string, limit int, lease time.Duration) ([]CalendarJob, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" || len(workerID) > 128 {
		return nil, fmt.Errorf("%w: invalid worker ID", ErrInvalidCalendarJob)
	}
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("%w: claim limit must be between 1 and 100", ErrInvalidCalendarJob)
	}
	if lease <= 0 {
		return nil, fmt.Errorf("%w: lease must be positive", ErrInvalidCalendarJob)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin calendar job claim: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(
		ctx,
		`UPDATE canvaslink_calendar_jobs AS jobs
		 SET status = 'cancelled',
		     last_error = 'source event reappeared or removal observation changed',
		     updated_at = NOW()
		 WHERE jobs.status = 'pending'
		   AND jobs.action = 'delete'
		   AND (
			jobs.expected_missing_since IS NULL
			OR jobs.expected_missing_count < 1
			OR jobs.delete_not_before IS NULL
			OR (
				jobs.canvas_cancelled = FALSE
				AND jobs.delete_not_before <= jobs.expected_missing_since
			)
			OR (
				jobs.canvas_cancelled = TRUE
				AND jobs.delete_not_before < jobs.expected_missing_since
			)
			OR NOT EXISTS (
				SELECT 1
				FROM canvaslink_synced_events AS events
				WHERE events.telegram_user_id = jobs.telegram_user_id
				  AND events.canvas_event_uid = jobs.source_uid
				  AND events.detached = FALSE
				  AND events.removal_eligible = TRUE
				  AND events.google_calendar_id = jobs.google_calendar_id
				  AND events.google_event_id = jobs.google_event_id
				  AND events.canvas_cancelled = jobs.canvas_cancelled
				  AND (
					(events.all_day = TRUE AND events.canvas_due_at + INTERVAL '1 day' > NOW())
					OR (events.all_day = FALSE AND events.canvas_due_at > NOW())
				  )
				  AND events.missing_since IS NOT DISTINCT FROM jobs.expected_missing_since
				  AND events.missing_count = jobs.expected_missing_count
			)
		   )`,
	); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(
		ctx,
		`UPDATE canvaslink_calendar_jobs AS current
		 SET status = 'cancelled',
		     lease_owner = '',
		     lease_until = NULL,
		     last_error = 'superseded by newer queued state',
		     updated_at = NOW()
		 WHERE current.status = 'processing'
		   AND current.lease_until <= NOW()
		   AND EXISTS (
			SELECT 1
			FROM canvaslink_calendar_jobs AS successor
			WHERE successor.telegram_user_id = current.telegram_user_id
			  AND successor.dedupe_key = current.dedupe_key
			  AND successor.status = 'pending'
			  AND successor.id <> current.id
		   )`,
	); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(
		ctx,
		`UPDATE canvaslink_calendar_jobs
		 SET status = 'failed',
		     lease_owner = '',
		     lease_until = NULL,
		     last_error = CASE WHEN last_error = '' THEN 'maximum attempts exhausted' ELSE last_error END,
		     updated_at = NOW()
		 WHERE status IN ('pending', 'processing')
		   AND attempts >= max_attempts
		   AND (status = 'pending' OR lease_until <= NOW())`,
	); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(
		ctx,
		`WITH candidates AS (
			SELECT id
			FROM canvaslink_calendar_jobs
			WHERE attempts < max_attempts
			  AND (
				(status = 'pending' AND available_at <= NOW())
				OR (status = 'processing' AND lease_until <= NOW())
			  )
			  AND (
				action <> 'delete'
				OR (delete_not_before IS NOT NULL AND delete_not_before <= NOW())
			  )
			ORDER BY available_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		 )
		 UPDATE canvaslink_calendar_jobs AS jobs
		 SET status = 'processing',
		     attempts = jobs.attempts + 1,
		     lease_version = jobs.lease_version + 1,
		     lease_owner = $2,
		     lease_until = NOW() + ($3 * INTERVAL '1 millisecond'),
		     updated_at = NOW()
		 FROM candidates
		 WHERE jobs.id = candidates.id
		 RETURNING `+prefixedCalendarJobColumns("jobs"),
		limit,
		workerID,
		max(lease.Milliseconds(), 1),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []CalendarJob
	for rows.Next() {
		var job CalendarJob
		if err := scanCalendarJob(rows, &job); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit calendar job claim: %w", err)
	}
	return jobs, nil
}

// CanRunCalendarJob rechecks a lease and all source/provider prerequisites
// immediately before an external call. Create/update require an enabled feed
// and a current Google token. Deletes require a token; automatic professor
// removals additionally require the feed and the exact missing observation.
func (s *Store) CanRunCalendarJob(ctx context.Context, jobID int64, workerID string, leaseVersion int64) (bool, error) {
	_, runnable, err := s.canRunCalendarJob(ctx, jobID, workerID, leaseVersion)
	return runnable, err
}

// CanRunCalendarDeleteJob is the action-specific form used by delete workers.
func (s *Store) CanRunCalendarDeleteJob(ctx context.Context, jobID int64, workerID string, leaseVersion int64) (bool, error) {
	action, runnable, err := s.canRunCalendarJob(ctx, jobID, workerID, leaseVersion)
	if err != nil {
		return false, err
	}
	if action != CalendarActionDelete {
		return false, fmt.Errorf("%w: job %d is not a delete", ErrInvalidCalendarJob, jobID)
	}
	return runnable, nil
}

func (s *Store) canRunCalendarJob(ctx context.Context, jobID int64, workerID string, leaseVersion int64) (string, bool, error) {
	var action string
	var expectedSince *time.Time
	var expectedCount int
	var deleteNotBefore *time.Time
	var feedExists bool
	var tokenExists bool
	var sourceActive bool
	var sourceStillMissing bool
	err := s.db.QueryRowContext(
		ctx,
		`SELECT jobs.action, jobs.expected_missing_since, jobs.expected_missing_count,
			jobs.delete_not_before,
			EXISTS(
				SELECT 1 FROM canvaslink_feeds AS feeds
				WHERE feeds.telegram_user_id = jobs.telegram_user_id
				  AND feeds.sync_enabled = TRUE
			),
			EXISTS(
				SELECT 1 FROM canvaslink_google_tokens AS tokens
				WHERE tokens.telegram_user_id = jobs.telegram_user_id
			),
			EXISTS(
				SELECT 1
				FROM canvaslink_synced_events AS events
				WHERE events.telegram_user_id = jobs.telegram_user_id
				  AND events.canvas_event_uid = jobs.source_uid
				  AND events.detached = FALSE
				  AND events.removal_eligible = TRUE
				  AND events.missing_since IS NULL
				  AND events.canvas_cancelled = FALSE
				  AND events.google_calendar_id = jobs.google_calendar_id
				  AND events.google_event_id = jobs.google_event_id
				  AND events.course_id = jobs.course_id
				  AND events.assignment_type = jobs.assignment_type
				  AND events.canvas_title = jobs.title
				  AND events.canvas_due_at IS NOT DISTINCT FROM jobs.due_at
				  AND events.all_day = jobs.all_day
				  AND events.canvas_dtstamp IS NOT DISTINCT FROM jobs.canvas_dtstamp
				  AND events.canvas_sequence = jobs.canvas_sequence
				  AND events.canvas_status = jobs.canvas_status
				  AND events.canvas_cancelled = jobs.canvas_cancelled
				  AND jobs.due_at IS NOT NULL
				  AND (
					(jobs.all_day = TRUE AND jobs.due_at + INTERVAL '1 day' > NOW())
					OR (jobs.all_day = FALSE AND jobs.due_at > NOW())
				  )
				  AND (
					(events.all_day = TRUE AND events.canvas_due_at + INTERVAL '1 day' > NOW())
					OR (events.all_day = FALSE AND events.canvas_due_at > NOW())
				  )
				  AND CASE COALESCE(
					(
						SELECT settings.mode
						FROM canvaslink_course_type_settings AS settings
						WHERE settings.telegram_user_id = jobs.telegram_user_id
						  AND settings.course_id = jobs.course_id
						  AND settings.assignment_type = jobs.assignment_type
					),
					'review'
				  )
					WHEN 'auto' THEN TRUE
					WHEN 'quiet' THEN TRUE
					WHEN 'notify' THEN TRUE
					WHEN 'active' THEN jobs.action = 'update' OR events.google_confirmed OR EXISTS (
						SELECT 1
						FROM canvaslink_pending_actions AS pending
						WHERE pending.telegram_user_id = jobs.telegram_user_id
						  AND pending.canvas_event_uid = jobs.source_uid
						  AND pending.status = 'added'
					)
					WHEN 'review' THEN jobs.action = 'update' OR events.google_confirmed OR EXISTS (
						SELECT 1
						FROM canvaslink_pending_actions AS pending
						WHERE pending.telegram_user_id = jobs.telegram_user_id
						  AND pending.canvas_event_uid = jobs.source_uid
						  AND pending.status = 'added'
					)
					ELSE FALSE
				  END
			),
			CASE
				WHEN jobs.expected_missing_since IS NULL
				  OR jobs.delete_not_before IS NULL
				  OR (
					jobs.canvas_cancelled = FALSE
					AND jobs.delete_not_before <= jobs.expected_missing_since
				  )
				  OR (
					jobs.canvas_cancelled = TRUE
					AND jobs.delete_not_before < jobs.expected_missing_since
				  )
				THEN FALSE
				ELSE EXISTS (
					SELECT 1
					FROM canvaslink_synced_events AS events
					WHERE events.telegram_user_id = jobs.telegram_user_id
					  AND events.canvas_event_uid = jobs.source_uid
					  AND events.detached = FALSE
					  AND events.removal_eligible = TRUE
					  AND events.google_calendar_id = jobs.google_calendar_id
					  AND events.google_event_id = jobs.google_event_id
					  AND events.canvas_cancelled = jobs.canvas_cancelled
					  AND (
						(events.all_day = TRUE AND events.canvas_due_at + INTERVAL '1 day' > NOW())
						OR (events.all_day = FALSE AND events.canvas_due_at > NOW())
					  )
					  AND events.missing_since IS NOT DISTINCT FROM jobs.expected_missing_since
					  AND events.missing_count = jobs.expected_missing_count
					  AND NOW() >= jobs.delete_not_before
				)
			END
		 FROM canvaslink_calendar_jobs AS jobs
		 WHERE jobs.id = $1
		   AND jobs.status = 'processing'
		   AND jobs.lease_owner = $2
		   AND jobs.lease_version = $3`,
		jobID,
		strings.TrimSpace(workerID),
		leaseVersion,
	).Scan(
		&action,
		&expectedSince,
		&expectedCount,
		&deleteNotBefore,
		&feedExists,
		&tokenExists,
		&sourceActive,
		&sourceStillMissing,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrCalendarJobLease
	}
	if err != nil {
		return "", false, err
	}
	switch action {
	case CalendarActionCreate, CalendarActionUpdate:
		return action, feedExists && tokenExists && sourceActive, nil
	case CalendarActionDelete:
		return action, expectedSince != nil && expectedCount > 0 && deleteNotBefore != nil &&
			tokenExists && feedExists && sourceStillMissing, nil
	default:
		return action, false, fmt.Errorf("%w: unknown action %q", ErrInvalidCalendarJob, action)
	}
}

// CompleteCalendarJob atomically marks externally completed work and applies
// its corresponding synced-event tracking change. The Google call can be
// safely retried before this method because creates use deterministic IDs.
func (s *Store) CompleteCalendarJob(ctx context.Context, jobID int64, workerID string, leaseVersion int64, googleEventID string) error {
	workerID = strings.TrimSpace(workerID)
	staleJob := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		job, err := getLeasedCalendarJob(ctx, tx, jobID, workerID, leaseVersion)
		if err != nil {
			return err
		}
		if googleEventID == "" {
			googleEventID = job.GoogleEventID
		}

		switch job.Action {
		case CalendarActionCreate, CalendarActionUpdate:
			if googleEventID == "" || job.DueAt == nil {
				return fmt.Errorf("%w: completed %s job lacks event ID or due date", ErrInvalidCalendarJob, job.Action)
			}
			var sourceAndConnectionStillValid bool
			if err := tx.QueryRowContext(
				ctx,
				`SELECT
					EXISTS(
						SELECT 1 FROM canvaslink_feeds
						WHERE telegram_user_id = $1 AND sync_enabled = TRUE
					)
					AND EXISTS(
						SELECT 1 FROM canvaslink_google_tokens
						WHERE telegram_user_id = $1
					)
					AND EXISTS(
						SELECT 1 FROM canvaslink_synced_events AS events
						WHERE events.telegram_user_id = $1
						  AND events.canvas_event_uid = $2
						  AND events.detached = FALSE
						  AND events.removal_eligible = TRUE
						  AND events.missing_since IS NULL
						  AND events.canvas_cancelled = FALSE
						  AND events.google_calendar_id = $3
						  AND events.google_event_id = $4
						  AND events.course_id = $5
						  AND events.assignment_type = $6
						  AND events.canvas_title = $9
						  AND events.canvas_due_at IS NOT DISTINCT FROM $7::timestamptz
						  AND events.all_day = $8
						  AND events.canvas_dtstamp IS NOT DISTINCT FROM $10::timestamptz
						  AND events.canvas_sequence = $11
						  AND events.canvas_status = $12
						  AND events.canvas_cancelled = $13
						  AND (
							($8 = TRUE AND $7::timestamptz + INTERVAL '1 day' > NOW())
							OR ($8 = FALSE AND $7::timestamptz > NOW())
						  )
						  AND CASE COALESCE(
							(
								SELECT settings.mode
								FROM canvaslink_course_type_settings AS settings
								WHERE settings.telegram_user_id = $1
								  AND settings.course_id = $5
								  AND settings.assignment_type = $6
							),
							'review'
						  )
							WHEN 'auto' THEN TRUE
							WHEN 'quiet' THEN TRUE
							WHEN 'notify' THEN TRUE
							WHEN 'active' THEN $14 = 'update' OR events.google_confirmed OR EXISTS (
								SELECT 1
								FROM canvaslink_pending_actions AS pending
								WHERE pending.telegram_user_id = $1
								  AND pending.canvas_event_uid = $2
								  AND pending.status = 'added'
							)
							WHEN 'review' THEN $14 = 'update' OR events.google_confirmed OR EXISTS (
								SELECT 1
								FROM canvaslink_pending_actions AS pending
								WHERE pending.telegram_user_id = $1
								  AND pending.canvas_event_uid = $2
								  AND pending.status = 'added'
							)
							ELSE FALSE
						  END
					)`,
				job.TelegramUserID,
				job.SourceUID,
				job.GoogleCalendarID,
				job.GoogleEventID,
				job.CourseID,
				job.AssignmentType,
				*job.DueAt,
				job.AllDay,
				job.Title,
				job.CanvasDTStamp,
				job.CanvasSequence,
				job.CanvasStatus,
				job.CanvasCancelled,
				job.Action,
			).Scan(&sourceAndConnectionStillValid); err != nil {
				return err
			}
			if !sourceAndConnectionStillValid {
				staleJob = true
				break
			}
			_, err = tx.ExecContext(
				ctx,
				`INSERT INTO canvaslink_synced_events (
					telegram_user_id, course_id, assignment_type, canvas_event_uid,
					canvas_title, canvas_due_at, all_day, google_calendar_id,
					google_event_id, canvas_dtstamp, canvas_sequence,
					canvas_status, canvas_cancelled, missing_since, missing_count,
					detached, removal_eligible, google_confirmed
				 )
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULL,0,FALSE,TRUE,TRUE)
				 ON CONFLICT (telegram_user_id, canvas_event_uid)
				 DO UPDATE SET
					course_id = EXCLUDED.course_id,
					assignment_type = EXCLUDED.assignment_type,
					canvas_title = EXCLUDED.canvas_title,
					canvas_due_at = EXCLUDED.canvas_due_at,
					all_day = EXCLUDED.all_day,
					google_calendar_id = EXCLUDED.google_calendar_id,
					google_event_id = EXCLUDED.google_event_id,
					canvas_dtstamp = EXCLUDED.canvas_dtstamp,
					canvas_sequence = EXCLUDED.canvas_sequence,
					canvas_status = EXCLUDED.canvas_status,
					canvas_cancelled = EXCLUDED.canvas_cancelled,
					missing_since = NULL,
					missing_count = 0,
					detached = FALSE,
					removal_eligible = TRUE,
					google_confirmed = TRUE,
					updated_at = NOW()`,
				job.TelegramUserID,
				job.CourseID,
				job.AssignmentType,
				job.SourceUID,
				job.Title,
				*job.DueAt,
				job.AllDay,
				job.GoogleCalendarID,
				googleEventID,
				job.CanvasDTStamp,
				job.CanvasSequence,
				job.CanvasStatus,
				job.CanvasCancelled,
			)
			if err != nil {
				return err
			}
		case CalendarActionDelete:
			if job.ExpectedMissingSince == nil ||
				job.ExpectedMissingCount < 1 ||
				job.DeleteNotBefore == nil ||
				job.DeleteNotBefore.Before(*job.ExpectedMissingSince) ||
				(!job.CanvasCancelled && !job.DeleteNotBefore.After(*job.ExpectedMissingSince)) {
				staleJob = true
				break
			}
			result, err := tx.ExecContext(
				ctx,
				`DELETE FROM canvaslink_synced_events
				 WHERE telegram_user_id = $1
				   AND canvas_event_uid = $2
				   AND detached = FALSE
				   AND removal_eligible = TRUE
				   AND google_calendar_id = $6
				   AND google_event_id = $7
				   AND canvas_cancelled = $8
				   AND (
					(all_day = TRUE AND canvas_due_at + INTERVAL '1 day' > NOW())
					OR (all_day = FALSE AND canvas_due_at > NOW())
				   )
				   AND missing_since IS NOT DISTINCT FROM $3
				   AND missing_count = $4
				   AND NOW() >= $5`,
				job.TelegramUserID,
				job.SourceUID,
				job.ExpectedMissingSince,
				job.ExpectedMissingCount,
				job.DeleteNotBefore,
				job.GoogleCalendarID,
				job.GoogleEventID,
				job.CanvasCancelled,
			)
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if affected == 0 {
				staleJob = true
			}
		default:
			return fmt.Errorf("%w: unknown action %q", ErrInvalidCalendarJob, job.Action)
		}

		status := CalendarJobStatusCompleted
		lastError := ""
		var completedAt any = time.Now().UTC()
		if staleJob {
			status = CalendarJobStatusCancelled
			lastError = "source state changed before calendar job completion"
			completedAt = nil
		}
		result, err := tx.ExecContext(
			ctx,
			`UPDATE canvaslink_calendar_jobs
			 SET status = $4,
			     google_event_id = $5,
			     lease_owner = '',
			     lease_until = NULL,
			     last_error = $6,
			     completed_at = $7,
			     updated_at = NOW()
			 WHERE id = $1
			   AND status = 'processing'
			   AND lease_owner = $2
			   AND lease_version = $3`,
			jobID,
			workerID,
			leaseVersion,
			status,
			googleEventID,
			lastError,
			completedAt,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return ErrCalendarJobLease
		}
		return nil
	})
	if err != nil {
		return err
	}
	if staleJob {
		return ErrStaleCalendarJob
	}
	return nil
}

// RetryCalendarJob releases a lease and schedules another attempt. Once the
// configured attempt limit is reached it transitions to failed instead.
func (s *Store) RetryCalendarJob(ctx context.Context, jobID int64, workerID string, leaseVersion int64, nextAttempt time.Time, lastError string) error {
	if nextAttempt.IsZero() {
		nextAttempt = time.Now().UTC()
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE canvaslink_calendar_jobs AS current
		 SET status = CASE
				WHEN EXISTS (
					SELECT 1
					FROM canvaslink_calendar_jobs AS successor
					WHERE successor.telegram_user_id = current.telegram_user_id
					  AND successor.dedupe_key = current.dedupe_key
					  AND successor.status = 'pending'
					  AND successor.id <> current.id
				) THEN 'cancelled'
				WHEN current.attempts >= current.max_attempts THEN 'failed'
				ELSE 'pending'
		     END,
		     available_at = $4,
		     lease_owner = '',
		     lease_until = NULL,
		     last_error = $5,
		     updated_at = NOW()
		 WHERE id = $1
		   AND status = 'processing'
		   AND lease_owner = $2
		   AND lease_version = $3`,
		jobID,
		strings.TrimSpace(workerID),
		leaseVersion,
		nextAttempt,
		truncateStoreError(lastError),
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrCalendarJobLease
	}
	return nil
}

// BlockCalendarJob releases work without consuming an attempt. Use it for
// environmental blockers such as a disconnected or unconfigured provider.
func (s *Store) BlockCalendarJob(ctx context.Context, jobID int64, workerID string, leaseVersion int64, nextAttempt time.Time, lastError string) error {
	if nextAttempt.IsZero() {
		nextAttempt = time.Now().UTC()
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE canvaslink_calendar_jobs AS current
		 SET status = CASE
				WHEN EXISTS (
					SELECT 1
					FROM canvaslink_calendar_jobs AS successor
					WHERE successor.telegram_user_id = current.telegram_user_id
					  AND successor.dedupe_key = current.dedupe_key
					  AND successor.status = 'pending'
					  AND successor.id <> current.id
				) THEN 'cancelled'
				ELSE 'pending'
		     END,
		     attempts = GREATEST(attempts - 1, 0),
		     available_at = $4,
		     lease_owner = '',
		     lease_until = NULL,
		     last_error = $5,
		     updated_at = NOW()
		 WHERE id = $1
		   AND status = 'processing'
		   AND lease_owner = $2
		   AND lease_version = $3`,
		jobID,
		strings.TrimSpace(workerID),
		leaseVersion,
		nextAttempt,
		truncateStoreError(lastError),
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrCalendarJobLease
	}
	return nil
}

// FailCalendarJob permanently fails work currently leased by workerID.
func (s *Store) FailCalendarJob(ctx context.Context, jobID int64, workerID string, leaseVersion int64, lastError string) error {
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE canvaslink_calendar_jobs
		 SET status = 'failed',
		     lease_owner = '',
		     lease_until = NULL,
		     last_error = $4,
		     updated_at = NOW()
		 WHERE id = $1
		   AND status = 'processing'
		   AND lease_owner = $2
		   AND lease_version = $3`,
		jobID,
		strings.TrimSpace(workerID),
		leaseVersion,
		truncateStoreError(lastError),
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrCalendarJobLease
	}
	return nil
}

// CancelCalendarJob terminally releases a leased operation that failed its
// final preflight, without applying any synced-event tracking change.
func (s *Store) CancelCalendarJob(ctx context.Context, jobID int64, workerID string, leaseVersion int64, reason string) error {
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE canvaslink_calendar_jobs
		 SET status = 'cancelled',
		     lease_owner = '',
		     lease_until = NULL,
		     last_error = $4,
		     updated_at = NOW()
		 WHERE id = $1
		   AND status = 'processing'
		   AND lease_owner = $2
		   AND lease_version = $3`,
		jobID,
		strings.TrimSpace(workerID),
		leaseVersion,
		truncateStoreError(reason),
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrCalendarJobLease
	}
	return nil
}

// CancelPendingCalendarDelete prevents a queued deletion from running when its
// Canvas event reappears. An already leased operation is deliberately not
// modified because its external call may already be in progress.
func (s *Store) CancelPendingCalendarDelete(ctx context.Context, userID int64, sourceUID string) error {
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE canvaslink_calendar_jobs
		 SET status = 'cancelled', updated_at = NOW()
		 WHERE telegram_user_id = $1
		   AND source_uid = $2
		   AND action = 'delete'
		   AND status IN ('pending', 'failed')`,
		userID,
		sourceUID,
	)
	return err
}

// CancelOpenCalendarSyncJobsBySource fences queued or leased create/update
// work after Canvas has conservatively confirmed that the source disappeared.
// Callers must hold the user's advisory operation lock.
func (s *Store) CancelOpenCalendarSyncJobsBySource(ctx context.Context, userID int64, sourceUID, reason string) error {
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE canvaslink_calendar_jobs
		 SET status = 'cancelled',
		     lease_owner = '',
		     lease_until = NULL,
		     last_error = $3,
		     updated_at = NOW()
		 WHERE telegram_user_id = $1
		   AND source_uid = $2
		   AND action IN ('create', 'update')
		   AND status IN ('pending', 'processing', 'failed')`,
		userID,
		sourceUID,
		truncateStoreError(reason),
	)
	return err
}

// CancelMismatchedOpenCalendarSyncJobs fences any create/update intent that no
// longer exactly matches the latest actionable Canvas observation. Callers
// must hold the user's advisory operation lock so a leased job cannot pass
// preflight while this comparison is being made.
func (s *Store) CancelMismatchedOpenCalendarSyncJobs(
	ctx context.Context,
	expected CalendarJobInput,
	reason string,
) (int64, error) {
	expected = normalizeCalendarJobInput(expected)
	if expected.Action != CalendarActionCreate && expected.Action != CalendarActionUpdate {
		return 0, fmt.Errorf("%w: expected source job must create or update", ErrInvalidCalendarJob)
	}
	if err := validateCalendarJobInput(expected); err != nil {
		return 0, err
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE canvaslink_calendar_jobs
		 SET status = 'cancelled',
		     lease_owner = '',
		     lease_until = NULL,
		     last_error = $15,
		     updated_at = NOW()
		 WHERE telegram_user_id = $1
		   AND source_uid = $2
		   AND action IN ('create', 'update')
		   AND status IN ('pending', 'processing', 'failed')
		   AND (
				action IS DISTINCT FROM $3
				OR course_id IS DISTINCT FROM $4
				OR assignment_type IS DISTINCT FROM $5
				OR title IS DISTINCT FROM $6
				OR due_at IS DISTINCT FROM $7
				OR all_day IS DISTINCT FROM $8
				OR google_calendar_id IS DISTINCT FROM $9
				OR google_event_id IS DISTINCT FROM $10
				OR canvas_dtstamp IS DISTINCT FROM $11
				OR canvas_sequence IS DISTINCT FROM $12
				OR canvas_status IS DISTINCT FROM $13
				OR canvas_cancelled IS DISTINCT FROM $14
		   )`,
		expected.TelegramUserID,
		expected.SourceUID,
		expected.Action,
		expected.CourseID,
		expected.AssignmentType,
		expected.Title,
		expected.DueAt,
		expected.AllDay,
		expected.GoogleCalendarID,
		expected.GoogleEventID,
		expected.CanvasDTStamp,
		expected.CanvasSequence,
		expected.CanvasStatus,
		expected.CanvasCancelled,
		truncateStoreError(reason),
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ListCalendarOwnershipCandidates returns every remote target CanvasLink may
// have created. Calendar jobs are included because Google can accept a create
// immediately before the process fails to commit the synced-event row.
func (s *Store) ListCalendarOwnershipCandidates(ctx context.Context, userID int64) ([]CalendarOwnershipCandidate, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT source_uid, google_calendar_id, google_event_id
		 FROM (
			SELECT canvas_event_uid AS source_uid,
			       google_calendar_id,
			       google_event_id,
			       updated_at
			FROM canvaslink_synced_events
			WHERE telegram_user_id = $1 AND google_event_id <> ''
			UNION ALL
			SELECT source_uid,
			       google_calendar_id,
			       google_event_id,
			       updated_at
			FROM canvaslink_calendar_jobs
			WHERE telegram_user_id = $1 AND google_event_id <> ''
		 ) AS candidates
		 ORDER BY source_uid, google_calendar_id, google_event_id, updated_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	var candidates []CalendarOwnershipCandidate
	for rows.Next() {
		var candidate CalendarOwnershipCandidate
		if err := rows.Scan(
			&candidate.CanvasEventUID,
			&candidate.GoogleCalendarID,
			&candidate.GoogleEventID,
		); err != nil {
			return nil, err
		}
		key := candidate.CanvasEventUID + "\x00" + candidate.GoogleCalendarID + "\x00" + candidate.GoogleEventID
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (s *Store) GetCalendarJob(ctx context.Context, userID, jobID int64) (*CalendarJob, error) {
	var job CalendarJob
	row := s.db.QueryRowContext(
		ctx,
		`SELECT `+calendarJobColumns+`
		 FROM canvaslink_calendar_jobs
		 WHERE telegram_user_id = $1 AND id = $2`,
		userID,
		jobID,
	)
	err := scanCalendarJob(row, &job)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func getLeasedCalendarJob(ctx context.Context, tx *sql.Tx, jobID int64, workerID string, leaseVersion int64) (*CalendarJob, error) {
	if workerID == "" || leaseVersion < 1 {
		return nil, ErrCalendarJobLease
	}
	var job CalendarJob
	row := tx.QueryRowContext(
		ctx,
		`SELECT `+calendarJobColumns+`
		 FROM canvaslink_calendar_jobs
		 WHERE id = $1
		   AND status = 'processing'
		   AND lease_owner = $2
		   AND lease_version = $3
		 FOR UPDATE`,
		jobID,
		workerID,
		leaseVersion,
	)
	err := scanCalendarJob(row, &job)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCalendarJobLease
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func scanCalendarJob(scanner rowScanner, job *CalendarJob) error {
	return scanner.Scan(calendarJobScanDestinations(job)...)
}

func calendarJobScanDestinations(job *CalendarJob) []any {
	return []any{
		&job.ID,
		&job.TelegramUserID,
		&job.DedupeKey,
		&job.Action,
		&job.SourceUID,
		&job.CourseID,
		&job.AssignmentType,
		&job.Title,
		&job.Description,
		&job.DueAt,
		&job.AllDay,
		&job.GoogleCalendarID,
		&job.GoogleEventID,
		&job.CanvasDTStamp,
		&job.CanvasSequence,
		&job.CanvasStatus,
		&job.CanvasCancelled,
		&job.ExpectedMissingSince,
		&job.ExpectedMissingCount,
		&job.DeleteNotBefore,
		&job.Status,
		&job.Attempts,
		&job.MaxAttempts,
		&job.AvailableAt,
		&job.LeaseOwner,
		&job.LeaseVersion,
		&job.LeaseUntil,
		&job.LastError,
		&job.CreatedAt,
		&job.UpdatedAt,
		&job.CompletedAt,
	}
}

const calendarJobColumns = `id, telegram_user_id, dedupe_key, action, source_uid,
	course_id, assignment_type, title, description, due_at, all_day,
	google_calendar_id, google_event_id, canvas_dtstamp, canvas_sequence,
	canvas_status, canvas_cancelled, expected_missing_since, expected_missing_count,
	delete_not_before, status, attempts, max_attempts, available_at, lease_owner, lease_version,
	lease_until, last_error, created_at, updated_at,
	completed_at`

func prefixedCalendarJobColumns(alias string) string {
	parts := strings.Split(calendarJobColumns, ",")
	for i, part := range parts {
		parts[i] = alias + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

// --- CanvasLink-Owned Google OAuth ---

// CreateOAuthState stores a new OAuth state only if the private account is
// still at the revision observed before the caller acquired its operation
// lock. This prevents a delayed authorization-link request from crossing a
// disconnect/reset boundary.
func (s *Store) CreateOAuthState(
	ctx context.Context,
	state string,
	telegramUserID int64,
	codeVerifier string,
	expiresAt time.Time,
	expectedStateRevision int64,
) error {
	storedVerifier, err := s.encodeSensitive(codeVerifier, oauthVerifierPurpose(state))
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(
			ctx,
			`DELETE FROM canvaslink_oauth_states
			 WHERE expires_at <= NOW() OR telegram_user_id = $1`,
			telegramUserID,
		); err != nil {
			return err
		}
		result, err := tx.ExecContext(
			ctx,
			`INSERT INTO canvaslink_oauth_states
				(state, telegram_user_id, code_verifier, expires_at, private_chat_bound)
			 SELECT $1, accounts.telegram_user_id, $3, $4, TRUE
			 FROM canvaslink_telegram_accounts AS accounts
			 WHERE accounts.telegram_user_id = $2
			   AND accounts.chat_id = accounts.telegram_user_id
			   AND accounts.state_revision = $5`,
			state,
			telegramUserID,
			storedVerifier,
			expiresAt,
			expectedStateRevision,
		)
		return requireAffected(result, err)
	})
}

// GetOAuthStateUserID resolves a live OAuth state without consuming it. The
// OAuth handler uses this only to acquire the user's distributed operation lock
// before atomically consuming the state and exchanging the authorization code.
func (s *Store) GetOAuthStateUserID(ctx context.Context, state string) (int64, error) {
	var telegramUserID int64
	err := s.db.QueryRowContext(
		ctx,
		`SELECT telegram_user_id
		 FROM canvaslink_oauth_states
		 WHERE state = $1
		   AND expires_at > NOW()
		   AND private_chat_bound = TRUE`,
		state,
	).Scan(&telegramUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return telegramUserID, nil
}

// ConsumeOAuthState atomically reads and deletes an OAuth state (one-time use).
func (s *Store) ConsumeOAuthState(ctx context.Context, state string) (int64, string, error) {
	var telegramUserID int64
	var codeVerifier string
	var valid bool
	err := s.db.QueryRowContext(
		ctx,
		`
		DELETE FROM canvaslink_oauth_states
		WHERE state = $1
		RETURNING telegram_user_id, code_verifier,
			expires_at > NOW() AND private_chat_bound = TRUE
		`,
		state,
	).Scan(&telegramUserID, &codeVerifier, &valid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", err
	}
	if !valid {
		return 0, "", nil
	}
	codeVerifier, err = s.decodeSensitive(codeVerifier, oauthVerifierPurpose(state))
	if err != nil {
		return 0, "", fmt.Errorf("decrypt OAuth verifier: %w", err)
	}
	return telegramUserID, codeVerifier, nil
}

// DeleteOAuthStates invalidates outstanding OAuth links for a user.
func (s *Store) DeleteOAuthStates(ctx context.Context, telegramUserID int64) error {
	_, err := s.db.ExecContext(
		ctx,
		`DELETE FROM canvaslink_oauth_states WHERE telegram_user_id = $1`,
		telegramUserID,
	)
	return err
}

// HasGoogleToken checks if a Telegram user has a stored Google token.
func (s *Store) HasGoogleToken(ctx context.Context, telegramUserID int64) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM canvaslink_google_tokens WHERE telegram_user_id = $1)`,
		telegramUserID,
	).Scan(&exists)
	return exists, err
}

// GetGoogleToken retrieves the stored Google token for a Telegram user.
func (s *Store) GetGoogleToken(ctx context.Context, telegramUserID int64) (*GoogleToken, error) {
	var token GoogleToken
	err := s.db.QueryRowContext(
		ctx,
		`
		SELECT telegram_user_id, access_token, refresh_token, token_type, expiry, scopes
		FROM canvaslink_google_tokens
		WHERE telegram_user_id = $1
		`,
		telegramUserID,
	).Scan(
		&token.TelegramUserID,
		&token.AccessToken,
		&token.RefreshToken,
		&token.TokenType,
		&token.Expiry,
		&token.Scopes,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	token.AccessToken, err = s.decodeSensitive(token.AccessToken, googleAccessTokenPurpose(telegramUserID))
	if err != nil {
		return nil, fmt.Errorf("decrypt Google access token for user %d: %w", telegramUserID, err)
	}
	token.RefreshToken, err = s.decodeSensitive(token.RefreshToken, googleRefreshTokenPurpose(telegramUserID))
	if err != nil {
		return nil, fmt.Errorf("decrypt Google refresh token for user %d: %w", telegramUserID, err)
	}
	return &token, nil
}

// SaveGoogleAuthorization stores a newly authorized grant. Unlike a token
// refresh, an empty refresh token intentionally replaces any previous grant.
func (s *Store) SaveGoogleAuthorization(ctx context.Context, telegramUserID int64, accessToken, refreshToken, tokenType, scopes string, expiry time.Time) error {
	storedAccessToken, err := s.encodeSensitive(accessToken, googleAccessTokenPurpose(telegramUserID))
	if err != nil {
		return err
	}
	storedRefreshToken, err := s.encodeSensitive(refreshToken, googleRefreshTokenPurpose(telegramUserID))
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(
			ctx,
			`UPDATE canvaslink_telegram_accounts
			 SET state_revision = state_revision + 1, updated_at = NOW()
			 WHERE telegram_user_id = $1`,
			telegramUserID,
		)
		if err := requireAffected(result, err); err != nil {
			return err
		}
		_, err = tx.ExecContext(
			ctx,
			`INSERT INTO canvaslink_google_tokens
				(telegram_user_id, access_token, refresh_token, token_type, expiry, scopes, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, NOW())
			 ON CONFLICT (telegram_user_id)
			 DO UPDATE SET
				access_token = EXCLUDED.access_token,
				refresh_token = EXCLUDED.refresh_token,
				token_type = EXCLUDED.token_type,
				expiry = EXCLUDED.expiry,
				scopes = EXCLUDED.scopes,
				updated_at = NOW()`,
			telegramUserID,
			storedAccessToken,
			storedRefreshToken,
			tokenType,
			expiry,
			scopes,
		)
		return err
	})
}

// UpsertGoogleToken persists a refreshed token while retaining an existing
// refresh token when the provider does not return it again.
func (s *Store) UpsertGoogleToken(ctx context.Context, telegramUserID int64, accessToken, refreshToken, tokenType, scopes string, expiry time.Time) error {
	storedAccessToken, err := s.encodeSensitive(accessToken, googleAccessTokenPurpose(telegramUserID))
	if err != nil {
		return err
	}
	storedRefreshToken, err := s.encodeSensitive(refreshToken, googleRefreshTokenPurpose(telegramUserID))
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(
		ctx,
		`
		UPDATE canvaslink_google_tokens
		SET access_token = $2,
			refresh_token = CASE
				WHEN $7 THEN refresh_token
				ELSE $3
			END,
			token_type = $4,
			expiry = $5,
			scopes = $6,
			updated_at = NOW()
		WHERE telegram_user_id = $1
		`,
		telegramUserID,
		storedAccessToken,
		storedRefreshToken,
		tokenType,
		expiry,
		scopes,
		refreshToken == "",
	)
	return requireAffected(result, err)
}

// DeleteGoogleToken removes a stored Google token for a Telegram user.
func (s *Store) DeleteGoogleToken(ctx context.Context, telegramUserID int64) error {
	_, err := s.db.ExecContext(
		ctx,
		`DELETE FROM canvaslink_google_tokens WHERE telegram_user_id = $1`,
		telegramUserID,
	)
	return err
}

// --- Types ---

type Feed struct {
	UserID       int64
	ICalURL      string
	LastSyncedAt *time.Time
}

type CourseTypeSeed struct {
	CourseID       string
	CourseName     string
	AssignmentType string
}

type Course struct {
	ID         int64
	CourseID   string
	CourseName string
}

type CourseSetting struct {
	ID             int64
	CourseID       string
	CourseName     string
	AssignmentType string
	Mode           string
}

type TelegramAccount struct {
	TelegramUserID      int64
	ChatID              int64
	Username            string
	OnboardingStatus    string
	OnboardingSettingID sql.NullInt64
	Timezone            string
	StateRevision       int64
}

type SyncedEventInput struct {
	TelegramUserID   int64
	CourseID         string
	AssignmentType   string
	CanvasEventUID   string
	CanvasTitle      string
	CanvasDueAt      time.Time
	AllDay           bool
	GoogleCalendarID string
	GoogleEventID    string
	CanvasDTStamp    *time.Time
	CanvasSequence   int
	CanvasStatus     string
	CanvasCancelled  bool
	RemovalEligible  bool
	GoogleConfirmed  bool
}

type SyncedEventObservation struct {
	TelegramUserID  int64
	CanvasEventUID  string
	DueAt           *time.Time
	AllDay          bool
	CanvasStatus    string
	CanvasCancelled bool
	RemovalEligible bool
}

// CalendarOwnershipCandidate is a Google event target CanvasLink may own.
// Candidates are always re-verified through Google's private ownership
// markers before a destructive wipe.
type CalendarOwnershipCandidate struct {
	CanvasEventUID   string
	GoogleCalendarID string
	GoogleEventID    string
}

type PendingActionInput struct {
	TelegramUserID int64
	CourseID       string
	AssignmentType string
	CanvasEventUID string
	CanvasTitle    string
	CanvasDueAt    time.Time
	AllDay         bool
	CanvasAllDay   bool
	TelegramChatID int64
}

type PendingAction struct {
	ID                int64
	TelegramUserID    int64
	CourseID          string
	AssignmentType    string
	CanvasEventUID    string
	CanvasTitle       string
	CanvasDueAt       time.Time
	CanvasAllDay      bool
	TelegramChatID    string
	TelegramMessageID sql.NullInt64
	Status            string
}

type PendingCalendarAddInput struct {
	TelegramUserID   int64
	PendingActionID  int64
	GoogleCalendarID string
	GoogleEventID    string
	Description      string
	MaxAttempts      int
}

type GoogleToken struct {
	TelegramUserID int64
	AccessToken    string
	RefreshToken   string
	TokenType      string
	Expiry         time.Time
	Scopes         string
}

type CalendarJobInput struct {
	TelegramUserID       int64
	DedupeKey            string
	Action               string
	SourceUID            string
	CourseID             string
	AssignmentType       string
	Title                string
	Description          string
	DueAt                *time.Time
	AllDay               bool
	GoogleCalendarID     string
	GoogleEventID        string
	CanvasDTStamp        *time.Time
	CanvasSequence       int
	CanvasStatus         string
	CanvasCancelled      bool
	ExpectedMissingSince *time.Time
	ExpectedMissingCount int
	DeleteNotBefore      *time.Time
	MaxAttempts          int
}

type CalendarJob struct {
	ID                   int64
	TelegramUserID       int64
	DedupeKey            string
	Action               string
	SourceUID            string
	CourseID             string
	AssignmentType       string
	Title                string
	Description          string
	DueAt                *time.Time
	AllDay               bool
	GoogleCalendarID     string
	GoogleEventID        string
	CanvasDTStamp        *time.Time
	CanvasSequence       int
	CanvasStatus         string
	CanvasCancelled      bool
	ExpectedMissingSince *time.Time
	ExpectedMissingCount int
	DeleteNotBefore      *time.Time
	Status               string
	Attempts             int
	MaxAttempts          int
	AvailableAt          time.Time
	LeaseOwner           string
	LeaseVersion         int64
	LeaseUntil           *time.Time
	LastError            string
	CreatedAt            time.Time
	UpdatedAt            time.Time
	CompletedAt          *time.Time
}

const (
	// Legacy mode names — kept for backward compatibility with existing DB values
	ModeAuto   = "auto"
	ModeActive = "active"

	// New mode names
	ModeQuiet  = "quiet"  // Sync to Google Calendar silently, no Telegram notification
	ModeNotify = "notify" // Sync to Google Calendar + send Telegram notification
	ModeReview = "review" // Send Telegram notification with confirmation before adding to calendar
	ModeIgnore = "ignore" // Filter out, do nothing

	PendingStatusPending = "pending"
	PendingStatusAdded   = "added"
	PendingStatusIgnored = "ignored"
	// PendingStatusSourceRemoved is an internal, reopenable state. It is
	// intentionally distinct from the user's terminal Ignore decision.
	PendingStatusSourceRemoved = "source_removed"

	OnboardingStatusCourseSetup = "course_setup"

	CalendarActionCreate = "create"
	CalendarActionUpdate = "update"
	CalendarActionDelete = "delete"

	CalendarJobStatusPending    = "pending"
	CalendarJobStatusProcessing = "processing"
	CalendarJobStatusCompleted  = "completed"
	CalendarJobStatusFailed     = "failed"
	CalendarJobStatusCancelled  = "cancelled"
)

// CanonicalMode maps legacy UI values to the worker's behavior vocabulary.
func CanonicalMode(mode string) string {
	switch mode {
	case ModeAuto:
		return ModeQuiet
	case ModeActive:
		return ModeReview
	default:
		return mode
	}
}

func IsValidMode(mode string) bool {
	switch mode {
	case ModeAuto, ModeActive, ModeQuiet, ModeNotify, ModeReview, ModeIgnore:
		return true
	default:
		return false
	}
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func optionalTimesEqual(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func requireAffected(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func ValidateTimezone(timezone string) error {
	timezone = strings.TrimSpace(timezone)
	if timezone == "" || len(timezone) > 100 || strings.ContainsAny(timezone, "\x00\r\n") {
		return fmt.Errorf("%w: %q", ErrInvalidTimezone, timezone)
	}
	if timezone == "Local" {
		return fmt.Errorf("%w: Local depends on the server", ErrInvalidTimezone)
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidTimezone, timezone)
	}
	return nil
}

func normalizeCalendarJobInput(input CalendarJobInput) CalendarJobInput {
	input.DedupeKey = strings.TrimSpace(input.DedupeKey)
	input.Action = strings.ToLower(strings.TrimSpace(input.Action))
	input.SourceUID = strings.TrimSpace(input.SourceUID)
	input.CourseID = strings.TrimSpace(input.CourseID)
	input.AssignmentType = strings.TrimSpace(input.AssignmentType)
	input.GoogleCalendarID = defaultString(input.GoogleCalendarID, "primary")
	input.GoogleEventID = strings.TrimSpace(input.GoogleEventID)
	input.CanvasStatus = strings.TrimSpace(input.CanvasStatus)
	if input.MaxAttempts == 0 {
		input.MaxAttempts = 8
	}
	return input
}

func validateCalendarJobInput(input CalendarJobInput) error {
	if input.TelegramUserID == 0 {
		return fmt.Errorf("%w: Telegram user ID is required", ErrInvalidCalendarJob)
	}
	if input.DedupeKey == "" || len(input.DedupeKey) > 256 || strings.ContainsAny(input.DedupeKey, "\x00\r\n") {
		return fmt.Errorf("%w: invalid dedupe key", ErrInvalidCalendarJob)
	}
	if input.SourceUID == "" || len(input.SourceUID) > 2048 || strings.ContainsRune(input.SourceUID, '\x00') {
		return fmt.Errorf("%w: invalid source UID", ErrInvalidCalendarJob)
	}
	if input.MaxAttempts < 1 || input.MaxAttempts > 100 {
		return fmt.Errorf("%w: max attempts must be between 1 and 100", ErrInvalidCalendarJob)
	}
	switch input.Action {
	case CalendarActionCreate, CalendarActionUpdate:
		if input.DueAt == nil || input.Title == "" {
			return fmt.Errorf("%w: %s jobs require a title and due date", ErrInvalidCalendarJob, input.Action)
		}
		if input.GoogleEventID == "" {
			return fmt.Errorf("%w: %s jobs require a deterministic Google event ID", ErrInvalidCalendarJob, input.Action)
		}
		if input.ExpectedMissingSince != nil || input.ExpectedMissingCount != 0 || input.DeleteNotBefore != nil {
			return fmt.Errorf("%w: %s jobs cannot carry deletion guards", ErrInvalidCalendarJob, input.Action)
		}
	case CalendarActionDelete:
		if input.GoogleEventID == "" {
			return fmt.Errorf("%w: delete jobs require a Google event ID", ErrInvalidCalendarJob)
		}
		if input.ExpectedMissingSince == nil || input.ExpectedMissingCount < 1 || input.DeleteNotBefore == nil {
			return fmt.Errorf("%w: delete jobs require a persisted missing-source guard", ErrInvalidCalendarJob)
		}
		if input.DeleteNotBefore.Before(*input.ExpectedMissingSince) {
			return fmt.Errorf("%w: delete jobs cannot run before the guarded missing observation", ErrInvalidCalendarJob)
		}
		if !input.CanvasCancelled && !input.DeleteNotBefore.After(*input.ExpectedMissingSince) {
			return fmt.Errorf("%w: non-cancelled delete jobs require a positive missing-source grace period", ErrInvalidCalendarJob)
		}
	default:
		return fmt.Errorf("%w: unknown action %q", ErrInvalidCalendarJob, input.Action)
	}
	return nil
}

func truncateStoreError(message string) string {
	const maxErrorLength = 2000
	message = strings.TrimSpace(message)
	if len(message) <= maxErrorLength {
		return message
	}
	return message[:maxErrorLength]
}

const (
	encryptedFieldPrefix           = "enc:v1:"
	fieldPurposeFeedURL            = "canvas-feed-url"
	fieldPurposeOAuthVerifier      = "oauth-code-verifier"
	fieldPurposeGoogleAccessToken  = "google-access-token"
	fieldPurposeGoogleRefreshToken = "google-refresh-token"
)

func feedURLPurpose(userID int64) string {
	return fmt.Sprintf("%s:%d", fieldPurposeFeedURL, userID)
}

func oauthVerifierPurpose(state string) string {
	return fieldPurposeOAuthVerifier + ":" + state
}

func googleAccessTokenPurpose(userID int64) string {
	return fmt.Sprintf("%s:%d", fieldPurposeGoogleAccessToken, userID)
}

func googleRefreshTokenPurpose(userID int64) string {
	return fmt.Sprintf("%s:%d", fieldPurposeGoogleRefreshToken, userID)
}

var encryptionKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var destructiveActionPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

type fieldCipher struct {
	primaryID string
	primary   cipher.AEAD
	keys      map[string]cipher.AEAD
}

func newFieldCipher(config EncryptionConfig) (*fieldCipher, error) {
	keyID := strings.TrimSpace(config.PrimaryKeyID)
	if !encryptionKeyIDPattern.MatchString(keyID) {
		return nil, fmt.Errorf("invalid primary encryption key ID %q", config.PrimaryKeyID)
	}
	primary, err := newGCM(config.PrimaryKey)
	if err != nil {
		return nil, fmt.Errorf("primary encryption key: %w", err)
	}
	fields := &fieldCipher{
		primaryID: keyID,
		primary:   primary,
		keys:      map[string]cipher.AEAD{keyID: primary},
	}
	for previousID, previousKey := range config.PreviousKeys {
		previousID = strings.TrimSpace(previousID)
		if !encryptionKeyIDPattern.MatchString(previousID) {
			return nil, fmt.Errorf("invalid previous encryption key ID %q", previousID)
		}
		if previousID == keyID {
			return nil, fmt.Errorf("previous encryption key ID %q duplicates the primary key", previousID)
		}
		aead, err := newGCM(previousKey)
		if err != nil {
			return nil, fmt.Errorf("previous encryption key %q: %w", previousID, err)
		}
		fields.keys[previousID] = aead
	}
	return fields, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	switch len(key) {
	case 16, 24, 32:
	default:
		return nil, fmt.Errorf("must contain 16, 24, or 32 bytes (got %d)", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (fields *fieldCipher) encrypt(plaintext, purpose string) (string, error) {
	nonce := make([]byte, fields.primary.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate encryption nonce: %w", err)
	}
	sealed := fields.primary.Seal(nonce, nonce, []byte(plaintext), []byte(purpose))
	return encryptedFieldPrefix + fields.primaryID + ":" +
		base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (fields *fieldCipher) decrypt(stored, purpose string) (plaintext string, needsRotation bool, err error) {
	if !strings.HasPrefix(stored, "enc:") {
		return stored, true, nil
	}
	if !strings.HasPrefix(stored, encryptedFieldPrefix) {
		return "", false, fmt.Errorf("unsupported encrypted field format")
	}
	parts := strings.SplitN(strings.TrimPrefix(stored, encryptedFieldPrefix), ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false, fmt.Errorf("malformed encrypted field")
	}
	keyID := parts[0]
	aead, ok := fields.keys[keyID]
	if !ok {
		return "", false, fmt.Errorf("%w: %q", ErrUnknownEncryptionKey, keyID)
	}
	sealed, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false, fmt.Errorf("decode encrypted field: %w", err)
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return "", false, fmt.Errorf("encrypted field is truncated")
	}
	nonce, ciphertext := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	opened, err := aead.Open(nil, nonce, ciphertext, []byte(purpose))
	if err != nil {
		return "", false, fmt.Errorf("authenticate encrypted field: %w", err)
	}
	return string(opened), keyID != fields.primaryID, nil
}

func (s *Store) encodeSensitive(plaintext, purpose string) (string, error) {
	if s.fields == nil {
		return plaintext, nil
	}
	return s.fields.encrypt(plaintext, purpose)
}

func (s *Store) decodeSensitive(stored, purpose string) (string, error) {
	if s.fields == nil {
		if strings.HasPrefix(stored, "enc:") {
			return "", ErrEncryptionRequired
		}
		return stored, nil
	}
	plaintext, _, err := s.fields.decrypt(stored, purpose)
	return plaintext, err
}

func (s *Store) migrateSensitiveFields(ctx context.Context, tx *sql.Tx) error {
	if err := s.migrateFeeds(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateOAuthVerifiers(ctx, tx); err != nil {
		return err
	}
	return s.migrateGoogleTokens(ctx, tx)
}

func (s *Store) migrateFeeds(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT telegram_user_id, ical_url FROM canvaslink_feeds`)
	if err != nil {
		return err
	}
	type record struct {
		userID int64
		value  string
	}
	var records []record
	for rows.Next() {
		var row record
		if err := rows.Scan(&row.userID, &row.value); err != nil {
			rows.Close()
			return err
		}
		records = append(records, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, record := range records {
		purpose := feedURLPurpose(record.userID)
		plaintext, rotate, err := s.fields.decrypt(record.value, purpose)
		if err != nil {
			return fmt.Errorf("feed user %d: %w", record.userID, err)
		}
		if !rotate {
			continue
		}
		encrypted, err := s.fields.encrypt(plaintext, purpose)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE canvaslink_feeds
			 SET ical_url = $2, updated_at = NOW()
			 WHERE telegram_user_id = $1 AND ical_url = $3`,
			record.userID,
			encrypted,
			record.value,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateOAuthVerifiers(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT state, code_verifier FROM canvaslink_oauth_states`)
	if err != nil {
		return err
	}
	type record struct {
		state string
		value string
	}
	var records []record
	for rows.Next() {
		var row record
		if err := rows.Scan(&row.state, &row.value); err != nil {
			rows.Close()
			return err
		}
		records = append(records, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, record := range records {
		purpose := oauthVerifierPurpose(record.state)
		plaintext, rotate, err := s.fields.decrypt(record.value, purpose)
		if err != nil {
			return fmt.Errorf("decrypt OAuth state verifier: %w", err)
		}
		if !rotate {
			continue
		}
		encrypted, err := s.fields.encrypt(plaintext, purpose)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE canvaslink_oauth_states
			 SET code_verifier = $2
			 WHERE state = $1 AND code_verifier = $3`,
			record.state,
			encrypted,
			record.value,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateGoogleTokens(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(
		ctx,
		`SELECT telegram_user_id, access_token, refresh_token FROM canvaslink_google_tokens`,
	)
	if err != nil {
		return err
	}
	type record struct {
		userID       int64
		accessToken  string
		refreshToken string
	}
	var records []record
	for rows.Next() {
		var row record
		if err := rows.Scan(&row.userID, &row.accessToken, &row.refreshToken); err != nil {
			rows.Close()
			return err
		}
		records = append(records, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, record := range records {
		accessPurpose := googleAccessTokenPurpose(record.userID)
		refreshPurpose := googleRefreshTokenPurpose(record.userID)
		access, rotateAccess, err := s.fields.decrypt(record.accessToken, accessPurpose)
		if err != nil {
			return fmt.Errorf("Google access token user %d: %w", record.userID, err)
		}
		refresh, rotateRefresh, err := s.fields.decrypt(record.refreshToken, refreshPurpose)
		if err != nil {
			return fmt.Errorf("Google refresh token user %d: %w", record.userID, err)
		}
		if !rotateAccess && !rotateRefresh {
			continue
		}
		encryptedAccess, err := s.fields.encrypt(access, accessPurpose)
		if err != nil {
			return err
		}
		encryptedRefresh, err := s.fields.encrypt(refresh, refreshPurpose)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE canvaslink_google_tokens
			 SET access_token = $2, refresh_token = $3, updated_at = NOW()
			 WHERE telegram_user_id = $1
			   AND access_token = $4
			   AND refresh_token = $5`,
			record.userID,
			encryptedAccess,
			encryptedRefresh,
			record.accessToken,
			record.refreshToken,
		); err != nil {
			return err
		}
	}
	return nil
}

// --- Schema DDL ---

const canvaslinkFeedsTable = `
CREATE TABLE IF NOT EXISTS canvaslink_feeds (
	telegram_user_id BIGINT PRIMARY KEY,
	ical_url TEXT NOT NULL,
	sync_enabled BOOLEAN NOT NULL DEFAULT TRUE,
	last_synced_at TIMESTAMPTZ,
	sync_interval TEXT NOT NULL DEFAULT '1h',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`

const canvaslinkTelegramAccountsTable = `
CREATE TABLE IF NOT EXISTS canvaslink_telegram_accounts (
	telegram_user_id BIGINT PRIMARY KEY,
	chat_id BIGINT NOT NULL,
	username TEXT NOT NULL DEFAULT '',
	onboarding_status TEXT NOT NULL DEFAULT '',
	onboarding_setting_id BIGINT,
	timezone TEXT NOT NULL DEFAULT 'UTC',
	state_revision BIGINT NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`

const canvaslinkCourseSettingsTable = `
CREATE TABLE IF NOT EXISTS canvaslink_course_type_settings (
	id BIGSERIAL PRIMARY KEY,
	telegram_user_id BIGINT NOT NULL,
	course_id TEXT NOT NULL,
	course_name TEXT NOT NULL DEFAULT '',
	assignment_type TEXT NOT NULL,
	mode TEXT NOT NULL CHECK (mode IN ('quiet', 'notify', 'review', 'ignore', 'auto', 'active')),
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	UNIQUE (telegram_user_id, course_id, assignment_type)
);
`

const canvaslinkSyncedEventsTable = `
CREATE TABLE IF NOT EXISTS canvaslink_synced_events (
	id BIGSERIAL PRIMARY KEY,
	telegram_user_id BIGINT NOT NULL,
	course_id TEXT NOT NULL,
	assignment_type TEXT NOT NULL,
	canvas_event_uid TEXT NOT NULL,
	canvas_title TEXT NOT NULL,
	canvas_due_at TIMESTAMPTZ NOT NULL,
	all_day BOOLEAN NOT NULL DEFAULT FALSE,
	google_calendar_id TEXT NOT NULL DEFAULT 'primary',
	google_event_id TEXT NOT NULL DEFAULT '',
	canvas_dtstamp TIMESTAMPTZ,
	canvas_sequence INT NOT NULL DEFAULT 0,
	canvas_status TEXT NOT NULL DEFAULT '',
	canvas_cancelled BOOLEAN NOT NULL DEFAULT FALSE,
	missing_since TIMESTAMPTZ,
	missing_count INT NOT NULL DEFAULT 0,
	detached BOOLEAN NOT NULL DEFAULT FALSE,
	removal_eligible BOOLEAN NOT NULL DEFAULT TRUE,
	google_confirmed BOOLEAN NOT NULL DEFAULT TRUE,
	synced_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	UNIQUE (telegram_user_id, canvas_event_uid)
);
`

const canvaslinkPendingActionsTable = `
CREATE TABLE IF NOT EXISTS canvaslink_pending_actions (
	id BIGSERIAL PRIMARY KEY,
	telegram_user_id BIGINT NOT NULL,
	course_id TEXT NOT NULL,
	assignment_type TEXT NOT NULL,
	canvas_event_uid TEXT NOT NULL,
	canvas_title TEXT NOT NULL,
	canvas_due_at TIMESTAMPTZ NOT NULL,
	all_day BOOLEAN NOT NULL DEFAULT FALSE,
	telegram_chat_id TEXT NOT NULL,
	telegram_message_id BIGINT,
	status TEXT NOT NULL CHECK (status IN ('pending', 'added', 'ignored', 'source_removed')) DEFAULT 'pending',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	UNIQUE (telegram_user_id, canvas_event_uid)
);
`

const migratePendingActionStatusConstraint = `
DO $migration$
BEGIN
	IF NOT EXISTS (
		SELECT 1
		FROM pg_constraint
		WHERE conrelid = 'canvaslink_pending_actions'::regclass
		  AND conname = 'canvaslink_pending_actions_status_check'
		  AND POSITION('source_removed' IN pg_get_constraintdef(oid)) > 0
	) THEN
		ALTER TABLE canvaslink_pending_actions
			DROP CONSTRAINT IF EXISTS canvaslink_pending_actions_status_check;
		ALTER TABLE canvaslink_pending_actions
			ADD CONSTRAINT canvaslink_pending_actions_status_check
			CHECK (status IN ('pending', 'added', 'ignored', 'source_removed'));
	END IF;
END;
$migration$
`

const canvaslinkOAuthStatesTable = `
CREATE TABLE IF NOT EXISTS canvaslink_oauth_states (
	state TEXT PRIMARY KEY,
	telegram_user_id BIGINT NOT NULL,
	code_verifier TEXT NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL,
	private_chat_bound BOOLEAN NOT NULL DEFAULT FALSE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`

// Existing OAuth rows predate private-chat enforcement and cannot prove where
// their links were issued. The fail-closed default marks them untrusted, then
// the startup migration invalidates them. New states explicitly write TRUE.
const addOAuthStatePrivateChatBindingColumn = `
ALTER TABLE canvaslink_oauth_states
ADD COLUMN IF NOT EXISTS private_chat_bound BOOLEAN NOT NULL DEFAULT FALSE
`

const invalidateUnboundOAuthStates = `
DELETE FROM canvaslink_oauth_states
WHERE private_chat_bound = FALSE
`

const canvaslinkGoogleTokensTable = `
CREATE TABLE IF NOT EXISTS canvaslink_google_tokens (
	telegram_user_id BIGINT PRIMARY KEY,
	access_token TEXT NOT NULL,
	refresh_token TEXT NOT NULL DEFAULT '',
	token_type TEXT NOT NULL DEFAULT 'Bearer',
	expiry TIMESTAMPTZ NOT NULL,
	scopes TEXT NOT NULL DEFAULT '',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`

const canvaslinkCalendarJobsTable = `
CREATE TABLE IF NOT EXISTS canvaslink_calendar_jobs (
	id BIGSERIAL PRIMARY KEY,
	telegram_user_id BIGINT NOT NULL,
	dedupe_key TEXT NOT NULL,
	action TEXT NOT NULL CHECK (action IN ('create', 'update', 'delete')),
	source_uid TEXT NOT NULL,
	course_id TEXT NOT NULL DEFAULT '',
	assignment_type TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	due_at TIMESTAMPTZ,
	all_day BOOLEAN NOT NULL DEFAULT FALSE,
	google_calendar_id TEXT NOT NULL DEFAULT 'primary',
	google_event_id TEXT NOT NULL DEFAULT '',
	canvas_dtstamp TIMESTAMPTZ,
	canvas_sequence INT NOT NULL DEFAULT 0,
	canvas_status TEXT NOT NULL DEFAULT '',
	canvas_cancelled BOOLEAN NOT NULL DEFAULT FALSE,
	expected_missing_since TIMESTAMPTZ,
	expected_missing_count INT NOT NULL DEFAULT 0,
	delete_not_before TIMESTAMPTZ,
	status TEXT NOT NULL DEFAULT 'pending'
		CHECK (status IN ('pending', 'processing', 'completed', 'failed', 'cancelled')),
	attempts INT NOT NULL DEFAULT 0 CHECK (attempts >= 0),
	max_attempts INT NOT NULL DEFAULT 8 CHECK (max_attempts > 0),
	available_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	lease_owner TEXT NOT NULL DEFAULT '',
	lease_version BIGINT NOT NULL DEFAULT 0,
	lease_until TIMESTAMPTZ,
	last_error TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	completed_at TIMESTAMPTZ
);
`

const canvaslinkDestructiveConfirmationsTable = `
CREATE TABLE IF NOT EXISTS canvaslink_destructive_confirmations (
	nonce TEXT PRIMARY KEY,
	telegram_user_id BIGINT NOT NULL,
	action TEXT NOT NULL,
	state_revision BIGINT NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`
