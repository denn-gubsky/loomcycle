package scheduler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// End to end, from the authoring tool to the fired run: whatever a tenant
// operator in acme does with the ScheduleDef tool — name another tenant in the
// overlay, or fork a shared schedule an admin pointed at another tenant —
// every schedule active in acme fires its run in acme. The fired run's tenant
// is what resolves agents / skills / MCP servers and receives its memory.
func TestScheduler_NonAdminAuthoredScheduleFiresInTheAuthorsTenant(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tool := &builtin.ScheduleDef{Store: st, Cfg: &config.Config{}}

	as := func(tenant string, scope string) context.Context {
		ctx := tools.WithScheduleDefPolicy(context.Background(), tools.ScheduleDefPolicyValue{Scopes: []string{"any"}, SelfName: "orchestrator"})
		ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test", TenantID: tenant})
		return auth.WithPrincipal(ctx, auth.Principal{TenantID: tenant, Subject: "op-" + tenant, Scopes: []string{scope}})
	}
	body := `"agent":"digest","schedule":"0 9 * * 1"`
	exec := func(ctx context.Context, in string) {
		// Refusals are the point for some of these; the assertion is on what fires.
		res, _ := tool.Execute(ctx, json.RawMessage(in))
		t.Logf("%s -> error=%v %s", in, res.IsError, res.Text)
	}

	acme := as("acme", auth.ScopeTenant)
	exec(acme, `{"op":"create","name":"foreign","overlay":{`+body+`,"tenant_id":"globex"}}`)
	exec(acme, `{"op":"create","name":"own","overlay":{`+body+`}}`)
	exec(acme, `{"op":"fork","name":"own","overlay":{"tenant_id":"globex"}}`)
	exec(as("", auth.ScopeAdmin), `{"op":"create","name":"shared","overlay":{`+body+`,"tenant_id":"globex"}}`)
	exec(acme, `{"op":"fork","name":"shared","overlay":{"user_id":"alice"}}`)

	fr := &fakeRunner{}
	sched := New(Config{TickInterval: 10 * time.Millisecond, FireTimeout: 5 * time.Second}, st, fr, nil, &fakeMCP{}, t.Logf)
	names, err := st.ScheduleDefListNames(context.Background())
	if err != nil {
		t.Fatalf("list names: %v", err)
	}
	fired := 0
	for _, n := range names {
		if n.TenantID != "acme" || n.ActiveDefID == "" {
			continue
		}
		row, err := st.ScheduleDefGet(context.Background(), n.ActiveDefID)
		if err != nil {
			t.Fatalf("get %s: %v", n.ActiveDefID, err)
		}
		before := len(fr.Calls())
		sched.fireOne(context.Background(), store.ScheduleDueRow{DefID: row.DefID, Name: row.Name, Definition: row.Definition}, time.Now())
		calls := fr.Calls()
		if len(calls) != before+1 {
			t.Fatalf("schedule %q did not fire a run", n.Name)
		}
		fired++
		if got := calls[len(calls)-1].TenantID; got != "acme" {
			t.Errorf("schedule %q, active in acme and authored by an acme tenant operator, fired its run in tenant %q", n.Name, got)
		}
	}
	if fired == 0 {
		t.Fatal("no schedule is active in acme — the fixture fired nothing, so the assertion is vacuous")
	}
}
