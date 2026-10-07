package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A resident child whose run ends leaves the registry, but its parent can
// still read how it ended: a poll after the end answers with that ending, and
// a send says the child has ended rather than that it does not exist.

// closingGatedProvider is laterCappedProvider whose closing turn waits for
// gate, so a send that drives the child to its limit is still running when a
// bounded wait for it runs out.
type closingGatedProvider struct {
	laterCappedProvider
	gate chan struct{}
}

func (p closingGatedProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	if closingTurnAsked(req) {
		select {
		case <-p.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return p.laterCappedProvider.Call(ctx, req)
}

func closingGatedResidentServer(t *testing.T, gate chan struct{}) *Server {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"capped-later": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are capped later", MaxIterations: 3},
	}
	srv, _ := makeServer(t, closingGatedProvider{gate: gate}, cfg)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	return srv
}

// The case that lost the answer: a send bounded by timeout_ms comes back
// running, the turn then ends at the child's iteration limit, and the run is
// gone from the registry before the parent polls. The poll by child_run_id,
// the poll by child_run_ids and a cancel all read the capped ending with its
// answer, more than once; a send is told the child has ended.
func TestResidentTombstone_APollAfterTheRunEndedReadsItsCappedAnswer(t *testing.T) {
	gate := make(chan struct{})
	srv := closingGatedResidentServer(t, gate)
	ctx, _ := lockedParentCtx(t, srv)
	ctx = tools.WithBackground(ctx, tools.NewBackground(ctx))
	agent := agentToolOf(t, srv)

	res, err := agent.Execute(ctx, json.RawMessage(`{"op":"open","name":"capped-later","prompt":"start"}`))
	if err != nil {
		t.Fatal(err)
	}
	var env residentEnvelope
	if res.IsError || json.Unmarshal([]byte(res.Text), &env) != nil || env.State != "awaiting_input" {
		t.Fatalf("open = error:%v %q, want it parked", res.IsError, res.Text)
	}
	id := env.ChildRunID

	res, err = agent.Execute(ctx, json.RawMessage(`{"op":"send","child_run_id":"`+id+`","prompt":"keep going","timeout_ms":50}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || json.Unmarshal([]byte(res.Text), &env) != nil || env.State != "running" {
		t.Fatalf("bounded send = error:%v %q, want state running", res.IsError, res.Text)
	}
	close(gate)
	waitResidentGone(t, srv, id)

	wantCapped := func(what string, res tools.Result) {
		t.Helper()
		if !res.IsError || !strings.Contains(res.Text, `sub-agent "capped-later" stopped at its iteration limit of 3`) ||
			!strings.Contains(res.Text, "Its last answer:\n\ncapped last answer") {
			t.Errorf("%s after the end = error:%v %q, want the capped error with its answer", what, res.IsError, res.Text)
		}
	}
	for i := 0; i < 2; i++ {
		res, err = agent.Execute(ctx, json.RawMessage(`{"op":"poll","child_run_id":"`+id+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		wantCapped("poll", res)
	}
	res, err = agent.Execute(ctx, json.RawMessage(`{"op":"cancel","child_run_id":"`+id+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	wantCapped("cancel", res)

	res, err = agent.Execute(ctx, json.RawMessage(`{"op":"poll","child_run_ids":["`+id+`"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var rows struct {
		Children []struct {
			State  string `json:"state"`
			Status string `json:"status"`
			Output string `json:"output"`
		} `json:"children"`
	}
	if res.IsError || json.Unmarshal([]byte(res.Text), &rows) != nil || len(rows.Children) != 1 {
		t.Fatalf("poll by child_run_ids = error:%v %q", res.IsError, res.Text)
	}
	if r := rows.Children[0]; r.State != tools.ChildFailed || r.Status != "max_iterations" || r.Output != "capped last answer" {
		t.Errorf("poll row after the end = %+v, want failed/max_iterations with its answer", r)
	}

	res, err = agent.Execute(ctx, json.RawMessage(`{"op":"send","child_run_id":"`+id+`","prompt":"more"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "has ended") || strings.Contains(res.Text, "not found") {
		t.Errorf("send after the end = error:%v %q, want it told the child has ended", res.IsError, res.Text)
	}
}

// A closed child's ending is that it was closed, for poll and send alike.
func TestResidentTombstone_AClosedChildSaysItWasClosed(t *testing.T) {
	srv := newResidentTestServer(t)
	ctx := residentParentCtx("parent-agent", "")
	runID, _, _, err := srv.openResidentChild(ctx, "child", "start", "", 0, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := srv.closeResidentChild(ctx, runID); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitResidentGone(t, srv, runID)
	if _, _, err := srv.pollResidentChild(ctx, runID, 0); err == nil || !strings.Contains(err.Error(), "was closed by its parent") {
		t.Errorf("poll after close: %v, want it closed by its parent", err)
	}
	if _, _, err := srv.sendResidentChild(ctx, runID, "x", 0); err == nil || !strings.Contains(err.Error(), "was closed by its parent") {
		t.Errorf("send after close: %v, want it closed by its parent", err)
	}
}

// An ended child's ending is read under the rule that addressed it alive: an
// isolated run of another user gets the bare not-found, every peer the ending.
func TestResidentTombstone_TheEndingFollowsAddressing(t *testing.T) {
	srv := newResidentTestServer(t)
	owner := residentOwnerCtx()
	runID, _, _, err := srv.openResidentChild(owner, "child", "start", "", 0, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := srv.closeResidentChild(owner, runID); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitResidentGone(t, srv, runID)
	for _, c := range residentStrangers {
		caller := tools.WithRunIdentity(context.Background(), c.id)
		if _, _, err := srv.pollResidentChild(caller, runID, 0); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%s: poll %v, want a bare not-found", c.name, err)
		}
	}
	for _, c := range residentPeers {
		caller := tools.WithRunIdentity(context.Background(), c.id)
		if _, _, err := srv.pollResidentChild(caller, runID, 0); err == nil || !strings.Contains(err.Error(), "was closed") {
			t.Errorf("%s: poll %v, want the ending", c.name, err)
		}
	}
}

// The endings kept are bounded in number — the oldest goes first — and in
// time.
func TestResidentTombstone_EndingsAreBoundedInCountAndTime(t *testing.T) {
	r := newResidentRegistry()
	start := time.Now()
	for i := 0; i < residentTombstoneMax+5; i++ {
		rc := &residentChild{runID: "r" + strings.Repeat("x", i), state: "completed"}
		r.m[rc.runID] = rc
		r.removeAt(rc, start.Add(time.Duration(i)*time.Millisecond))
	}
	if n := len(r.gone); n != residentTombstoneMax {
		t.Fatalf("kept %d endings, want the cap %d", n, residentTombstoneMax)
	}
	if _, ok := r.gone["r"]; ok {
		t.Error("the oldest ending was kept over the cap")
	}
	if _, ok := r.gone["r"+strings.Repeat("x", residentTombstoneMax+4)]; !ok {
		t.Error("the newest ending was evicted")
	}
	r.pruneGone(start.Add(residentTombstoneTTL + time.Hour))
	if n := len(r.gone); n != 0 {
		t.Errorf("kept %d endings past their TTL, want none", n)
	}
}

// A close that reaches the child's run through the cancel registry alone — as
// one sent from another replica does — still leaves the ending "closed by its
// parent", read from the reason the run was cancelled with.
func TestResidentTombstone_ACloseThroughTheCancelRegistrySaysItWasClosed(t *testing.T) {
	srv := newResidentTestServer(t)
	ctx := residentParentCtx("parent-agent", "")
	runID, _, _, err := srv.openResidentChild(ctx, "child", "start", "", 0, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	rc, _ := srv.residentReg.get(runID)
	if _, found := srv.cancelReg.Cancel(rc.agentID, "closed by parent (resident sub-agent)"); !found {
		t.Fatal("the child is not in the cancel registry")
	}
	waitResidentGone(t, srv, runID)
	if _, _, err := srv.pollResidentChild(ctx, runID, 0); err == nil || !strings.Contains(err.Error(), "was closed by its parent") {
		t.Errorf("poll after a registry close: %v, want it closed by its parent", err)
	}
}

// Every reason the runtime ends a resident child's run with reads back as the
// ending it names; any other cancel reads as a cancel with its reason.
func TestResidentEndedFor_ReadsEveryReasonTheRuntimeCancelsWith(t *testing.T) {
	for _, tc := range []struct{ reason, reap, closed string }{
		{"", "", ""},
		{residentReasonClosedByParent, "", "closed by its parent"},
		{residentReasonClosedByOperator, "", "closed by the operator"},
		{residentReasonParentEnded, "", "closed when its parent run ended"},
		{residentReapIdle + " unused for longer than 30m0s" + residentReasonSuffix, residentReapIdle + " unused for longer than 30m0s", ""},
		{residentReapCeiling + " a turn ran longer than 2h0m0s" + residentReasonSuffix, residentReapCeiling + " a turn ran longer than 2h0m0s", ""},
		{"cancelled by api", "", "cancelled (cancelled by api)"},
	} {
		if reap, closed := residentEndedFor(tc.reason); reap != tc.reap || closed != tc.closed {
			t.Errorf("residentEndedFor(%q) = (%q, %q), want (%q, %q)", tc.reason, reap, closed, tc.reap, tc.closed)
		}
	}
}
