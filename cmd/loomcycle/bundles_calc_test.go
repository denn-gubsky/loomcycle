package main

import (
	"slices"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// bundledCalc is the owner's ruling (RFC GY P1, Q5) for every bundled agent:
// a conversational, manager or answering agent keeps the Calc default; a
// single-purpose worker (a fixed extraction, transform or judge task, or a
// deterministic code-js worker) carries "-Calc". true = holds Calc.
var bundledCalc = map[string]map[string]bool{
	"agent-teams":    {"agent/assistant": true, "team/assistant": true, "team/orchestrator": true},
	"chat":           {"chat/medium": true, "chat/local": true, "chat/local-small": true},
	"dev-exec":       {"dev/exec": false},
	"dev-workspace":  {"dev/exec": false},
	"doc-colorizer":  {"doc/colorizer": false},
	"document-agent": {"doc/manager": true},
	"knowledge-base": {
		"kb/linker": false, "kb/verdict": false, "kb/judge": false, "kb/judge-verdict": false,
		"kb/annotator": false, "kb/annotate": false, "kb/taxonomist": false, "kb/taxonomy-writer": false,
		"kb/ontologist": false, "kb/ontology-writer": false,
		"kb/classify": false, "kb/screen": false, "kb/answer": false, "kb/read": false,
		"kb/ask": true, "kb/survey": true, "kb/search": true, "kb/walker": true,
	},
	"memory": {
		"memory/consolidator": false, "memory/extractor": false, "memory/judge": false,
		"memory/conflict-judge": false, "memory/ontologist": false,
	},
	"skybits-sync": {"skybits/sync": false, "skybits/import": false, "skybits/structure": false},
	"team-examples": {
		"sdlc/architect": true, "sdlc/coder": true, "sdlc/reviewer": true,
		"marketing/writer": true, "marketing/editor": true,
	},
}

// TestBundledAgents_CalcFollowsTheRuling pins the table above against every
// agent each bundle declares: an agent added to a bundle without a ruling
// fails here, as does a worker that loses its "-Calc".
func TestBundledAgents_CalcFollowsTheRuling(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "") // inline skills only
	base, err := config.LoadLayers(layersFor(t, "base")...)
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	for bundle, want := range bundledCalc {
		cfg, err := config.LoadLayers(layersFor(t, "base", bundle)...)
		if err != nil {
			t.Fatalf("base + %s: %v", bundle, err)
		}
		var declared []string
		for name, a := range cfg.Agents {
			if _, inBase := base.Agents[name]; inBase {
				continue
			}
			declared = append(declared, name)
			holds, ruled := want[name]
			if !ruled {
				t.Errorf("%s: agent %s has no Calc ruling in bundledCalc", bundle, name)
				continue
			}
			if got := slices.Contains(heldTools(a), "Calc"); got != holds {
				t.Errorf("%s: %s holds Calc = %v, want %v (tools %v)", bundle, name, got, holds, a.Tools)
			}
		}
		sort.Strings(declared)
		if len(declared) != len(want) {
			t.Errorf("%s declares %v; the ruling names %d agents", bundle, declared, len(want))
		}
	}
}
