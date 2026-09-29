package sync

import (
	"context"
	"fmt"
	"log"
	"sync"
)

// RunScheduled performs all work within the authenticated scheduler request.
// Feed cadence lives in PostgreSQL; cold starts do not cause extra feed checks.
// User locks and calendar-job leases remain authoritative for side effects.
func (w *Worker) RunScheduled(ctx context.Context) (err error) {
	release, acquired, err := w.store.TryRuntimeLock(ctx, "scheduled")
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	} // An overlapping invocation is already doing the work.
	defer func() {
		if e := release(); e != nil {
			log.Printf("release scheduled lock: %v", e)
		}
	}()
	if err := w.store.PruneTelegramReceipts(ctx); err != nil {
		return err
	}
	// Deliver existing deadlines first; a slow Canvas provider must not hold them up.
	w.runPlannerCycle(ctx, false)
	feeds, err := w.store.ListEnabledFeeds(ctx)
	if err != nil {
		return err
	}
	var group sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentFeeds)
	for _, feed := range feeds {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			group.Wait()
			return ctx.Err()
		}
		if ctx.Err() != nil {
			<-sem
			break
		}
		ready, err := w.store.ReserveFeedCheck(ctx, feed.UserID, w.interval)
		if err != nil {
			<-sem
			group.Wait()
			return fmt.Errorf("reserve feed check: %w", err)
		}
		if !ready {
			<-sem
			continue
		}
		group.Add(1)
		go func() { defer group.Done(); defer func() { <-sem }(); w.syncFeed(ctx, feed) }()
	}
	group.Wait()
	w.runCalendarJobs(ctx)
	return ctx.Err()
}
