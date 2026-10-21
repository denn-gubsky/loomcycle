package http

// Golden SSE transcripts for the two HTTP run-start routes, POST /v1/runs and
// POST /v1/sessions/{id}/messages.
//
// WHY: both handlers keep their own inline copy of the run start that RunOnce
// does for every other transport. Making them call RunOnce (RFC ET P1) must not
// change what an HTTP client sees except where a difference is deliberate, so
// the wire is recorded here first: the status line, and for a stream every
// frame's event name and payload shape, in order. Ids, timestamps and
// durations are masked, so a transcript is stable across runs; everything else
// (keys, values, frame order) is compared byte for byte.
//
// Regenerate with: go test ./internal/api/http -run TestRunDispatchGolden -update

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

var updateGoldens = flag.Bool("update", false, "rewrite the golden SSE transcripts under testdata/run_dispatch_golden")

// goldenProvider answers by what the newest message asks for, so one provider
// serves parents, children and continuations without depending on call order:
//
//	a tool result         → "parent done", end_turn
//	"SPAWN"               → a call to the Agent tool for "child", tool_use
//	"HOLD"                → "held", then end_turn once the gate opens
//	"FAIL"                → a stream that closes without a done event
//	anything else         → "hello", end_turn
type goldenProvider struct {
	gate chan struct{}

	mu    sync.Mutex
	calls []string // each call's messages, as JSON
}

// sawCall returns the messages of the first call whose newest message
// contains marker.
func (p *goldenProvider) sawCall(marker string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.calls {
		if strings.Contains(c, marker) {
			return c, true
		}
	}
	return "", false
}

func (p *goldenProvider) ID() string                    { return "stub" }
func (p *goldenProvider) Probe(_ context.Context) error { return nil }
func (p *goldenProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *goldenProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *goldenProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	last := ""
	if n := len(req.Messages); n > 0 {
		b, _ := json.Marshal(req.Messages[n-1])
		last = string(b)
	}
	all, _ := json.Marshal(req.Messages)
	p.mu.Lock()
	p.calls = append(p.calls, string(all))
	p.mu.Unlock()
	done := func(stop string) providers.Event {
		return providers.Event{Type: providers.EventDone, StopReason: stop, Usage: &providers.Usage{InputTokens: 3, OutputTokens: 2}}
	}
	ch := make(chan providers.Event, 4)
	switch {
	case strings.Contains(last, `"tool_result"`):
		ch <- providers.Event{Type: providers.EventText, Text: "parent done"}
		ch <- done("end_turn")
	case strings.Contains(last, "SPAWN"):
		ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
			ID: "tu_golden_1", Name: "Agent", Input: json.RawMessage(`{"name":"child","prompt":"child task"}`),
		}}
		ch <- done("tool_use")
	case strings.Contains(last, "HOLD"):
		go func() {
			defer close(ch)
			ch <- providers.Event{Type: providers.EventText, Text: "held"}
			select {
			case <-p.gate:
				ch <- done("end_turn")
			case <-ctx.Done():
			}
		}()
		return ch, nil
	case strings.Contains(last, "FAIL"):
		// Closed with no done event: a call that never finished.
	default:
		ch <- providers.Event{Type: providers.EventText, Text: "hello"}
		ch <- done("end_turn")
	}
	close(ch)
	return ch, nil
}

const (
	goldenAdminBearer      = "golden-legacy-admin"
	goldenRestrictedBearer = "golden-tok-restricted" // may start only "plain"
	goldenRatedBearer      = "golden-tok-rated"      // one start, then none for a minute
	goldenMemberBearer     = "golden-tok-member"     // unrestricted member of "acme"
	goldenIsolatedBearer   = "golden-tok-isolated"   // isolated member "eve" of "acme": its own sessions only
)

type goldenServer struct {
	t    *testing.T
	srv  *Server
	ts   *httptest.Server
	st   store.Store
	gate chan struct{}
	prov *goldenProvider
	// bearer is sent on every request unless a call names another.
	bearer string
}

type goldenOpts struct {
	maxRuns int  // run slots; 0 = 4
	authed  bool // a legacy admin token plus member tokens, instead of open mode
}

