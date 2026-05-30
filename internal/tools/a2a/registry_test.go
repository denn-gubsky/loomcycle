package a2a

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakePeerLister stubs the substrate name listing for RegisterTools.
type fakePeerLister struct {
	names []store.A2AAgentDefNameSummary
	err   error
}

func (f fakePeerLister) A2AAgentDefListNames(ctx context.Context) ([]store.A2AAgentDefNameSummary, error) {
	return f.names, f.err
}

func toolNames(ts []toolNamer) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	sort.Strings(out)
	return out
}

type toolNamer interface{ Name() string }

// resolverFor returns a DefResolver backed by an in-memory peer map so
// RegisterTools can resolve each enumerated peer to its expected_skills.
func resolverFor(peers map[string]config.A2AAgent) DefResolver {
	return func(ctx context.Context, name string) (config.A2AAgent, bool) {
		a, ok := peers[name]
		return a, ok
	}
}

// TestRegisterTools_OneToolPerYamlPeerSkill asserts the static mirror of
// MCP: each (yaml peer, expected_skill) pair becomes one
// a2a__<peer>__<skill> tool.
func TestRegisterTools_OneToolPerYamlPeerSkill(t *testing.T) {
	cfg := &config.Config{A2AAgents: map[string]config.A2AAgent{
		"alpha": {ExpectedSkills: []config.A2AExpectedSkill{{ID: "research"}, {ID: "summarize"}}},
		"beta":  {ExpectedSkills: []config.A2AExpectedSkill{{ID: "translate"}}},
	}}
	resolve := resolverFor(map[string]config.A2AAgent{
		"alpha": cfg.A2AAgents["alpha"],
		"beta":  cfg.A2AAgents["beta"],
	})
	got := RegisterTools(context.Background(), cfg, fakePeerLister{}, resolve, factoryFor(&fakePeer{}), nil)

	namers := make([]toolNamer, len(got))
	for i, tl := range got {
		namers[i] = tl
	}
	names := toolNames(namers)
	want := []string{"a2a__alpha__research", "a2a__alpha__summarize", "a2a__beta__translate"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

// TestRegisterTools_IncludesActiveSubstratePeers asserts a peer present
// only as an ACTIVE substrate def (not in yaml) is registered too.
func TestRegisterTools_IncludesActiveSubstratePeers(t *testing.T) {
	cfg := &config.Config{}
	peers := map[string]config.A2AAgent{
		"gamma": {ExpectedSkills: []config.A2AExpectedSkill{{ID: "classify"}}},
	}
	lister := fakePeerLister{names: []store.A2AAgentDefNameSummary{
		{Name: "gamma", ActiveDefID: "def-1"},
	}}
	got := RegisterTools(context.Background(), cfg, lister, resolverFor(peers), factoryFor(&fakePeer{}), nil)
	if len(got) != 1 || got[0].Name() != "a2a__gamma__classify" {
		t.Fatalf("got %v, want one a2a__gamma__classify tool", got)
	}
}

// TestRegisterTools_SkipsSubstrateNamesWithNoActiveVersion asserts a
// substrate name that has no active def (draft/retired only) is NOT
// registered — its tools would always error.
func TestRegisterTools_SkipsSubstrateNamesWithNoActiveVersion(t *testing.T) {
	cfg := &config.Config{}
	lister := fakePeerLister{names: []store.A2AAgentDefNameSummary{
		{Name: "draftonly", ActiveDefID: ""},
	}}
	got := RegisterTools(context.Background(), cfg, lister, resolverFor(map[string]config.A2AAgent{}), factoryFor(&fakePeer{}), nil)
	if len(got) != 0 {
		t.Fatalf("got %d tools, want 0 for a name with no active version", len(got))
	}
}

// TestRegisterTools_SkipsPeerWithNoExpectedSkills asserts a peer that
// declares no expected_skills produces no tool (the synthetic tool fronts
// exactly one skill, so there is nothing to target).
func TestRegisterTools_SkipsPeerWithNoExpectedSkills(t *testing.T) {
	cfg := &config.Config{A2AAgents: map[string]config.A2AAgent{
		"noskills": {},
	}}
	got := RegisterTools(context.Background(), cfg, fakePeerLister{}, resolverFor(map[string]config.A2AAgent{"noskills": {}}), factoryFor(&fakePeer{}), nil)
	if len(got) != 0 {
		t.Fatalf("got %d tools, want 0 for a peer with no expected_skills", len(got))
	}
}

// TestRegisterTools_DedupsYamlAndSubstrateSameName asserts a name present
// in BOTH yaml and the substrate registers ONE set of tools (yaml is the
// resolver's precedence source; no duplicate tools).
func TestRegisterTools_DedupsYamlAndSubstrateSameName(t *testing.T) {
	cfg := &config.Config{A2AAgents: map[string]config.A2AAgent{
		"dup": {ExpectedSkills: []config.A2AExpectedSkill{{ID: "s1"}}},
	}}
	lister := fakePeerLister{names: []store.A2AAgentDefNameSummary{
		{Name: "dup", ActiveDefID: "def-1"},
	}}
	got := RegisterTools(context.Background(), cfg, lister, resolverFor(cfg.A2AAgents), factoryFor(&fakePeer{}), nil)
	if len(got) != 1 {
		t.Fatalf("got %d tools, want 1 (deduped)", len(got))
	}
}

// TestRegisterTools_SubstrateListErrorIsNonFatal asserts a store error
// listing substrate peers does NOT block registration of the yaml peers —
// a transient store failure must not stop loomcycle boot.
func TestRegisterTools_SubstrateListErrorIsNonFatal(t *testing.T) {
	cfg := &config.Config{A2AAgents: map[string]config.A2AAgent{
		"alpha": {ExpectedSkills: []config.A2AExpectedSkill{{ID: "research"}}},
	}}
	lister := fakePeerLister{err: errors.New("db down")}
	got := RegisterTools(context.Background(), cfg, lister, resolverFor(cfg.A2AAgents), factoryFor(&fakePeer{}), nil)
	if len(got) != 1 || got[0].Name() != "a2a__alpha__research" {
		t.Fatalf("got %v, want yaml peer still registered despite substrate list error", got)
	}
}
