package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// canary is a string that appears in everything a run says in these tests:
// its prompt, its system prompt, the model's text, a tool call's arguments,
// the child's answer, the final answer, an error. A no-trace run must leave
// it nowhere on disk.
const canary = "CANARY-7f3a91c2-no-trace"

func noTraceConfig() *config.Config {
	cfg := strictLimitsConfig()
	p, c := cfg.Agents["parent"], cfg.Agents["child"]
	p.SystemPrompt, c.SystemPrompt = "parent system "+canary, "child system "+canary
	cfg.Agents["parent"], cfg.Agents["child"] = p, c
	return cfg
}

// canaryTree is a parent that says the canary, spawns a child with it, and
// answers with it; the child answers with it too.
func canaryTree() *scriptedProvider {
	return &scriptedProvider{scripts: [][]providers.Event{
		{
			{Type: providers.EventText, Text: "thinking about " + canary},
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent",
				Input: json.RawMessage(`{"name":"child","prompt":"judge ` + canary + `"}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 11, OutputTokens: 3}, Reasoning: "reasoned " + canary},
		},
		{{Type: providers.EventText, Text: "child verdict " + canary}, spend("end_turn", 5)},
		{{Type: providers.EventText, Text: "final " + canary}, spend("end_turn", 7)},
	}}
}

const canaryRunBody = `{"agent":"parent","agent_id":"a_parent","user_id":"alice","segments":[{"role":"user","content":[{"type":"trusted-text","text":"review ` + canary + `"}]}]}`

// onDisk reports whether the canary is anywhere in the database file or its
// write-ahead log. The files are searched as bytes, so a table nobody thought
// of counts.
func onDisk(t *testing.T, srv *Server, dbPath string) bool {
	t.Helper()
	found := false
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-journal"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if bytes.Contains(b, []byte(canary)) {
			found = true
		}
	}
	// And through the store, in case a backend ever stores text encoded.
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
			if bytes.Contains(ev.Payload, []byte(canary)) {
				found = true
			}
		}
	}
	return found
}

func TestNoTrace_ARunTreeLeavesItsContentNowhereOnDisk(t *testing.T) {
	for _, noTrace := range []bool{true, false} {
		prov := canaryTree()
		srv, dbPath := makeServer(t, prov, noTraceConfig())
		if noTrace {
			srv.noTraceAgents = map[string]bool{"parent": true}
		}
		code, body := postStrictRun(t, srv, canaryRunBody)
		if code != 200 {
			t.Fatalf("noTrace=%v: status %d: %s", noTrace, code, body)
		}
		// Delivery is unchanged: the caller on the connection gets it all,
		// the child's answer included.
		for _, want := range []string{"final " + canary, "child verdict " + canary} {
			if !strings.Contains(body, want) {
				t.Errorf("noTrace=%v: the live stream is missing %q", noTrace, want)
			}
		}
		if n := prov.calls.Load(); n != 3 {
			t.Errorf("noTrace=%v: model calls = %d, want 3", noTrace, n)
		}
		found := onDisk(t, srv, dbPath)
		if noTrace && found {
			t.Error("the canary is on disk after a no-trace run")
		}
		// The check has to be able to fail: an ordinary run stores it.
		if !noTrace && !found {
			t.Fatal("the canary is not on disk after an ordinary run: this test cannot detect a leak")
		}
		if !noTrace {
			continue
		}

		// What IS kept: the rows, their links, their usage, and a marker for
		// every event.
		parent := runOfAgent(t, srv, "a_parent")
		if parent.Retention != store.RetentionNone || parent.Status != store.RunCompleted || parent.StopReason != "end_turn" {
			t.Errorf("parent row: retention=%q status=%q stop=%q", parent.Retention, parent.Status, parent.StopReason)
		}
		if parent.InputTokens != 18 || !strings.Contains(string(parent.Result), `"content_retained":false`) {
			t.Errorf("parent row: input tokens = %d (want 18), result = %s", parent.InputTokens, parent.Result)
		}
		children, err := srv.store.ListRunsByParentAgentID(context.Background(), "a_parent")
		if err != nil || len(children) != 1 {
			t.Fatalf("children = %d, err = %v", len(children), err)
		}
		// The child's own agent is not a no-trace agent: it is one because
		// its parent is.
		if children[0].Retention != store.RetentionNone || children[0].SessionID == parent.SessionID {
			t.Errorf("child row: retention=%q, session shared with parent=%v", children[0].Retention, children[0].SessionID == parent.SessionID)
		}
		for _, sid := range []string{parent.SessionID, children[0].SessionID} {
			if sess, err := srv.store.GetSession(context.Background(), sid); err != nil || sess.Retention != store.RetentionNone {
				t.Errorf("session %s retention = %q (err %v)", sid, sess.Retention, err)
			}
		}
		events, err := srv.store.GetTranscript(context.Background(), parent.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		types := map[string]int{}
		for _, ev := range events {
			types[ev.Type]++
			var m eventMarkerRecord
			if err := json.Unmarshal(ev.Payload, &m); err != nil || m.Retained {
				t.Errorf("event %s is not a marker: %s", ev.Type, ev.Payload)
			}
			if ev.Type == "tool_call" && (m.Tool != "Agent" || m.ToolUse == nil || m.ToolUse.ID != "tu_1" || m.Bytes == 0) {
				t.Errorf("tool_call marker = %s, want the tool, its id and a size", ev.Payload)
			}
		}
		for _, typ := range []string{"user_input", "system_prompt", "text", "tool_call", "tool_result", "usage", "done"} {
			if types[typ] == 0 {
				t.Errorf("no %s marker: an event of a no-trace run must still be recorded as having happened (have %v)", typ, types)
			}
		}
		rows, err := srv.store.UsageReport(context.Background(), store.UsageQuery{})
		if err != nil {
			t.Fatal(err)
		}
		var tokens int64
		for _, r := range rows {
			tokens += r.InputTokens + r.OutputTokens
		}
		if tokens != 11+3+5+7 {
			t.Errorf("usage ledger holds %d tokens, want 26: a no-trace run is still metered", tokens)
		}
	}
}

// A provider's error can quote the prompt it was sent. The run row keeps the
// class of the error; the caller still gets the text.
func TestNoTrace_AnErrorIsStoredAsItsClass(t *testing.T) {
	srv, dbPath := makeServer(t, &failingProvider{err: fmt.Errorf("%w: invalid request, prompt was: %s", runner.ErrTokenLimitExceeded, canary)}, noTraceConfig())
	srv.noTraceAgents = map[string]bool{"parent": true}
	_, body := postStrictRun(t, srv, canaryRunBody)
	if !strings.Contains(body, "prompt was: "+canary) {
		t.Errorf("the caller did not get its own error text:\n%s", body)
	}
	run := runOfAgent(t, srv, "a_parent")
	// The class, which only the place that still holds the error can know.
	if run.Status != store.RunFailed || run.ErrorMsg != "error class: business" {
		t.Errorf("run row: status=%q error=%q, want failed with its class and no text", run.Status, run.ErrorMsg)
	}
	if onDisk(t, srv, dbPath) {
		t.Error("the canary is on disk after a no-trace run that failed")
	}
}

type failingProvider struct{ err error }

func (p *failingProvider) ID() string                                   { return "scripted" }
func (p *failingProvider) Probe(context.Context) error                  { return nil }
func (p *failingProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *failingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *failingProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	return nil, p.err
}

func TestNoTrace_WhatANoTraceRunCannotDo(t *testing.T) {
	srv, _ := makeServer(t, &scriptedProvider{defaultS: finalAnswer}, noTraceConfig())
	srv.noTraceAgents = map[string]bool{"parent": true}
	ctx := context.Background()
	seg := `"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]`
	segs := []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}}
	runsBefore := func() int {
		sessions, _, err := srv.store.ListSessions(ctx, store.SessionFilter{}, 1000, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(sessions)
	}

	// An ordinary session of another agent, and a finished no-trace run.
	if code, body := postStrictRun(t, srv, `{"agent":"child","agent_id":"a_plain",`+seg+`}`); code != 200 {
		t.Fatalf("plain run: %d %s", code, body)
	}
	plain := runOfAgent(t, srv, "a_plain")
	if code, body := postStrictRun(t, srv, `{"agent":"parent","agent_id":"a_quiet",`+seg+`}`); code != 200 {
		t.Fatalf("no-trace run: %d %s", code, body)
	}
	quiet := runOfAgent(t, srv, "a_quiet")
	before := runsBefore()

	for what, body := range map[string]string{
		"interactive":                      `{"agent":"parent","interactive":true,` + seg + `}`,
		"review":                           `{"agent":"parent","review":true,` + seg + `}`,
		"into an existing session":         `{"agent":"parent","session_id":"` + plain.SessionID + `",` + seg + `}`,
		"an ordinary run into its session": `{"agent":"child","session_id":"` + quiet.SessionID + `",` + seg + `}`,
	} {
		code, resp := postStrictRun(t, srv, body)
		if code != 400 && code != 409 || !strings.Contains(resp, "retention_none_unsupported") {
			t.Errorf("%s: status %d, want 400 or 409 retention_none_unsupported: %s", what, code, resp)
		}
	}
	for what, in := range map[string]runner.RunInput{
		"RunOnce interactive":                   {Agent: "parent", Interactive: true, Segments: segs},
		"RunOnce into a session":                {SessionID: quiet.SessionID, Segments: segs},
		"RunOnce asked for none into a session": {Agent: "child", Retention: store.RetentionNone, SessionID: plain.SessionID, Segments: segs},
	} {
		if err := srv.RunOnce(ctx, in, runner.RunCallbacks{}); !errors.Is(err, runner.ErrRetentionUnsupported) {
			t.Errorf("%s: err = %v, want ErrRetentionUnsupported", what, err)
		}
	}

	// Continuing, compacting, recapping and replaying the no-trace session.
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/sessions/"+quiet.SessionID+"/messages", "application/json", strings.NewReader(`{`+seg+`}`))
	if err != nil {
		t.Fatal(err)
	}
	contBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 409 || !strings.Contains(string(contBody), "retention_none_unsupported") {
		t.Errorf("continuation: status %d, want 409: %s", resp.StatusCode, contBody)
	}
	if _, err := srv.CompactRun(ctx, quiet.ID); err == nil || !strings.Contains(err.Error(), "no-trace") {
		t.Errorf("compact: err = %v, want it refused", err)
	}
	if _, err := srv.RecapSession(ctx, quiet.SessionID); !errors.Is(err, runner.ErrRetentionUnsupported) {
		t.Errorf("recap: err = %v, want ErrRetentionUnsupported", err)
	}
	if _, err := srv.ReplaySession(ctx, connector.ReplaySessionRequest{SourceSessionID: quiet.SessionID, Agent: "child"}); err == nil || !strings.Contains(err.Error(), "retention_none_unsupported") {
		t.Errorf("replay of a no-trace session: err = %v, want it refused", err)
	}
	if _, err := srv.ReplaySession(ctx, connector.ReplaySessionRequest{SourceSessionID: plain.SessionID, Agent: "parent"}); err == nil || !strings.Contains(err.Error(), "retention_none_unsupported") {
		t.Errorf("replay into a no-trace agent: err = %v, want it refused", err)
	}

	// From inside a no-trace run: a team walk, and a resident child.
	inside := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_quiet", UserID: "alice", Retention: store.RetentionNone})
	if _, err := srv.admitTeamRun(inside); !errors.Is(err, runner.ErrRetentionUnsupported) {
		t.Errorf("a team walk from a no-trace run: err = %v, want ErrRetentionUnsupported", err)
	}
	if _, err := srv.prepareSubRun(inside, "child", "", "hi", "", true, func(providers.Event) {}); !errors.Is(err, runner.ErrRetentionUnsupported) {
		t.Errorf("a resident child of a no-trace run: err = %v, want ErrRetentionUnsupported", err)
	}

	if after := runsBefore(); after != before {
		t.Errorf("a refused request opened a session: %d before, %d after", before, after)
	}
}

func TestNoTrace_ARunStartedOffHTTPIsNoTraceWhenAskedOrWhenItsAgentIs(t *testing.T) {
	prov := &scriptedProvider{defaultS: []providers.Event{{Type: providers.EventText, Text: "answer " + canary}, spend("end_turn", 2)}}
	srv, dbPath := makeServer(t, prov, noTraceConfig())
	segs := []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "q " + canary}}}}
	var live strings.Builder
	err := srv.RunOnce(context.Background(), runner.RunInput{Agent: "child", AgentID: "a_asked", UserID: "alice", Retention: store.RetentionNone, Segments: segs},
		runner.RunCallbacks{OnEvent: func(ev providers.Event) { live.WriteString(ev.Text) }})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(live.String(), "answer "+canary) {
		t.Errorf("the caller's stream is missing the answer: %q", live.String())
	}
	if run := runOfAgent(t, srv, "a_asked"); run.Retention != store.RetentionNone {
		t.Errorf("retention = %q, want none", run.Retention)
	}
	if onDisk(t, srv, dbPath) {
		t.Error("the canary is on disk after a no-trace RunOnce")
	}
}

