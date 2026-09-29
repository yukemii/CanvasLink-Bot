package store_test

import (
	"context"
	"github.com/markadodo/canvaslink/internal/testsupport"
	"testing"
	"time"
)

func TestRuntimeLocksReceiptsAndFeedCadence(t *testing.T) {
	s := testsupport.Store(t)
	ctx := context.Background()
	release, ok, err := s.TryRuntimeLock(ctx, "test-runtime")
	if err != nil || !ok {
		t.Fatalf("first lock: %v %v", ok, err)
	}
	_, ok, err = s.TryRuntimeLock(ctx, "test-runtime")
	if err != nil || ok {
		t.Fatalf("overlap lock: %v %v", ok, err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	release, ok, err = s.TryRuntimeLock(ctx, "test-runtime")
	if err != nil || !ok {
		t.Fatal("lock not released")
	}
	defer release()
	for i := 0; i < 2; i++ {
		ok, err := s.ClaimTelegramUpdate(ctx, 987)
		if err != nil || ok != (i == 0) {
			t.Fatalf("receipt attempt %d: %v %v", i, ok, err)
		}
	}
	if err := s.UpsertTelegramAccountWithTimezone(ctx, 55, 55, "tester", "UTC"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertFeed(ctx, 55, "https://canvas.example/calendar.ics"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		ok, err := s.ReserveFeedCheck(ctx, 55, time.Hour)
		if err != nil || ok != (i == 0) {
			t.Fatalf("feed attempt %d: %v %v", i, ok, err)
		}
	}
	if err := s.UpsertTelegramAccountWithTimezone(ctx, 56, 56, "tester", "UTC"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertFeed(ctx, 56, "https://canvas.example/calendar.ics"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOnboardingStatus(ctx, 56, "awaiting_url"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReserveFeedCheck(ctx, 56, time.Hour); err != nil || ok {
		t.Fatal("onboarding feed must not be reserved")
	}
	if err := s.PruneTelegramReceipts(ctx); err != nil {
		t.Fatal(err)
	}
}
