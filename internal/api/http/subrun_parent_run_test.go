package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A sub-run's row names the run that spawned it. Before, no live path wrote
// runs.parent_run_id, so the run tree could only be rebuilt through
// parent_agent_id — ambiguous, since every run of an agent reuses its id.

// spawnParentChild posts one run of "parent" whose first turn calls the Agent
// tool with agentInput, and returns the parent's row and its children's.
func spawnParentChild(t *testing.T, parentAgentID, agentInput string, childTurns int) (store.Run, []store.Run) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "parent", MaxConcurrentChildren: 4},
		"child":  {Model: "stub-model", Tools: []string{}, SystemPrompt: "child"},
	}
	done := func(text string) []providers.Event {
		return []providers.Event{
			{Type: providers.EventText, Text: text},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		}
	}
	scripts := [][]providers.Event{{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_spawn", Name: "Agent", Input: json.RawMessage(agentInput)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	for i := 0; i < childTurns; i++ {
		scripts = append(scripts, done("child answer"))
	}
	prov := &scriptedProvider{scripts: scripts, defaultS: done("parent done")}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "parent_run.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(8, 8, 5*time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","agent_id":"`+parentAgentID+`","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "parent done") {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}

	ctx := context.Background()
	parent, err := st.GetRunByAgentID(ctx, parentAgentID)
	if err != nil {
		t.Fatalf("parent row: %v", err)
	}
	children, err := st.ListRunsByParentAgentID(ctx, parentAgentID)
	if err != nil {
		t.Fatalf("children: %v", err)
	}
	if len(children) != childTurns {
		t.Fatalf("got %d child runs, want %d", len(children), childTurns)
	}
	return parent, children
}

// A one-shot Agent spawn records the parent's run id; the top-level run
// (POST /v1/runs) records none.
func TestSubAgent_OneShotChildRecordsTheParentRunID(t *testing.T) {
	parent, children := spawnParentChild(t, "a_parent_one", `{"name":"child","prompt":"hi"}`, 1)
	if parent.ParentRunID != "" {
		t.Errorf("top-level run's parent run = %q, want none", parent.ParentRunID)
	}
	if got := children[0].ParentRunID; got != parent.ID {
		t.Errorf("child's parent run = %q, want the parent's run %q", got, parent.ID)
	}
}

// Every parallel_spawn child records the same parent run.
func TestSubAgent_ParallelSpawnChildrenRecordTheParentRunID(t *testing.T) {
	parent, children := spawnParentChild(t, "a_parent_fan",
		`{"op":"parallel_spawn","spawns":[{"name":"child","prompt":"one"},{"name":"child","prompt":"two"}]}`, 2)
	for _, c := range children {
		if c.ParentRunID != parent.ID {
			t.Errorf("child %s's parent run = %q, want the parent's run %q", c.ID, c.ParentRunID, parent.ID)
		}
	}
}