func TestEventMarker_KeepsWhatIsNotContent(t *testing.T) {
	ev := providers.Event{Type: providers.EventToolCall, Text: "secret text", IsError: true,
		ToolUse:    &providers.ToolUse{ID: "tu_9", Name: "Channel", Input: json.RawMessage(`{"op":"subscribe","kind":"k1","wait_ms":5000,"channel":"c","message":"secret body"}`)},
		Usage:      &providers.Usage{InputTokens: 4},
		StopReason: "end_turn", Reasoning: "secret reasoning", Error: "secret error",
	}
	payload, _ := json.Marshal(ev)
	marker := eventMarker("tool_call", payload)
	if bytes.Contains(marker, []byte("secret")) {
		t.Fatalf("a marker carries content: %s", marker)
	}
	var m eventMarkerRecord
	if err := json.Unmarshal(marker, &m); err != nil {
		t.Fatal(err)
	}
	if m.Retained || m.Bytes != len(payload) || m.Tool != "Channel" || !m.IsError || m.Usage == nil || m.Usage.InputTokens != 4 || m.StopReason != "end_turn" {
		t.Errorf("marker = %s", marker)
	}
	// The three fields the awaited-state derivation reads, and no others.
	if string(m.ToolUse.Input) != `{"op":"subscribe","kind":"k1","wait_ms":5000}` {
		t.Errorf("marker tool input = %s", m.ToolUse.Input)
	}
	// It still reads as an event to the readers that parse stored events.
	var back providers.Event
	if err := json.Unmarshal(marker, &back); err != nil || back.Type != providers.EventToolCall || back.ToolUse.Name != "Channel" || back.Text != "" {
		t.Errorf("a marker does not read back as an empty event: %+v (err %v)", back, err)
	}

	// A field that should hold a name cannot hold a sentence.
	sneaky, _ := json.Marshal(providers.Event{Type: providers.EventToolCall, StopReason: "the user said: secret",
		ToolUse: &providers.ToolUse{ID: "an id with secret words", Name: "secret tool name here", Input: json.RawMessage(`{"op":"secret op with spaces"}`)}})
	if got := eventMarker("tool_call", sneaky); bytes.Contains(got, []byte("secret")) {
		t.Errorf("a marker kept free text from a name field: %s", got)
	}
	// A payload that is not an event at all.
	if got := string(eventMarker("user_input", []byte(`[{"role":"user","content":[{"type":"trusted-text","text":"secret"}]}]`))); strings.Contains(got, "secret") || !strings.Contains(got, `"retained":false`) || !strings.Contains(got, `"type":"user_input"`) {
		t.Errorf("a non-event payload's marker = %s", got)
	}
}

