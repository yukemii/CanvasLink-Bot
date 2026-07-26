package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

type fixedSQLResult int64

func (fixedSQLResult) LastInsertId() (int64, error) {
	return 0, nil
}

func (result fixedSQLResult) RowsAffected() (int64, error) {
	return int64(result), nil
}

type recordingExecer struct {
	results []sql.Result
	errs    []error
	queries []string
	args    [][]any
}

func (e *recordingExecer) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	e.queries = append(e.queries, query)
	e.args = append(e.args, args)
	index := len(e.queries) - 1
	var err error
	if index < len(e.errs) {
		err = e.errs[index]
	}
	if index < len(e.results) {
		return e.results[index], err
	}
	return fixedSQLResult(1), err
}

func TestCanonicalMode(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		ModeAuto:   ModeQuiet,
		ModeActive: ModeReview,
		ModeQuiet:  ModeQuiet,
		ModeNotify: ModeNotify,
		ModeReview: ModeReview,
		ModeIgnore: ModeIgnore,
	}
	for input, want := range tests {
		if got := CanonicalMode(input); got != want {
			t.Errorf("CanonicalMode(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIsValidMode(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{ModeAuto, ModeActive, ModeQuiet, ModeNotify, ModeReview, ModeIgnore} {
		if !IsValidMode(mode) {
			t.Errorf("IsValidMode(%q) = false", mode)
		}
	}
	for _, mode := range []string{"", "unknown", "AUTO"} {
		if IsValidMode(mode) {
			t.Errorf("IsValidMode(%q) = true", mode)
		}
	}
}

func TestFieldCipherRoundTripAndAuthentication(t *testing.T) {
	t.Parallel()

	fields, err := newFieldCipher(EncryptionConfig{
		PrimaryKeyID: "current",
		PrimaryKey:   bytes.Repeat([]byte{0x42}, 32),
	})
	if err != nil {
		t.Fatalf("newFieldCipher() error = %v", err)
	}

	first, err := fields.encrypt("secret-value", fieldPurposeFeedURL)
	if err != nil {
		t.Fatalf("encrypt() error = %v", err)
	}
	second, err := fields.encrypt("secret-value", fieldPurposeFeedURL)
	if err != nil {
		t.Fatalf("encrypt() second error = %v", err)
	}
	if first == second {
		t.Fatal("encrypt() reused a nonce; ciphertexts should differ")
	}
	if !strings.HasPrefix(first, encryptedFieldPrefix+"current:") {
		t.Fatalf("ciphertext %q lacks version and key ID prefix", first)
	}

	plaintext, rotate, err := fields.decrypt(first, fieldPurposeFeedURL)
	if err != nil {
		t.Fatalf("decrypt() error = %v", err)
	}
	if plaintext != "secret-value" || rotate {
		t.Fatalf("decrypt() = %q, rotate=%v; want secret-value, false", plaintext, rotate)
	}

	if _, _, err := fields.decrypt(first, fieldPurposeGoogleAccessToken); err == nil {
		t.Fatal("decrypt() with the wrong field purpose succeeded")
	}

	parts := strings.SplitN(strings.TrimPrefix(first, encryptedFieldPrefix), ":", 2)
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode ciphertext: %v", err)
	}
	raw[len(raw)-1] ^= 0x01
	tampered := encryptedFieldPrefix + parts[0] + ":" + base64.RawURLEncoding.EncodeToString(raw)
	if _, _, err := fields.decrypt(tampered, fieldPurposeFeedURL); err == nil {
		t.Fatal("decrypt() accepted tampered ciphertext")
	}
}

