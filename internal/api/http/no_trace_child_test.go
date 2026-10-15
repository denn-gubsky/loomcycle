package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// childCanary is what a no-trace child answers. Its ordinary parent's own
// content is stored as usual; the child's answer must not be, anywhere.
const childCanary = "CHILD-CANARY-5be0c4d1-no-trace"

// secretOnDisk reports whether secret is anywhere in the database files or
// in any stored event.
func secretOnDisk(t *testing.T, srv *Server, dbPath, secret string) bool {
	t.Helper()
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-journal"} {
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(secret)) {
			return true
		}
	}
	sessions, _, err := srv.store.ListSessions(context.Background(), store.SessionFilter{}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, sess := range sessions {
		events, err := srv.store.GetTranscript(context.Background(), sess.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range events {
			if bytes.Contains(ev.Payload, []byte(secret)) {
				return true
			}
		}
	}
	return false
}

// childRouter answers a parent from a script and a child with childCanary;
// which one is calling is read from the system prompt. It records what the
// parent's model was sent.
type childRouter struct {
	mu     sync.Mutex
	parent [][]providers.Event
	seen   []providers.Request
}

func (p *childRouter) ID() string                  { return "stub" }
func (p *childRouter) Probe(context.Context) error { return nil }
func (p *childRouter) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *childRouter) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *childRouter) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	var events []providers.Event
	if len(req.System) > 0 && strings.HasPrefix(req.System[0].Text, "child system") {
		events = answer("child verdict " + childCanary)
	} else {
		p.mu.Lock()
		i := len(p.seen)
		p.seen = append(p.seen, req)
		if i < len(p.parent) {
			events = p.parent[i]
		} else {
			events = answer("done")
		}
		p.mu.Unlock()
	}
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// parentSaw reports whether the parent's model was handed the child's answer.
func (p *childRouter) parentSaw(secret string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, req := range p.seen {
		b, _ := json.Marshal(req.Messages)
		if bytes.Contains(b, []byte(secret)) {
			return true
		}
	}
	return false
}

// An ordinary parent that spawns a no-trace child — waiting for it, in a
// parallel_spawn with the spawn ledger on, or in poll mode — still hands its
// model the child's answer, and stores none of it: not in the tool_result row,
// not in the spawn ledger's result row, not in the poll-mode result row.
func TestNoTrace_ANoTraceChildsAnswerIsStoredNowhereInItsOrdinaryParent(t *testing.T) {
	cases := map[string][][]providers.Event{
		"spawn":          {agentCall("tu_1", `{"op":"spawn","name":"child","prompt":"judge it"}`), answer("final ok")},
		"parallel_spawn": {agentCall("tu_1", `{"op":"parallel_spawn","spawns":[{"name":"child","prompt":"judge it"}]}`), answer("final ok")},
		"poll": {
			agentCall("tu_1", `{"op":"spawn","name":"child","prompt":"judge it","mode":"poll"}`),
			agentCall("tu_2", `{"op":"poll","wait":"all"}`),
			answer("final ok"),
		},
	}
	for name, script := range cases {
		for _, noTrace := range []bool{true, false} {
			prov := &childRouter{parent: script}
			cfg := noTraceConfig()
			cfg.Env.ResumeFanout = true // the spawn ledger, whose result row is one of the copies
			srv, dbPath := makeServer(t, prov, cfg)
			if noTrace {
				markNoTrace(srv, "child")
			}
			code, body := postStrictRun(t, srv, parentRunBody)
			cancelAllRuns(srv)
			if code != 200 {
				t.Fatalf("%s noTrace=%v: status %d: %s", name, noTrace, code, body)
			}
			if !prov.parentSaw(childCanary) {
				t.Errorf("%s noTrace=%v: the parent's model was not handed its child's answer", name, noTrace)
			}
			found := secretOnDisk(t, srv, dbPath, childCanary)
			if noTrace && found {
				t.Errorf("%s: a no-trace child's answer is on disk in its ordinary parent's records", name)
			}
			if !noTrace && !found {
				t.Fatalf("%s: an ordinary child's answer is not on disk: this test cannot detect a leak", name)
			}
			if !noTrace {
				continue
			}
			// The parent itself is ordinary: its own content is stored.
			parent := runOfAgent(t, srv, "a_parent")
			if parent.Retention != "" || !strings.Contains(string(parent.Result), "final ok") {
				t.Errorf("%s: parent row retention=%q result=%s, want an ordinary run that kept its answer", name, parent.Retention, parent.Result)
			}
		}
	}
}

// A walk records each member's answer for the states after it, so a no-trace
// agent cannot be a member of one.
func TestNoTrace_ANoTraceAgentIsRefusedAsATeamMember(t *testing.T) {
	srv, _ := makeServer(t, &childRouter{}, noTraceConfig())
	markNoTrace(srv, "child")
	_, err := srv.runTeamMember(context.Background(), "child", teamrun.Prompt{Input: "hi"}, "")
	if !errors.Is(err, runner.ErrRetentionUnsupported) {
		t.Errorf("a no-trace team member: err = %v, want ErrRetentionUnsupported", err)
	}
}
