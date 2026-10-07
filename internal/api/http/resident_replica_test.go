package http

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A resident child lives on the replica that opened it, but its parent's calls
// may land on any replica. The tests here hold the child on one Server and
// address it from a second that shares its store, as a cluster's replicas do.

// echoProvider answers each turn of a resident child "reply to <latest
// message>". A latest message of "slow" waits for gate (when set) first; a
// turn cancelled while it waits ends with nothing more, and the child parks.
type echoProvider struct{ gate chan struct{} }

func (echoProvider) ID() string                  { return "stub" }
func (echoProvider) Probe(context.Context) error { return nil }
func (echoProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (echoProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p echoProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	last := lastText(req)
	ch := make(chan providers.Event, 2)
	go func() {
		defer close(ch)
		if last == "slow" && p.gate != nil {
			select {
			case <-p.gate:
			case <-ctx.Done():
				return
			}
		}
		for _, ev := range answer("reply to " + last) {
			ch <- ev
		}
	}()
	return ch, nil
}

// waitAwaitingInputs waits until runID's transcript records n parks.
func waitAwaitingInputs(t *testing.T, st store.Store, runID string, n int) {
	t.Helper()
	waitFor(t, "the child to park", func() bool {
		evs, err := st.GetRunEventsSince(context.Background(), runID, 0, 1000)
		got := 0
		for _, e := range evs {
			if e.Type == string(providers.EventAwaitingInput) {
				got++
			}
		}
		return err == nil && got >= n
	})
}

// A send that reaches the child's replica from another one arrives as a
// steer message alone, with no turn begun for it there. The child still
// takes it as a turn: its answer is that turn's alone, and the turn's end
// restarts its idle clock.
func TestResidentChild_ASendFromElsewhereIsATurnOnItsReplica(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Agents["child"] = cfg.Agents["default"]
	srv, _ := makeServer(t, echoProvider{}, cfg)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ctx := residentParentCtx("parent-agent", "")
	runID, out, _, err := srv.openResidentChild(ctx, "child", "start", "", 0, 0)
	if err != nil || out != "reply to start" {
		t.Fatalf("open: %q %v", out, err)
	}
	defer func() {
		_ = srv.closeResidentChild(ctx, runID)
		waitResidentGone(t, srv, runID)
	}()
	ageResidentChild(t, srv, runID, time.Hour)

	// What the steer coordinator's listener does with a send from elsewhere.
	if delivered, found, _ := srv.steerReg.PushLocal(runID, steer.Message{Text: "next", Source: "agent", EnqueuedAt: time.Now()}); !delivered || !found {
		t.Fatalf("push: delivered %v found %v", delivered, found)
	}
	waitAwaitingInputs(t, srv.store, runID, 2)

	// Swept before the poll, which restarts the idle clock itself.
	srv.sweepResidentChildren(time.Now())
	if rc, ok := srv.residentReg.get(runID); !ok || rc.ending().reapReason != "" {
		t.Fatalf("the child was reaped as idle right after a turn")
	}
	if out, state, err := srv.pollResidentChild(ctx, runID, 0); err != nil || state != "awaiting_input" || out != "reply to next" {
		t.Errorf("poll after the turn = %q %q %v, want only that turn's answer", out, state, err)
	}
}

// The cluster routes the coordinators give a replica, reduced to what they
// do on arrival: the owner's local registries.
type peerSteer struct{ owner *Server }

func (p peerSteer) PushRemote(_ context.Context, runID string, m steer.Message) (bool, bool, error) {
	delivered, found, _ := p.owner.steerReg.PushLocal(runID, m)
	return delivered, found, nil
}

type peerCancel struct{ owner *Server }

func (p peerCancel) CancelRemote(_ context.Context, agentID, reason string) (cancel.CancelResult, bool, error) {
	res, found := p.owner.cancelReg.CancelLocal(agentID, reason)
	return res, found, nil
}

type peerTurnCancel struct{ owner *Server }

func (p peerTurnCancel) CancelRemote(_ context.Context, runID, reason string) (bool, error) {
	return p.owner.turnCancelReg.CancelLocal(runID, reason), nil
}

// twoReplicas returns a, which will hold the resident children, and b, which
// shares a's store and reaches a only through the cluster routes (when routed).
func twoReplicas(t *testing.T, prov providers.Provider, cfg *config.Config, routed bool) (a, b *Server) {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "cluster.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a = New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(8, 8, time.Second), st)
	b = New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(8, 8, time.Second), st)
	a.SetSteerRegistry(steer.NewRegistry(0))
	b.SetSteerRegistry(steer.NewRegistry(0))
	if routed {
		b.steerReg.SetClusterSteerer(peerSteer{a})
		b.cancelReg.SetClusterCanceller(peerCancel{a})
		b.turnCancelReg.SetClusterCanceller(peerTurnCancel{a})
	}
	t.Cleanup(func() {
		cancelAllRuns(a)
		for deadline := time.Now().Add(10 * time.Second); a.cancelReg.Count() > 0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	})
	return a, b
}

