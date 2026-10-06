package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/awaited"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// familyProvider answers a parent from a script and holds every child until
// release is closed. Who is calling is read from the system prompt.
type familyProvider struct {
	mu      sync.Mutex
	parent  [][]providers.Event
	calls   []providers.Request // the parent's
	release chan struct{}
}

func (p *familyProvider) ID() string                  { return "stub" }
func (p *familyProvider) Probe(context.Context) error { return nil }
func (p *familyProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *familyProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *familyProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	var events []providers.Event
	if len(req.System) > 0 && strings.Contains(req.System[0].Text, "you are a child") {
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		events = []providers.Event{
			{Type: providers.EventText, Text: "child result"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		}
	} else {
		p.mu.Lock()
		i := len(p.calls)
		p.calls = append(p.calls, req)
		if i < len(p.parent) {
			events = p.parent[i]
		}
		p.mu.Unlock()
		if events == nil {
			return nil, context.Canceled
		}
	}
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (p *familyProvider) parentCalls() []providers.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providers.Request(nil), p.calls...)
}

func agentCall(id, input string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: "Agent", Input: json.RawMessage(input)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
}

func answer(text string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventText, Text: text},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
}

func familyConfig() *config.Config {
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"lead":   {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are the lead"},
		"worker": {Model: "stub-model", SystemPrompt: "you are a child"},
	}
	return cfg
}

// End to end on a real server: a parent that spawns two children in poll mode
// and ends its turn early is reported waiting on them (awaited_state
// "children", still running), wakes once both have ended with a note naming
// them, reads their results with poll and completes. Each child ran under the
// id it was handed out as, as a child of the parent's run.
func TestPollMode_EarlyEndingParentWaitsForItsChildrenThenCompletes(t *testing.T) {
	prov := &familyProvider{
		release: make(chan struct{}),
		parent: [][]providers.Event{
			agentCall("tu_1", `{"op":"parallel_spawn","mode":"poll","spawns":[{"name":"worker","prompt":"one"},{"name":"worker","prompt":"two"}]}`),
			answer("started them"),
			agentCall("tu_2", `{"op":"poll"}`),
			answer("both done"),
		},
	}
	srv, _ := makeServer(t, prov, familyConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := make(chan string, 1)
	go func() {
		resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"lead","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`))
		if err != nil {
			body <- "post: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()

	parentRun := ""
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for _, e := range srv.cancelReg.ListAll() {
			if e.ParentAgentID == "" {
				parentRun = e.RunID
			}
		}
		if parentRun != "" {
			if state, _ := awaited.ForRun(context.Background(), srv.store, parentRun); state == awaited.Children {
				break
			}
		}
	}
	state, on := awaited.ForRun(context.Background(), srv.store, parentRun)
	if state != awaited.Children || strings.Count(on, "r_") != 2 {
		t.Fatalf("parent awaited_state = %q on %q, want children naming both", state, on)
	}
	if run, err := srv.store.GetRun(context.Background(), parentRun); err != nil || run.Status != store.RunRunning {
		t.Fatalf("parked parent row = %+v, %v; want it still running", run, err)
	}
	if n := len(prov.parentCalls()); n != 2 {
		t.Fatalf("parent made %d calls while waiting, want 2", n)
	}

	close(prov.release)
	stream := <-body
	for _, want := range []string{"event: awaiting_children", "event: children_note", "child result"} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream lacks %q:\n%s", want, stream)
		}
	}
	calls := prov.parentCalls()
	if len(calls) != 4 {
		t.Fatalf("parent made %d calls, want 4", len(calls))
	}
	wake := calls[2].Messages[len(calls[2].Messages)-1].Content[0].Text
	if !strings.HasPrefix(wake, "Every background child you were waiting for has ended:") || strings.Count(wake, "(worker): completed") != 2 {
		t.Errorf("wake note = %q", wake)
	}
	parent, err := srv.store.GetRun(context.Background(), parentRun)
	if err != nil || parent.Status != store.RunCompleted {
		t.Fatalf("parent = %+v, %v; want completed", parent, err)
	}
	ids := strings.Split(on, ", ")
	for _, id := range ids {
		child, err := srv.store.GetRun(context.Background(), id)
		if err != nil {
			t.Fatalf("child %s has no run under the id it was handed out as: %v", id, err)
		}
		if child.Status != store.RunCompleted || child.ParentRunID != parentRun {
			t.Errorf("child %s = status %q parent %q, want completed under %s", id, child.Status, child.ParentRunID, parentRun)
		}
	}
	if state, _ := awaited.ForRun(context.Background(), srv.store, parentRun); state != "" {
		t.Errorf("finished parent still reports awaited_state %q", state)
	}
}