func TestFieldCipherRotatesPlaintextAndPreviousKeys(t *testing.T) {
	t.Parallel()

	oldFields, err := newFieldCipher(EncryptionConfig{
		PrimaryKeyID: "old",
		PrimaryKey:   bytes.Repeat([]byte{0x11}, 32),
	})
	if err != nil {
		t.Fatalf("new old cipher: %v", err)
	}
	oldCiphertext, err := oldFields.encrypt("rotating", fieldPurposeOAuthVerifier)
	if err != nil {
		t.Fatalf("encrypt old value: %v", err)
	}

	currentFields, err := newFieldCipher(EncryptionConfig{
		PrimaryKeyID: "current",
		PrimaryKey:   bytes.Repeat([]byte{0x22}, 32),
		PreviousKeys: map[string][]byte{
			"old": bytes.Repeat([]byte{0x11}, 32),
		},
	})
	if err != nil {
		t.Fatalf("new current cipher: %v", err)
	}

	plaintext, rotate, err := currentFields.decrypt(oldCiphertext, fieldPurposeOAuthVerifier)
	if err != nil {
		t.Fatalf("decrypt previous-key value: %v", err)
	}
	if plaintext != "rotating" || !rotate {
		t.Fatalf("decrypt previous = %q, rotate=%v; want rotating, true", plaintext, rotate)
	}
	plaintext, rotate, err = currentFields.decrypt("legacy plaintext", fieldPurposeOAuthVerifier)
	if err != nil {
		t.Fatalf("decrypt plaintext: %v", err)
	}
	if plaintext != "legacy plaintext" || !rotate {
		t.Fatalf("decrypt plaintext = %q, rotate=%v; want legacy plaintext, true", plaintext, rotate)
	}

	withoutOldKey, err := newFieldCipher(EncryptionConfig{
		PrimaryKeyID: "current",
		PrimaryKey:   bytes.Repeat([]byte{0x22}, 32),
	})
	if err != nil {
		t.Fatalf("new cipher without old key: %v", err)
	}
	if _, _, err := withoutOldKey.decrypt(oldCiphertext, fieldPurposeOAuthVerifier); !errors.Is(err, ErrUnknownEncryptionKey) {
		t.Fatalf("decrypt without old key error = %v, want ErrUnknownEncryptionKey", err)
	}
}

func TestEncryptionConfigValidation(t *testing.T) {
	t.Parallel()

	tests := []EncryptionConfig{
		{PrimaryKeyID: "", PrimaryKey: bytes.Repeat([]byte{1}, 32)},
		{PrimaryKeyID: "contains:colon", PrimaryKey: bytes.Repeat([]byte{1}, 32)},
		{PrimaryKeyID: "valid", PrimaryKey: []byte("too-short")},
		{
			PrimaryKeyID: "same",
			PrimaryKey:   bytes.Repeat([]byte{1}, 32),
			PreviousKeys: map[string][]byte{"same": bytes.Repeat([]byte{2}, 32)},
		},
	}
	for _, config := range tests {
		if _, err := newFieldCipher(config); err == nil {
			t.Errorf("newFieldCipher(%+v) succeeded", config)
		}
	}
}

func TestStoreWithoutKeysRejectsEncryptedValues(t *testing.T) {
	t.Parallel()

	store := &Store{}
	if _, err := store.decodeSensitive("enc:v1:key:value", fieldPurposeFeedURL); !errors.Is(err, ErrEncryptionRequired) {
		t.Fatalf("decodeSensitive() error = %v, want ErrEncryptionRequired", err)
	}
	if plaintext, err := store.decodeSensitive("legacy", fieldPurposeFeedURL); err != nil || plaintext != "legacy" {
		t.Fatalf("decodeSensitive(plaintext) = %q, %v", plaintext, err)
	}
}

func TestValidateTimezone(t *testing.T) {
	t.Parallel()

	for _, timezone := range []string{"UTC", "Asia/Singapore", "America/New_York"} {
		if err := ValidateTimezone(timezone); err != nil {
			t.Errorf("ValidateTimezone(%q) error = %v", timezone, err)
		}
	}
	for _, timezone := range []string{"", "Local", "Mars/Olympus", "UTC\nInjected"} {
		if err := ValidateTimezone(timezone); !errors.Is(err, ErrInvalidTimezone) {
			t.Errorf("ValidateTimezone(%q) error = %v, want ErrInvalidTimezone", timezone, err)
		}
	}
}