func newGoldenServer(t *testing.T, o goldenOpts) *goldenServer {
	t.Helper()
	if o.maxRuns == 0 {
		o.maxRuns = 4
	}
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"plain":  {Model: "stub-model", SystemPrompt: "be brief"},
			"other":  {Model: "stub-model", SystemPrompt: "other"},
			"term":   {Model: "stub-model", SystemPrompt: "terminal", UnboundedIterations: true},
			"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are the parent"},
			"child":  {Model: "stub-model", SystemPrompt: "you are the child"},
		},
		// Configured so an unknown user_tier is refused (the check is skipped
		// when there is no user_tiers block). Every agent pins its model, so
		// the overlays change no routing.
		UserTiers:   map[string]config.UserTier{"default": {}, "pro": {}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: o.maxRuns, MaxQueueDepth: 8},
	}
	cfg.Env.AuthToken = ""
	if o.authed {
		cfg.Env.AuthToken = goldenAdminBearer
		cfg.Env.OperatorTokenPepper = testTokenPepper
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "golden.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if o.authed {
		member := []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}
		isolated := []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeUser}
		for _, tok := range []struct {
			id, bearer, subject, restrictions string
			scopes                            []string
		}{
			{"def_golden_restricted", goldenRestrictedBearer, "dev", `{"agents":["plain"]}`, member},
			{"def_golden_rated", goldenRatedBearer, "dev", `{"rate":{"per_minute":1,"burst":1}}`, member},
			{"def_golden_member", goldenMemberBearer, "dev", "", member},
			{"def_golden_isolated", goldenIsolatedBearer, "eve", "", isolated},
		} {
			if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
				DefID: tok.id, Name: "u-" + tok.id, TenantID: "acme", Subject: tok.subject,
				TokenHash: auth.HashToken(testTokenPepper, tok.bearer), AllowedScopes: tok.scopes,
				RestrictionsJSON: tok.restrictions,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	gate := make(chan struct{}, 8)
	sem := concurrency.New(o.maxRuns, 8, 0)
	prov := &goldenProvider{gate: gate}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, sem, st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	g := &goldenServer{t: t, srv: srv, ts: ts, st: st, gate: gate, prov: prov}
	if o.authed {
		g.bearer = goldenAdminBearer
	}
	return g
}

// goldenFrame is one SSE frame as it arrived.
type goldenFrame struct{ event, data string }

// goldenResp is one response: its status and headers, and either its frames
// (a stream) or its body.
type goldenResp struct {
	method, path string
	status       int
	header       http.Header
	body         string // a response that is not a stream

	mu     sync.Mutex
	frames []goldenFrame
	notify chan struct{}
	closed chan struct{}
	cancel context.CancelFunc
}

// post sends a request and returns once its headers arrive. A stream is read
// in the background; wait for frames with until, and for its end with drain.
func (g *goldenServer) post(path, body string) *goldenResp {
	return g.postAs(g.bearer, path, body)
}

func (g *goldenServer) postAs(bearer, path, body string) *goldenResp {
	g.t.Helper()
	return g.do(http.MethodPost, bearer, path, body)
}

// get sends a GET (a re-attach to a run's stream) as post sends a POST.
func (g *goldenServer) get(path string) *goldenResp {
	g.t.Helper()
	return g.do(http.MethodGet, g.bearer, path, "")
}

func (g *goldenServer) do(method, bearer, path, body string) *goldenResp {
	g.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, method, g.ts.URL+path, strings.NewReader(body))
	if err != nil {
		g.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 5 * time.Second}}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		g.t.Fatalf("%s %s: %v", method, path, err)
	}
	r := &goldenResp{method: method, path: path, status: resp.StatusCode, header: resp.Header,
		notify: make(chan struct{}, 1), closed: make(chan struct{}), cancel: cancel}
	g.t.Cleanup(cancel)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		r.body = string(b)
		close(r.closed)
		return r
	}
	go func() {
		defer close(r.closed)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		var ev, data string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev != "" || data != "" {
					r.mu.Lock()
					r.frames = append(r.frames, goldenFrame{ev, data})
					r.mu.Unlock()
					select {
					case r.notify <- struct{}{}:
					default:
					}
				}
				ev, data = "", ""
			case strings.HasPrefix(line, ":"):
				// A keepalive comment: not part of the transcript.
			case strings.HasPrefix(line, "event:"):
				ev = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}()
	return r
}

