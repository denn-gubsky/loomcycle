package http

import (
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
)

// schedulerTestLogger routes a scheduler's log lines into the test log, as
// passing t.Logf did when the scheduler took a printf func. With a non-nil
// over, lines written once it is set are dropped: a goroutine that outlives
// the test would otherwise call t.Log after the test returned, which panics.
func schedulerTestLogger(t *testing.T, over *atomic.Bool) *slog.Logger {
	return slog.New(slog.NewTextHandler(tLogWriter{t: t, over: over}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type tLogWriter struct {
	t    *testing.T
	over *atomic.Bool
}

func (w tLogWriter) Write(p []byte) (int, error) {
	if w.over == nil || !w.over.Load() {
		w.t.Log(strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}
