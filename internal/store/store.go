package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Store struct {
	db *sql.DB
}

func Connect(databaseURL string) (*Store, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.Ping(); err != nil {
		return nil, err
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) InitSchema(ctx context.Context) error {
	statements := []string{
		canvaslinkTelegramAccountsTable,
		canvaslinkFeedsTable,
		canvaslinkCourseSettingsTable,
		canvaslinkSyncedEventsTable,
		canvaslinkPendingActionsTable,
		canvaslinkOAuthStatesTable,
		canvaslinkGoogleTokensTable,
	}

	for _, q := range statements {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
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
	}

	for _, q := range migrations {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}

	return nil
}

// --- Feeds ---

func (s *Store) UpsertFeed(ctx context.Context, userID int64, icalURL string) error {
	_, err := s.db.ExecContext(
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
		icalURL,
	)
	return err
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
		feeds = append(feeds, feed)
	}
	return feeds, rows.Err()
}

func (s *Store) TouchFeedSync(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(
		ctx,
		`
		UPDATE canvaslink_feeds
		SET last_synced_at = NOW(), updated_at = NOW()
		WHERE telegram_user_id = $1
		`,
		userID,
	)
	return err
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
	return &feed, nil
}

// UpdateSyncInterval stores the sync interval preference for a user.
func (s *Store) UpdateSyncInterval(ctx context.Context, userID int64, interval string) error {
	_, err := s.db.ExecContext(
		ctx,
		`
		UPDATE canvaslink_feeds
		SET sync_interval = $2, updated_at = NOW()
		WHERE telegram_user_id = $1
		`,
		userID, interval,
	)
	return err
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

// DeleteFeed removes a user's feed and all associated settings, synced events, and pending actions.
func (s *Store) DeleteFeed(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM canvaslink_feeds WHERE telegram_user_id = $1`, userID)
	return err
}

// DeleteAllCourseSettings removes all course type settings for a user.
func (s *Store) DeleteAllCourseSettings(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM canvaslink_course_type_settings WHERE telegram_user_id = $1`, userID)
	return err
}

// DeleteAllSyncedEvents removes all synced events for a user.
func (s *Store) DeleteAllSyncedEvents(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM canvaslink_synced_events WHERE telegram_user_id = $1`, userID)
	return err
}

// DeleteAllPendingActions removes all pending actions for a user.
func (s *Store) DeleteAllPendingActions(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM canvaslink_pending_actions WHERE telegram_user_id = $1`, userID)
	return err
}

// DeleteTelegramAccount removes a user's telegram account record.
func (s *Store) DeleteTelegramAccount(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM canvaslink_telegram_accounts WHERE telegram_user_id = $1`, userID)
	return err
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
	_, err := s.db.ExecContext(
		ctx,
		`
		INSERT INTO canvaslink_telegram_accounts (telegram_user_id, chat_id, username)
		VALUES ($1, $2, $3)
		ON CONFLICT (telegram_user_id)
		DO UPDATE SET
			chat_id = EXCLUDED.chat_id,
			username = EXCLUDED.username,
			updated_at = NOW()
		`,
		telegramUserID,
		chatID,
		username,
	)
	return err
}

func (s *Store) GetTelegramAccount(ctx context.Context, userID int64) (*TelegramAccount, error) {
	var row TelegramAccount
	err := s.db.QueryRowContext(
		ctx,
		`
		SELECT telegram_user_id, chat_id, username, onboarding_status
		FROM canvaslink_telegram_accounts
		WHERE telegram_user_id = $1
		`,
		userID,
	).Scan(&row.TelegramUserID, &row.ChatID, &row.Username, &row.OnboardingStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *Store) SetOnboardingStatus(ctx context.Context, userID int64, status string) error {
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE canvaslink_telegram_accounts SET onboarding_status = $2, updated_at = NOW() WHERE telegram_user_id = $1`,
		userID,
		status,
	)
	return err
}

// --- Course Settings ---

func (s *Store) SeedDefaultSettings(ctx context.Context, userID int64, rows []CourseTypeSeed) error {
	if len(rows) == 0 {
		return nil
	}
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
		_, err := s.db.ExecContext(
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
			return err
		}
	}
	return nil
}

