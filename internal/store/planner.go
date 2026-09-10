package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const plannerSchema = `
CREATE TABLE IF NOT EXISTS canvaslink_preferences (
 telegram_user_id BIGINT PRIMARY KEY REFERENCES canvaslink_telegram_accounts(telegram_user_id) ON DELETE CASCADE,
 settings JSONB NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS canvaslink_tasks (
 id BIGSERIAL PRIMARY KEY,
 telegram_user_id BIGINT NOT NULL REFERENCES canvaslink_telegram_accounts(telegram_user_id) ON DELETE CASCADE,
 source_uid TEXT NOT NULL, title TEXT NOT NULL, course_id TEXT NOT NULL DEFAULT '', assignment_type TEXT NOT NULL DEFAULT '',
 due_at TIMESTAMPTZ NOT NULL, all_day BOOLEAN NOT NULL DEFAULT FALSE, source_url TEXT NOT NULL DEFAULT '',
 manual BOOLEAN NOT NULL DEFAULT FALSE, present BOOLEAN NOT NULL DEFAULT TRUE, done BOOLEAN NOT NULL DEFAULT FALSE,
 target_at TIMESTAMPTZ, snoozed_until TIMESTAMPTZ, revision BIGINT NOT NULL DEFAULT 1,
 UNIQUE(telegram_user_id, source_uid)
);
CREATE INDEX IF NOT EXISTS canvaslink_tasks_user_due ON canvaslink_tasks(telegram_user_id,due_at);
CREATE TABLE IF NOT EXISTS canvaslink_deliveries (
 id BIGSERIAL PRIMARY KEY, telegram_user_id BIGINT NOT NULL REFERENCES canvaslink_telegram_accounts(telegram_user_id) ON DELETE CASCADE,
 dedupe_key TEXT NOT NULL, kind TEXT NOT NULL, task_id BIGINT REFERENCES canvaslink_tasks(id) ON DELETE CASCADE,
 revision BIGINT NOT NULL DEFAULT 0, body TEXT NOT NULL, expires_at TIMESTAMPTZ NOT NULL,
 next_attempt TIMESTAMPTZ NOT NULL DEFAULT NOW(), sent BOOLEAN NOT NULL DEFAULT FALSE,
 UNIQUE(telegram_user_id,dedupe_key)
);
CREATE TABLE IF NOT EXISTS canvaslink_connection_health (
 telegram_user_id BIGINT NOT NULL REFERENCES canvaslink_telegram_accounts(telegram_user_id) ON DELETE CASCADE,
 service TEXT NOT NULL, failures INT NOT NULL DEFAULT 0, alerted BOOLEAN NOT NULL DEFAULT FALSE,
 last_success TIMESTAMPTZ, episode BIGINT NOT NULL DEFAULT 0,
 PRIMARY KEY(telegram_user_id,service)
);
CREATE TABLE IF NOT EXISTS canvaslink_planner_inputs (
 telegram_user_id BIGINT PRIMARY KEY REFERENCES canvaslink_telegram_accounts(telegram_user_id) ON DELETE CASCADE,
 action TEXT NOT NULL, expires_at TIMESTAMPTZ NOT NULL
);`

// Preferences are independent from calendar sync modes. Empty offsets explicitly disable reminders.
type PlannerPreferences struct {
	Reminders    []int             `json:"reminders"`
	Overrides    map[string][]int  `json:"overrides"`
	Daily        bool              `json:"daily"`
	DailyTime    string            `json:"daily_time"`
	Weekly       bool              `json:"weekly"`
	WeeklyTime   string            `json:"weekly_time"`
	Weekday      int               `json:"weekday"`
	QuietStart   int               `json:"quiet_start"`
	QuietEnd     int               `json:"quiet_end"`
	CalendarID   string            `json:"calendar_id"`
	CalendarName string            `json:"calendar_name"`
	TitleStyle   string            `json:"title_style"`
	Colors       map[string]string `json:"colors"`
	DefaultMode  string            `json:"default_mode"`
	FirstSummary bool              `json:"first_summary"`
}

