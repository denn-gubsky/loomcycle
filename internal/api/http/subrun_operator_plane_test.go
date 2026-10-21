package http

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Operator authority belongs to the CALLER of an operator plane (the HTTP,
// gRPC and MCP substrate surfaces), never to a model-driven run that call
// happens to start. A TeamDef op=run is such a call: its walk spawns members
// from the plane's ctx, so whatever the plane stamped and the spawn boundary
// did not replace reached every member and every run under it.

// The child of a ctx carrying every operator-plane marker carries none of them.
// The parent is built by the real HTTP operator plane (open mode, so the
// unauthenticated-operator marker is on it too).
func TestPrepareSubRun_ChildCarriesNoOperatorPlaneAuthority(t *testing.T) {
	h := newReviewHarness(t)
	parent := tools.WithRunID(substrateAdminCtx(context.Background()), "r_walk")
	if !tools.IsSubstrateOperator(parent) || !tools.IsUnauthenticatedOperator(parent) ||
		!tools.OperatorTokenDefPolicy(parent).Admin || len(tools.ScheduleDefPolicy(parent).Scopes) == 0 {
		t.Fatal("fixture: the operator plane no longer stamps what this test strips; re-derive the fixture")
	}
	prep, err := h.srv.prepareSubRunValues(parent, "writer", nameFromAgentTool, "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer prep.cleanup()
	child := prep.LoopCtx

	if tools.IsSubstrateOperator(child) {
		t.Error("child is marked as the operator's own substrate call")
	}
	if tools.IsUnauthenticatedOperator(child) {
		t.Error("child is marked as the unauthenticated operator")
	}
	if p := tools.OperatorTokenDefPolicy(child); p.Admin {
		t.Error("child may mint operator tokens")
	}
	for name, scopes := range map[string][]string{
		"ScheduleDef":       tools.ScheduleDefPolicy(child).Scopes,
		"A2AServerCardDef":  tools.A2AServerCardDefPolicy(child).Scopes,
		"A2AAgentDef":       tools.A2AAgentDefPolicy(child).Scopes,
		"WebhookDef":        tools.WebhookDefPolicy(child).Scopes,
		"MemoryBackendDef":  tools.MemoryBackendDefPolicy(child).Scopes,
		"DocumentSourceDef": tools.DocumentSourceDefPolicy(child).Scopes,
	} {
		if len(scopes) != 0 {
			t.Errorf("child holds the operator plane's %s scopes %v; its own definition grants none", name, scopes)
		}
	}
}

// Every setter an operator plane calls is called again — or cleared — at the
// spawn boundary, so a grant added to an operator plane later cannot reach a
// run by being forgotten here. Read from the source rather than listed, so the
// test cannot drift from the planes it guards.
func TestPrepareSubRun_EveryOperatorPlaneSetterIsReplacedForTheChild(t *testing.T) {
	planes := map[string][]string{
		"substrate_admin.go":   {"substrateAdminCtx", "substrateAdminUserCtx"},
		"../mcp/context.go":    {"mcpPrincipalCtx", "operatorCtx", "grantOperatorPolicies"},
		"../grpc/substrate.go": {"substrateGRPCCtx", "substrateGRPCUserCtx"},
	}
	stamped := map[string]bool{}
	for file, fns := range planes {
		for _, fn := range fns {
			for name := range toolsSetterCalls(t, []string{file}, fn, true) {
				stamped[name] = true
			}
		}
	}
	if len(stamped) < 10 {
		t.Fatalf("found only %d operator-plane setters (%v); the scan is not reading the planes", len(stamped), stamped)
	}
	pkgFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var src []string
	for _, f := range pkgFiles {
		if !strings.HasSuffix(f, "_test.go") {
			src = append(src, f)
		}
	}
	replaced := toolsSetterCalls(t, src, "prepareSubRunValues", true)
	for name := range toolsSetterCalls(t, src, "stripOperatorPlane", false) {
		replaced[name] = true
	}
	var missing []string
	for name := range stamped {
		if !replaced[name] && !replaced["Without"+strings.TrimPrefix(name, "With")] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("an operator plane stamps %v, and the spawn boundary neither restamps nor clears it: "+
			"a run the plane starts hands it to every model-driven child", missing)
	}
}

// toolsSetterCalls returns the tools.With*/Without* functions fn calls, looking
// for fn in files. A required fn that is not found fails the test, so renaming
// a plane cannot make the guard pass vacuously; an optional one (the spawn
// boundary's helper) only adds names, so its absence can only fail more.
func toolsSetterCalls(t *testing.T, files []string, fn string, required bool) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != fn || fd.Body == nil {
				continue
			}
			out := map[string]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "tools" && strings.HasPrefix(sel.Sel.Name, "With") {
					out[sel.Sel.Name] = true
				}
				return true
			})
			return out
		}
	}
	if required {
		t.Fatalf("function %s not found in %v", fn, files)
	}
	return map[string]bool{}
}