func (s *Store) ListUserCourses(ctx context.Context, userID int64) ([]Course, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`
		SELECT DISTINCT course_id, course_name
		FROM canvaslink_course_type_settings
		WHERE telegram_user_id = $1
		ORDER BY course_name ASC, course_id ASC
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
		if err := rows.Scan(&c.CourseID, &c.CourseName); err != nil {
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
		SELECT course_id, course_name, assignment_type, mode
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
		if err := rows.Scan(&r.CourseID, &r.CourseName, &r.AssignmentType, &r.Mode); err != nil {
			return nil, err
		}
		settings = append(settings, r)
	}
	return settings, rows.Err()
}

func (s *Store) SetCourseTypeMode(ctx context.Context, userID int64, courseID, assignmentType, mode string) error {
	_, err := s.db.ExecContext(
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
	return err
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
	return mode, err
}

// --- Synced Events ---

// SyncedEvent represents a row from canvaslink_synced_events.
type SyncedEvent struct {
	ID              int64
	TelegramUserID  int64
	CourseID        string
	AssignmentType  string
	CanvasEventUID  string
	CanvasTitle     string
	CanvasDueAt     time.Time
	GoogleCalendarID string
	GoogleEventID   string
	CanvasDTStamp   *time.Time
	CanvasSequence  int
	SyncedAt        time.Time
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
				google_calendar_id,
				google_event_id,
				canvas_dtstamp,
				canvas_sequence
			)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
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
		defaultString(input.GoogleCalendarID, "primary"),
		input.GoogleEventID,
		input.CanvasDTStamp,
		input.CanvasSequence,
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
			canvas_title = $3,
			canvas_due_at = $4,
			canvas_dtstamp = $5,
			canvas_sequence = $6,
			updated_at = NOW()
		WHERE telegram_user_id = $1 AND canvas_event_uid = $2
		`,
		input.TelegramUserID,
		input.CanvasEventUID,
		input.CanvasTitle,
		input.CanvasDueAt,
		input.CanvasDTStamp,
		input.CanvasSequence,
	)
	if err != nil {
		return false, err
	}
	rows, _ := result.RowsAffected()
	return rows > 0, nil
}

// DeleteSyncedEvent removes a synced event (e.g., when a Canvas event is removed).
func (s *Store) DeleteSyncedEvent(ctx context.Context, userID int64, canvasEventUID string) error {
	_, err := s.db.ExecContext(
		ctx,
		`
		DELETE FROM canvaslink_synced_events
		WHERE telegram_user_id = $1 AND canvas_event_uid = $2
		`,
		userID,
		canvasEventUID,
	)
	return err
}

// ListSyncedEventsByCourse returns all synced events for a specific course.
func (s *Store) ListSyncedEventsByCourse(ctx context.Context, userID int64, courseID string) ([]SyncedEvent, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`
		SELECT id, telegram_user_id, course_id, assignment_type, canvas_event_uid,
			canvas_title, canvas_due_at, google_calendar_id, google_event_id,
			canvas_dtstamp, canvas_sequence, synced_at
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
		if err := rows.Scan(
			&ev.ID, &ev.TelegramUserID, &ev.CourseID, &ev.AssignmentType,
			&ev.CanvasEventUID, &ev.CanvasTitle, &ev.CanvasDueAt,
			&ev.GoogleCalendarID, &ev.GoogleEventID,
			&ev.CanvasDTStamp, &ev.CanvasSequence, &ev.SyncedAt,
		); err != nil {
			return nil, err
		}
		result = append(result, ev)
	}
	return result, rows.Err()
}

// DeleteSyncedEventsByCourse removes all synced events for a specific course.
func (s *Store) DeleteSyncedEventsByCourse(ctx context.Context, userID int64, courseID string) error {
	_, err := s.db.ExecContext(
		ctx,
		`
		DELETE FROM canvaslink_synced_events
		WHERE telegram_user_id = $1 AND course_id = $2
		`,
		userID, courseID,
	)
	return err
}

// ListSyncedEvents returns all synced events for a user, keyed by canvas_event_uid.
func (s *Store) ListSyncedEvents(ctx context.Context, userID int64) (map[string]SyncedEvent, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`
		SELECT id, telegram_user_id, course_id, assignment_type, canvas_event_uid,
			canvas_title, canvas_due_at, google_calendar_id, google_event_id,
			canvas_dtstamp, canvas_sequence, synced_at
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
		if err := rows.Scan(
			&ev.ID, &ev.TelegramUserID, &ev.CourseID, &ev.AssignmentType,
			&ev.CanvasEventUID, &ev.CanvasTitle, &ev.CanvasDueAt,
			&ev.GoogleCalendarID, &ev.GoogleEventID,
			&ev.CanvasDTStamp, &ev.CanvasSequence, &ev.SyncedAt,
		); err != nil {
			return nil, err
		}
		result[ev.CanvasEventUID] = ev
	}
	return result, rows.Err()
}

