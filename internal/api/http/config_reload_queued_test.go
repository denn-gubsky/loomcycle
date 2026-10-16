package http

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestReloadConcurrency_AppliesTheQueuedPerUserCap — a reloaded
// concurrency.max_queued_runs_per_user takes effect: set to 0, a run over its
// user's cap is refused instead of queued. Before the fix the reload reported
// the section applied but never set this cap, so runs kept queueing up to the
// boot value until a restart.
func TestReloadConcurrency_AppliesTheQueuedPerUserCap(t *testing.T) {
	s := &Server{}
	s.sem = concurrency.New(8, 8, 0).WithPerUserCap(1).WithQueuedPerUserCap(4)
	s.SetCallScheduler(concurrency.NewCallScheduler(nil))
	rel, err := s.sem.Admit(context.Background(), concurrency.AdmitRequest{TenantID: "t", UserID: "u"})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	defer rel()

	s.ReloadConcurrency(&config.Config{Concurrency: config.Concurrency{
		MaxConcurrentRuns: 8, MaxQueueDepth: 8, MaxConcurrentRunsPerUser: 1, MaxQueuedRunsPerUser: 0,
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = s.sem.Admit(ctx, concurrency.AdmitRequest{TenantID: "t", UserID: "u"})
	var quota *concurrency.ErrPerUserQuotaExhausted
	if !errors.As(err, &quota) {
		t.Fatalf("over the cap with no waiting allowed after the reload: got %v, want ErrPerUserQuotaExhausted", err)
	}
}