func DefaultPlannerPreferences() PlannerPreferences {
	return PlannerPreferences{Reminders: []int{1440}, Overrides: map[string][]int{}, DailyTime: "08:00", WeeklyTime: "18:00", Weekday: 0, QuietStart: 22, QuietEnd: 8, TitleStyle: "course", Colors: map[string]string{}, DefaultMode: ModeActive}
}
func (s *Store) PlannerPreferences(ctx context.Context, user int64) (PlannerPreferences, error) {
	p := DefaultPlannerPreferences()
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT settings FROM canvaslink_preferences WHERE telegram_user_id=$1`, user).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(data, &p)
	if p.Overrides == nil {
		p.Overrides = map[string][]int{}
	}
	if p.Colors == nil {
		p.Colors = map[string]string{}
	}
	return p, err
}
func (s *Store) SavePlannerPreferences(ctx context.Context, user int64, p PlannerPreferences) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO canvaslink_preferences(telegram_user_id,settings) VALUES($1,$2) ON CONFLICT(telegram_user_id) DO UPDATE SET settings=EXCLUDED.settings`, user, string(data))
	return err
}
func ReminderKey(course, kind string) string { return course + "\x1f" + kind }
func (p PlannerPreferences) Offsets(course, kind string) []int {
	if v, ok := p.Overrides[ReminderKey(course, kind)]; ok {
		return v
	}
	if v, ok := p.Overrides[ReminderKey(course, "")]; ok {
		return v
	}
	return p.Reminders
}

type PlannerTask struct {
	ID, UserID                                 int64
	SourceUID, Title, CourseID, AssignmentType string
	DueAt                                      time.Time
	AllDay                                     bool
	URL                                        string
	Manual, Present, Done                      bool
	TargetAt, SnoozedUntil                     *time.Time
	Revision                                   int64
}

const taskColumns = `id,telegram_user_id,source_uid,title,course_id,assignment_type,due_at,all_day,source_url,manual,present,done,target_at,snoozed_until,revision`

func scanTask(row interface{ Scan(...any) error }) (PlannerTask, error) {
	var t PlannerTask
	err := row.Scan(&t.ID, &t.UserID, &t.SourceUID, &t.Title, &t.CourseID, &t.AssignmentType, &t.DueAt, &t.AllDay, &t.URL, &t.Manual, &t.Present, &t.Done, &t.TargetAt, &t.SnoozedUntil, &t.Revision)
	return t, err
}
func (s *Store) PlannerTasks(ctx context.Context, user int64) ([]PlannerTask, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM canvaslink_tasks WHERE telegram_user_id=$1 ORDER BY due_at,id`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []PlannerTask
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}
func (s *Store) PlannerTask(ctx context.Context, user, id int64) (PlannerTask, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM canvaslink_tasks WHERE telegram_user_id=$1 AND id=$2`, user, id))
}

