package concurrency

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// refuseAdmit admits r with a deadline, so a run that queues instead of being
// refused fails the test rather than waiting for ever.
func refuseAdmit(ctx context.Context, s *Semaphore, r AdmitRequest) error {
	short, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	_, err := s.Admit(short, r)
	return err
}

// TestAdmit_QueueFullRefusalCarriesTheBound pins RFC GK D6: a waiting run
// refused because a queue is at its bound is told the bound and how many runs
// hold it, in the typed error and in its text, so a caller can size its next
// batch.
func TestAdmit_QueueFullRefusalCarriesTheBound(t *testing.T) {
	t.Run("the user's share of the queue", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s := New(1, 8, 0).WithQueuedPerUserCap(2)
		busy, err := s.Admit(ctx, AdmitRequest{TenantID: "a", UserID: "u"})
		if err != nil {
			t.Fatal(err)
		}
		defer busy()
		for i := 0; i < 2; i++ {
			_, infos := admitAsync(t, ctx, s, AdmitRequest{TenantID: "a", UserID: "u"})
			recvInfo(t, infos)
		}
		var bp *BackpressureError
		err = refuseAdmit(ctx, s, AdmitRequest{TenantID: "a", UserID: "u"})
		if !errors.As(err, &bp) || bp.QueueCapacity != 2 || bp.QueuedNow != 2 {
			t.Fatalf("refusal = %#v (%v), want queue_capacity 2, queued_now 2", bp, err)
		}
		if !strings.Contains(err.Error(), "queue full for this user (queue_capacity=2, queued_now=2)") {
			t.Errorf("refusal text %q does not name the bound", err)
		}
	})
	t.Run("the whole queue", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s := New(1, 1, 0)
		busy, err := s.Admit(ctx, AdmitRequest{TenantID: "a", UserID: "u"})
		if err != nil {
			t.Fatal(err)
		}
		defer busy()
		_, infos := admitAsync(t, ctx, s, AdmitRequest{TenantID: "b", UserID: "v"})
		recvInfo(t, infos)
		var bp *BackpressureError
		err = refuseAdmit(ctx, s, AdmitRequest{TenantID: "c", UserID: "w"})
		if !errors.As(err, &bp) || bp.QueueCapacity != 1 || bp.QueuedNow != 1 {
			t.Fatalf("refusal = %#v (%v), want queue_capacity 1, queued_now 1", bp, err)
		}
	})
	t.Run("the user's runs waiting on its cap", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s := New(4, 8, 0).WithPerUserCap(1).WithQueuedPerUserCap(1)
		busy, err := s.Admit(ctx, AdmitRequest{TenantID: "a", UserID: "u"})
		if err != nil {
			t.Fatal(err)
		}
		defer busy()
		_, infos := admitAsync(t, ctx, s, AdmitRequest{TenantID: "a", UserID: "u"})
		recvInfo(t, infos)
		var pue *ErrPerUserQuotaExhausted
		err = refuseAdmit(ctx, s, AdmitRequest{TenantID: "a", UserID: "u"})
		if !errors.As(err, &pue) || pue.QueueCapacity != 1 || pue.QueuedNow != 1 {
			t.Fatalf("refusal = %#v (%v), want queue_capacity 1, queued_now 1", pue, err)
		}
	})
	t.Run("a run that asked not to wait names no queue", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s := New(1, 8, 0)
		busy, err := s.Admit(ctx, AdmitRequest{TenantID: "a", UserID: "u"})
		if err != nil {
			t.Fatal(err)
		}
		defer busy()
		var bp *BackpressureError
		err = refuseAdmit(ctx, s, AdmitRequest{TenantID: "b", UserID: "v", Reject: true})
		if !errors.As(err, &bp) || bp.QueueCapacity != 0 || strings.Contains(err.Error(), "queue_capacity") {
			t.Fatalf("reject refusal = %#v (%v), want no queue figures", bp, err)
		}
	})
}
