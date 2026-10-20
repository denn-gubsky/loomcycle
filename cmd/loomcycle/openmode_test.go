package main

import (
	"context"
	"fmt"
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
			err := openModeRefusal(tc.authConfigured, &tc.env)
			if (err != nil) != tc.wantRefused {
				t.Fatalf("openModeRefusal = %v, want refused=%v", err, tc.wantRefused)
			}
			if err != nil && !strings.Contains(err.Error(), "LOOMCYCLE_ALLOW_OPEN_MODE=1") {
				t.Errorf("refusal should name the opt-in: %v", err)
			}
		})
	}
}

// logRecorder counts warnings and signals each one.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
	ch    chan struct{}
}

func (r *logRecorder) logf(format string, args ...any) {
	r.mu.Lock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
	r.mu.Unlock()
	r.ch <- struct{}{}
}

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
			func(context.Context) bool { return configured.Load() }, rec.logf)
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
	if !strings.Contains(rec.lines[0], "open mode") {
		t.Errorf("warning = %q, want it to say open mode", rec.lines[0])
	}
}

func TestWarnOpenMode_ReturnsOnContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &logRecorder{ch: make(chan struct{}, 8)}
	done := make(chan struct{})
	go func() {
		warnOpenMode(ctx, time.Hour, func(context.Context) bool { return false }, rec.logf)
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
	warnOpenMode(context.Background(), time.Millisecond, func(context.Context) bool { return true }, rec.logf)
	if len(rec.lines) != 0 {
		t.Errorf("logged %v with auth configured", rec.lines)
	}
}
