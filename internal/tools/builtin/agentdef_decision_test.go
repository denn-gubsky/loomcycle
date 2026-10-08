package builtin

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

// decisionAgentDefFixture is agentDefFixture on a server whose operator lists
// the decision models decide (default), decide-deep and nimble-lit.
func decisionAgentDefFixture(t *testing.T) (*AgentDef, context.Context, func()) {
	t.Helper()
	tool, ctx, cleanup := agentDefFixture(t)
	tool.Cfg.Decision = config.DecisionConfig{Default: "decide", Models: []string{"decide-deep", "nimble-lit"}, Provider: "ollama-local"}
	return tool, ctx, cleanup
}

// TestAgentDecision_OverlayRoundTripsCreateAndFork — authored over the AgentDef
// substrate, the block survives create → persist → lookup and fork → persist →
// lookup. A capability with no field in the persisted overlay comes back as its
// default on reload, which here would silently widen a narrowed agent to the
// operator's whole list. A fork that sets the block replaces it whole; one that
// does not keeps the parent's. It is content-identifying: a fork changing only
// the block mints a different content_sha256.
func TestAgentDecision_OverlayRoundTripsCreateAndFork(t *testing.T) {
	tool, ctx, cleanup := decisionAgentDefFixture(t)
	defer cleanup()
	reload := func() *config.AgentDecision {
		t.Helper()
		def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "router")
		if !ok {
			t.Fatal("the agent did not resolve after reload")
		}
		return def.Decision
	}

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"router","overlay":{`+
		`"system_prompt":"route","decision":{"default":"decide-deep","models":["decide-deep","nimble-lit"]}}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	createSHA, _ := decodeResult(t, res.Text)["content_sha256"].(string)
	want := &config.AgentDecision{Default: "decide-deep", Models: []string{"decide-deep", "nimble-lit"}}
	if got := reload(); !reflect.DeepEqual(got, want) {
		t.Fatalf("create → reload: decision = %+v, want %+v", got, want)
	}

	// A fork that does not mention the block keeps it.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"router","promote":true,"overlay":{"system_prompt":"route better"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if got := reload(); !reflect.DeepEqual(got, want) {
		t.Fatalf("fork → reload: decision = %+v, want the parent's %+v", got, want)
	}
	keptSHA, _ := decodeResult(t, res.Text)["content_sha256"].(string)

	// A fork that sets it replaces it whole: the parent's default does not survive
	// beside a list that no longer holds it.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"router","promote":true,"overlay":{"decision":{"models":["decide"]}}}`))
	if res.IsError {
		t.Fatalf("fork with a new block: %s", res.Text)
	}
	if got, want := reload(), (&config.AgentDecision{Models: []string{"decide"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("fork with a new block → reload: decision = %+v, want %+v", got, want)
	}
	forkSHA, _ := decodeResult(t, res.Text)["content_sha256"].(string)
	if createSHA == "" || keptSHA == "" || forkSHA == keptSHA {
		t.Errorf("a fork changing only decision kept content_sha256 %q — the block is not hashed", keptSHA)
	}
}

// TestAgentDecision_OverlayOutsideTheOperatorsListIsRefused — a definition
// authored at run time is held to the operator's list where it is authored, on
// create and on fork, as operator yaml is at load.
func TestAgentDecision_OverlayOutsideTheOperatorsListIsRefused(t *testing.T) {
	tool, ctx, cleanup := decisionAgentDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"router","overlay":{`+
		`"system_prompt":"route","decision":{"models":["decide","clef"]}}}`))
	if !res.IsError || !strings.Contains(res.Text, `decision.models: "clef" is not one of the decision models`) {
		t.Errorf("create outside the list must be refused, got: %s", res.Text)
	}
	if _, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "router"); ok {
		t.Error("a refused create still stored the agent")
	}

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"router","overlay":{"system_prompt":"route"}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"router","promote":true,"overlay":{"decision":{"default":"clef"}}}`))
	if !res.IsError || !strings.Contains(res.Text, `decision.default: "clef" is not one of the decision models`) {
		t.Errorf("fork outside the list must be refused, got: %s", res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"router","promote":true,"overlay":{"decision":{"default":"decide","models":["nimble-lit"]}}}`))
	if !res.IsError || !strings.Contains(res.Text, `decision.default: "decide" is not one of decision.models`) {
		t.Errorf("a default outside its own models must be refused, got: %s", res.Text)
	}

	// On a server that lists no decision models there is nothing to narrow.
	bare, bctx, bcleanup := agentDefFixture(t)
	defer bcleanup()
	res, _ = bare.Execute(bctx, json.RawMessage(`{"op":"create","name":"router","overlay":{"system_prompt":"route","decision":{"models":["decide"]}}}`))
	if !res.IsError || !strings.Contains(res.Text, "declares no decision models") {
		t.Errorf("a block on a server with no decision models must be refused, got: %s", res.Text)
	}
}

// TestAgentDecision_AbsentLeavesTheContentHashAlone — an agent that never
// mentions the block hashes exactly as it did before the block existed: an empty
// block in the overlay is the same content as no block.
func TestAgentDecision_AbsentLeavesTheContentHashAlone(t *testing.T) {
	sha := func(overlay string) string {
		t.Helper()
		tool, ctx, cleanup := decisionAgentDefFixture(t)
		defer cleanup()
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"router","overlay":`+overlay+`}`))
		if res.IsError {
			t.Fatalf("create: %s", res.Text)
		}
		s, _ := decodeResult(t, res.Text)["content_sha256"].(string)
		if s == "" {
			t.Fatal("create returned no content_sha256")
		}
		return s
	}
	absent := sha(`{"system_prompt":"route","tools":[]}`)
	if empty := sha(`{"system_prompt":"route","tools":[],"decision":{}}`); empty != absent {
		t.Errorf("an empty decision block hashed %s, an absent one %s", empty, absent)
	}
	if set := sha(`{"system_prompt":"route","tools":[],"decision":{"default":"decide"}}`); set == absent {
		t.Error("a set decision block did not change the content hash")
	}
}

// TestAgentDecision_RoundTripsTheAgentDefinition — declared in yaml, frontmatter
// or an AgentDef overlay, the block must survive every adapter its sibling does.
// Counted against memory_units, which every adapter carries.
func TestAgentDecision_RoundTripsTheAgentDefinition(t *testing.T) {
	for _, f := range []string{"../../agents/loader.go", "../../agents/sign.go", "../../lookup/agent.go", "agentdef.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		// Counted as CARRIES — a struct-literal field ("Name: …") or an
		// assignment ("x.Name = …") — not as mentions.
		carries := func(name string) int {
			n := 0
			for _, m := range regexp.MustCompile(`(?m)(^\s+`+name+`:\s|\.`+name+` = ).*$`).FindAllString(src, -1) {
				if !strings.HasSuffix(strings.TrimSpace(m), "= nil") { // normalising a field away is not a carry
					n++
				}
			}
			return n
		}
		sib, mine := carries("MemoryUnits"), carries("Decision")
		if sib == 0 {
			t.Fatalf("%s carries MemoryUnits nowhere — the guard is vacuous", f)
		}
		if mine < sib {
			t.Errorf("%s carries MemoryUnits %d times but Decision only %d", f, sib, mine)
		}
	}
}
