package a2a

import (
	"context"
	"errors"
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// An A2A task is a run, and a member reads and acts on only its own runs: a
// peer authenticated with a member's token gets another user's task — and a
// tenant-level one — as a task that does not exist, on get and on cancel, in
// single-tenant mode as well as with a routed tenant. A tenant operator's
// peer reaches every task of its tenant.
func TestTaskStore_AMemberPeerReachesOnlyItsOwnTasks(t *testing.T) {
	runs := &fakeRuns{byAgentID: map[string]store.Run{
		"task-alice":  {ID: "r_alice", AgentID: "task-alice", TenantID: "acme", UserID: "alice", Status: store.RunCompleted},
		"task-bob":    {ID: "r_bob", AgentID: "task-bob", TenantID: "acme", UserID: "bob", Status: store.RunCompleted},
		"task-tenant": {ID: "r_tenant", AgentID: "task-tenant", TenantID: "acme", Status: store.RunCompleted},
	}}
	ts := NewTaskStore(runs)
	scopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	member := WithPeerPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice", Scopes: scopes})
	operator := WithPeerPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}})

	for _, id := range []string{"task-bob", "task-tenant"} {
		if _, err := ts.Get(member, a2asdk.TaskID(id)); !errors.Is(err, a2asdk.ErrTaskNotFound) {
			t.Errorf("member get %s: err=%v, want task not found", id, err)
		}
	}
	if _, err := ts.Get(member, "task-alice"); err != nil {
		t.Errorf("member get its own task: %v", err)
	}
	if _, err := ts.Get(operator, "task-bob"); err != nil {
		t.Errorf("tenant operator get a member's task: %v", err)
	}

	fc := &fakeConnector{}
	ex := NewExecutor(&fakeRunner{}, fc, runs, "qa-agent")
	_, errs := collect(ex.Cancel(member, &a2asrv.ExecutorContext{TaskID: "task-bob"}))
	if len(errs) != 1 || !errors.Is(errs[0], a2asdk.ErrTaskNotFound) || fc.cancelledAgentID != "" {
		t.Errorf("member cancel bob's task: errs=%v cancelled=%q, want task not found and nothing cancelled", errs, fc.cancelledAgentID)
	}
	if _, errs := collect(ex.Cancel(member, &a2asrv.ExecutorContext{TaskID: "task-alice"})); len(errs) != 0 || fc.cancelledAgentID != "task-alice" {
		t.Errorf("member cancel its own task: errs=%v cancelled=%q", errs, fc.cancelledAgentID)
	}
}
