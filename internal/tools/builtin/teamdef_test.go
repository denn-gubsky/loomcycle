package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

type admitMarkerKey struct{}

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

func TestTeamDefTool_Run_LinearTeam(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()

	var spawned []string
	tool.Spawn = func(_ context.Context, agent, input, defID string) (string, error) {
		spawned = append(spawned, agent)
		return agent + "(" + input + ")", nil
	}

	createTeam(t, tool, ctx, "run-linear", `{
	  "entry":"a",
	  "states":[
	    {"state":"a","handler":{"kind":"agent","agent":"agent-a"}},
	    {"state":"b","handler":{"kind":"agent","agent":"agent-b"}},
	    {"state":"done","handler":{"kind":"terminal"}}
	  ],
	  "transitions":[
	    {"from":"a","to":"b","on":"success"},
	    {"from":"b","to":"done","on":"success"}
	  ]}`)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"run-linear","input":"seed"}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["status"] != "completed" {
		t.Errorf("status = %v, want completed", out["status"])
	}
	if out["final_state"] != "done" {
		t.Errorf("final_state = %v, want done", out["final_state"])
	}
	if out["final_output"] != "agent-b(agent-a(seed))" {
		t.Errorf("final_output = %v, want agent-b(agent-a(seed)) (threaded)", out["final_output"])
	}
	if len(spawned) != 2 || spawned[0] != "agent-a" || spawned[1] != "agent-b" {
		t.Errorf("spawned %v, want [agent-a agent-b]", spawned)
	}
	if steps, _ := out["steps"].([]any); len(steps) != 2 {
		t.Errorf("steps len = %d, want 2", len(steps))
	}
}

func TestTeamDefTool_Run_AdmitRefusalAbortsBeforeSpawn(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()

	spawnCalled := false
	tool.Spawn = func(_ context.Context, agent, input, defID string) (string, error) {
		spawnCalled = true
		return "ok", nil
	}
	// Admit refuses (e.g. token budget exceeded / too deep) → the walk must not
	// start and no agent may be spawned.
	tool.Admit = func(c context.Context) (context.Context, error) {
		return nil, errors.New("token budget exceeded: tenant acme over hard ceiling")
	}
	createTeam(t, tool, ctx, "admit-refuse", `{
	  "entry":"a",
	  "states":[
	    {"state":"a","handler":{"kind":"agent","agent":"agent-a"}},
	    {"state":"done","handler":{"kind":"terminal"}}
	  ],
	  "transitions":[{"from":"a","to":"done","on":"success"}]}`)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"admit-refuse","input":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "token budget exceeded") {
		t.Fatalf("admission refusal should abort the run; got %q (isErr=%v)", res.Text, res.IsError)
	}
	if spawnCalled {
		t.Errorf("no agent may be spawned when admission refuses")
	}
}

func TestTeamDefTool_Run_AdmittedCtxFlowsToSpawn(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()

	// Admit enriches the ctx; the Spawn closure must run under THAT ctx (proving
	// the operator-key restriction / depth increment the real Admit stamps reach
	// every spawned agent).
	tool.Admit = func(c context.Context) (context.Context, error) {
		return context.WithValue(c, admitMarkerKey{}, "admitted"), nil
	}
	sawMarker := false
	tool.Spawn = func(c context.Context, agent, input, defID string) (string, error) {
		if c.Value(admitMarkerKey{}) == "admitted" {
			sawMarker = true
		}
		return "ok", nil
	}
	createTeam(t, tool, ctx, "admit-ctx", `{
	  "entry":"a",
	  "states":[
	    {"state":"a","handler":{"kind":"agent","agent":"agent-a"}},
	    {"state":"done","handler":{"kind":"terminal"}}
	  ],
	  "transitions":[{"from":"a","to":"done","on":"success"}]}`)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"admit-ctx","input":"x"}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	if !sawMarker {
		t.Errorf("spawn did not run under the admitted ctx")
	}
}

