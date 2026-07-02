package ollama

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// TestStream_InStreamErrorEmitsEventError is the regression for Ollama's silent
// in-stream failure: /api/chat commits a 200 then, on a mid-generation fault,
// writes a final {"error":"..."} NDJSON line with no done:true. The chunk struct
// had no Error field, so the frame was ignored and the run ended as a clean
// EventDone{StopReason:""} — the loop treated a failed generation as success
// with truncated/empty output and NO EventError. The driver must now surface it.
func TestStream_InStreamErrorEmitsEventError(t *testing.T) {
	frames := []string{
		`{"model":"qwen3.6:latest","message":{"role":"assistant","content":"partial ans"},"done":false}` + "\n",
		`{"error":"llama runner process has terminated: signal: killed"}` + "\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "qwen3.6:latest",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var gotError bool
	var errText string
	var text strings.Builder
	for ev := range ch {
		switch ev.Type {
		case providers.EventError:
			gotError = true
			errText = ev.Error
		case providers.EventText:
			text.WriteString(ev.Text)
		}
	}

	if !gotError {
		t.Fatal("in-stream {\"error\":...} frame must surface EventError (was silently swallowed → false success)")
	}
	if !strings.Contains(errText, "terminated") {
		t.Errorf("EventError text = %q, want it to carry the Ollama error message", errText)
	}
	// Text delivered before the error is still flushed (not silently dropped).
	if text.String() != "partial ans" {
		t.Errorf("pre-error text = %q, want the delivered bytes flushed (%q)", text.String(), "partial ans")
	}
}
