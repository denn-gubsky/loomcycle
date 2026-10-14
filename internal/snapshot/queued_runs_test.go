package snapshot

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A restore takes a queued run's request only in the shape Capture writes: a
// hand-made envelope cannot bring in sealed secrets, store a no-trace run's
// prompt, or store bytes that are not a request.
func TestRestorableQueuedInput_RefusesWhatCaptureNeverWrites(t *testing.T) {
	withSecrets := json.RawMessage(`{"input":{"Agent":"r"},"secrets":{"key_id":"k","nonce":"n","ciphertext":"c"}}`)
	got, ok := restorableQueuedInput(QueuedRunEntry{QueuedInput: withSecrets})
	if !ok || strings.Contains(string(got), "ciphertext") || !strings.Contains(string(got), `"Agent":"r"`) {
		t.Errorf("an envelope with sealed secrets restored %s (%v), want the request without them", got, ok)
	}
	if _, ok := restorableQueuedInput(QueuedRunEntry{QueuedInput: json.RawMessage(`{"input":{}}`), Retention: store.RetentionNone}); ok {
		t.Error("a no-trace run's request was restored")
	}
	for _, bad := range []string{`"a string"`, `[1,2]`, `not json`} {
		if _, ok := restorableQueuedInput(QueuedRunEntry{QueuedInput: json.RawMessage(bad)}); ok {
			t.Errorf("request %s was restored, want refused", bad)
		}
	}
	if _, ok := restorableQueuedInput(QueuedRunEntry{}); ok {
		t.Error("an empty request was restored")
	}
}