// until waits for the n-th frame (1-based) named event and returns its data.
func (r *goldenResp) until(t *testing.T, event string, n int) string {
	t.Helper()
	find := func() (string, bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		seen := 0
		for _, f := range r.frames {
			if f.event == event {
				seen++
				if seen == n {
					return f.data, true
				}
			}
		}
		return "", false
	}
	deadline := time.After(5 * time.Second)
	for {
		if data, ok := find(); ok {
			return data
		}
		select {
		case <-r.notify:
		case <-r.closed:
			// The reader appends every frame before it closes: one last look.
			if data, ok := find(); ok {
				return data
			}
			t.Fatalf("%s %s: stream ended before frame %q #%d; frames: %v", r.method, r.path, event, n, r.events())
		case <-deadline:
			t.Fatalf("%s %s: no frame %q #%d within 5s; frames: %v", r.method, r.path, event, n, r.events())
		}
	}
}

func (r *goldenResp) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.frames))
	for i, f := range r.frames {
		out[i] = f.event
	}
	return out
}

// drain waits for the response to end.
func (r *goldenResp) drain(t *testing.T) {
	t.Helper()
	select {
	case <-r.closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s %s: stream did not end within 5s; frames: %v", r.method, r.path, r.events())
	}
}

// ids reads the run's ids from its agent frame.
func (r *goldenResp) ids(t *testing.T) (agentID, runID, sessionID string) {
	t.Helper()
	var a struct {
		AgentID   string `json:"agent_id"`
		RunID     string `json:"run_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(r.until(t, "agent", 1)), &a); err != nil {
		t.Fatal(err)
	}
	return a.AgentID, a.RunID, a.SessionID
}

// goldenTranscript accumulates the normalized responses of one case.
type goldenTranscript struct {
	b  strings.Builder
	m  *goldenMasker
	st store.Store // set by the case once it has a server; nil records no stored state
}

func newGoldenTranscript() *goldenTranscript {
	return &goldenTranscript{m: &goldenMasker{labels: map[string]string{}}}
}

// add records a finished response under a label naming its role in the case.
func (tr *goldenTranscript) add(t *testing.T, label string, r *goldenResp) {
	t.Helper()
	r.drain(t)
	fmt.Fprintf(&tr.b, "== %s: %s %s\n", label, r.method, tr.m.text(r.path))
	fmt.Fprintf(&tr.b, "status: %d\n", r.status)
	ct := r.header.Get("Content-Type")
	fmt.Fprintf(&tr.b, "content-type: %s\n", ct)
	if ra := r.header.Get("Retry-After"); ra != "" {
		fmt.Fprintf(&tr.b, "retry-after: %s\n", ra)
	}
	if !strings.HasPrefix(ct, "text/event-stream") {
		fmt.Fprintf(&tr.b, "body: %s\n", tr.m.text(strings.TrimRight(r.body, "\n")))
		return
	}
	r.mu.Lock()
	frames := append([]goldenFrame(nil), r.frames...)
	r.mu.Unlock()
	runID, dedup := "", false
	for _, f := range frames {
		fmt.Fprintf(&tr.b, "event: %s %s\n", f.event, tr.m.data(f.data))
		if f.event == "agent" && runID == "" {
			var a struct {
				RunID        string `json:"run_id"`
				Deduplicated bool   `json:"deduplicated"`
			}
			_ = json.Unmarshal([]byte(f.data), &a)
			runID, dedup = a.RunID, a.Deduplicated
		}
	}
	if runID != "" && !dedup && tr.st != nil {
		tr.addStored(t, runID)
	}
}

// addStored records what the run left in the store once it has ended: its row
// and its events, so a change in what is persisted shows as plainly as one on
// the wire.
func (tr *goldenTranscript) addStored(t *testing.T, runID string) {
	t.Helper()
	ctx := context.Background()
	var run store.Run
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := tr.st.GetRun(ctx, runID)
		if err == nil && r.Status != store.RunRunning && r.Status != store.RunQueued {
			run = r
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not end within 5s (status %q, err %v)", runID, r.Status, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&tr.b, "stored row: %s\n", tr.m.data(string(b)))
	events, err := tr.st.GetRunEventsSince(ctx, runID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		fmt.Fprintf(&tr.b, "stored event: %s %s\n", ev.Type, tr.m.data(string(ev.Payload)))
	}
}

func (tr *goldenTranscript) check(t *testing.T, name string) {
	t.Helper()
	path := filepath.Join("testdata", "run_dispatch_golden", name+".golden")
	got := tr.b.String()
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with -update)", path, err)
	}
	if got != string(want) {
		t.Errorf("transcript differs from %s (regenerate with -update if the change is intended)\n%s", path, goldenDiff(string(want), got))
	}
}

// goldenDiff is a line diff good enough to read a transcript change.
func goldenDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			fmt.Fprintf(&b, "line %d:\n  - %s\n  + %s\n", i+1, wl, gl)
		}
	}
	return b.String()
}

// goldenMasker replaces what differs between two runs of the same case with
// stable placeholders. An id becomes <id:N>, numbered by first appearance in
// the case, so two mentions of one run still read as the same run.
type goldenMasker struct {
	labels map[string]string
}

var (
	goldenIDRe   = regexp.MustCompile(`\b[a-z]{1,8}_[0-9a-f]{12,}\b`)
	goldenUUIDRe = regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	goldenTimeRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
	// goldenDurRe masks a Go duration in prose ("retry in 59.9s").
	goldenDurRe = regexp.MustCompile(`\b\d+(\.\d+)?(ns|µs|ms|s|m|h)\b`)
)

func (m *goldenMasker) text(s string) string {
	s = goldenTimeRe.ReplaceAllString(s, "<time>")
	mask := func(id string) string {
		if l, ok := m.labels[id]; ok {
			return l
		}
		l := fmt.Sprintf("<id:%d>", len(m.labels)+1)
		m.labels[id] = l
		return l
	}
	s = goldenIDRe.ReplaceAllStringFunc(s, mask)
	s = goldenUUIDRe.ReplaceAllStringFunc(s, mask)
	return goldenDurRe.ReplaceAllString(s, "<dur>")
}

// goldenVolatileKey reports whether a JSON key holds a time or a duration,
// which a transcript leaves out.
func goldenVolatileKey(k string) bool {
	switch k {
	case "ts", "time", "timestamp", "elapsed", "duration", "retry_after", "queue_position":
		return true
	}
	return strings.HasSuffix(k, "_ms") || strings.HasSuffix(k, "_at") || strings.HasSuffix(k, "_seconds_left")
}

func (m *goldenMasker) value(k string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for kk := range x {
			keys = append(keys, kk)
		}
		sort.Strings(keys) // ids are numbered in a stable key order
		out := make(map[string]any, len(x))
		for _, kk := range keys {
			// Dropped, not masked: a duration is omitted when it rounds to 0,
			// so even its presence varies from run to run.
			if goldenVolatileKey(kk) {
				continue
			}
			out[kk] = m.value(kk, x[kk])
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = m.value(k, e)
		}
		return out
	case string:
		return m.text(x)
	}
	return v
}

func (m *goldenMasker) data(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return m.text(s)
	}
	// Unescaped, so a masked id reads as <id:1> and not \u003cid:1\u003e.
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m.value("", v)); err != nil {
		return m.text(s)
	}
	return strings.TrimRight(b.String(), "\n")
}

func goldenRunBody(agent, prompt, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"agent":%q,"segments":[{"role":"user","content":[{"type":"trusted-text","text":%q}]}]%s}`, agent, prompt, extra)
}