func TestNoTraceFilter_ReducesTheFinishAndFailsClosed(t *testing.T) {
	if got := safeStopReason("its parent ended its turn (on_parent_end: cancel)"); got != "other" {
		t.Errorf("a free-text stop reason was kept: %q", got)
	}
	if got := safeStopReason("max_iterations"); got != "max_iterations" {
		t.Errorf("a stop reason from the list was changed: %q", got)
	}
	if got := safeErrorText("anthropic 400: " + canary); got != "error class: unclassified" {
		t.Errorf("an error text was kept: %q", got)
	}
	if got := safeErrorText(noTraceErrorText(runner.ErrTokenLimitExceeded)); got != "error class: business" {
		t.Errorf("a classified error lost its class: %q", got)
	}
	if got := string(notRetainedResult(json.RawMessage(`{"final_text":"` + canary + `","terminal":"done"}`))); got != `{"terminal":"done","content_retained":false}` {
		t.Errorf("result = %s", got)
	}

	// A run whose row cannot be read is treated as no-trace, and the answer
	// is not remembered.
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	f := srv.store.(*traceFilterStore)
	if !f.modes.noTrace(context.Background(), f.Store, "r_no_such_run") {
		t.Fatal("an unreadable run was treated as ordinary")
	}
	f.modes.mu.RLock()
	_, remembered := f.modes.known["r_no_such_run"]
	f.modes.mu.RUnlock()
	if remembered {
		t.Error("a failed lookup was remembered")
	}
	// A run of another process (nothing noted it here) is read from its row.
	sess, _ := f.Store.CreateSession(context.Background(), "", "default", "")
	run, err := f.Store.CreateRun(context.Background(), sess.ID, store.RunIdentity{AgentID: "a_elsewhere", Retention: store.RetentionNone})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AppendEvent(context.Background(), run.ID, "text", []byte(`{"type":"text","text":"`+canary+`"}`)); err != nil {
		t.Fatal(err)
	}
	events, _ := srv.store.GetTranscript(context.Background(), sess.ID)
	if len(events) != 1 || bytes.Contains(events[0].Payload, []byte(canary)) {
		t.Errorf("an event of a no-trace run created elsewhere was stored whole: %v", events)
	}
	// Its end goes through the filter as well, whoever writes it and with
	// whatever text: a path that has only a string still stores no content.
	if err := srv.store.FinishRun(context.Background(), run.ID, store.RunFailed, "the operator said "+canary,
		store.Usage{Result: json.RawMessage(`{"final_text":"` + canary + `"}`)}, "boom "+canary); err != nil {
		t.Fatal(err)
	}
	got, err := srv.store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StopReason != "other" || got.ErrorMsg != "error class: unclassified" || string(got.Result) != `{"content_retained":false}` {
		t.Errorf("finished row: stop=%q error=%q result=%s", got.StopReason, got.ErrorMsg, got.Result)
	}
}

