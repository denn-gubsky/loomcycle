package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A minimal valid team graph: entry state runs an agent, then a success edge
// to a terminal state. Validates cleanly (entry resolves, unique states, valid
// handlers, well-formed transition, terminal has no outbound, all reachable).
const validTeamGraph = `{
  "entry": "review",
  "states": [
    {"state": "review", "handler": {"kind": "agent", "agent": "reviewer"}},
    {"state": "done", "handler": {"kind": "terminal"}}
  ],
  "transitions": [
    {"from": "review", "to": "done", "on": "success"}
  ]
}`

// teamDefFixture builds a TeamDef tool over in-memory SQLite. The ctx carries a
// RunIdentity (shared "" tenant) so create/get/list/promote/retire resolve
// against the same tenant.
func teamDefFixture(t *testing.T) (*TeamDef, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	tool := &TeamDef{
		Store:               s,
		MaxDefinitionBytes:  131072,
		MaxDescriptionBytes: 8192,
	}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_test"})
	return tool, ctx, func() { _ = s.Close() }
}

func createTeam(t *testing.T, tool *TeamDef, ctx context.Context, name, overlay string) map[string]any {
	t.Helper()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"`+name+`","overlay":`+overlay+`}`))
	if res.IsError {
		t.Fatalf("create %q: %s", name, res.Text)
	}
	return decodeResult(t, res.Text)
}

func TestTeamDefTool_CreateValidGraphPersists(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()

	out := createTeam(t, tool, ctx, "review-team", validTeamGraph)
	if out["name"] != "review-team" {
		t.Errorf("name = %v, want review-team", out["name"])
	}
	if out["version"].(float64) != 1 {
		t.Errorf("version = %v, want 1", out["version"])
	}
	if out["promoted"].(bool) != true {
		t.Errorf("create default promote = false; want true")
	}
	sha, _ := out["content_sha256"].(string)
	if !strings.HasPrefix(sha, "sha256:") {
		t.Errorf("content_sha256 = %q, want sha256:-prefixed", sha)
	}
}

// TestTeamDefTool_CreateInvalidGraphRefusedPersistsNothing pins the RFC AP
// validate-before-write invariant: an invalid graph is refused AND nothing is
// stored (fail-before: drop the teamgraph.Validate call in execCreate and the
// dangling-transition graph persists, so list would return 1 version).
func TestTeamDefTool_CreateInvalidGraphRefusedPersistsNothing(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()

	// A dangling transition (to a non-existent state).
	dangling := `{
      "entry": "review",
      "states": [{"state": "review", "handler": {"kind": "agent", "agent": "reviewer"}}],
      "transitions": [{"from": "review", "to": "ghost", "on": "success"}]
    }`
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"bad-team","overlay":`+dangling+`}`))
	if !res.IsError {
		t.Fatalf("dangling-transition graph should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "does not resolve to a state") {
		t.Errorf("refusal should name the validation failure; got %s", res.Text)
	}

	// A parallel handler missing its consolidator.
	noConsolidator := `{
      "entry": "fan",
      "states": [
        {"state": "fan", "handler": {"kind": "parallel", "agents": ["a","b"]}},
        {"state": "done", "handler": {"kind": "terminal"}}
      ],
      "transitions": [{"from": "fan", "to": "done", "on": "success"}]
    }`
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"bad-team","overlay":`+noConsolidator+`}`))
	if !res.IsError {
		t.Fatalf("parallel-without-consolidator should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "consolidator") {
		t.Errorf("refusal should mention the missing consolidator; got %s", res.Text)
	}

	// Nothing persisted for the refused name.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"bad-team"}`))
	if res.IsError {
		t.Fatalf("list: %s", res.Text)
	}
	versions := decodeResult(t, res.Text)["versions"].([]any)
	if len(versions) != 0 {
		t.Errorf("refused creates persisted %d versions; want 0", len(versions))
	}
}

