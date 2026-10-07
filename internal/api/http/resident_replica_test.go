package http

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A resident child lives on the replica that opened it, but its parent's calls
// may land on any replica. The tests here hold the child on one Server and
// address it from a second that shares its store, as a cluster's replicas do.

// echoProvider answers each turn of a resident child "reply to <latest
// message>". A latest message of "slow" waits for gate (when set) first.
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
	if last == "slow" && p.gate != nil {
		select {
		case <-p.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	events := answer("reply to " + last)
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
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