func TestTeamDefTool_Run_NotConfigured(t *testing.T) {
	tool, ctx, done := teamDefFixture(t) // fixture leaves Spawn nil
	defer done()
	createTeam(t, tool, ctx, "run-nocfg", `{
	  "entry":"a",
	  "states":[{"state":"a","handler":{"kind":"terminal"}}]}`)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"run-nocfg","input":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "not configured for execution") {
		t.Fatalf("run without a wired runner should error; got %q (isErr=%v)", res.Text, res.IsError)
	}
}

func TestTeamDefTool_Run_IterationCap(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	tool.Spawn = func(_ context.Context, agent, input, defID string) (string, error) { return "ok", nil }

	// a ⇄ b ping-pong on success (no terminal reachable) → the walk never
	// converges and the per-state cap must fire.
	createTeam(t, tool, ctx, "run-loop", `{
	  "entry":"a",
	  "max_iterations":2,
	  "states":[
	    {"state":"a","handler":{"kind":"agent","agent":"a"}},
	    {"state":"b","handler":{"kind":"agent","agent":"b"}}
	  ],
	  "transitions":[
	    {"from":"a","to":"b","on":"success"},
	    {"from":"b","to":"a","on":"success"}
	  ]}`)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"run-loop","input":"x"}`))
	if res.IsError {
		t.Fatalf("iteration cap should be a reported outcome, not a tool error: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["status"] != "iteration_cap" {
		t.Errorf("status = %v, want iteration_cap", out["status"])
	}
	if out["capped_state"] != "a" {
		t.Errorf("capped_state = %v, want a (entry entered first each cycle)", out["capped_state"])
	}
}

func TestTeamDefTool_Run_UnknownTeam(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	tool.Spawn = func(_ context.Context, agent, input, defID string) (string, error) { return "", nil }
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"ghost","input":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "not found") {
		t.Fatalf("unknown team should be not-found; got %q (isErr=%v)", res.Text, res.IsError)
	}
}

func TestTeamDefTool_Run_ParallelNotYetSupported(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	tool.Spawn = func(_ context.Context, agent, input, defID string) (string, error) { return "", nil }
	createTeam(t, tool, ctx, "run-parallel", `{
	  "entry":"fan",
	  "states":[
	    {"state":"fan","handler":{"kind":"parallel","agents":["x","y"],"consolidator":"c"}},
	    {"state":"end","handler":{"kind":"terminal"}}
	  ],
	  "transitions":[{"from":"fan","to":"end","on":"success"}]}`)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"run-parallel","input":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "Phase 3") {
		t.Fatalf("parallel handler should error as not-yet-supported; got %q (isErr=%v)", res.Text, res.IsError)
	}
}

func TestTeamDefTool_RenderDiagram(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()

	createTeam(t, tool, ctx, "rd-team", `{
	  "entry":"implementation",
	  "states":[
	    {"state":"implementation","handler":{"kind":"agent","agent":"code-guru"}},
	    {"state":"review","handler":{"kind":"parallel","agents":["sec-rev","code-rev"],"wait":"all","consolidator":"rev-consol"}},
	    {"state":"pr","handler":{"kind":"terminal"}}
	  ],
	  "transitions":[
	    {"from":"implementation","to":"review","on":"success"},
	    {"from":"review","to":"pr","on":"success"},
	    {"from":"review","to":"implementation","on":"pushback:code-fix"}
	  ]}`)

	// Render the active version by name, highlighting the current state.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"render_diagram","name":"rd-team","highlight_state":"review"}`))
	if res.IsError {
		t.Fatalf("render_diagram: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["format"] != "mermaid" {
		t.Errorf("format = %v, want mermaid", out["format"])
	}
	diagram, _ := out["diagram"].(string)
	for _, want := range []string{"stateDiagram-v2", "[*] --> implementation", "note right of review", "classDef c", "class review c", "_hl"} {
		if !strings.Contains(diagram, want) {
			t.Errorf("diagram missing %q\n---\n%s", want, diagram)
		}
	}

	// d2 is deferred → clear error.
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"render_diagram","name":"rd-team","format":"d2"}`)); !res.IsError || !strings.Contains(res.Text, "d2") {
		t.Errorf("format=d2 should error as deferred; got %q (isErr=%v)", res.Text, res.IsError)
	}

	// Unknown team → not found (no leak).
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"render_diagram","name":"ghost"}`)); !res.IsError || !strings.Contains(res.Text, "not found") {
		t.Errorf("unknown team should be not-found; got %q (isErr=%v)", res.Text, res.IsError)
	}
}

