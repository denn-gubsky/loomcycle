package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

func TestOpenModeRefusal_OnlyOpenModeOnAPublicListenerWithoutOptIn(t *testing.T) {
	cases := []struct {
		name           string
		authConfigured bool
		env            config.Env
		wantRefused    bool
	}{
		{"open on loopback", false, config.Env{ListenAddr: "127.0.0.1:8787"}, false},
		{"open on all interfaces", false, config.Env{ListenAddr: "0.0.0.0:8787"}, true},
		{"open, gRPC on all interfaces", false, config.Env{ListenAddr: "127.0.0.1:8787", GrpcAddr: ":8788"}, true},
		{"open, opted in", false, config.Env{ListenAddr: "0.0.0.0:8787", AllowOpenMode: true}, false},
		{"auth configured on all interfaces", true, config.Env{ListenAddr: "0.0.0.0:8787", GrpcAddr: ":8788"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := openModeRefusal(tc.authConfigured, nil, &tc.env)
			if (err != nil) != tc.wantRefused {
				t.Fatalf("openModeRefusal = %v, want refused=%v", err, tc.wantRefused)
			}
			if err != nil && !strings.Contains(err.Error(), "LOOMCYCLE_ALLOW_OPEN_MODE=1") {
				t.Errorf("refusal should name the opt-in: %v", err)
			}
		})
	}
}

// A store that cannot be read at boot leaves the answer unknown; the boot is
// refused rather than guessed in either direction, on loopback too.
func TestOpenModeRefusal_StoreErrorRefuses(t *testing.T) {
	env := config.Env{ListenAddr: "127.0.0.1:8787"}
	err := openModeRefusal(false, errors.New("store unavailable"), &env)
	if err == nil || !strings.Contains(err.Error(), "cannot tell whether auth is configured") {
		t.Fatalf("openModeRefusal with a store error = %v, want a refusal saying why", err)
	}
}

// logRecorder is a slog.Handler that records each line's level and message
// and signals each one.
type logRecorder struct {
	mu     sync.Mutex
	lines  []string
	levels []slog.Level
	ch     chan struct{}
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	r.lines = append(r.lines, rec.Message)
	r.levels = append(r.levels, rec.Level)
	r.mu.Unlock()
	// Never block the loop under test: a full channel only drops a wake-up.
	select {
	case r.ch <- struct{}{}:
	default:
	}
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }

func (r *logRecorder) logger() *slog.Logger { return slog.New(r) }

func waitForLine(t *testing.T, r *logRecorder) {
	t.Helper()
	select {
	case <-r.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no warning logged within 5s")
	}
}

func TestWarnOpenMode_RepeatsUntilAuthConfigured(t *testing.T) {
	var configured atomic.Bool
	rec := &logRecorder{ch: make(chan struct{}, 8)}
	done := make(chan struct{})
	go func() {
		warnOpenMode(context.Background(), 5*time.Millisecond,
			func(context.Context) (bool, error) { return configured.Load(), nil }, rec.logger())
		close(done)
	}()
	waitForLine(t, rec) // at once
	waitForLine(t, rec) // and again
	configured.Store(true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("warnOpenMode kept running after auth was configured")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !strings.Contains(rec.lines[0], "open mode") {
		t.Errorf("warning = %q, want it to say open mode", rec.lines[0])
	}
	if rec.levels[0] != slog.LevelWarn {
		t.Errorf("warning level = %v, want WARN", rec.levels[0])
	}
}

func TestWarnOpenMode_ReturnsOnContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &logRecorder{ch: make(chan struct{}, 8)}
	done := make(chan struct{})
	go func() {
		warnOpenMode(ctx, time.Hour, func(context.Context) (bool, error) { return false, nil }, rec.logger())
		close(done)
	}()
	waitForLine(t, rec)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("warnOpenMode did not return when its context ended")
	}
}

func TestWarnOpenMode_SilentWhenAuthConfigured(t *testing.T) {
	rec := &logRecorder{ch: make(chan struct{}, 8)}
	warnOpenMode(context.Background(), time.Millisecond, func(context.Context) (bool, error) { return true, nil }, rec.logger())
	if len(rec.lines) != 0 {
		t.Errorf("logged %v with auth configured", rec.lines)
	}
}

// A tick whose store read fails says nothing — it does not know the server is
// open — and does not stop the loop, which warns again once the read works.
func TestWarnOpenMode_StoreErrorTickNeitherWarnsNorStops(t *testing.T) {
	var calls atomic.Int64
	rec := &logRecorder{ch: make(chan struct{}, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		warnOpenMode(ctx, 5*time.Millisecond, func(context.Context) (bool, error) {
			if calls.Add(1) <= 3 {
				return false, errors.New("store unavailable")
			}
			return false, nil
		}, rec.logger())
		close(done)
	}()
	waitForLine(t, rec)
	cancel()
	<-done
	if n := calls.Load(); n < 4 {
		t.Errorf("warned after %d reads, want the first warning only after the 3 failed reads", n)
	}
}
