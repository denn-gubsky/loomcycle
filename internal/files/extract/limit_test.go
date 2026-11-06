package extract

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// heldSink blocks every extraction writing to it until open is closed, so a
// test can keep extractions running for as long as it needs to.
type heldSink struct {
	memSink
	started chan<- struct{}
	open    <-chan struct{}
	once    sync.Once
}

func (s *heldSink) Text(md string) error {
	s.once.Do(func() { s.started <- struct{}{} })
	<-s.open
	return s.memSink.Text(md)
}

// waitFor polls cond until it holds or a second passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestExtract_ConcurrencyIsBounded: with a limit of N, N extractions run and
// the N+1th waits for one of them to finish before it starts; a waiting
// extraction whose context ends gives up at once with ErrBusy and the
// context's error, holding nothing; and a context already ended takes no slot.
func TestExtract_ConcurrencyIsBounded(t *testing.T) {
	const n = 3
	lim := NewLimiter(n)
	if st := lim.Stats(); st.Limit != n {
		t.Fatalf("limit = %d, want %d", st.Limit, n)
	}
	csv := []byte("a,b\n1,2\n")
	started := make(chan struct{}, n+1)
	open := make(chan struct{})
	extract := func(ctx context.Context) error {
		release, err := lim.Acquire(ctx)
		if err != nil {
			return err
		}
		defer release()
		_, err = CSV{}.Extract(ctx, bytes.NewReader(csv), int64(len(csv)), &heldSink{started: started, open: open}, Options{Name: "t.csv"})
		return err
	}

	errs := make(chan error, n+1)
	for range n + 1 {
		go func() { errs <- extract(context.Background()) }()
	}
	for range n {
		<-started
	}
	waitFor(t, "the extra extraction to queue", func() bool { return lim.Stats().Waiting == 1 })
	if st := lim.Stats(); st.InFlight != n {
		t.Fatalf("in flight = %d, want %d", st.InFlight, n)
	}
	select {
	case <-started:
		t.Fatal("the N+1th extraction started while N were running")
	case <-time.After(50 * time.Millisecond):
	}

	// A waiting extraction whose caller goes away leaves the queue promptly.
	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() { gaveUp <- extract(ctx) }()
	waitFor(t, "the cancelled extraction to queue", func() bool { return lim.Stats().Waiting == 2 })
	cancel()
	select {
	case err := <-gaveUp:
		if !errors.Is(err, ErrBusy) || !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled wait: err = %v, want ErrBusy and context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a cancelled extraction kept waiting for a slot")
	}
	waitFor(t, "the cancelled extraction to leave the queue", func() bool { return lim.Stats().Waiting == 1 })

	// A context already ended is refused, without waiting.
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if _, err := lim.Acquire(done); !errors.Is(err, ErrBusy) || !errors.Is(err, context.Canceled) {
		t.Errorf("ended context: err = %v", err)
	}

	// One finishing lets the waiting one start; all of them complete.
	close(open)
	for range n + 1 {
		if err := <-errs; err != nil {
			t.Errorf("extraction: %v", err)
		}
	}
	if st := lim.Stats(); st.InFlight != 0 || st.Waiting != 0 {
		t.Errorf("after: %+v, want nothing held or waiting", st)
	}
}

// A slot is given back once however many times release is called, and the
// default limit is the processor bound.
func TestLimiter_ReleaseIsIdempotentAndDefaultIsPositive(t *testing.T) {
	lim := NewLimiter(1)
	release, err := lim.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	if st := lim.Stats(); st.InFlight != 0 {
		t.Fatalf("in flight = %d after a double release", st.InFlight)
	}
	r2, err := lim.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r2()
	for _, n := range []int{0, -4} {
		if got := NewLimiter(n).Stats().Limit; got < 1 {
			t.Errorf("NewLimiter(%d) limit = %d", n, got)
		}
	}
}