// op=delete hard-removes a whole team by name (all versions + active pointer),
// scoped to the caller's tenant; a missing team is a not-found error.
func TestTeamDefTool_Delete(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()

	createTeam(t, tool, ctx, "del-team", `{
	  "entry":"a",
	  "states":[{"state":"a","handler":{"kind":"agent","agent":"x"}},{"state":"b","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"a","to":"b","on":"success"}]}`)

	// It renders (active) before the delete.
	if r, _ := tool.Execute(ctx, json.RawMessage(`{"op":"render_diagram","name":"del-team"}`)); r.IsError {
		t.Fatalf("pre-delete render: %s", r.Text)
	}

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"delete","name":"del-team"}`))
	if res.IsError {
		t.Fatalf("delete: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["deleted"] != true {
		t.Errorf("deleted = %v, want true", out["deleted"])
	}

	// Gone: rendering the active version is now not-found.
	if r, _ := tool.Execute(ctx, json.RawMessage(`{"op":"render_diagram","name":"del-team"}`)); !r.IsError || !strings.Contains(r.Text, "not found") {
		t.Errorf("after delete, render should be not-found; got %q (isErr=%v)", r.Text, r.IsError)
	}

	// Deleting a missing team → not-found error (no leak, no panic).
	if r, _ := tool.Execute(ctx, json.RawMessage(`{"op":"delete","name":"ghost"}`)); !r.IsError || !strings.Contains(r.Text, "not found") {
		t.Errorf("delete missing should be not-found; got %q (isErr=%v)", r.Text, r.IsError)
	}
}

// Dry-run preview: render_diagram with an inline overlay renders (and
// syntax-checks) an UNSAVED graph without persisting anything — this backs the
// Web UI editor's "refresh diagram" preview.
func TestTeamDefTool_RenderDiagram_InlineOverlayDryRun(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()

	overlay := `{
	  "entry":"draft",
	  "states":[
	    {"state":"draft","handler":{"kind":"agent","agent":"writer"}},
	    {"state":"done","handler":{"kind":"terminal"}}
	  ],
	  "transitions":[{"from":"draft","to":"done","on":"success"}]}`

	// Render the unsaved overlay directly — no create first.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"render_diagram","name":"preview-team","overlay":`+overlay+`}`))
	if res.IsError {
		t.Fatalf("dry-run render_diagram: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["preview"] != true {
		t.Errorf("preview flag = %v, want true", out["preview"])
	}
	if out["def_id"] != "" {
		t.Errorf("def_id = %q, want empty (dry-run persists nothing)", out["def_id"])
	}
	if d, _ := out["diagram"].(string); !strings.Contains(d, "[*] --> draft") {
		t.Errorf("dry-run diagram missing entry edge:\n%s", d)
	}

	// It persisted NOTHING — rendering the same name (no overlay) is still not-found.
	if r, _ := tool.Execute(ctx, json.RawMessage(`{"op":"render_diagram","name":"preview-team"}`)); !r.IsError || !strings.Contains(r.Text, "not found") {
		t.Errorf("dry-run must not persist; got %q (isErr=%v)", r.Text, r.IsError)
	}

	// The syntax check runs exactly like create: an invalid graph (dangling
	// transition to a non-existent state) is refused, no diagram returned.
	bad := `{"op":"render_diagram","name":"bad","overlay":{"entry":"a","states":[{"state":"a","handler":{"kind":"agent","agent":"x"}},{"state":"b","handler":{"kind":"terminal"}}],"transitions":[{"from":"a","to":"ghost","on":"success"}]}}`
	if r, _ := tool.Execute(ctx, json.RawMessage(bad)); !r.IsError {
		t.Errorf("invalid overlay should fail the syntax check; got %q", r.Text)
	}
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