// The filter is only as good as its coverage: every write of an event from
// this package has to go through the Server's store handle, which is the
// filter. The inner store is reachable in exactly one file.
func TestNoTraceFilter_NothingInThisPackageWritesAroundIt(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	if _, ok := srv.store.(*traceFilterStore); !ok {
		t.Fatalf("the Server's store is %T, not the filter", srv.store)
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// Either the filter's inner store, or the raw store New was handed.
	inner := regexp.MustCompile(`(\.Store|\bst)\.(AppendEvent|FinishRun|CreateRun)\(`)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "no_trace.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !inner.MatchString(line) {
				continue
			}
			t.Errorf("%s:%d reaches a store around the no-trace filter: %s", name, i+1, strings.TrimSpace(line))
		}
	}
}

// While a no-trace run is in flight: it cannot be sent a message or retuned
// (both are stored and relayed), and a pause does not park it — it is
// reported as unpausable and keeps running.
func TestNoTrace_ARunInFlightIsUnpausableAndTakesNoMessages(t *testing.T) {
	prov := &canaryHangProvider{started: make(chan struct{})}
	srv, _ := makeServer(t, prov, noTraceConfig())
	srv.noTraceAgents = map[string]bool{"parent": true}
	srv.SetSteerRegistry(steer.NewRegistry(8))
	mgr := pause.NewManager(srv.store, 2*time.Second)
	srv.SetPauseManager(mgr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- srv.RunOnce(ctx, runner.RunInput{Agent: "parent", AgentID: "a_parent", UserID: "alice", Segments: canarySegments()}, runner.RunCallbacks{})
	}()
	select {
	case <-prov.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the model call never started")
	}
	run := runOfAgent(t, srv, "a_parent")
	// Its row holds its mode and none of its settings.
	if string(run.RunConfig) != `{"retention":"none"}` {
		t.Errorf("run_config = %s, want only the mode", run.RunConfig)
	}

	if _, err := srv.SteerRun(context.Background(), run.ID, "a message "+canary, "api"); !errors.Is(err, runner.ErrRetentionUnsupported) {
		t.Errorf("a message to a no-trace run: err = %v, want ErrRetentionUnsupported", err)
	}
	if _, err := srv.retuneRun(context.Background(), run, &runOverridesWire{Model: "stub-model"}); !errors.Is(err, runner.ErrRetentionUnsupported) {
		t.Errorf("a retune of a no-trace run: err = %v, want ErrRetentionUnsupported", err)
	}

	// A pause returns without waiting for it, and says it left one running.
	start := time.Now()
	res, err := mgr.Pause(context.Background(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.UnpausableRunsCount != 1 || res.PausedRunsCount != 0 {
		t.Errorf("pause report: unpausable=%d paused=%d, want 1 and 0", res.UnpausableRunsCount, res.PausedRunsCount)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the pause waited %v for a run it does not park", d)
	}
	if got, _ := srv.store.GetRun(context.Background(), run.ID); got.Status != store.RunRunning || got.PauseState == store.PauseStatePaused {
		t.Errorf("the run under a pause: status=%q pause_state=%q, want it still running", got.Status, got.PauseState)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end after its cancel")
	}
	waitRunsTerminal(t, srv)
	if n := mgr.UnpausableRuns(); n != 0 {
		t.Errorf("unpausable runs after it ended = %d, want 0", n)
	}
}

// The run-state bus fans a run's end out to every replica and every stream
// watching its tenant. A no-trace run's text does not ride it.
func TestNoTrace_TheRunStateBusCarriesNoText(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), noTraceConfig())
	f := srv.store.(*traceFilterStore)
	sess, _ := f.Store.CreateSession(context.Background(), "", "parent", "alice")
	quiet, err := srv.store.CreateRun(context.Background(), sess.ID, store.RunIdentity{AgentID: "a_bus", Retention: store.RetentionNone})
	if err != nil {
		t.Fatal(err)
	}
	plainSess, _ := f.Store.CreateSession(context.Background(), "", "parent", "alice")
	plain, err := srv.store.CreateRun(context.Background(), plainSess.ID, store.RunIdentity{AgentID: "a_bus_plain"})
	if err != nil {
		t.Fatal(err)
	}
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)
	sub := bus.Subscribe("")
	defer sub.Close()

	srv.publishRunState(runStateMeta{RunID: quiet.ID}, "failed", "the operator said "+canary, "boom "+canary)
	srv.publishRunState(runStateMeta{RunID: plain.ID}, "failed", "max_iterations", "an ordinary error")
	for _, want := range []struct{ id, stop, errText string }{
		{quiet.ID, "other", "error class: unclassified"},
		{plain.ID, "max_iterations", "an ordinary error"},
	} {
		select {
		case ev := <-sub.C:
			if ev.RunID != want.id || ev.StopReason != want.stop || ev.Error != want.errText {
				t.Errorf("bus event = run %s stop %q error %q, want run %s stop %q error %q", ev.RunID, ev.StopReason, ev.Error, want.id, want.stop, want.errText)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no bus event")
		}
	}
}

