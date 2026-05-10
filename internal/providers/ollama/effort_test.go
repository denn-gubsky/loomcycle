package ollama

import (
	"testing"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// Ollama doesn't translate effort — it has no operator-controlled
// thinking-budget knob today. The capability flag tells the loop
// to log "effort dropped" once per Run for visibility. These tests
// pin both the capability contract and the silent-drop invariant.

func TestCapabilities_SupportsEffortIsFalse(t *testing.T) {
	// SupportsEffort=false means: the loop logs "effort dropped"
	// when an agent declares effort on an Ollama-resolved run.
	// Operators see clearly that the hint was discarded rather
	// than silently believing the agent thought hard.
	d := New("http://localhost:11434", streamhttp.Options{}, nil)
	if d.Capabilities().SupportsEffort {
		t.Error("Ollama driver must report SupportsEffort=false (no thinking-budget knob today)")
	}
}

// We don't test the wire body here because Ollama's Capabilities
// flag is the contract — drivers with SupportsEffort=false promise
// the field is dropped, but they don't have to make it visible at
// the wire level. The loop's log-once-per-Run is the operator-
// facing signal. The behavior is exercised end-to-end in the
// internal/loop tests via fakeProvider with SupportsEffort=false.
