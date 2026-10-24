package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC EY: a run is narrowed by its CALLER's access-control rules at its start
// — tools withheld, volumes made read-only, keyspaces dropped, the set carried
// on the policy to its children. A resumed run has no caller on its ctx, and
// used to come back on its agent's full grants: every run paused for a
// snapshot, or in flight across a restart (the shutdown pause), lost its
// caller's rules. These tests start runs for real through POST /v1/runs under
// a member token bound to a restricting list, park them, resume them through
// ResumePausedRuns, and compare what the resumed run holds with what it held
// live.

// policyProbe records the run policy and the rollout mode each call sees.
type policyProbe struct {
	mu    sync.Mutex
	seen  []tools.RunPolicy
	modes []authz.Mode
}

func (p *policyProbe) Name() string                 { return "Probe" }
func (p *policyProbe) Description() string          { return "records the run policy" }
func (p *policyProbe) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *policyProbe) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	p.mu.Lock()
	p.seen = append(p.seen, tools.PolicyOf(ctx))
	p.modes = append(p.modes, authz.ModeFrom(ctx))
	p.mu.Unlock()
	return tools.Result{Text: "probed"}, nil
}

func (p *policyProbe) calls() []tools.RunPolicy {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.seen)
}

// toolsSeenProvider is roleProvider plus the tool names each role's calls
// offered the model: a withheld tool is absent from the schema, not merely
// refused on call.
type toolsSeenProvider struct {
	*roleProvider
	mu   sync.Mutex
	seen map[string][][]string
}

func (p *toolsSeenProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	role := ""
	for _, b := range req.System {
		for _, r := range p.roles {
			if strings.Contains(b.Text, "role:"+r+".") {
				role = r
			}
		}
	}
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		names = append(names, t.Name)
	}
	p.mu.Lock()
	p.seen[role] = append(p.seen[role], names)
	p.mu.Unlock()
	return p.roleProvider.Call(ctx, req)
}

func (p *toolsSeenProvider) callsOf(role string) int {
	p.roleProvider.mu.Lock()
	defer p.roleProvider.mu.Unlock()
	return p.roleProvider.calls[role]
}

func (p *toolsSeenProvider) toolsOf(role string) [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.seen[role])
}

type aclResumeEnv struct {
	srv   *Server
	st    store.Store
	ts    *httptest.Server
	probe *policyProbe
	prov  *toolsSeenProvider
	audit *recordingSink
}

// The list the member `dev` of tenant acme is bound to: no Bash, the scratch
// volume read-only, the tenant keyspace unreadable, and the `forbidden` agent
// not to be spawned.
const aclResumeLimits = `[
	{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]},
	{"effect":"deny","actions":["write"],"resources":["volume:scratch"]},
	{"effect":"deny","actions":["read"],"resources":["memory:tenant"]},
	{"effect":"deny","actions":["spawn"],"resources":["agent:forbidden"]}
]`

// aclResumeFixture serves `worker` and `child` (each with Bash, Probe, Agent
// and the read-write scratch volume) and `forbidden`, under the RFC EY
// resolver in compat mode, with one member token (workerBearer: acme/dev).
// bind binds aclResumeLimits to dev before any run starts.
func aclResumeFixture(t *testing.T, bind bool, scripts map[string][][]providers.Event) *aclResumeEnv {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Env.AuthToken = "legacy-admin-secret"
	cfg.Env.OperatorTokenPepper = testTokenPepper
	cfg.Volumes = map[string]config.Volume{"scratch": {Path: t.TempDir(), Mode: "rw"}}
	agentTools := []string{"Bash", "Probe", "Agent"}
	cfg.Agents = map[string]config.AgentDef{
		"worker":    {Model: "stub-model", Tools: agentTools, Volumes: []string{"scratch"}, SystemPrompt: "role:worker."},
		"child":     {Model: "stub-model", Tools: agentTools, Volumes: []string{"scratch"}, SystemPrompt: "role:child."},
		"forbidden": {Model: "stub-model", Tools: []string{}, SystemPrompt: "role:forbidden."},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "aclresume.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "def_worker", Name: "u-def_worker", TenantID: "acme", Subject: "dev",
		TokenHash: auth.HashToken(testTokenPepper, workerBearer), AllowedScopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "dev-limits", Rules: json.RawMessage(aclResumeLimits)}); err != nil {
		t.Fatal(err)
	}
	if bind {
		bindACL(t, st, "dev-limits")
	}
	prov := &toolsSeenProvider{
		roleProvider: &roleProvider{roles: []string{"worker", "child", "forbidden"}, scripts: scripts, calls: map[string]int{}},
		seen:         map[string][][]string{},
	}
	probe := &policyProbe{}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{namedStubTool{"Bash"}, probe}, concurrency.New(8, 8, 5*time.Second), st)
	srv.SetACLResolver(aclres.New(st, authz.ModeCompat, 0))
	sink := &recordingSink{}
	srv.SetTokenAudit(sink)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return &aclResumeEnv{srv: srv, st: st, ts: ts, probe: probe, prov: prov, audit: sink}
}

