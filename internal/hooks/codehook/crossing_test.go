package codehook

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The whole path, with the real Interruption tool: the dispatcher hands a code
// hook to the runner, the body asks, the ask is a pending interrupt on the
// run, and resolving it the way the operator's resolve endpoint does lets the
// body decide. The agent itself is not allowed to interrupt — the hook asks
// under its own grant.
func TestCodeHook_AnAskIsAPendingInterruptAndItsAnswerDecides(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "t", "agent", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a1", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bus := channels.NewBus()
	it := &builtin.Interruption{Store: st, Bus: bus, MaxTimeout: time.Minute}

	reg := hooks.NewRegistry()
	if _, err := reg.Register(&hooks.Hook{Owner: "ops", Name: "gate", Phase: hooks.PhasePre, Tools: []string{"HTTP"}, Code: `
		function hook(ev) {
			var a = Interruption.ask({question: "Let " + ev.agent + " call " + ev.tool_call.input.url + "?", options: ["allow", "deny"]});
			return a === "allow" ? {} : {decision: "deny", reason: "the operator said no"};
		}`}); err != nil {
		t.Fatal(err)
	}
	d := hooks.NewDispatcher(reg, nil)
	d.SetCodeRunner(New(it))

	runCtx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", AgentID: "a1"})
	runCtx = tools.WithRunID(runCtx, run.ID)
	runCtx = tools.WithInterruptionPolicy(runCtx, tools.InterruptionPolicyValue{Enabled: false})

	for _, answer := range []string{"deny", "allow"} {
		done := make(chan hooks.PreOutcome, 1)
		go func() {
			done <- d.RunPre(runCtx, hooks.Identity{Agent: "fetcher", RunID: run.ID},
				hooks.ToolCall{ID: "t1", Name: "HTTP", Input: json.RawMessage(`{"url":"https://example.test"}`)})
		}()

		var pending []store.InterruptRow
		for deadline := time.Now().Add(5 * time.Second); len(pending) == 0; {
			if time.Now().After(deadline) {
				t.Fatalf("%s: no pending interrupt appeared", answer)
			}
			time.Sleep(10 * time.Millisecond)
			if pending, err = st.InterruptListByRun(ctx, run.ID, store.InterruptStatusPending); err != nil {
				t.Fatal(err)
			}
		}
		if q := pending[0].Question; q != "Let fetcher call https://example.test?" {
			t.Errorf("question = %q", q)
		}
		select {
		case out := <-done:
			t.Fatalf("%s: the call was decided before the answer: %+v", answer, out)
		default:
		}
		if err := st.InterruptResolve(ctx, pending[0].InterruptID, answer, store.InterruptResolvedByWebUI, nil); err != nil {
			t.Fatal(err)
		}
		bus.Notify("intr:" + pending[0].InterruptID)

		select {
		case out := <-done:
			switch answer {
			case "deny":
				if out.Deny == nil || out.Deny.Text != "the operator said no" {
					t.Errorf("deny: outcome = %+v", out)
				}
			case "allow":
				if out.Deny != nil || len(out.Decisions) != 0 {
					t.Errorf("allow: outcome = %+v", out)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the call was not decided after the answer", answer)
		}
	}
}