// RecordPlannerSnapshot stores all feed items, including items not approved for Google.
// It never overwrites student completion/targets. Missing/ambiguous sources suspend reminders.
func (s *Store) RecordPlannerSnapshot(ctx context.Context, user int64, tasks []PlannerTask, location *time.Location) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE canvaslink_tasks SET present=FALSE WHERE telegram_user_id=$1 AND NOT manual`, user); err != nil {
			return err
		}
		for _, t := range tasks {
			old, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM canvaslink_tasks WHERE telegram_user_id=$1 AND source_uid=$2`, user, t.SourceUID))
			exists := err == nil
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			changed := exists && (!old.DueAt.Equal(t.DueAt) || old.AllDay != t.AllDay || old.Title != t.Title)
			revision := int64(1)
			if exists {
				revision = old.Revision
			}
			if changed {
				revision++
			}
			var id int64
			err = tx.QueryRowContext(ctx, `INSERT INTO canvaslink_tasks(telegram_user_id,source_uid,title,course_id,assignment_type,due_at,all_day,source_url,revision)
    VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(telegram_user_id,source_uid) DO UPDATE SET
    title=EXCLUDED.title,course_id=EXCLUDED.course_id,assignment_type=EXCLUDED.assignment_type,due_at=EXCLUDED.due_at,all_day=EXCLUDED.all_day,
    source_url=EXCLUDED.source_url,present=TRUE,revision=EXCLUDED.revision RETURNING id`, user, t.SourceUID, t.Title, t.CourseID, t.AssignmentType, t.DueAt, t.AllDay, t.URL, revision).Scan(&id)
			if err != nil {
				return err
			}
			if changed {
				body := fmt.Sprintf("📅 Canvas changed: %s\nPreviously: %s — %s\nNow: %s — %s\nYour official deadline is shown above. Any personal target stays separate.", t.Title, old.Title, PlannerDate(old.DueAt, old.AllDay, location), t.Title, PlannerDate(t.DueAt, t.AllDay, location))
				if _, err := tx.ExecContext(ctx, `INSERT INTO canvaslink_deliveries(telegram_user_id,dedupe_key,kind,task_id,revision,body,expires_at) VALUES($1,$2,'change',$3,$4,$5,NOW()+INTERVAL '7 days') ON CONFLICT DO NOTHING`, user, fmt.Sprintf("change:%d:%d", id, revision), id, revision, body); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
func PlannerDate(d time.Time, allDay bool, loc *time.Location) string {
	if allDay {
		return d.Format("Mon 02 Jan 2006") + " · all day"
	}
	return d.In(loc).Format("Mon 02 Jan 2006 · 15:04 MST")
}
func (t PlannerTask) EffectiveDue(loc *time.Location) time.Time {
	if t.AllDay {
		y, m, d := t.DueAt.Date()
		return time.Date(y, m, d, 23, 59, 59, 0, loc)
	}
	return t.DueAt
}
func (t PlannerTask) ReminderAt(offset int, loc *time.Location) time.Time {
	if t.TargetAt != nil && t.TargetAt.Before(t.EffectiveDue(loc)) {
		return t.TargetAt.Add(-time.Duration(offset) * time.Minute)
	}
	if t.AllDay {
		y, m, d := t.DueAt.Date()
		base := time.Date(y, m, d, 9, 0, 0, 0, loc)
		if offset%1440 == 0 {
			return base.AddDate(0, 0, -offset/1440)
		}
		return base.Add(-time.Duration(offset) * time.Minute)
	}
	return t.DueAt.Add(-time.Duration(offset) * time.Minute)
}
func (s *Store) AddManualTask(ctx context.Context, user int64, title string, due time.Time) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO canvaslink_tasks(telegram_user_id,source_uid,title,due_at,manual) VALUES($1,'manual:'||gen_random_uuid()::text,$2,$3,TRUE) RETURNING id`, user, title, due).Scan(&id)
	return id, err
}
func (s *Store) ChangePlannerTask(ctx context.Context, user, id int64, action string, value *time.Time) error {
	var q string
	switch action {
	case "done":
		q = `done=TRUE,snoozed_until=NULL`
	case "undo":
		q = `done=FALSE`
	case "snooze":
		q = `snoozed_until=$3`
	case "target":
		q = `target_at=$3,snoozed_until=NULL,revision=revision+1`
	case "due":
		q = `due_at=$3,snoozed_until=NULL,revision=revision+1`
	case "delete":
		_, err := s.db.ExecContext(ctx, `DELETE FROM canvaslink_tasks WHERE telegram_user_id=$1 AND id=$2 AND manual`, user, id)
		return err
	default:
		return errors.New("invalid task action")
	}
	args := []any{user, id}
	if strings.Contains(q, "$3") {
		args = append(args, value)
	}
	where := ""
	if action == "due" {
		where = " AND manual"
	}
	res, err := s.db.ExecContext(ctx, `UPDATE canvaslink_tasks SET `+q+` WHERE telegram_user_id=$1 AND id=$2`+where, args...)
	return requireAffected(res, err)
}
func (s *Store) SetPlannerInput(ctx context.Context, user int64, action string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO canvaslink_planner_inputs VALUES($1,$2,NOW()+INTERVAL '15 minutes') ON CONFLICT(telegram_user_id) DO UPDATE SET action=$2,expires_at=EXCLUDED.expires_at`, user, action)
	return err
}
func (s *Store) PlannerInput(ctx context.Context, user int64) (string, error) {
	var a string
	err := s.db.QueryRowContext(ctx, `SELECT action FROM canvaslink_planner_inputs WHERE telegram_user_id=$1 AND expires_at>NOW()`, user).Scan(&a)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return a, err
}
func (s *Store) ClearPlannerInput(ctx context.Context, user int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM canvaslink_planner_inputs WHERE telegram_user_id=$1`, user)
	return err
}
func (s *Store) PlannerUsers(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT telegram_user_id FROM canvaslink_telegram_accounts WHERE chat_id=telegram_user_id AND onboarding_status=''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type PlannerDelivery struct {
	ID, TaskID, Revision int64
	Kind, Body, Key      string
}

func (s *Store) QueuePlannerDelivery(ctx context.Context, user int64, key, kind string, t *PlannerTask, body string, expires time.Time) error {
	var id any
	var revision int64
	if t != nil {
		id = t.ID
		revision = t.Revision
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO canvaslink_deliveries(telegram_user_id,dedupe_key,kind,task_id,revision,body,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, user, key, kind, id, revision, body, expires)
	return err
}
func (s *Store) PlannerDeliveries(ctx context.Context, user int64) ([]PlannerDelivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(task_id,0),revision,kind,body,dedupe_key FROM canvaslink_deliveries WHERE telegram_user_id=$1 AND NOT sent AND next_attempt<=NOW() AND expires_at>NOW() ORDER BY id LIMIT 10`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlannerDelivery
	for rows.Next() {
		var d PlannerDelivery
		if err := rows.Scan(&d.ID, &d.TaskID, &d.Revision, &d.Kind, &d.Body, &d.Key); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Store) FinishPlannerDelivery(ctx context.Context, user, id int64, sent bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE canvaslink_deliveries SET sent=$3,next_attempt=NOW()+INTERVAL '5 minutes' WHERE telegram_user_id=$1 AND id=$2`, user, id, sent)
	return err
}

// RecordHealth queues one alert per failure episode, then a single recovery notice.
func (s *Store) RecordHealth(ctx context.Context, user int64, service string, healthy bool, threshold int, warning string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO canvaslink_connection_health(telegram_user_id,service) VALUES($1,$2) ON CONFLICT DO NOTHING`, user, service); err != nil {
			return err
		}
		var failures int
		var alerted bool
		var episode int64
		if err := tx.QueryRowContext(ctx, `SELECT failures,alerted,episode FROM canvaslink_connection_health WHERE telegram_user_id=$1 AND service=$2 FOR UPDATE`, user, service).Scan(&failures, &alerted, &episode); err != nil {
			return err
		}
		body := ""
		key := ""
		if healthy {
			if alerted {
				var warningSent bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM canvaslink_deliveries WHERE telegram_user_id=$1 AND dedupe_key=$2 AND sent)`, user, fmt.Sprintf("health:%s:%d:bad", service, episode)).Scan(&warningSent); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE canvaslink_deliveries SET sent=TRUE WHERE telegram_user_id=$1 AND dedupe_key=$2`, user, fmt.Sprintf("health:%s:%d:bad", service, episode)); err != nil {
					return err
				}
				if warningSent {
					body = "✅ " + service + " is working again. Automatic checks have resumed."
				}
				key = fmt.Sprintf("health:%s:%d:ok", service, episode)
			}
			failures = 0
			alerted = false
		} else {
			failures++
			if !alerted && failures >= threshold {
				episode++
				alerted = true
				body = warning
				key = fmt.Sprintf("health:%s:%d:bad", service, episode)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE canvaslink_connection_health SET failures=$3,alerted=$4,episode=$5,last_success=CASE WHEN $6 THEN NOW() ELSE last_success END WHERE telegram_user_id=$1 AND service=$2`, user, service, failures, alerted, episode, healthy); err != nil {
			return err
		}
		if body != "" {
			_, err := tx.ExecContext(ctx, `INSERT INTO canvaslink_deliveries(telegram_user_id,dedupe_key,kind,body,expires_at) VALUES($1,$2,'health',$3,NOW()+INTERVAL '2 days') ON CONFLICT DO NOTHING`, user, key, body)
			return err
		}
		return nil
	})
}
func (s *Store) SetAllCourseModes(ctx context.Context, user int64, mode string) error {
	if mode != ModeAuto && mode != ModeActive && mode != ModeIgnore {
		return ErrInvalidMode
	}
	_, err := s.db.ExecContext(ctx, `UPDATE canvaslink_course_type_settings SET mode=$2 WHERE telegram_user_id=$1`, user, mode)
	return err
}
func (s *Store) TaskAllowed(ctx context.Context, t PlannerTask) (bool, error) {
	if !t.Present {
		return false, nil
	}
	if t.Manual {
		return true, nil
	}
	var mode string
	err := s.db.QueryRowContext(ctx, `SELECT mode FROM canvaslink_course_type_settings WHERE telegram_user_id=$1 AND course_id=$2 AND assignment_type=$3`, t.UserID, t.CourseID, t.AssignmentType).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || mode == ModeIgnore {
		return false, err
	}
	var ignored bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM canvaslink_pending_actions WHERE telegram_user_id=$1 AND canvas_event_uid=$2 AND status='ignored')`, t.UserID, t.SourceUID).Scan(&ignored)
	return !ignored, err
}
func SortedOffsets(values []int) []int { out := append([]int{}, values...); sort.Ints(out); return out }