func echoConfig() *config.Config {
	cfg := makeBaseConfig()
	cfg.Agents["child"] = cfg.Agents["default"]
	return cfg
}

// A poll from a replica that does not hold the child reads its parked turn,
// a turn still running, and that turn once it ends, from the store.
func TestResidentElsewhere_APollReadsTheChildFromTheStore(t *testing.T) {
	gate := make(chan struct{})
	a, b := twoReplicas(t, echoProvider{gate: gate}, echoConfig(), false)
	ctx := residentParentCtx("parent-agent", "")
	runID, _, _, err := a.openResidentChild(ctx, "child", "start", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out, state, err := b.pollResidentChild(ctx, runID, 0); err != nil || state != "awaiting_input" || out != "reply to start" {
		t.Fatalf("poll of the parked child elsewhere = %q %q %v, want its parked turn", out, state, err)
	}
	if _, state, err := a.sendResidentChild(ctx, runID, "slow", 50); err != nil || state != "running" {
		t.Fatalf("bounded send = %q %v, want running", state, err)
	}
	if _, state, err := b.pollResidentChild(ctx, runID, 0); err != nil || state != "running" {
		t.Errorf("poll of the running child elsewhere = %q %v, want running", state, err)
	}
	type polled struct {
		out, state string
		err        error
	}
	done := make(chan polled, 1)
	go func() {
		out, state, err := b.pollResidentChild(ctx, runID, 10_000)
		done <- polled{out, state, err}
	}()
	close(gate)
	select {
	case p := <-done:
		if p.err != nil || p.state != "awaiting_input" || p.out != "reply to slow" {
			t.Errorf("waiting poll elsewhere = %q %q %v, want the turn's answer", p.out, p.state, p.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the waiting poll elsewhere did not return")
	}
}

// A child whose run ended at its iteration limit reads, from the store, as
// the same capped error with the turn's answer; a closed one as closed.
func TestResidentElsewhere_APollReadsHowTheChildEnded(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"capped": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are capped", MaxIterations: 2},
		"whole":  {Model: "stub-model", Tools: []string{}, SystemPrompt: "you are whole"},
	}
	a, b := twoReplicas(t, laterCappedProvider{}, cfg, false)
	ctx := residentParentCtx("parent-agent", "")

	runID, _, _, err := a.openResidentChild(ctx, "capped", "x", "", 0, 0)
	var capped *builtin.ChildCappedError
	if !errors.As(err, &capped) {
		t.Fatalf("open of a capped child: %v", err)
	}
	waitResidentGone(t, a, runID)
	if _, _, err := b.pollResidentChild(ctx, runID, 0); !errors.As(err, &capped) || capped.Output != "capped last answer" {
		t.Errorf("poll of the capped child elsewhere: %v, want the capped error with its answer", err)
	}

	runID, _, _, err = a.openResidentChild(ctx, "whole", "x", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = a.closeResidentChild(ctx, runID)
	waitResidentGone(t, a, runID)
	if _, _, err := b.pollResidentChild(ctx, runID, 0); err == nil || !strings.Contains(err.Error(), "was closed by its parent") {
		t.Errorf("poll of the closed child elsewhere: %v, want it closed by its parent", err)
	}
}

// The store answers only for a resident child the caller could address on
// its own replica: an isolated run of another user, and any id that is not a
// resident child's run, get the bare not-found.
func TestResidentElsewhere_TheStoreAnswersOnlyWhomTheChildWould(t *testing.T) {
	a, b := twoReplicas(t, echoProvider{}, echoConfig(), false)
	owner := residentOwnerCtx()
	runID, _, _, err := a.openResidentChild(owner, "child", "start", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range residentStrangers {
		caller := tools.WithRunIdentity(context.Background(), c.id)
		if _, _, err := b.pollResidentChild(caller, runID, 0); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%s: poll elsewhere %v, want a bare not-found", c.name, err)
		}
	}
	for _, c := range residentPeers {
		caller := tools.WithRunIdentity(context.Background(), c.id)
		if out, _, err := b.pollResidentChild(caller, runID, 0); err != nil || out != "reply to start" {
			t.Errorf("%s: poll elsewhere %q %v, want the child", c.name, out, err)
		}
	}
	sess, err := b.store.CreateSession(context.Background(), "acme", "lead", "alice")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := b.store.CreateRun(context.Background(), sess.ID, store.RunIdentity{AgentID: "a_plain", UserID: "alice", TenantID: "acme", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.pollResidentChild(owner, plain.ID, 0); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("poll of a run that is not a resident child: %v, want not-found", err)
	}
	if _, _, err := b.sendResidentChild(owner, plain.ID, "x", 0); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("send to a run that is not a resident child: %v, want not-found", err)
	}
}

// With the cluster's routes, send, cancel and close reach a child held by
// another replica, and their answers come back through the store.
func TestResidentElsewhere_SendCancelAndCloseReachTheChild(t *testing.T) {
	gate := make(chan struct{})
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	a, b := twoReplicas(t, echoProvider{gate: gate}, echoConfig(), true)
	ctx := residentParentCtx("parent-agent", "")
	runID, _, _, err := a.openResidentChild(ctx, "child", "start", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out, state, err := b.sendResidentChild(ctx, runID, "next", 0); err != nil || state != "awaiting_input" || out != "reply to next" {
		t.Fatalf("send elsewhere = %q %q %v, want the turn's answer", out, state, err)
	}
	// The slow turn says nothing before its gate opens: whether or not the
	// child took the instruction yet, none of the last turn's answer is this
	// one's.
	if out, state, err := b.sendResidentChild(ctx, runID, "slow", 50); err != nil || state != "running" || out != "" {
		t.Fatalf("bounded send elsewhere = %q %q %v, want running with no answer yet", out, state, err)
	}
	if _, _, err := b.sendResidentChild(ctx, runID, "more", 0); err == nil || !strings.Contains(err.Error(), "still running its previous turn") {
		t.Errorf("send elsewhere over a running turn: %v, want it refused", err)
	}
	waitFor(t, "the slow turn to arm its cancel", func() bool { return a.turnCancelReg.IsArmed(runID) })
	if _, state, err := b.cancelResidentChildTurn(ctx, runID); err != nil || state != "awaiting_input" {
		t.Errorf("cancel elsewhere = %q %v, want the child parked again", state, err)
	}
	if err := b.closeResidentChild(ctx, runID); err != nil {
		t.Fatalf("close elsewhere: %v", err)
	}
	waitResidentGone(t, a, runID)
	if _, _, err := b.pollResidentChild(ctx, runID, 0); err == nil || !strings.Contains(err.Error(), "was closed by its parent") {
		t.Errorf("poll after a close elsewhere: %v, want it closed by its parent", err)
	}
}

// Without a route to the child's replica, a send says it cannot reach it
// rather than that it does not exist; the poll by child_run_ids still reads
// the child.
func TestResidentElsewhere_WithoutARouteASendSaysSo(t *testing.T) {
	a, b := twoReplicas(t, echoProvider{}, echoConfig(), false)
	ctx := residentParentCtx("parent-agent", "")
	runID, _, _, err := a.openResidentChild(ctx, "child", "start", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.sendResidentChild(ctx, runID, "next", 0); err == nil || !strings.Contains(err.Error(), "not held by this replica") {
		t.Errorf("send elsewhere with no route: %v, want it unreachable from here", err)
	}
	if err := b.closeResidentChild(ctx, runID); err == nil || !strings.Contains(err.Error(), "not held by this replica") {
		t.Errorf("close elsewhere with no route: %v, want it unreachable from here", err)
	}

	pctx := tools.WithBackground(ctx, tools.NewBackground(ctx))
	tools.BackgroundOf(pctx).AddResident(runID, "child")
	res, err := agentToolOf(t, b).Execute(pctx, json.RawMessage(`{"op":"poll","child_run_ids":["`+runID+`"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Text, `"state":"idle"`) || !strings.Contains(res.Text, `"output":"reply to start"`) {
		t.Errorf("poll by child_run_ids elsewhere = %q, want it idle with its answer", res.Text)
	}
}