func TestCalendarJobInputValidation(t *testing.T) {
	t.Parallel()

	dueAt := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	valid := normalizeCalendarJobInput(CalendarJobInput{
		TelegramUserID: 1,
		DedupeKey:      " create:uid:1 ",
		Action:         " CREATE ",
		SourceUID:      "uid",
		Title:          "Assignment",
		DueAt:          &dueAt,
		GoogleEventID:  "deterministicid",
	})
	if valid.GoogleCalendarID != "primary" || valid.MaxAttempts != 8 ||
		valid.Action != CalendarActionCreate || valid.DedupeKey != "create:uid:1" {
		t.Fatalf("normalizeCalendarJobInput() = %+v", valid)
	}
	if err := validateCalendarJobInput(valid); err != nil {
		t.Fatalf("validateCalendarJobInput(valid) error = %v", err)
	}
	missingSince := dueAt.Add(-time.Hour)
	deleteNotBefore := missingSince.Add(30 * time.Minute)
	earlyDeleteThreshold := missingSince.Add(-time.Second)
	guardedDelete := normalizeCalendarJobInput(CalendarJobInput{
		TelegramUserID:       1,
		DedupeKey:            "delete:uid",
		Action:               CalendarActionDelete,
		SourceUID:            "uid",
		GoogleEventID:        "deterministicid",
		ExpectedMissingSince: &missingSince,
		ExpectedMissingCount: 3,
		DeleteNotBefore:      &deleteNotBefore,
	})
	if err := validateCalendarJobInput(guardedDelete); err != nil {
		t.Fatalf("validateCalendarJobInput(guarded delete) error = %v", err)
	}
	immediateCancelledDelete := guardedDelete
	immediateCancelledDelete.CanvasCancelled = true
	immediateCancelledDelete.DeleteNotBefore = &missingSince
	if err := validateCalendarJobInput(immediateCancelledDelete); err != nil {
		t.Fatalf("validateCalendarJobInput(immediate cancelled delete) error = %v", err)
	}

	tests := []CalendarJobInput{
		{},
		{TelegramUserID: 1, DedupeKey: "key", Action: "unknown", SourceUID: "uid", MaxAttempts: 8},
		{TelegramUserID: 1, DedupeKey: "key", Action: CalendarActionCreate, SourceUID: "uid", DueAt: &dueAt, Title: "title", MaxAttempts: 8},
		{TelegramUserID: 1, DedupeKey: "key", Action: CalendarActionDelete, SourceUID: "uid", MaxAttempts: 8},
		{
			TelegramUserID:       1,
			DedupeKey:            "key",
			Action:               CalendarActionDelete,
			SourceUID:            "uid",
			GoogleEventID:        "event",
			ExpectedMissingSince: &missingSince,
			ExpectedMissingCount: 0,
			DeleteNotBefore:      &deleteNotBefore,
			MaxAttempts:          8,
		},
		{
			TelegramUserID:       1,
			DedupeKey:            "key",
			Action:               CalendarActionDelete,
			SourceUID:            "uid",
			GoogleEventID:        "event",
			ExpectedMissingSince: &missingSince,
			ExpectedMissingCount: 1,
			DeleteNotBefore:      &missingSince,
			MaxAttempts:          8,
		},
		{
			TelegramUserID:       1,
			DedupeKey:            "key",
			Action:               CalendarActionDelete,
			SourceUID:            "uid",
			GoogleEventID:        "event",
			ExpectedMissingSince: &missingSince,
			ExpectedMissingCount: 1,
			DeleteNotBefore:      &earlyDeleteThreshold,
			MaxAttempts:          8,
		},
		{
			TelegramUserID:       1,
			DedupeKey:            "key",
			Action:               CalendarActionDelete,
			SourceUID:            "uid",
			GoogleEventID:        "event",
			ExpectedMissingSince: &missingSince,
			ExpectedMissingCount: 1,
			MaxAttempts:          8,
		},
		{
			TelegramUserID:       1,
			DedupeKey:            "key",
			Action:               CalendarActionCreate,
			SourceUID:            "uid",
			Title:                "title",
			DueAt:                &dueAt,
			GoogleEventID:        "event",
			ExpectedMissingSince: &missingSince,
			ExpectedMissingCount: 1,
			DeleteNotBefore:      &deleteNotBefore,
			MaxAttempts:          8,
		},
	}
	for _, input := range tests {
		input = normalizeCalendarJobInput(input)
		if err := validateCalendarJobInput(input); !errors.Is(err, ErrInvalidCalendarJob) {
			t.Errorf("validateCalendarJobInput(%+v) error = %v, want ErrInvalidCalendarJob", input, err)
		}
	}
}

func TestPendingStatusPolicySeparatesUserIgnoreFromSourceRemoval(t *testing.T) {
	t.Parallel()

	if PendingStatusIgnored == PendingStatusSourceRemoved {
		t.Fatal("user Ignore and source removal must remain distinct states")
	}
	for _, schema := range []string{canvaslinkPendingActionsTable, migratePendingActionStatusConstraint} {
		if !strings.Contains(schema, "'source_removed'") {
			t.Fatal("pending-action schema does not allow the source-removed lifecycle state")
		}
		if !strings.Contains(schema, "'ignored'") {
			t.Fatal("pending-action schema lost the terminal user-ignored state")
		}
	}
}

func TestDestructiveActionValidation(t *testing.T) {
	t.Parallel()

	for _, action := range []string{"reset", "disconnect_canvas", "wipe-all"} {
		if !destructiveActionPattern.MatchString(action) {
			t.Errorf("valid destructive action %q rejected", action)
		}
	}
	for _, action := range []string{"", "Reset", "reset|user", strings.Repeat("x", 33)} {
		if destructiveActionPattern.MatchString(action) {
			t.Errorf("invalid destructive action %q accepted", action)
		}
	}
}

