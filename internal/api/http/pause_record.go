package http

import (
	"context"
	"encoding/json"
	"log"
	"slices"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A run's pauses, on its own transcript.
//
// A timeout_ms clock does not run while the runtime is paused. The live clock
// watches the pause manager; a clock re-armed after a resume — a parent
// resumed on this instance, after a restart, or restored from a snapshot on
// another one, waiting on a child it no longer runs — can only read what the
// child's run recorded. So a run records each pause it parks for:
//
//   - pause_began when it parks (Park, or PauseIdle for a run that is
//     waiting), written before the barrier is credited so a snapshot taken at
//     the pause carries it. Its `since` is when the RUNTIME paused, which is
//     where the live clock stopped — a run finishing its call parks later.
//   - pause_ended when it is released: by the resume, or by its re-dispatch
//     after a restore or a restart, so the downtime in between is inside the
//     pause.
//
// Store-only rows: the live stream never carries them, and a re-attach skips
// them. A run that never parked for a pause — one still in a tool call
// throughout — records none, and a re-armed clock counts that pause.
const (
	eventPauseBegan = "pause_began"
	eventPauseEnded = "pause_ended"
)

// pauseBeganRecord is pause_began's payload.
type pauseBeganRecord struct {
	Since time.Time `json:"since"`
}

// pauseEndingEvents end a recorded pause whose pause_ended never landed: only
// a run that is working writes them, so it was no longer parked.
var pauseEndingEvents = []string{"text", "tool_call", "tool_result", "done"}

// appendPauseRecord writes a pause row on the run's transcript under a
// bounded, non-cancellable ctx, as the pause_state write is made. A failed
// write is logged: the run still parks or resumes, and a re-armed timeout_ms
// then counts that pause.
func appendPauseRecord(parent context.Context, st store.Store, runID, typ string, payload any) {
	if st == nil || runID == "" {
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), pauseStatePersistTimeout)
	defer cancel()
	if err := st.AppendEvent(ctx, runID, typ, b); err != nil {
		log.Printf("pause: record %s for run %s failed: %v — a resumed timeout_ms counts this pause", typ, runID, err)
	}
}

// pauseOpen reports whether the run's last recorded pause has not ended.
func pauseOpen(events []store.Event) bool {
	open := false
	for _, ev := range events {
		switch {
		case ev.Type == eventPauseBegan:
			open = true
		case ev.Type == eventPauseEnded || slices.Contains(pauseEndingEvents, ev.Type):
			open = false
		}
	}
	return open
}