// Context distillation can bank the turns it drops, for a later pass to read.
// A banked span is the conversation itself; a no-trace run banks none.
func TestNoTrace_ADistillationBanksNothing(t *testing.T) {
	srv, dbPath := makeServer(t, completingProvider(), noTraceConfig())
	on := true
	def := config.AgentDef{MemoryScopes: []string{"user"}, Compaction: &config.Compaction{MemoryFlush: &on}}
	dropped := []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "an old turn " + canary}}}}
	bank := func(retention string) string {
		t.Helper()
		sess, err := srv.store.CreateSession(context.Background(), "", "parent", "alice")
		if err != nil {
			t.Fatal(err)
		}
		run, err := srv.store.CreateRun(context.Background(), sess.ID, store.RunIdentity{AgentID: "a_bank_" + retention, UserID: "alice", Retention: retention})
		if err != nil {
			t.Fatal(err)
		}
		id, err := srv.bankCompactedSpanFn(def, false, "", "alice", "parent", run.ID, sess.ID)(context.Background(), dropped)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	if id := bank(store.RetentionNone); id != "" || onDisk(t, srv, dbPath) {
		t.Errorf("a no-trace run banked a span (id %q, canary on disk: %v)", id, onDisk(t, srv, dbPath))
	}
	// The same call for an ordinary run does bank, so the check above means something.
	if id := bank(""); id == "" || !onDisk(t, srv, dbPath) {
		t.Errorf("an ordinary run banked nothing (id %q): this test cannot see a banked span", id)
	}
}