// The row carries the parent's id while the child's own ctx carries the
// child's — stamping the parent must not leak it into tools.RunID.
func TestPrepareSubRun_ChildRowRecordsTheParentRunID(t *testing.T) {
	h := newReviewHarness(t)
	parent := tools.WithRunID(context.Background(), "r_parent")
	prep, err := h.srv.prepareSubRunValues(parent, "writer", "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer prep.Slot.releaseCurrent()
	defer prep.cleanup()
	row, err := h.st.GetRun(context.Background(), prep.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if row.ParentRunID != "r_parent" {
		t.Errorf("child row's parent run = %q, want r_parent", row.ParentRunID)
	}
	if got := tools.RunID(prep.LoopCtx); got != prep.RunID {
		t.Errorf("child ctx run id = %q, want its own %q", got, prep.RunID)
	}
}

// delegatorTeam runs one member that spawns a sub-agent of its own.
const delegatorTeam = `{"entry":"d","states":[` +
	`{"state":"d","handler":{"kind":"agent","agent":"delegator"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"d","to":"done","on":"success"}]}`

// A walk member's parent run is the walk: the walk run is what spawned and
// cancels it. The member's own sub-agent's parent is the member, not the walk.
// A walk started on the substrate plane has no calling run, so no parent.
func TestTeamDefRun_MembersRecordTheWalkRunAsParent(t *testing.T) {
	h := newStampHarness(t)
	seedTenantTeam(t, h.st, "acme", "deleg", delegatorTeam)
	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"deleg","input":"go"}`)

	runs, err := h.st.ListActiveRunsByUser(context.Background(), "", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	var walk, member, helper *store.Run
	for i := range runs {
		switch r := &runs[i]; {
		case r.ID == walkID:
			walk = r
		case r.ParentContext != nil && r.ParentContext.WalkID == walkID:
			member = r
		}
	}
	if walk == nil || member == nil {
		t.Fatalf("walk %v / member %v not found among %d runs", walk, member, len(runs))
	}
	for i := range runs {
		if runs[i].ParentAgentID == member.AgentID {
			helper = &runs[i]
		}
	}
	if helper == nil {
		t.Fatal("the member's own sub-agent never ran")
	}
	if walk.ParentRunID != "" {
		t.Errorf("walk's parent run = %q, want none (the substrate plane has no run)", walk.ParentRunID)
	}
	if member.ParentRunID != walkID {
		t.Errorf("member's parent run = %q, want the walk %q", member.ParentRunID, walkID)
	}
	if helper.ParentRunID != member.ID {
		t.Errorf("member's sub-agent's parent run = %q, want the member %q", helper.ParentRunID, member.ID)
	}
}

// A walk started by an agent's TeamDef call records that agent's run.
func TestOpenTeamWalkRun_RecordsTheCallingRunAsParent(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	caller := tools.WithRunID(substrateAdminCtx(context.Background()), "r_caller")
	walkCtx, runID, finish, err := srv.openTeamWalkRun(caller, builtin.WalkRunSpec{Name: "triage"})
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	defer finish("", nil)
	run, err := srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ParentRunID != "r_caller" {
		t.Errorf("walk's parent run = %q, want the calling run r_caller", run.ParentRunID)
	}
	if got := tools.RunID(walkCtx); got != runID {
		t.Errorf("walk ctx run id = %q, want the walk's own %q", got, runID)
	}
}

// A configured draft is a top-level run: neither its creation nor its start
// names a parent.
func TestConfiguredRun_StartedDraftRecordsNoParentRun(t *testing.T) {
	_, ts, _, st := configuredServer(t, 4)
	c := createDraft(t, ts, "")
	if run, err := st.GetRun(context.Background(), c.RunID); err != nil || run.ParentRunID != "" {
		t.Fatalf("draft row = %+v (%v), want no parent run", run, err)
	}
	if code, body := do(t, "POST", ts.URL+"/v1/runs/"+c.RunID+"/start", ""); code != http.StatusOK {
		t.Fatalf("start = %d %s", code, body)
	}
	if run, err := st.GetRun(context.Background(), c.RunID); err != nil || run.ParentRunID != "" {
		t.Errorf("started row = %+v (%v), want no parent run", run, err)
	}
}

// A resumed sub-run keeps the parent its row was created with: resume re-enters
// the loop on the existing row and must not rewrite its lineage.
func TestResumePausedRuns_ResumedChildKeepsItsParentRunID(t *testing.T) {
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"resumer": {Provider: "scripted", Model: "stub-model", SystemPrompt: "resume"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	prov := &scriptedProvider{defaultS: []providers.Event{
		{Type: providers.EventText, Text: "resumed"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
	}}
	srv, _ := makeServer(t, prov, cfg)
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "resumer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_resumed_child", ParentAgentID: "a_parent", ParentRunID: "r_parent", UserID: "alice", Model: "stub-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go on"}}},
	})
	if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	if n, warnings := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("ResumePausedRuns re-dispatched %d, want 1 (warnings: %v)", n, warnings)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := srv.store.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == store.RunCompleted || got.Status == store.RunFailed {
			if got.Status == store.RunFailed {
				t.Fatalf("resumed run failed: %s", got.ErrorMsg)
			}
			if got.ParentRunID != "r_parent" {
				t.Errorf("resumed child's parent run = %q, want r_parent", got.ParentRunID)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("resumed run did not finish (status=%q)", got.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The HTTP run read and the connector's (MCP get_run) name the parent run, and
// a top-level run's reads omit it.
func TestRunReads_CarryTheParentRunID(t *testing.T) {
	srv, _ := makeServer(t, &scriptedProvider{}, makeBaseConfig())
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "default", "alice")
	if err != nil {
		t.Fatal(err)
	}
	top, _ := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_top_read", UserID: "alice"})
	child, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_child_read", ParentAgentID: "a_top_read", ParentRunID: top.ID, UserID: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}

	read := func(agentID string) map[string]any {
		req := httptest.NewRequest("GET", "/v1/agents/"+agentID, nil)
		req.SetPathValue("agent_id", agentID)
		rec := httptest.NewRecorder()
		srv.handleGetAgent(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("GET /v1/agents/%s = %d %s", agentID, rec.Code, rec.Body)
		}
		return out
	}
	if got := read("a_child_read")["parent_run_id"]; got != top.ID {
		t.Errorf("HTTP read parent_run_id = %v, want %q", got, top.ID)
	}
	if _, ok := read("a_top_read")["parent_run_id"]; ok {
		t.Error("a top-level run's HTTP read carries parent_run_id")
	}

	got, err := srv.GetRunByRunID(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentRunID != top.ID {
		t.Errorf("connector read parent run = %q, want %q", got.ParentRunID, top.ID)
	}
	list, err := srv.ListRuns(ctx, connector.ListRunsFilter{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range list {
		if r.RunID == child.ID {
			found = r.ParentRunID == top.ID
		}
	}
	if !found {
		t.Errorf("connector listing does not carry the child's parent run: %+v", list)
	}
}
