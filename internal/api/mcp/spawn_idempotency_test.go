package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// dupRunner refuses every run as a duplicate of an existing one, as RunOnce
// does for a key that is already held.
type dupRunner struct{ lastInput runner.RunInput }

func (d *dupRunner) RunOnce(_ context.Context, in runner.RunInput, _ runner.RunCallbacks) error {
	d.lastInput = in
	return &runner.DuplicateRunError{RunID: "r_first", AgentID: "a_first", SessionID: "s_first"}
}

// The streaming branch builds its RunInput by hand and reads the result from
// the run's own events. A duplicate has neither: the key must still reach
// RunOnce, and the answer must come from the connector's join.
func TestSpawnRunStreaming_ADuplicateKeyIsAnsweredByTheConnectorsJoin(t *testing.T) {
	existing := connector.SpawnRunResult{AgentID: "a_first", RunID: "r_first", SessionID: "s_first",
		Status: "completed", FinalText: "the first answer", Deduplicated: true}
	mc := &mockConnector{spawnResult: existing}
	dr := &dupRunner{}
	sess := NewSession()
	sess.MarkInitialized()
	sess.SetRunEventsEnabled(true)
	env := &handlerEnv{connector: mc, runner: dr, session: sess}

	res, err := spawnRunStreaming(context.Background(), env, connector.SpawnRunRequest{Agent: "a", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("spawnRunStreaming: %v", err)
	}
	if dr.lastInput.ClientIdempotencyKey != "k1" || dr.lastInput.IdempotencyKey != "" {
		t.Errorf("RunOnce got ClientIdempotencyKey=%q IdempotencyKey=%q; want the caller's key in the first only",
			dr.lastInput.ClientIdempotencyKey, dr.lastInput.IdempotencyKey)
	}
	if res.RunID != "r_first" || !res.Deduplicated || res.FinalText != "the first answer" {
		t.Errorf("result = %+v, want the existing run from the connector", res)
	}
	if got, _ := mc.spawnReq.Load().(connector.SpawnRunRequest); got.IdempotencyKey != "k1" {
		t.Errorf("the connector was asked with key %q, want k1", got.IdempotencyKey)
	}
}

// The transport timeout cancels a run this call started and reports it as
// timed out. A run it only joined by key is neither cancelled nor timed out:
// the result says it is still running.
func TestSpawnRun_ATimeoutOnAJoinedRunIsNotReportedAsCancelled(t *testing.T) {
	mc := &mockConnector{
		spawnGate:   make(chan struct{}), // never opened: the join outlasts the timeout
		spawnResult: connector.SpawnRunResult{RunID: "r_first", Status: "running", Deduplicated: true},
	}
	env := &handlerEnv{connector: mc, session: NewSession()}
	start := time.Now()
	out, err := handleSpawnRun(context.Background(), env,
		[]byte(`{"agent":"a","idempotency_key":"k1","timeout_ms":100,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatalf("handleSpawnRun: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("the call did not honour its timeout")
	}
	if len(out.Content) == 0 {
		t.Fatalf("no content in %+v", out)
	}
	text := out.Content[0].Text
	for _, want := range []string{`"status":"running"`, `"deduplicated":true`} {
		if !strings.Contains(text, want) {
			t.Errorf("result %s lacks %s", text, want)
		}
	}
	if strings.Contains(text, "timeout") || strings.Contains(text, "cancelled") {
		t.Errorf("result reports a timeout or a cancel for a run this call did not start: %s", text)
	}
}