func bindACL(t *testing.T, st store.Store, list string) {
	t.Helper()
	if _, err := st.AclBindingPut(context.Background(), store.AclBindingRow{TenantID: "acme", ACLName: list, SubjectKind: "user", SubjectID: "dev"}); err != nil {
		t.Fatal(err)
	}
}

// startAs runs agent as dev to its end, live, and returns its row.
func (e *aclResumeEnv) startAs(t *testing.T, agent, agentID string) store.Run {
	t.Helper()
	body := `{"agent":"` + agent + `","agent_id":"` + agentID + `","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
	if code, out := bearerCall(t, e.ts, workerBearer, "POST", "/v1/runs", body); code != 200 {
		t.Fatalf("status %d: %s", code, out)
	}
	run, err := e.st.GetRunByAgentID(context.Background(), agentID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// waitProbes waits until the probe has recorded n calls.
func (e *aclResumeEnv) waitProbes(t *testing.T, n int) []tools.RunPolicy {
	t.Helper()
	waitFor(t, "the probe calls", func() bool { return len(e.probe.calls()) >= n })
	return e.probe.calls()
}

// aclNarrowed is what a run's policy holds because of its caller's rules.
type aclNarrowed struct {
	Tools        []string
	Removed      []string
	Volumes      []tools.VolumeBinding
	Memory       []string
	SqlMem       []string
	History      []string
	Skills       []string
	ACLSubject   string
	ACLGroupsSet bool
}

func aclNarrowingOf(p tools.RunPolicy) aclNarrowed {
	n := aclNarrowed{
		Tools: slices.Sorted(slices.Values(p.Tools)), Removed: p.ACLRemovedTools, Volumes: p.Volumes.Bindings,
		Memory: p.Memory.AllowedScopes, SqlMem: p.SqlMem.AllowedScopes, History: p.History.Scopes, Skills: p.Skill.Patterns,
	}
	if p.ACL != nil {
		n.ACLSubject = p.ACL.Subject.TenantID + "/" + p.ACL.Subject.ID
	}
	return n
}

// assertNarrowedLikeLive fails unless the resumed policy holds exactly what
// the caller's rules gave the live one, and the live one holds what the
// fixture's list says (so the comparison is never of two unnarrowed runs).
func assertNarrowedLikeLive(t *testing.T, live, resumed tools.RunPolicy) {
	t.Helper()
	ln := aclNarrowingOf(live)
	if live.ACL == nil || slices.Contains(ln.Tools, "Bash") || !slices.Equal(ln.Removed, []string{"Bash"}) ||
		len(ln.Volumes) != 1 || !ln.Volumes[0].ReadOnly || slices.Contains(ln.Memory, "tenant") {
		t.Fatalf("fixture: the live run is not narrowed by dev's list: %+v", ln)
	}
	if rn := aclNarrowingOf(resumed); !equalJSON(rn, ln) {
		t.Errorf("the resumed run is not narrowed as it was live:\n resumed %+v\n live    %+v", rn, ln)
	}
}

func equalJSON(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func probeTurn(id string) []providers.Event { return toolCallTurn(id, "Probe", `{}`) }

// A top-level run started by a principal whose list withholds Bash, makes the
// scratch volume read-only and drops the tenant keyspace is narrowed the same
// way after a resume: Bash is not in the schema the resumed turn offers the
// model, the volume is read-only, the keyspace gone, and the set is on the
// policy for its children.
func TestResume_TopLevelRunKeepsItsCallersACL(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{
		"worker": {probeTurn("tu_live"), endTurn(), probeTurn("tu_resumed"), endTurn()},
	})
	run := e.startAs(t, "worker", "a_top")
	parkForResume(t, e.srv, run.ID)
	resumeOne(t, e.srv)
	seen := e.waitProbes(t, 2)

	assertNarrowedLikeLive(t, seen[0], seen[1])
	offered := e.prov.toolsOf("worker")
	if len(offered) < 3 || slices.Contains(offered[2], "Bash") || !slices.Contains(offered[2], "Probe") {
		t.Errorf("the resumed turn offered the model %v; Bash must stay withheld", offered)
	}
	e.probe.mu.Lock()
	mode := e.probe.modes[1]
	e.probe.mu.Unlock()
	if mode != authz.ModeCompat {
		t.Errorf("the resumed run evaluates under mode %q, want the operator's (compat)", mode)
	}
}

// A sub-run is bound by the ORIGINATING caller's set, inherited from its
// parent. Resumed on its own — no parent ctx to inherit from — it is narrowed
// the same, and a spawn the caller's list denies is still refused.
func TestResume_SubRunKeepsTheOriginatingCallersACL(t *testing.T) {
	resumedTurn := []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_probe", Name: "Probe", Input: json.RawMessage(`{}`)}},
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_spawn", Name: "Agent", Input: json.RawMessage(`{"name":"forbidden","prompt":"x"}`)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	e := aclResumeFixture(t, true, map[string][][]providers.Event{
		"worker": {toolCallTurn("tu_child", "Agent", `{"name":"child","prompt":"work"}`), endTurn()},
		"child":  {probeTurn("tu_live"), endTurn(), resumedTurn, endTurn()},
	})
	e.startAs(t, "worker", "a_parent")
	kids, err := e.st.ListRunsByParentAgentID(context.Background(), "a_parent")
	if err != nil || len(kids) != 1 {
		t.Fatalf("children of a_parent: %d, %v", len(kids), err)
	}
	child := kids[0]
	transcript := resumeAndFinish(t, e.srv, child)
	seen := e.waitProbes(t, 2)

	assertNarrowedLikeLive(t, seen[0], seen[1])
	if res := resumedToolResult(t, e.srv, child, "tu_spawn"); !strings.Contains(res, "acl_denied") {
		t.Errorf("the resumed child's spawn of a denied agent was not refused: %s\ntranscript: %s", res, transcript)
	}
	if n := e.prov.callsOf("forbidden"); n != 0 {
		t.Errorf("the denied agent ran %d model call(s) after the resume", n)
	}
}

// The rules are read again on resume, not restored: a deny bound while the
// run was paused holds once it resumes, though the run started without it.
func TestResume_ADenyBoundDuringThePauseHoldsAfterIt(t *testing.T) {
	e := aclResumeFixture(t, false, map[string][][]providers.Event{
		"worker": {probeTurn("tu_live"), endTurn(), probeTurn("tu_resumed"), endTurn()},
	})
	run := e.startAs(t, "worker", "a_revoked")
	parkForResume(t, e.srv, run.ID)
	if seen := e.probe.calls(); len(seen) != 1 || !slices.Contains(seen[0].Tools, "Bash") {
		t.Fatalf("fixture: the live run should hold Bash before the deny is bound: %+v", seen)
	}
	bindACL(t, e.st, "dev-limits")
	resumeOne(t, e.srv)
	seen := e.waitProbes(t, 2)
	if got := aclNarrowingOf(seen[1]); slices.Contains(got.Tools, "Bash") || !slices.Equal(got.Removed, []string{"Bash"}) || !got.Volumes[0].ReadOnly {
		t.Errorf("a deny bound during the pause does not hold after the resume: %+v", got)
	}
}

// failingACLStore is an ACL store that cannot be read.
type failingACLStore struct{ aclres.Store }

func (failingACLStore) AclVersion(context.Context, string) (int64, error) {
	return 0, errors.New("acl store unreachable")
}

// A run whose caller's set cannot be read on resume does not resume on its
// agent's full grants: it stays paused, the refusal is audited as
// acl_unavailable, and a later pass resumes it narrowed once the set can be
// read.
func TestResume_UnreadableCallerACLLeavesTheRunPaused(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{
		"worker": {probeTurn("tu_live"), endTurn(), probeTurn("tu_resumed"), endTurn()},
	})
	run := e.startAs(t, "worker", "a_paused")
	parkForResume(t, e.srv, run.ID)
	working := e.srv.ACLResolver()
	e.srv.SetACLResolver(aclres.New(failingACLStore{e.st}, authz.ModeCompat, 0))

	n, warnings := e.srv.ResumePausedRuns(context.Background())
	if n != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "acl_unavailable") {
		t.Fatalf("resumed %d (warnings %v); want none resumed and an acl_unavailable warning", n, warnings)
	}
	got, err := e.st.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.RunRunning || got.PauseState != store.PauseStatePaused {
		t.Errorf("run is %s/%s; want it still paused", got.Status, got.PauseState)
	}
	if calls := e.prov.callsOf("worker"); calls != 2 {
		t.Errorf("the worker made %d model calls; the refused resume must make none past the live 2", calls)
	}
	if !slices.ContainsFunc(e.audit.events, func(ev audit.Event) bool {
		return ev.Action == "acl_unavailable" && ev.ActorSubject == "dev" && ev.ActorTenant == "acme"
	}) {
		t.Errorf("no acl_unavailable audit event for dev; events: %+v", e.audit.events)
	}

	e.srv.SetACLResolver(working)
	resumeOne(t, e.srv)
	seen := e.waitProbes(t, 2)
	assertNarrowedLikeLive(t, seen[0], seen[1])
}

// With no resolver wired, access control is not configured: a live request
// carries no set (enrichPrincipal) and neither does a trigger's run
// (triggerOwnerCtx). A resumed run is treated the same — it resumes, as a
// fresh request would start, with no set — rather than staying paused for
// ever on an error no retry can clear.
func TestResume_NoACLResolverWiredResumesAsALiveRequestWould(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{
		"worker": {probeTurn("tu_live"), endTurn(), probeTurn("tu_resumed"), endTurn()},
	})
	run := e.startAs(t, "worker", "a_unwired")
	parkForResume(t, e.srv, run.ID)
	e.srv.SetACLResolver(nil)

	n, warnings := e.srv.ResumePausedRuns(context.Background())
	if n != 1 || len(warnings) != 0 {
		t.Fatalf("resumed %d (warnings %v); want the run resumed with no warning", n, warnings)
	}
	seen := e.waitProbes(t, 2)
	if got := seen[1]; got.ACL != nil || !slices.Contains(got.Tools, "Bash") {
		t.Errorf("with no resolver wired the resumed run is narrowed: %+v", aclNarrowingOf(got))
	}
	if slices.ContainsFunc(e.audit.events, func(ev audit.Event) bool { return ev.Action == "acl_unavailable" }) {
		t.Errorf("an unconfigured resolver was audited as acl_unavailable: %+v", e.audit.events)
	}
}

// A run with no record of its caller's rules — one started before they were
// recorded — resumes as every resumed run did before: unnarrowed. Pinned so
// the pre-fix rows' behaviour is a decision, not an accident.
func TestResume_RunWithNoACLRecordResumesAsBefore(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{
		"worker": {probeTurn("tu_live"), endTurn(), probeTurn("tu_resumed"), endTurn()},
	})
	run := e.startAs(t, "worker", "a_legacy")
	parkForResume(t, e.srv, run.ID)
	if _, err := e.srv.updateRunConfig(context.Background(), run.ID, func(rec *runConfigRecord, _ bool) error {
		if rec.ACLOrigin == nil {
			t.Fatal("fixture: the run recorded no acl_origin to strip")
		}
		rec.ACLOrigin = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	resumeOne(t, e.srv)
	seen := e.waitProbes(t, 2)
	if got := seen[1]; got.ACL != nil || !slices.Contains(got.Tools, "Bash") {
		t.Errorf("a run with no record resumed narrowed: %+v", aclNarrowingOf(got))
	}
}

// The record names the caller and its token, and never a credential.
func TestRunConfig_ACLOriginNamesTheCallerAndNoSecret(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {endTurn()}})
	run := e.startAs(t, "worker", "a_rec")
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok || rec.ACLOrigin == nil {
		t.Fatalf("no acl_origin recorded: %s", run.RunConfig)
	}
	want := aclOriginRecord{TenantID: "acme", Subject: "dev", TokenDefID: "def_worker", Class: "member", Transport: "http"}
	if *rec.ACLOrigin != want {
		t.Errorf("acl_origin = %+v, want %+v", *rec.ACLOrigin, want)
	}
	if strings.Contains(string(run.RunConfig), workerBearer) {
		t.Error("the run's record holds the caller's bearer")
	}
}
