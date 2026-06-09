package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// emitFixture builds a Server over in-memory sqlite with a session + run, so a
// makeRecordingEmit closure can persist events. redactor may be nil (disabled).
func emitFixture(t *testing.T, redactor *redact.Redactor) (*Server, store.Store, string, context.Context, func()) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "tenant", "agent", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_test"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	srv := &Server{store: st, redactor: redactor}
	return srv, st, run.ID, ctx, func() { _ = st.Close() }
}

// storedToolResult reads back the single tool_result event's persisted payload,
// decoded into a providers.Event.
func storedToolResult(t *testing.T, st store.Store, ctx context.Context) providers.Event {
	t.Helper()
	evs, _, err := st.ListEvents(ctx, store.EventFilter{Type: string(providers.EventToolResult)}, 100, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("expected exactly 1 tool_result event, got %d", len(evs))
	}
	var decoded providers.Event
	if err := json.Unmarshal(evs[0].Payload, &decoded); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return decoded
}

const emitSecret = "abc123def456ghi789xyz"

// TestRecordingEmit_RedactsSecretInToolResult is the F32 transcript regression:
// a secret in a tool_result's Text AND in the tool_call Input is masked in the
// PERSISTED event, while the event forwarded to the live SSE stream is left
// intact. Fails on the pre-F32 code, which marshaled ev verbatim.
func TestRecordingEmit_RedactsSecretInToolResult(t *testing.T) {
	redactor := redact.New(map[string]string{"LOOMCYCLE_GITEA_TOKEN": emitSecret}, true)
	srv, st, runID, ctx, cleanup := emitFixture(t, redactor)
	defer cleanup()

	var forwarded providers.Event
	emit := srv.makeRecordingEmit(ctx, runID, func(ev providers.Event) { forwarded = ev })

	input := json.RawMessage(`{"command":"curl -H \"Authorization: token ` + emitSecret + `\" https://gitea"}`)
	emit(providers.Event{
		Type:    providers.EventToolResult,
		ToolUse: &providers.ToolUse{ID: "t1", Name: "Bash", Input: input},
		Text:    "ran: Authorization: token " + emitSecret + " -> 200 OK",
	})

	// Persisted copy: the secret is gone from both Text and Input.
	got := storedToolResult(t, st, ctx)
	if strings.Contains(got.Text, emitSecret) {
		t.Errorf("secret survived in persisted Text: %q", got.Text)
	}
	if strings.Contains(string(got.ToolUse.Input), emitSecret) {
		t.Errorf("secret survived in persisted Input: %s", got.ToolUse.Input)
	}
	if !strings.Contains(got.Text, "[redacted:LOOMCYCLE_GITEA_TOKEN]") {
		t.Errorf("expected named redaction marker in Text: %q", got.Text)
	}
	if !json.Valid(got.ToolUse.Input) {
		t.Errorf("persisted Input is not valid JSON: %s", got.ToolUse.Input)
	}

	// Live stream: the forwarded event is UNredacted (caller already holds it).
	if !strings.Contains(forwarded.Text, emitSecret) {
		t.Errorf("forwarded (SSE) event should NOT be redacted; got %q", forwarded.Text)
	}
}

// TestRecordingEmit_NoRedactorWhenDisabled — LOOMCYCLE_REDACT_SECRETS=0 leaves
// s.redactor nil; the payload is persisted verbatim (opt-out works).
func TestRecordingEmit_NoRedactorWhenDisabled(t *testing.T) {
	srv, st, runID, ctx, cleanup := emitFixture(t, nil) // redaction disabled
	defer cleanup()

	emit := srv.makeRecordingEmit(ctx, runID, func(providers.Event) {})
	emit(providers.Event{
		Type:    providers.EventToolResult,
		ToolUse: &providers.ToolUse{ID: "t1", Name: "Bash", Input: json.RawMessage(`{"x":"y"}`)},
		Text:    "token " + emitSecret,
	})

	got := storedToolResult(t, st, ctx)
	if !strings.Contains(got.Text, emitSecret) {
		t.Errorf("with redaction disabled the secret should persist verbatim; got %q", got.Text)
	}
}