func TestTruncateStoreError(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 2500)
	if got := truncateStoreError(long); len(got) != 2000 {
		t.Fatalf("truncateStoreError() length = %d, want 2000", len(got))
	}
}

func TestSaveFeedWithSettingsRejectsStaleAccountRevisionBeforeSeeding(t *testing.T) {
	t.Parallel()

	execer := &recordingExecer{
		results: []sql.Result{
			fixedSQLResult(1),
			fixedSQLResult(0),
		},
	}
	err := saveFeedWithSettingsTx(
		context.Background(),
		execer,
		42,
		"encrypted-feed",
		"awaiting_url",
		"google_prompt",
		7,
		[]CourseTypeSeed{{
			CourseID:       "CS2040S",
			CourseName:     "CS2040S",
			AssignmentType: "assignment",
		}},
	)
	if !errors.Is(err, ErrStaleOnboardingStep) {
		t.Fatalf("saveFeedWithSettingsTx() error = %v, want ErrStaleOnboardingStep", err)
	}
	if len(execer.queries) != 2 {
		t.Fatalf("executed %d statements; stale transition must stop before seeding", len(execer.queries))
	}
	if len(execer.args[1]) != 4 || execer.args[1][3] != int64(7) {
		t.Fatalf("account transition args = %#v; expected revision 7", execer.args[1])
	}
	if !strings.Contains(execer.queries[1], "onboarding_status = $3") ||
		!strings.Contains(execer.queries[1], "state_revision = $4") {
		t.Fatalf("account transition is not status/revision conditional: %s", execer.queries[1])
	}
}

func TestOAuthStateUpgradeIsFailClosed(t *testing.T) {
	t.Parallel()

	if !strings.Contains(canvaslinkOAuthStatesTable, "private_chat_bound BOOLEAN NOT NULL DEFAULT FALSE") {
		t.Fatal("new OAuth state table does not default private-chat provenance to false")
	}
	if !strings.Contains(addOAuthStatePrivateChatBindingColumn, "DEFAULT FALSE") {
		t.Fatal("OAuth state migration does not mark pre-upgrade rows as untrusted")
	}
	if !strings.Contains(invalidateUnboundOAuthStates, "WHERE private_chat_bound = FALSE") {
		t.Fatal("OAuth state migration does not invalidate untrusted rows")
	}
}

func TestAdvisoryLockReleaseSuccessIsIdempotent(t *testing.T) {
	t.Parallel()

	unlockCalls := 0
	closeCalls := 0
	discarded := false
	release := makeAdvisoryLockRelease(
		func() (bool, error) {
			unlockCalls++
			return true, nil
		},
		func(discard bool) error {
			closeCalls++
			discarded = discard
			return nil
		},
	)

	if err := release(); err != nil {
		t.Fatalf("release() error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("second release() error = %v", err)
	}
	if unlockCalls != 1 || closeCalls != 1 {
		t.Fatalf("release ran unlock=%d close=%d times; want one each", unlockCalls, closeCalls)
	}
	if discarded {
		t.Fatal("healthy unlocked connection was discarded")
	}
}

func TestAdvisoryLockReleaseDiscardsUncertainSession(t *testing.T) {
	t.Parallel()

	unlockErr := errors.New("connection lost")
	closeErr := errors.New("close failed")
	tests := []struct {
		name      string
		unlocked  bool
		unlockErr error
		closeErr  error
		wantError error
		wantText  string
	}{
		{
			name:      "unlock query error",
			unlockErr: unlockErr,
			wantError: unlockErr,
		},
		{
			name:     "lock unexpectedly absent",
			wantText: "lock was not held",
		},
		{
			name:      "discard close error is surfaced",
			unlockErr: unlockErr,
			closeErr:  closeErr,
			wantError: closeErr,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			discarded := false
			release := makeAdvisoryLockRelease(
				func() (bool, error) {
					return test.unlocked, test.unlockErr
				},
				func(discard bool) error {
					discarded = discard
					return test.closeErr
				},
			)

			err := release()
			if err == nil {
				t.Fatal("release() error = nil")
			}
			if test.wantError != nil && !errors.Is(err, test.wantError) {
				t.Fatalf("release() error = %v, want errors.Is(%v)", err, test.wantError)
			}
			if test.wantText != "" && !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("release() error = %v, want text %q", err, test.wantText)
			}
			if !discarded {
				t.Fatal("uncertain advisory-lock session was returned to the pool")
			}
		})
	}
}