// --- Pending Actions ---

func (s *Store) CreatePendingActionIfAbsent(ctx context.Context, input PendingActionInput) (*PendingAction, bool, error) {
	var action PendingAction
	var inserted bool
	err := s.db.QueryRowContext(
		ctx,
		`
		WITH inserted AS (
			INSERT INTO canvaslink_pending_actions (
				telegram_user_id,
				course_id,
				assignment_type,
				canvas_event_uid,
				canvas_title,
				canvas_due_at,
				telegram_chat_id
			)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (telegram_user_id, canvas_event_uid) DO NOTHING
			RETURNING id, telegram_user_id, course_id, assignment_type, canvas_event_uid, canvas_title, canvas_due_at, telegram_chat_id, telegram_message_id, status
		)
		SELECT id, telegram_user_id, course_id, assignment_type, canvas_event_uid, canvas_title, canvas_due_at, telegram_chat_id, telegram_message_id, status, true
		FROM inserted
		UNION ALL
		SELECT id, telegram_user_id, course_id, assignment_type, canvas_event_uid, canvas_title, canvas_due_at, telegram_chat_id, telegram_message_id, status, false
		FROM canvaslink_pending_actions
		WHERE telegram_user_id = $1 AND canvas_event_uid = $4
		LIMIT 1
		`,
		input.TelegramUserID,
		input.CourseID,
		input.AssignmentType,
		input.CanvasEventUID,
		input.CanvasTitle,
		input.CanvasDueAt,
		fmt.Sprintf("%d", input.TelegramChatID),
	).Scan(
		&action.ID,
		&action.TelegramUserID,
		&action.CourseID,
		&action.AssignmentType,
		&action.CanvasEventUID,
		&action.CanvasTitle,
		&action.CanvasDueAt,
		&action.TelegramChatID,
		&action.TelegramMessageID,
		&action.Status,
		&inserted,
	)
	if err != nil {
		return nil, false, err
	}
	return &action, inserted, nil
}

func (s *Store) SetPendingMessageID(ctx context.Context, userID int64, pendingID int64, messageID int) error {
	_, err := s.db.ExecContext(
		ctx,
		`
		UPDATE canvaslink_pending_actions
		SET telegram_message_id = $3, updated_at = NOW()
		WHERE telegram_user_id = $1 AND id = $2
		`,
		userID,
		pendingID,
		messageID,
	)
	return err
}

