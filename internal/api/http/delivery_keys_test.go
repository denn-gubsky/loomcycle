package http

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The webhook receiver hands RunOnce both durable dedup keys of a delivery;
// the run row must carry both, and a later run reusing either is refused
// before its loop starts.
func TestRunOnce_PersistsBothDeliveryKeysAndRefusesAnAltDuplicate(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Agents = map[string]config.AgentDef{
		"worker": {Model: "stub-model", Tools: []string{}, SystemPrompt: "static"},
	}
	srv, st, _ := spawnCeilingServer(t, cfg, &recordingScriptedProvider{defaultS: endTurn()}, nil)
	ctx := context.Background()
	run := func(agentID, key, alt string) error {
		return srv.RunOnce(ctx, runner.RunInput{
			Agent: "worker", AgentID: agentID, IdempotencyKey: key, DeliveryAltKey: alt,
			Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "x"}}}},
		}, runner.RunCallbacks{})
	}

	if err := run("a_first", "key-1", "alt-1"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, err := st.GetRunByAgentID(ctx, "a_first")
	if err != nil {
		t.Fatal(err)
	}
	if got.IdempotencyKey != "key-1" || got.DeliveryAltKey != "alt-1" {
		t.Errorf("run keys = (%q, %q), want (key-1, alt-1)", got.IdempotencyKey, got.DeliveryAltKey)
	}

	if err := run("a_dup", "key-2", "alt-1"); !errors.Is(err, store.ErrDuplicateIdempotencyKey) {
		t.Fatalf("RunOnce reusing the alt key = %v, want ErrDuplicateIdempotencyKey", err)
	}
	var nf *store.ErrNotFound
	if _, err := st.GetRunByAgentID(ctx, "a_dup"); !errors.As(err, &nf) {
		t.Errorf("the refused delivery left a run row (err=%v)", err)
	}
}