// TestTeamDefTool_ForkColoursOnlyKeepsHash: a fork applying only a `colors`
// overlay produces a new version whose content_sha256 EQUALS the parent's,
// because colours are excluded from the content hash (teamgraph.Sign).
func TestTeamDefTool_ForkColoursOnlyKeepsHash(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()

	parent := createTeam(t, tool, ctx, "colour-team", validTeamGraph)
	parentSHA := parent["content_sha256"].(string)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"colour-team","overlay":{"colors":{"states":{"review":"#eeeeee"}}}}`))
	if res.IsError {
		t.Fatalf("colours-only fork: %s", res.Text)
	}
	fork := decodeResult(t, res.Text)
	if fork["version"].(float64) != 2 {
		t.Errorf("fork version = %v, want 2", fork["version"])
	}
	if fork["content_sha256"].(string) != parentSHA {
		t.Errorf("colours-only fork changed the hash: got %s, want %s (parent)", fork["content_sha256"], parentSHA)
	}
	// The colours DID land in the persisted definition.
	defBytes, err := json.Marshal(fork["definition"])
	if err != nil {
		t.Fatalf("marshal fork definition: %v", err)
	}
	if !strings.Contains(string(defBytes), "#eeeeee") {
		t.Errorf("fork definition missing the overlay colours: %v", fork["definition"])
	}
}

// TestTeamDefTool_ForkGraphReplacingChangesHash: a fork whose overlay replaces
// the graph (new entry + states + transitions) changes the content_sha256.
func TestTeamDefTool_ForkGraphReplacingChangesHash(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()

	parent := createTeam(t, tool, ctx, "graph-team", validTeamGraph)
	parentSHA := parent["content_sha256"].(string)

	replacement := `{
      "entry": "plan",
      "states": [
        {"state": "plan", "handler": {"kind": "agent", "agent": "planner"}},
        {"state": "end", "handler": {"kind": "terminal"}}
      ],
      "transitions": [{"from": "plan", "to": "end", "on": "success"}]
    }`
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"graph-team","overlay":`+replacement+`}`))
	if res.IsError {
		t.Fatalf("graph-replacing fork: %s", res.Text)
	}
	fork := decodeResult(t, res.Text)
	if fork["content_sha256"].(string) == parentSHA {
		t.Errorf("graph-replacing fork should change the hash; both = %s", parentSHA)
	}
}

// TestTeamDefTool_RoundTrip exercises get / list / retire / promote / verify.
func TestTeamDefTool_RoundTrip(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()

	created := createTeam(t, tool, ctx, "rt-team", validTeamGraph)
	defID := created["def_id"].(string)
	sha := created["content_sha256"].(string)

	// get by def_id.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if res.IsError {
		t.Fatalf("get: %s", res.Text)
	}
	if decodeResult(t, res.Text)["def_id"].(string) != defID {
		t.Errorf("get returned wrong def_id")
	}

	// list has exactly the one version.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"rt-team"}`))
	if res.IsError {
		t.Fatalf("list: %s", res.Text)
	}
	if n := len(decodeResult(t, res.Text)["versions"].([]any)); n != 1 {
		t.Errorf("list returned %d versions, want 1", n)
	}

	// verify: correct hash matches; a bogus one does not (deployed stays true).
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"rt-team","content_sha256":"`+sha+`"}`))
	if res.IsError {
		t.Fatalf("verify(match): %s", res.Text)
	}
	v := decodeResult(t, res.Text)
	if v["matches"].(bool) != true || v["deployed"].(bool) != true {
		t.Errorf("verify(match) = %v, want matches+deployed true", v)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"rt-team","content_sha256":"sha256:deadbeef"}`))
	if decodeResult(t, res.Text)["matches"].(bool) != false {
		t.Error("verify(mismatch) should report matches=false")
	}

	// retire true → false round-trip.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":true}`))
	if res.IsError || decodeResult(t, res.Text)["retired"].(bool) != true {
		t.Fatalf("retire(true): %s", res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":false}`))
	if res.IsError || decodeResult(t, res.Text)["retired"].(bool) != false {
		t.Fatalf("retire(false): %s", res.Text)
	}

	// A fork (not auto-promoted) then explicit promote flips the active pointer.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"rt-team","overlay":{"colors":{"states":{"review":"#abcabc"}}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	forkID := decodeResult(t, res.Text)["def_id"].(string)
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"promote","def_id":"`+forkID+`"}`))
	if res.IsError || decodeResult(t, res.Text)["promoted"].(bool) != true {
		t.Fatalf("promote: %s", res.Text)
	}
}