func (s *Store) GetPendingAction(ctx context.Context, userID int64, pendingID int64) (*PendingAction, error) {
	var action PendingAction
	err := s.db.QueryRowContext(
		ctx,
		`
		SELECT id, telegram_user_id, course_id, assignment_type, canvas_event_uid, canvas_title, canvas_due_at, telegram_chat_id, telegram_message_id, status
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
	_, err := s.db.ExecContext(
		ctx,
		`
		UPDATE canvaslink_pending_actions
		SET status = $3, updated_at = NOW()
		WHERE telegram_user_id = $1 AND id = $2
		`,
		userID,
		pendingID,
		status,
	)
	return err
}

// --- CanvasLink-Owned Google OAuth ---

// CreateOAuthState stores a new OAuth state for a Telegram user.
func (s *Store) CreateOAuthState(ctx context.Context, state string, telegramUserID int64, codeVerifier string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(
		ctx,
		`
		INSERT INTO canvaslink_oauth_states (state, telegram_user_id, code_verifier, expires_at)
		VALUES ($1, $2, $3, $4)
		`,
		state,
		telegramUserID,
		codeVerifier,
		expiresAt,
	)
	return err
}

// ConsumeOAuthState atomically reads and deletes an OAuth state (one-time use).
func (s *Store) ConsumeOAuthState(ctx context.Context, state string) (int64, string, error) {
	var telegramUserID int64
	var codeVerifier string
	err := s.db.QueryRowContext(
		ctx,
		`
		DELETE FROM canvaslink_oauth_states
		WHERE state = $1 AND expires_at > NOW()
		RETURNING telegram_user_id, code_verifier
		`,
		state,
	).Scan(&telegramUserID, &codeVerifier)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", err
	}
	return telegramUserID, codeVerifier, nil
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
	return &token, nil
}

// UpsertGoogleToken stores or updates a Google token for a Telegram user.
func (s *Store) UpsertGoogleToken(ctx context.Context, telegramUserID int64, accessToken, refreshToken, tokenType, scopes string, expiry time.Time) error {
	_, err := s.db.ExecContext(
		ctx,
		`
		INSERT INTO canvaslink_google_tokens (telegram_user_id, access_token, refresh_token, token_type, expiry, scopes, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (telegram_user_id)
		DO UPDATE SET
			access_token = EXCLUDED.access_token,
			refresh_token = CASE
				WHEN EXCLUDED.refresh_token = '' THEN canvaslink_google_tokens.refresh_token
				ELSE EXCLUDED.refresh_token
			END,
			token_type = EXCLUDED.token_type,
			expiry = EXCLUDED.expiry,
			scopes = EXCLUDED.scopes,
			updated_at = NOW()
		`,
		telegramUserID,
		accessToken,
		refreshToken,
		tokenType,
		expiry,
		scopes,
	)
	return err
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
	CourseID   string
	CourseName string
}

type CourseSetting struct {
	CourseID       string
	CourseName     string
	AssignmentType string
	Mode           string
}

type TelegramAccount struct {
	TelegramUserID   int64
	ChatID           int64
	Username         string
	OnboardingStatus string
}

type SyncedEventInput struct {
	TelegramUserID   int64
	CourseID         string
	AssignmentType   string
	CanvasEventUID   string
	CanvasTitle      string
	CanvasDueAt      time.Time
	GoogleCalendarID string
	GoogleEventID    string
	CanvasDTStamp    *time.Time
	CanvasSequence   int
}

type PendingActionInput struct {
	TelegramUserID int64
	CourseID       string
	AssignmentType string
	CanvasEventUID string
	CanvasTitle    string
	CanvasDueAt    time.Time
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
	TelegramChatID    string
	TelegramMessageID sql.NullInt64
	Status            string
}

type GoogleToken struct {
	TelegramUserID int64
	AccessToken    string
	RefreshToken   string
	TokenType      string
	Expiry         time.Time
	Scopes         string
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
)

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// --- Schema DDL ---

const canvaslinkFeedsTable = `
CREATE TABLE IF NOT EXISTS canvaslink_feeds (
	telegram_user_id BIGINT PRIMARY KEY,
	ical_url TEXT NOT NULL,
	sync_enabled BOOLEAN NOT NULL DEFAULT TRUE,
	last_synced_at TIMESTAMPTZ,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`

const canvaslinkTelegramAccountsTable = `
CREATE TABLE IF NOT EXISTS canvaslink_telegram_accounts (
	telegram_user_id BIGINT PRIMARY KEY,
	chat_id BIGINT NOT NULL,
	username TEXT NOT NULL DEFAULT '',
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
	mode TEXT NOT NULL CHECK (mode IN ('auto', 'active', 'ignore')),
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
	google_calendar_id TEXT NOT NULL DEFAULT 'primary',
	google_event_id TEXT NOT NULL DEFAULT '',
	canvas_dtstamp TIMESTAMPTZ,
	canvas_sequence INT NOT NULL DEFAULT 0,
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
	telegram_chat_id TEXT NOT NULL,
	telegram_message_id BIGINT,
	status TEXT NOT NULL CHECK (status IN ('pending', 'added', 'ignored')) DEFAULT 'pending',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	UNIQUE (telegram_user_id, canvas_event_uid)
);
`

const canvaslinkOAuthStatesTable = `
CREATE TABLE IF NOT EXISTS canvaslink_oauth_states (
	state TEXT PRIMARY KEY,
	telegram_user_id BIGINT NOT NULL,
	code_verifier TEXT NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
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
