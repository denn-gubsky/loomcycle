package webhook

import (
	"net/http"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// F28: a def with no `goal` payload_mapping defaults the agent's segment to the
// RAW signed body (was "" → silent no-op).
func TestEffectiveGoal_NoMapping_DefaultsToRawBody(t *testing.T) {
	body := []byte(`{"pull_request":{"title":"Add slugify"}}`)
	w := config.Webhook{Agent: "a"} // no PayloadMapping
	proj := projectResult{Fields: map[string]string{}, RawBody: body}
	if got := effectiveGoal(w, proj); got != string(body) {
		t.Fatalf("effectiveGoal = %q, want raw body %q", got, string(body))
	}
}

// An explicit `goal` mapping wins over the raw-body default — the operator chose
// which field is the goal.
func TestEffectiveGoal_ExplicitMapping_UsesProjected(t *testing.T) {
	w := config.Webhook{Agent: "a", PayloadMapping: map[string]string{"goal": "$.pull_request.title"}}
	proj := projectResult{Fields: map[string]string{"goal": "Add slugify"}, RawBody: []byte(`{"whole":"body"}`)}
	if got := effectiveGoal(w, proj); got != "Add slugify" {
		t.Fatalf("effectiveGoal = %q, want the mapped value", got)
	}
}

// A mapped-but-empty goal is the operator's explicit choice — NOT overridden by
// the raw body (only an absent `goal` target triggers the default).
func TestEffectiveGoal_MappedButEmpty_Respected(t *testing.T) {
	w := config.Webhook{Agent: "a", PayloadMapping: map[string]string{"goal": "$.absent.path"}}
	proj := projectResult{Fields: map[string]string{"goal": ""}, RawBody: []byte(`{"x":1}`)}
	if got := effectiveGoal(w, proj); got != "" {
		t.Fatalf("effectiveGoal = %q, want empty (mapped target respected)", got)
	}
}

// buildRunInput threads the default through to the actual prompt segment.
func TestBuildRunInput_NoGoalMapping_SegmentIsRawBody(t *testing.T) {
	body := []byte(`{"action":"opened","number":7}`)
	w := config.Webhook{Agent: "a"}
	proj := projectResult{Fields: map[string]string{}, RawBody: body}
	in := buildRunInput(w, proj, map[string]bool{}, func(string) string { return "" }, nil)
	if len(in.Segments) != 1 || len(in.Segments[0].Content) != 1 {
		t.Fatalf("unexpected segment shape: %+v", in.Segments)
	}
	blk := in.Segments[0].Content[0]
	if blk.Text != string(body) {
		t.Errorf("segment text = %q, want raw body %q", blk.Text, string(body))
	}
	// Still fenced as untrusted — the body is attacker-influenceable.
	if blk.Type != "untrusted-block" || blk.Kind != "webhook_payload" {
		t.Errorf("segment not fenced as untrusted webhook_payload: %+v", blk)
	}
}

// End-to-end regression (fails on main): a signed delivery to a webhook with NO
// payload_mapping spawns the agent with the FULL body as its goal, not "".
func TestReceiver_NoGoalMapping_SpawnsWithBody(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"pull_request":{"title":"do the thing"},"action":"opened"}`)

	wh := config.Webhook{
		Enabled:  true,
		Delivery: "spawn",
		Agent:    "reviewer",
		Auth:     config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		// deliberately NO PayloadMapping
	}
	fr := &fakeRunner{runID: "r", agentID: "a"}
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr,
		nil, map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	w := doPost(rec, "gh", body, h)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if !fr.wasCalled() {
		t.Fatal("runner not invoked")
	}
	got := fr.input()
	if len(got.Segments) != 1 || len(got.Segments[0].Content) != 1 {
		t.Fatalf("unexpected segment shape: %+v", got.Segments)
	}
	if txt := got.Segments[0].Content[0].Text; txt != string(body) {
		t.Errorf("agent goal = %q, want the full signed body %q", txt, string(body))
	}
}