// With on_parent_end: cancel the parent completes as soon as it ends its
// turn, and its children's runs end cancelled.
func TestPollMode_OnParentEndCancelCompletesAtOnce(t *testing.T) {
	prov := &familyProvider{
		release: make(chan struct{}),
		parent: [][]providers.Event{
			agentCall("tu_1", `{"op":"spawn","name":"worker","prompt":"one","mode":"poll","on_parent_end":"cancel"}`),
			answer("not waiting"),
		},
	}
	defer close(prov.release)
	srv, _ := makeServer(t, prov, familyConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"lead","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	stream := string(b)
	if strings.Contains(stream, "event: awaiting_children") {
		t.Fatal("the parent waited for a child started to be cancelled at its turn end")
	}
	i := strings.Index(stream, `\"child_run_id\":\"`)
	if i < 0 {
		t.Fatalf("no child_run_id in the stream:\n%s", stream)
	}
	id := stream[i+len(`\"child_run_id\":\"`):]
	id = id[:strings.Index(id, `\"`)]
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		run, err := srv.store.GetRun(context.Background(), id)
		if err == nil && run.Status == store.RunCancelled {
			if !strings.Contains(run.StopReason, "on_parent_end") {
				t.Errorf("child stop reason = %q, want the turn-end cancel", run.StopReason)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %s = %+v, %v; want cancelled", id, run, err)
		}
	}
}

// Cancelling a parent that waits for its children cancels them too.
func TestPollMode_CancellingAParkedParentCancelsItsChildren(t *testing.T) {
	prov := &familyProvider{
		release: make(chan struct{}),
		parent: [][]providers.Event{
			agentCall("tu_1", `{"op":"spawn","name":"worker","prompt":"one","mode":"poll"}`),
			answer("started"),
		},
	}
	defer close(prov.release)
	srv, _ := makeServer(t, prov, familyConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"lead","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	var parentAgent, parentRun, child string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for _, e := range srv.cancelReg.ListAll() {
			if e.ParentAgentID == "" {
				parentAgent, parentRun = e.AgentID, e.RunID
			} else {
				child = e.RunID
			}
		}
		if parentRun != "" {
			if state, _ := awaited.ForRun(context.Background(), srv.store, parentRun); state == awaited.Children {
				break
			}
		}
	}
	if child == "" {
		t.Fatal("the child never started")
	}
	if _, ok := srv.cancelReg.Cancel(parentAgent, "operator stop"); !ok {
		t.Fatal("parent not cancellable")
	}
	<-done
	for _, id := range []string{parentRun, child} {
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			run, err := srv.store.GetRun(context.Background(), id)
			if err == nil && run.Status == store.RunCancelled {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("run %s = %+v, %v; want cancelled", id, run, err)
			}
		}
	}
}

// A child created under a minted id does not hand that id on: its own
// children's runs get ids of their own, so a background child can spawn.
func TestPollMode_AChildsOwnChildrenGetTheirOwnIDs(t *testing.T) {
	prov := &familyProvider{
		release: make(chan struct{}),
		parent: [][]providers.Event{ // the middle agent's script
			agentCall("tu_1", `{"op":"spawn","name":"worker","prompt":"one"}`),
			answer("mid done"),
		},
	}
	close(prov.release)
	cfg := familyConfig()
	cfg.Agents["mid"] = config.AgentDef{Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are the mid"}
	srv, _ := makeServer(t, prov, cfg)
	ctx, _ := lockedParentCtx(t, srv)
	minted := store.NewRunID()
	out, _, runID, err := srv.runAgentToolChild(tools.WithChildRunID(ctx, minted), "mid", "go", "")
	if err != nil {
		t.Fatalf("mid failed: %v", err)
	}
	if runID != minted || !strings.Contains(out, "mid done") {
		t.Fatalf("mid ran as %q answering %q, want %q answering mid done", runID, out, minted)
	}
	calls := prov.parentCalls()
	if len(calls) != 2 {
		t.Fatalf("mid made %d calls", len(calls))
	}
	last := calls[1].Messages[len(calls[1].Messages)-1].Content[0]
	if last.IsError || !strings.Contains(last.Text, "child result") {
		t.Fatalf("the grandchild's spawn answered %+v, want its result", last)
	}
	i := strings.Index(last.Text, "run_id=")
	grand := strings.Fields(last.Text[i+len("run_id="):])[0]
	grand = strings.TrimSuffix(grand, "]")
	run, err := srv.store.GetRun(context.Background(), grand)
	if err != nil || grand == minted || run.ParentRunID != minted {
		t.Errorf("grandchild %q = %+v, %v; want its own run under %s", grand, run, err, minted)
	}
}