// CompleteSnooze acknowledges the snooze and consumes already elapsed offsets in
// one transaction, so the next scheduler pass cannot immediately repeat them.
func (s *Store) CompleteSnooze(ctx context.Context, user, delivery int64, t PlannerTask, offsets []int, now time.Time, loc *time.Location) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE canvaslink_deliveries SET sent=TRUE WHERE telegram_user_id=$1 AND id=$2`, user, delivery); err != nil {
			return err
		}
		for _, offset := range offsets {
			if now.Before(t.ReminderAt(offset, loc)) {
				continue
			}
			key := fmt.Sprintf("reminder:%d:%d:%d", t.ID, t.Revision, offset)
			if _, err := tx.ExecContext(ctx, `INSERT INTO canvaslink_deliveries(telegram_user_id,dedupe_key,kind,task_id,revision,body,expires_at,sent) VALUES($1,$2,'reminder',$3,$4,'',NOW(),TRUE) ON CONFLICT(telegram_user_id,dedupe_key) DO UPDATE SET sent=TRUE`, user, key, t.ID, t.Revision); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE canvaslink_tasks SET snoozed_until=NULL WHERE telegram_user_id=$1 AND id=$2`, user, t.ID)
		return err
	})
}
func (s *Store) QueueFirstPlannerSummary(ctx context.Context, user int64, body string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO canvaslink_preferences(telegram_user_id) VALUES($1) ON CONFLICT DO NOTHING`, user); err != nil {
			return err
		}
		var sent bool
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE((settings->>'first_summary')::boolean,FALSE) FROM canvaslink_preferences WHERE telegram_user_id=$1 FOR UPDATE`, user).Scan(&sent); err != nil {
			return err
		}
		if sent {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO canvaslink_deliveries(telegram_user_id,dedupe_key,kind,body,expires_at) VALUES($1,'first-summary','summary',$2,NOW()+INTERVAL '7 days') ON CONFLICT DO NOTHING`, user, body); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE canvaslink_preferences SET settings=jsonb_set(settings,'{first_summary}','true') WHERE telegram_user_id=$1`, user)
		return err
	})
}

