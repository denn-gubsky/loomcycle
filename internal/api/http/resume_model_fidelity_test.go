package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
)

// tierServerForResume builds a server whose agent routes by TIER (not a pin),
// with two candidates. The pin path deliberately has no cascade, so only a
// tiered agent exercises the Gap 5 restore.
func tierServerForResume(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		Agents: map[string]config.AgentDef{
			"router": {Tier: "middle", SystemPrompt: "p", Tools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	srv, _ := makeServer(t, completingProvider(), cfg)
	res := resolve.NewResolver([]string{"primary", "secondary"}, map[string][]resolve.Candidate{
		"middle": {
			{Provider: "primary", Model: "model-a"},
			{Provider: "secondary", Model: "model-b"},
		},
	})
	res.SetReachable("primary", true, []string{"model-a"}, "")
	res.SetReachable("secondary", true, []string{"model-b"}, "")
	srv.SetResolver(res)
	return srv
}

// RFC DD Gap 5. The runs row records the model a run STARTED on; resume used to
// ignore it and re-derive from the definition, so a run came back on whatever
// the tier resolves to now — silently, with nothing in the transcript saying it
// moved.
func TestResume_RestoresTheModelTheRunStartedOn(t *testing.T) {
	srv := tierServerForResume(t)
	def := srv.cfg().Agents["router"]
	ctx := context.Background()

	// Sanity: the definition's own answer is model-a. Without this the test
	// could pass while restoring nothing, because "restored" and "re-derived"
	// would be the same string.
	_, derived, _, err := srv.resolveAgentDef(ctx, def, "", "alice", "router", "", false)
	if err != nil {
		t.Fatalf("resolveAgentDef: %v", err)
	}
	if derived != "model-a" {
		t.Fatalf("fixture drifted: definition resolves %q, expected model-a", derived)
	}

	// The run started on the SECOND candidate.
	pid, ok := srv.providerForModel(ctx, def, "", "alice", "router", "", false, "model-b")
	if !ok {
		t.Fatal("model-b is a live candidate for this tier but was not accepted — " +
			"a resumed run would silently move to model-a")
	}
	if pid != "secondary" {
		t.Errorf("provider = %q, want secondary", pid)
	}
}

// Re-validation, not blind trust: a model the definition no longer offers must
// be refused, so a snapshot cannot reintroduce a model the operator removed.
func TestResume_RefusesAModelTheDefinitionNoLongerOffers(t *testing.T) {
	srv := tierServerForResume(t)
	def := srv.cfg().Agents["router"]

	if _, ok := srv.providerForModel(context.Background(), def, "", "alice", "router", "", false, "model-withdrawn"); ok {
		t.Error("accepted a model outside the definition's cascade — a promoted def that " +
			"dropped a model could have it carried back in through a snapshot")
	}
}

// A PINNED agent has no cascade: the definition names the model outright, so
// the re-derived value already IS the definition's answer and there is nothing
// to restore. Returning false here is what keeps the pinned path byte-identical.
func TestResume_PinnedAgentHasNothingToRestore(t *testing.T) {
	srv := tierServerForResume(t)
	pinned := config.AgentDef{Provider: "primary", Model: "model-a"}

	if _, ok := srv.providerForModel(context.Background(), pinned, "", "alice", "pinned", "", false, "model-a"); ok {
		t.Error("pinned agent reported a cascade match; the pin path must stay untouched")
	}
}

// Guards the two degenerate inputs rather than leaving them to a nil deref.
func TestResume_ProviderForModelHandlesEmptyInputs(t *testing.T) {
	srv := tierServerForResume(t)
	def := srv.cfg().Agents["router"]
	ctx := context.Background()

	if _, ok := srv.providerForModel(ctx, def, "", "alice", "router", "", false, ""); ok {
		t.Error("an empty model was accepted")
	}
	noResolver := &Server{}
	if _, ok := noResolver.providerForModel(ctx, def, "", "alice", "router", "", false, "model-b"); ok {
		t.Error("a server with no resolver reported a match")
	}
}