// agentDefCallerProvider is a member that calls AgentDef twice — a plain
// create, then one claiming schedule_def_scopes — and records both answers.
type agentDefCallerProvider struct {
	mu      sync.Mutex
	answers []string
}

func (p *agentDefCallerProvider) ID() string                  { return "stub" }
func (p *agentDefCallerProvider) Probe(context.Context) error { return nil }
func (p *agentDefCallerProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *agentDefCallerProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *agentDefCallerProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	var results []string
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if c.Type == "tool_result" {
				results = append(results, c.Text)
			}
		}
	}
	usage := &providers.Usage{InputTokens: 1, OutputTokens: 1}
	call := func(id, input string) []providers.Event {
		return []providers.Event{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: "AgentDef", Input: json.RawMessage(input)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: usage},
		}
	}
	var evs []providers.Event
	switch len(results) {
	case 0:
		evs = call("ad1", `{"op":"create","name":"minted","overlay":{"system_prompt":"x"}}`)
	case 1:
		evs = call("ad2", `{"op":"create","name":"minted-sched","overlay":{"system_prompt":"x","schedule_def_scopes":["any"]}}`)
	default:
		p.mu.Lock()
		p.answers = append([]string(nil), results...)
		p.mu.Unlock()
		evs = []providers.Event{{Type: providers.EventText, Text: "done"}, {Type: providers.EventDone, StopReason: "end_turn", Usage: usage}}
	}
	ch := make(chan providers.Event, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (p *agentDefCallerProvider) wait(t *testing.T) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		got := p.answers
		p.mu.Unlock()
		if len(got) == 2 {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the member never finished its two AgentDef calls")
	return nil
}

const memberTeam = `{"entry":"a","states":[{"state":"a","handler":{"kind":"agent","agent":"member"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],"transitions":[{"from":"a","to":"done","on":"success"}]}`

// A tenant member's token starts a walk over POST /v1/_teamdef. The walk's
// member is an agent like any other: what it writes is not operator-authored,
// and it cannot grant a capability its own definition does not hold.
func TestTeamDefRun_HTTPWalkMemberIsNotTheOperator(t *testing.T) {
	h := newWalkHarness(t)
	prov := &agentDefCallerProvider{}
	h.srv.providers = &stubResolver{p: prov}
	cfg := h.srv.cfg()
	cfg.Agents["member"] = config.AgentDef{
		Model: "stub-model", SystemPrompt: "member",
		Tools: []string{"AgentDef"}, AgentDefScopes: []string{"any"},
	}
	h.srv.tools = append(h.srv.tools, &builtin.AgentDef{Cfg: cfg, Store: h.st})
	seedTenantTeam(t, h.st, "acme", "mteam", memberTeam)

	h.postTeamDef(alicePrincipal, `{"op":"run","name":"mteam","input":"go"}`)
	answers := prov.wait(t)

	row, err := h.st.AgentDefGetActive(context.Background(), "acme", "minted")
	if err != nil {
		t.Fatalf("the member's plain create did not land (%v); it answered %q", err, answers[0])
	}
	if row.OperatorAuthored {
		t.Error("a walk member's AgentDef is recorded as operator-authored")
	}
	if _, err := h.st.AgentDefGetActive(context.Background(), "acme", "minted-sched"); err == nil {
		t.Errorf("a walk member granted schedule_def_scopes [any] its own definition does not hold; it answered %q", answers[1])
	}
	if !strings.Contains(answers[1], "schedule_def_scopes") {
		t.Errorf("the refusal does not name the capability: %q", answers[1])
	}
}