// VisiblePlannerTasks resolves course and per-item exclusions in one query.
func (s *Store) VisiblePlannerTasks(ctx context.Context, user int64) ([]PlannerTask, error) {
	columns := strings.Split(taskColumns, ",")
	for i := range columns {
		columns[i] = "t." + columns[i]
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+strings.Join(columns, ",")+` FROM canvaslink_tasks t WHERE t.telegram_user_id=$1 AND t.present AND
 (t.manual OR (EXISTS(SELECT 1 FROM canvaslink_course_type_settings s WHERE s.telegram_user_id=t.telegram_user_id AND s.course_id=t.course_id AND s.assignment_type=t.assignment_type AND s.mode<>'ignore')
 AND NOT EXISTS(SELECT 1 FROM canvaslink_pending_actions p WHERE p.telegram_user_id=t.telegram_user_id AND p.canvas_event_uid=t.source_uid AND p.status='ignored')))
 ORDER BY t.due_at,t.id`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []PlannerTask
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}
func (s *Store) PlannerSyncStatus(ctx context.Context, user int64, loc *time.Location) (string, error) {
	var last *time.Time
	err := s.db.QueryRowContext(ctx, `SELECT last_synced_at FROM canvaslink_feeds WHERE telegram_user_id=$1`, user).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return "Canvas is not connected.", nil
	}
	if err != nil {
		return "", err
	}
	if last == nil {
		return "Canvas is waiting for its first successful check.", nil
	}
	return "Canvas last checked: " + last.In(loc).Format("02 Jan 15:04 MST") + ".", nil
}
func (s *Store) PrunePlannerDeliveries(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM canvaslink_deliveries WHERE expires_at<NOW()-INTERVAL '30 days'`)
	return err
}
