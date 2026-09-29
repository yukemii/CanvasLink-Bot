package store

import (
	"context"
	"errors"
	"time"
)

const runtimeSchema = `
CREATE TABLE IF NOT EXISTS canvaslink_telegram_receipts (
 update_id BIGINT PRIMARY KEY,
 received_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS canvaslink_telegram_receipts_age ON canvaslink_telegram_receipts(received_at);
ALTER TABLE canvaslink_feeds ADD COLUMN IF NOT EXISTS last_attempted_at TIMESTAMPTZ;
`

// TryRuntimeLock uses a namespace distinct from user-operation locks so handlers
// may acquire those locks themselves without deadlocking. Locks span revisions.
func (s *Store) TryRuntimeLock(ctx context.Context, name string) (func() error, bool, error) {
	pool := s.lockDB
	if pool == nil {
		pool = s.db
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	const expr = `hashtextextended('canvaslink:runtime:' || $1::text, 0)`
	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(`+expr+`)`, name).Scan(&acquired); err != nil {
		return nil, false, errors.Join(err, closeAdvisoryLockConnection(conn, true))
	}
	if !acquired {
		return nil, false, conn.Close()
	}
	release := makeAdvisoryLockRelease(func() (bool, error) {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		err := conn.QueryRowContext(unlockCtx, `SELECT pg_advisory_unlock(`+expr+`)`, name).Scan(&unlocked)
		return unlocked, err
	}, func(discard bool) error { return closeAdvisoryLockConnection(conn, discard) })
	return release, true, nil
}

// ClaimTelegramUpdate deliberately reserves before dispatch. A crash after this
// commit must not replay a non-idempotent command (e.g. creating a personal task).
// We store no message bodies. A user may need to repeat a command after a crash.
func (s *Store) ClaimTelegramUpdate(ctx context.Context, id int) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO canvaslink_telegram_receipts(update_id) VALUES($1) ON CONFLICT DO NOTHING`, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) PruneTelegramReceipts(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM canvaslink_telegram_receipts WHERE received_at < NOW() - INTERVAL '7 days'`)
	return err
}

// ReserveFeedCheck persists cadence across cold starts, including failed checks.
func (s *Store) ReserveFeedCheck(ctx context.Context, user int64, interval time.Duration) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE canvaslink_feeds f SET last_attempted_at=NOW()
 WHERE telegram_user_id=$1 AND sync_enabled=TRUE
 AND (last_attempted_at IS NULL OR last_attempted_at <= NOW() - $2 * INTERVAL '1 second')
 AND (last_synced_at IS NULL OR last_synced_at <= NOW() - $2 * INTERVAL '1 second')
 AND EXISTS(SELECT 1 FROM canvaslink_telegram_accounts a WHERE a.telegram_user_id=f.telegram_user_id AND a.onboarding_status='')`, user, interval.Seconds())
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