func goldenMsgBody(prompt, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"segments":[{"role":"user","content":[{"type":"trusted-text","text":%q}]}]%s}`, prompt, extra)
}

// completedSession runs agent to its end through POST /v1/runs and returns
// its session id, for a continuation to continue. Not part of any transcript.
func (g *goldenServer) completedSession(agent string) string {
	g.t.Helper()
	r := g.post("/v1/runs", goldenRunBody(agent, "first turn", ""))
	if r.status != http.StatusOK {
		g.t.Fatalf("seed run: status %d: %s", r.status, r.body)
	}
	_, runID, sessionID := r.ids(g.t)
	r.drain(g.t)
	waitRunStatus(g.t, g.st, runID, store.RunCompleted)
	return sessionID
}

// holdSlot starts a run that keeps its run slot until one gate token is sent.
func (g *goldenServer) holdSlot() *goldenResp {
	g.t.Helper()
	r := g.post("/v1/runs", goldenRunBody("plain", "HOLD the slot", ""))
	r.until(g.t, "text", 1)
	return r
}

func TestRunDispatchGolden(t *testing.T) {
	cases := map[string]func(t *testing.T, tr *goldenTranscript){
		// ---- POST /v1/runs ----
		"runs_plain": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			tr.add(t, "run", g.post("/v1/runs", goldenRunBody("plain", "hi", `"user_id":"u1","agent_id":"a_golden_plain"`)))
		},
		"runs_prompt_sugar_and_metadata": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			tr.add(t, "run", g.post("/v1/runs", `{"agent":"plain","prompt":"hi","metadata":{"repo":"x"},"parent_context":{"function_key":"fk"}}`))
		},
		"runs_provider_failure": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			tr.add(t, "run", g.post("/v1/runs", goldenRunBody("plain", "FAIL now", "")))
		},
		"runs_subagent": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			tr.add(t, "run", g.post("/v1/runs", goldenRunBody("parent", "SPAWN a child", "")))
		},
		"runs_interactive": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			r := g.post("/v1/runs", goldenRunBody("term", "hi", `"interactive":true`))
			agentID, runID, _ := r.ids(t)
			r.until(t, "awaiting_input", 1)
			in := g.post("/v1/runs/"+runID+"/input", `{"text":"keep going"}`)
			r.until(t, "awaiting_input", 2)
			cancel := g.post("/v1/agents/"+agentID+"/cancel", "")
			tr.add(t, "run", r)
			tr.add(t, "input", in)
			tr.add(t, "cancel", cancel)
		},
		"runs_review_detached": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			r := g.post("/v1/runs", goldenRunBody("plain", "write it", `"review":true`))
			_, runID, _ := r.ids(t)
			r.until(t, "awaiting_review", 1)
			v := g.post("/v1/runs/"+runID+"/review", `{"decision":"approve"}`)
			tr.add(t, "run", r)
			tr.add(t, "verdict", v)
		},
		"runs_queued": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{maxRuns: 1})
			tr.st = g.st
			holder := g.holdSlot()
			r := g.post("/v1/runs", goldenRunBody("plain", "queued run", ""))
			r.until(t, "queued", 1)
			g.gate <- struct{}{}
			holder.drain(t)
			r.until(t, "done", 1)
			tr.add(t, "queued run", r)
		},
		"runs_admission_reject": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{maxRuns: 1})
			tr.st = g.st
			holder := g.holdSlot()
			tr.add(t, "rejected", g.post("/v1/runs", goldenRunBody("plain", "no room", `"admission":"reject"`)))
			g.gate <- struct{}{}
			holder.drain(t)
		},
		"runs_idempotent_replay": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			body := goldenRunBody("plain", "hi", `"user_id":"u1","idempotency_key":"golden-key-1"`)
			first := g.post("/v1/runs", body)
			_, runID, _ := first.ids(t)
			first.drain(t)
			waitRunStatus(t, g.st, runID, store.RunCompleted)
			tr.add(t, "first", first)
			tr.add(t, "replay", g.post("/v1/runs", body))
		},
		"runs_with_session_id": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			sid := g.completedSession("plain")
			tr.add(t, "run in session", g.post("/v1/runs", goldenRunBody("plain", "again", `"session_id":"`+sid+`"`)))
			tr.add(t, "another agent in the session", g.post("/v1/runs", goldenRunBody("other", "again", `"session_id":"`+sid+`"`)))
		},
		"runs_refusals": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			for _, c := range []struct{ label, body string }{
				{"bad json", `{"agent":`},
				{"no agent", `{"prompt":"hi"}`},
				{"no input", `{"agent":"plain"}`},
				{"bad block type", `{"agent":"plain","segments":[{"role":"user","content":[{"type":"trusted-txt","text":"hi"}]}]}`},
				{"bad user_id", goldenRunBody("plain", "hi", `"user_id":"bad id!"`)},
				{"bad agent_id", goldenRunBody("plain", "hi", `"agent_id":"bad id!"`)},
				{"unknown agent", goldenRunBody("nope", "hi", "")},
				{"bad admission", goldenRunBody("plain", "hi", `"admission":"maybe"`)},
				{"bad retention", goldenRunBody("plain", "hi", `"retention":"forever"`)},
				{"bad max_wall_seconds", goldenRunBody("plain", "hi", `"max_wall_seconds":-1`)},
				{"bad user_bearer", goldenRunBody("plain", "hi", `"user_bearer":"short"`)},
				{"bad user_credentials", goldenRunBody("plain", "hi", `"user_credentials":{"bad key!":"v"}`)},
				{"bad idempotency_key", goldenRunBody("plain", "hi", `"idempotency_key":"bad key!"`)},
				{"idempotency_key with session", goldenRunBody("plain", "hi", `"idempotency_key":"k1","session_id":"s_none"`)},
				{"unknown session", goldenRunBody("plain", "hi", `"session_id":"s_0123456789abcdef"`)},
				{"bad tool_choice", goldenRunBody("plain", "hi", `"tool_choice":{"mode":"sometimes"}`)},
				{"unknown user_tier", goldenRunBody("plain", "hi", `"user_tier":"gold"`)},
				{"known user_tier", goldenRunBody("plain", "hi", `"user_tier":"pro"`)},
				{"no-trace interactive", goldenRunBody("term", "hi", `"retention":"none","interactive":true`)},
			} {
				tr.add(t, c.label, g.post("/v1/runs", c.body))
			}
		},
		"runs_refused_budget": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			goldenHardBudget(t, g)
			tr.add(t, "over budget", g.post("/v1/runs", goldenRunBody("plain", "hi", "")))
		},
		"runs_refused_paused": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			goldenPause(t, g)
			tr.add(t, "paused", g.post("/v1/runs", goldenRunBody("plain", "hi", "")))
		},
		"runs_refused_restricted_agent": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{authed: true})
			tr.st = g.st
			tr.add(t, "other agent", g.postAs(goldenRestrictedBearer, "/v1/runs", goldenRunBody("other", "hi", "")))
			tr.add(t, "unknown agent", g.postAs(goldenRestrictedBearer, "/v1/runs", goldenRunBody("nope", "hi", "")))
		},
		"runs_refused_rate": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{authed: true})
			tr.st = g.st
			tr.add(t, "first", g.postAs(goldenRatedBearer, "/v1/runs", goldenRunBody("plain", "hi", "")))
			tr.add(t, "second", g.postAs(goldenRatedBearer, "/v1/runs", goldenRunBody("plain", "hi", "")))
		},
		"runs_member_identity": func(t *testing.T, tr *goldenTranscript) {
			// A real principal: its tenant and subject replace the wire's.
			g := newGoldenServer(t, goldenOpts{authed: true})
			tr.st = g.st
			tr.add(t, "run", g.postAs(goldenMemberBearer, "/v1/runs", goldenRunBody("plain", "hi", `"user_id":"someone-else","tenant_id":"other"`)))
		},

		// ---- POST /v1/sessions/{id}/messages ----
		"messages_continue": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			sid := g.completedSession("plain")
			tr.add(t, "continuation", g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("second turn", `"agent_id":"a_golden_cont"`)))
		},
		"messages_provider_failure": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			sid := g.completedSession("plain")
			tr.add(t, "continuation", g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("FAIL now", "")))
		},
		"messages_subagent": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			sid := g.completedSession("parent")
			tr.add(t, "continuation", g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("SPAWN a child", "")))
		},
		"messages_interactive": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			sid := g.completedSession("term")
			r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("hi again", `"interactive":true`))
			agentID, runID, _ := r.ids(t)
			r.until(t, "awaiting_input", 1)
			in := g.post("/v1/runs/"+runID+"/input", `{"text":"keep going"}`)
			r.until(t, "awaiting_input", 2)
			cancel := g.post("/v1/agents/"+agentID+"/cancel", "")
			tr.add(t, "continuation", r)
			tr.add(t, "input", in)
			tr.add(t, "cancel", cancel)
		},
		"messages_review": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			sid := g.completedSession("plain")
			r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("revise it", `"review":true`))
			_, runID, _ := r.ids(t)
			r.until(t, "awaiting_review", 1)
			v := g.post("/v1/runs/"+runID+"/review", `{"decision":"approve"}`)
			tr.add(t, "continuation", r)
			tr.add(t, "verdict", v)
		},
		"messages_queued": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{maxRuns: 1})
			tr.st = g.st
			sid := g.completedSession("plain")
			holder := g.holdSlot()
			r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("queued turn", ""))
			r.until(t, "queued", 1)
			g.gate <- struct{}{}
			holder.drain(t)
			r.until(t, "done", 1)
			tr.add(t, "queued continuation", r)
		},
		"messages_session_busy": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			sid := g.completedSession("plain")
			busy := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("HOLD the session", ""))
			busy.until(t, "text", 1)
			tr.add(t, "second turn", g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("me too", "")))
			g.gate <- struct{}{}
			busy.drain(t)
		},
		"messages_refusals": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			sid := g.completedSession("plain")
			for _, c := range []struct{ label, path, body string }{
				{"unknown session", "/v1/sessions/s_0123456789abcdef/messages", goldenMsgBody("hi", "")},
				{"bad json", "/v1/sessions/" + sid + "/messages", `{"segments":`},
				{"bad block type", "/v1/sessions/" + sid + "/messages", `{"segments":[{"role":"user","content":[{"type":"trusted-txt","text":"hi"}]}]}`},
				{"bad agent_id", "/v1/sessions/" + sid + "/messages", goldenMsgBody("hi", `"agent_id":"bad id!"`)},
				{"bad user_tier", "/v1/sessions/" + sid + "/messages", goldenMsgBody("hi", `"user_tier":"gold"`)},
				{"bad user_bearer", "/v1/sessions/" + sid + "/messages", goldenMsgBody("hi", `"user_bearer":"short"`)},
				{"bad tool_choice", "/v1/sessions/" + sid + "/messages", goldenMsgBody("hi", `"tool_choice":{"mode":"sometimes"}`)},
				{"empty segments", "/v1/sessions/" + sid + "/messages", `{"segments":[]}`},
			} {
				tr.add(t, c.label, g.post(c.path, c.body))
			}
		},
		"messages_refused_budget": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			sid := g.completedSession("plain")
			goldenHardBudget(t, g)
			tr.add(t, "over budget", g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("hi", "")))
		},
		"messages_refused_paused": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{})
			tr.st = g.st
			sid := g.completedSession("plain")
			goldenPause(t, g)
			tr.add(t, "paused", g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("hi", "")))
		},
		"messages_refused_restricted_agent": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{authed: true})
			tr.st = g.st
			r := g.postAs(goldenMemberBearer, "/v1/runs", goldenRunBody("other", "first turn", ""))
			_, _, sid := r.ids(t)
			r.drain(t)
			tr.add(t, "continue other", g.postAs(goldenRestrictedBearer, "/v1/sessions/"+sid+"/messages", goldenMsgBody("hi", "")))
		},
		"messages_refused_rate": func(t *testing.T, tr *goldenTranscript) {
			g := newGoldenServer(t, goldenOpts{authed: true})
			tr.st = g.st
			r := g.postAs(goldenMemberBearer, "/v1/runs", goldenRunBody("plain", "first turn", ""))
			_, _, sid := r.ids(t)
			r.drain(t)
			tr.add(t, "first", g.postAs(goldenRatedBearer, "/v1/sessions/"+sid+"/messages", goldenMsgBody("hi", "")))
			tr.add(t, "second", g.postAs(goldenRatedBearer, "/v1/sessions/"+sid+"/messages", goldenMsgBody("hi", "")))
		},
	}
	names := make([]string, 0, len(cases))
	for n := range cases {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			tr := newGoldenTranscript()
			cases[name](t, tr)
			tr.check(t, name)
		})
	}
}

// goldenHardBudget puts a hard operator budget of 0 tokens in place: every new
// run is over it.
func goldenHardBudget(t *testing.T, g *goldenServer) {
	t.Helper()
	ctx := context.Background()
	if err := g.st.TokenLimitPut(ctx, store.TokenLimitRow{Scope: "operator", HardLimit: i64p(0), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := g.srv.SeedLimits(ctx); err != nil {
		t.Fatal(err)
	}
}

// goldenPause pauses the runtime: no new run is admitted.
func goldenPause(t *testing.T, g *goldenServer) {
	t.Helper()
	mgr := pause.NewManager(g.st, 200*time.Millisecond)
	g.srv.SetPauseManager(mgr)
	if _, err := mgr.Pause(context.Background(), 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
}
