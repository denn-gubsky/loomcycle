package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A live sub-run is bounded by its PARENT as well as by its own definition: it
// sees at most the parent's volumes, reaches at most the parent's hosts, and
// fans out no wider than its ancestors allow. A resumed sub-run has no parent
// context to read any of that from. These tests spawn a real child under a
// narrowing parent, pause it, resume it, and check what its tools actually do.

// roleProvider scripts each agent separately, keyed by a "role:<name>" marker
// in its system prompt, so concurrent children cannot take each other's turns.
type roleProvider struct {
	mu      sync.Mutex
	roles   []string
	scripts map[string][][]providers.Event
	calls   map[string]int
	onCall  func(role string) // runs before the call is answered; may block
}

func (p *roleProvider) ID() string                    { return "scripted" }
func (p *roleProvider) Probe(_ context.Context) error { return nil }
func (p *roleProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *roleProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *roleProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	var sys strings.Builder
	for _, b := range req.System {
		sys.WriteString(b.Text)
	}
	role := ""
	for _, r := range p.roles {
		if strings.Contains(sys.String(), "role:"+r+".") {
			role = r
		}
	}
	if p.onCall != nil {
		p.onCall(role)
	}
	p.mu.Lock()
	idx := p.calls[role]
	p.calls[role]++
	events := endTurn()
	if s := p.scripts[role]; idx < len(s) {
		events = s[idx]
	}
	p.mu.Unlock()
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func toolCallTurn(id, name, input string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
}

// spawnCeilingServer serves cfg with the given tools over a real store.
func spawnCeilingServer(t *testing.T, cfg *config.Config, prov providers.Provider, toolset []tools.Tool) (*Server, store.Store, *httptest.Server) {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "spawn_ceiling.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, toolset, concurrency.New(8, 8, 5*time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return srv, st, ts
}

// spawnLiveChild posts one run of "parent" (agent id parentAgentID) whose first
// turn spawns "child", and returns the child's row once the whole tree is done.
// extra is spliced into the request body (e.g. an allowed_hosts list).
func spawnLiveChild(t *testing.T, st store.Store, ts *httptest.Server, parentAgentID, extra string) store.Run {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","agent_id":"`+parentAgentID+`"`+extra+
			`,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	children, err := st.ListRunsByParentAgentID(context.Background(), parentAgentID)
	if err != nil || len(children) != 1 {
		t.Fatalf("children of %s: %d, %v", parentAgentID, len(children), err)
	}
	return children[0]
}

// resumeAndFinish parks run as a pause would, resumes it, and waits for the
// resumed goroutine to end. Its row finished once already (live), so status
// cannot say when the RESUMED loop is done; leaving the cancel registry can —
// resume registers the run before it returns.
func resumeAndFinish(t *testing.T, srv *Server, run store.Run) string {
	t.Helper()
	parkForResume(t, srv, run.ID)
	resumeOne(t, srv)
	waitFor(t, "the resumed run to finish", func() bool {
		_, live := srv.cancelReg.Get(run.AgentID)
		return !live
	})
	return runTranscriptText(t, srv.store, run.SessionID, run.ID)
}

// resumedToolResult returns the payload of the tool_result for toolUseID.
func resumedToolResult(t *testing.T, srv *Server, run store.Run, toolUseID string) string {
	t.Helper()
	events, err := srv.store.GetTranscript(context.Background(), run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.RunID == run.ID && e.Type == "tool_result" && strings.Contains(string(e.Payload), toolUseID) {
			return string(e.Payload)
		}
	}
	t.Fatalf("no tool_result for %s in run %s", toolUseID, run.ID)
	return ""
}

// loopbackTarget is a server the operator's static list allows (127.0.0.1)
// and the parent's caller list does not.
func loopbackTarget(t *testing.T) *httptest.Server {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("child-reached-the-floor-host"))
	}))
	t.Cleanup(target.Close)
	return target
}

// hostCeilingSetup is intersect mode with an operator floor of 127.0.0.1: a
// run with no caller list reaches the loopback target, a run narrowed to
// another host does not.
func hostCeilingSetup(t *testing.T, targetURL string) (*config.Config, *roleProvider, []tools.Tool) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Env.HTTPCallerAuthoritative = false
	cfg.Env.HTTPHostAllowlist = []string{"127.0.0.1"}
	cfg.Env.HTTPPrivateHostAllowlist = []string{"127.0.0.1"} // dial-layer loopback exemption
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "role:parent."},
		"child":  {Model: "stub-model", Tools: []string{"HTTP"}, SystemPrompt: "role:child."},
	}
	prov := &roleProvider{
		roles: []string{"parent", "child"},
		scripts: map[string][][]providers.Event{
			"parent": {toolCallTurn("tu_spawn", "Agent", `{"name":"child","prompt":"hi"}`)},
			// live: nothing to do; resumed: try the floor host.
			"child": {endTurn(), toolCallTurn("tu_get", "HTTP", `{"method":"GET","url":"`+targetURL+`"}`)},
		},
		calls: map[string]int{},
	}
	httpTool := &builtin.HTTP{HostAllowlist: cfg.Env.HTTPHostAllowlist, PrivateHostAllowlist: cfg.Env.HTTPPrivateHostAllowlist}
	return cfg, prov, []tools.Tool{httpTool}
}

// Pins the carry #1253 already made: the child's record holds its parent's
// caller list, and a resumed child is narrowed by it rather than falling back
// to the wider operator floor.
func TestResumedChild_StaysWithinItsParentsCallerHosts(t *testing.T) {
	target := loopbackTarget(t)
	cfg, prov, toolset := hostCeilingSetup(t, target.URL)
	srv, st, ts := spawnCeilingServer(t, cfg, prov, toolset)

	child := spawnLiveChild(t, st, ts, "a_host_parent", `,"allowed_hosts":["api.example.com"]`)
	transcript := resumeAndFinish(t, srv, child)

	if strings.Contains(transcript, "child-reached-the-floor-host") {
		t.Fatalf("the resumed child reached a host its parent's caller list excluded:\n%s", transcript)
	}
	if res := resumedToolResult(t, srv, child, "tu_get"); !strings.Contains(res, "allowlist") {
		t.Errorf("resumed child's HTTP call was not refused by the host allowlist: %s", res)
	}
}

// volumeCeilingSetup has a read-only "docs" volume, a read-write "scratch"
// volume, and a read-write operator "default" volume. The parent is bound to
// docs only.
func volumeCeilingSetup(t *testing.T, childVolumes []string, childResumedTurns ...[]providers.Event) (*config.Config, *roleProvider, map[string]string) {
	t.Helper()
	dirs := map[string]string{}
	for _, name := range []string{"docs", "scratch", "default"} {
		d, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		dirs[name] = d
	}
	if err := os.WriteFile(filepath.Join(dirs["docs"], "notes.txt"), []byte("docs-volume-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Volumes = map[string]config.Volume{
		"docs":    {Path: dirs["docs"], Mode: "ro"},
		"scratch": {Path: dirs["scratch"], Mode: "rw"},
		"default": {Path: dirs["default"], Mode: "rw"},
	}
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "role:parent.", Volumes: []string{"docs"}},
		"child":  {Model: "stub-model", Tools: []string{"Write", "Read"}, SystemPrompt: "role:child.", Volumes: childVolumes},
	}
	prov := &roleProvider{
		roles: []string{"parent", "child"},
		scripts: map[string][][]providers.Event{
			"parent": {toolCallTurn("tu_spawn", "Agent", `{"name":"child","prompt":"hi"}`)},
			"child":  append([][]providers.Event{endTurn()}, childResumedTurns...),
		},
		calls: map[string]int{},
	}
	return cfg, prov, dirs
}

// assertNoFileAnywhere fails if out.txt landed in any of the volume dirs.
func assertNoFileAnywhere(t *testing.T, dirs map[string]string) {
	t.Helper()
	for name, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, "out.txt")); err == nil {
			t.Errorf("the resumed child wrote out.txt into the %q volume", name)
		}
	}
}

func TestResumedChild_StaysWithinItsParentsVolumes(t *testing.T) {
	cases := []struct {
		name         string
		childVolumes []string
		write        string
		refusal      string
	}{{
		// An undeclared child works inside its parent's volumes, so it is
		// read-only here; on its own it would bind the read-write default.
		name:    "inherits the parent's read-only binding",
		write:   `{"path":"out.txt","content":"x"}`,
		refusal: "read-only",
	}, {
		// A child's own binding the parent lacks is dropped live.
		name:         "a volume the parent lacked stays dropped",
		childVolumes: []string{"docs", "scratch"},
		write:        `{"path":"out.txt","content":"x","volume":"scratch"}`,
		refusal:      "scratch",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, prov, dirs := volumeCeilingSetup(t, tc.childVolumes,
				toolCallTurn("tu_write", "Write", tc.write),
				// What the parent DID allow still works: the ceiling was
				// carried, not replaced by "no volumes at all".
				toolCallTurn("tu_read", "Read", `{"path":"notes.txt","volume":"docs"}`))
			srv, st, ts := spawnCeilingServer(t, cfg, prov, []tools.Tool{&builtin.Write{}, &builtin.Read{}})

			child := spawnLiveChild(t, st, ts, "a_vol_parent", "")
			resumeAndFinish(t, srv, child)

			assertNoFileAnywhere(t, dirs)
			res := resumedToolResult(t, srv, child, "tu_write")
			if !strings.Contains(res, `"is_error":true`) || !strings.Contains(res, tc.refusal) {
				t.Errorf("resumed child's write was not refused (want %q): %s", tc.refusal, res)
			}
			if res := resumedToolResult(t, srv, child, "tu_read"); !strings.Contains(res, "docs-volume-content") {
				t.Errorf("resumed child lost the parent's docs volume it could read live: %s", res)
			}
		})
	}
}

// A live child runs at the fan-out width it inherits from its ancestors, not
// its own definition's. Measured on the wire: how many grandchild model calls
// are in flight at once, live and after the resume.
func TestResumedChild_FansOutNoWiderThanItDidLive(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Agents = map[string]config.AgentDef{
		"parent":     {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "role:parent.", MaxConcurrentChildren: 1},
		"child":      {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "role:child.", MaxConcurrentChildren: 4},
		"grandchild": {Model: "stub-model", Tools: []string{}, SystemPrompt: "role:grandchild."},
	}
	fan := toolCallTurn("tu_fan", "Agent",
		`{"op":"parallel_spawn","spawns":[{"name":"grandchild","prompt":"a"},{"name":"grandchild","prompt":"b"}]}`)

	var mu sync.Mutex
	inFlight, peak := 0, 0
	prov := &roleProvider{
		roles: []string{"parent", "child", "grandchild"},
		scripts: map[string][][]providers.Event{
			"parent": {toolCallTurn("tu_spawn", "Agent", `{"name":"child","prompt":"hi"}`)},
			"child":  {fan, endTurn(), fan, endTurn()},
		},
		calls: map[string]int{},
		// Each grandchild call waits a moment for a sibling to join it, so a
		// width of two is observed whenever it is permitted.
		onCall: func(role string) {
			if role != "grandchild" {
				return
			}
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()
			deadline := time.Now().Add(300 * time.Millisecond)
			for time.Now().Before(deadline) {
				mu.Lock()
				joined := inFlight >= 2
				mu.Unlock()
				if joined {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			mu.Lock()
			inFlight--
			mu.Unlock()
		},
	}
	peakSince := func() int {
		mu.Lock()
		defer mu.Unlock()
		p := peak
		peak = 0
		return p
	}
	srv, st, ts := spawnCeilingServer(t, cfg, prov, nil)

	child := spawnLiveChild(t, st, ts, "a_fan_parent", "")
	if live := peakSince(); live != 1 {
		t.Fatalf("live child's grandchildren peaked at %d in flight, want the parent's width 1", live)
	}
	resumeAndFinish(t, srv, child)
	if resumed := peakSince(); resumed != 1 {
		t.Errorf("resumed child's grandchildren peaked at %d in flight, want the width it had live (1)", resumed)
	}
}

// A sub-run recorded before its ceiling was: nobody can say what its parent
// allowed, so it resumes on the narrowest value rather than its definition's.
// The top-level run beside it, with the same definition and no record either,
// keeps its definition's reach — this is not a blanket downgrade.
func TestResumedChild_WithNoRecordedCeilingFailsClosed(t *testing.T) {
	t.Run("hosts", func(t *testing.T) {
		target := loopbackTarget(t)
		for _, sub := range []bool{true, false} {
			cfg, prov, toolset := hostCeilingSetup(t, target.URL)
			// Straight to the resumed turn: these rows had no live phase here.
			prov.scripts["child"] = prov.scripts["child"][1:]
			srv, _ := makeServerWithTools(t, prov, cfg, toolset)
			ident := store.RunIdentity{AgentID: "a_legacy"}
			if sub {
				ident.ParentAgentID = "a_gone_parent"
			}
			run := createCeilingRun(t, srv, "child", ident)
			transcript := resumeAndFinish(t, srv, run)
			reached := strings.Contains(transcript, "child-reached-the-floor-host")
			if sub && reached {
				t.Errorf("a sub-run with no record resumed on the operator floor and reached its host:\n%s", transcript)
			}
			if !sub && !reached {
				t.Errorf("a top-level run with no record lost its definition's reach:\n%s", transcript)
			}
		}
	})
	t.Run("volumes", func(t *testing.T) {
		for _, sub := range []bool{true, false} {
			cfg, prov, dirs := volumeCeilingSetup(t, []string{"scratch"},
				toolCallTurn("tu_write", "Write", `{"path":"out.txt","content":"x","volume":"scratch"}`))
			prov.scripts["child"] = prov.scripts["child"][1:]
			srv, _ := makeServerWithTools(t, prov, cfg, []tools.Tool{&builtin.Write{}})
			ident := store.RunIdentity{AgentID: "a_legacy"}
			if sub {
				ident.ParentAgentID = "a_gone_parent"
			}
			run := createCeilingRun(t, srv, "child", ident)
			resumeAndFinish(t, srv, run)
			_, err := os.Stat(filepath.Join(dirs["scratch"], "out.txt"))
			if sub && err == nil {
				t.Error("a sub-run with no recorded ceiling resumed on its definition's read-write volume and wrote to it")
			}
			if !sub && err != nil {
				t.Errorf("a top-level run with no record lost its definition's volume: %v", err)
			}
		}
	})
}

// The fan-out width is the third bound a sub-run's record carries, and it
// fails closed like the other two: a sub-run whose record has no spawn entry
// resumes serial, not on its definition's width of 16. A recorded width and a
// top-level run's definition are left as they were. Measured on the wire: the
// peak number of grandchild model calls in flight during the resumed fan-out.
//
// The grandchild calls meet at a barrier so the peak does not depend on how
// fast a loaded box starts them. A width of N is proven by holding each call
// until N are in flight, with a timeout generous enough never to fire when N
// are permitted. Serial is proven the other way: each call waits briefly for
// all three, which a missing cap would let arrive, and must still be alone.
func TestResumedChild_FanOutWidthFailsClosedWithoutASpawnRecord(t *testing.T) {
	cases := []struct {
		name      string
		sub       bool
		runConfig json.RawMessage
		want      int
	}{{
		name:      "sub-run whose record predates the spawn entry resumes serial",
		sub:       true,
		runConfig: runConfigRecord{RunTimeoutSeconds: 600}.marshal(),
		want:      1,
	}, {
		name: "sub-run with no record at all resumes serial",
		sub:  true,
		want: 1,
	}, {
		name:      "sub-run with a recorded width keeps it",
		sub:       true,
		runConfig: runConfigRecord{Spawn: spawnRecordOf(tools.VolumePolicyValue{}, 2)}.marshal(),
		want:      2,
	}, {
		name:      "top-level run keeps its definition's width",
		runConfig: runConfigRecord{RunTimeoutSeconds: 600}.marshal(),
		want:      3,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := makeBaseConfig()
			cfg.Defaults.Provider = "scripted"
			cfg.Agents = map[string]config.AgentDef{
				"child":      {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "role:child.", MaxConcurrentChildren: 16},
				"grandchild": {Model: "stub-model", Tools: []string{}, SystemPrompt: "role:grandchild."},
			}
			probe := newFanWidthProbe(tc.want, 10*time.Second)
			if tc.want == 1 {
				probe = newFanWidthProbe(3, 500*time.Millisecond)
			}
			prov := &roleProvider{
				roles: []string{"child", "grandchild"},
				scripts: map[string][][]providers.Event{
					// Straight to the resumed turn: these rows had no live phase here.
					"child": {toolCallTurn("tu_fan", "Agent",
						`{"op":"parallel_spawn","spawns":[{"name":"grandchild","prompt":"a"},{"name":"grandchild","prompt":"b"},{"name":"grandchild","prompt":"c"}]}`)},
				},
				calls:  map[string]int{},
				onCall: probe.onCall,
			}
			srv, _ := makeServerWithTools(t, prov, cfg, nil)
			ident := store.RunIdentity{AgentID: "a_legacy", RunConfig: tc.runConfig}
			if tc.sub {
				ident.ParentAgentID = "a_gone_parent"
			}
			run := createCeilingRun(t, srv, "child", ident)
			parkForResume(t, srv, run.ID)
			resumeOne(t, srv)
			if tc.want > 1 {
				// Bounded here rather than by the run: a width that is not
				// permitted would otherwise hold each call for the whole
				// barrier timeout, and the failure would read as a stuck run.
				probe.awaitPeak(tc.want, 10*time.Second)
				probe.release()
			}
			waitFor(t, "the resumed run to finish", func() bool {
				_, live := srv.cancelReg.Get(run.AgentID)
				return !live
			})
			if got := probe.peakInFlight(); got != tc.want {
				t.Errorf("resumed run's grandchildren peaked at %d in flight, want %d", got, tc.want)
			}
		})
	}
}

// fanWidthProbe records the peak number of grandchild model calls in flight.
// Each call is held until joinUpTo calls have been in flight together or wait
// passes, so a width up to joinUpTo is observed whenever it is permitted. The
// release is a latch: once the barrier opens it stays open, so the calls that
// were held do not wait out the timeout after the first of them returns.
type fanWidthProbe struct {
	joinUpTo int
	wait     time.Duration

	mu       sync.Mutex
	inFlight int
	peak     int
	released chan struct{} // closed once joinUpTo calls are in flight, or by release
}

func newFanWidthProbe(joinUpTo int, wait time.Duration) *fanWidthProbe {
	return &fanWidthProbe{joinUpTo: joinUpTo, wait: wait, released: make(chan struct{})}
}

func (p *fanWidthProbe) onCall(role string) {
	if role != "grandchild" {
		return
	}
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > p.peak {
		p.peak = p.inFlight
	}
	if p.inFlight >= p.joinUpTo {
		p.openLocked()
	}
	p.mu.Unlock()
	select {
	case <-p.released:
	case <-time.After(p.wait):
	}
	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
}

// awaitPeak returns once want calls have been in flight together, or within
// has passed.
func (p *fanWidthProbe) awaitPeak(want int, within time.Duration) {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) && p.peakInFlight() < want {
		time.Sleep(5 * time.Millisecond)
	}
}

// release opens the barrier for every call still held and every later one.
func (p *fanWidthProbe) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.openLocked()
}

func (p *fanWidthProbe) openLocked() {
	select {
	case <-p.released:
	default:
		close(p.released)
	}
}

func (p *fanWidthProbe) peakInFlight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak
}

// makeServerWithTools is makeServer with a tool set.
func makeServerWithTools(t *testing.T, prov providers.Provider, cfg *config.Config, toolset []tools.Tool) (*Server, store.Store) {
	t.Helper()
	srv, st, _ := spawnCeilingServer(t, cfg, prov, toolset)
	return srv, st
}

// createCeilingRun files a run of agent with no configuration record, as every
// row written before records existed has.
func createCeilingRun(t *testing.T, srv *Server, agent string, ident store.RunIdentity) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, ident.TenantID, agent, "alice")
	if err != nil {
		t.Fatal(err)
	}
	ident.UserID, ident.Model = "alice", "stub-model"
	run, err := srv.store.CreateRun(ctx, sess.ID, ident)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// Every field of a volume binding is either carried by the record or, for the
// root, deliberately re-resolved on resume. A field added to VolumeBinding
// without a decision here would be silently dropped across a pause — and a
// dropped restriction is a widening.
func TestSpawnRecord_AccountsForEveryVolumeField(t *testing.T) {
	handled := map[string]bool{"Name": true, "ReadOnly": true, "Default": true, "Root": true}
	bt := reflect.TypeOf(tools.VolumeBinding{})
	for i := 0; i < bt.NumField(); i++ {
		if f := bt.Field(i).Name; !handled[f] {
			t.Errorf("tools.VolumeBinding.%s is neither recorded in volumeCeilingBinding nor re-resolved on resume", f)
		}
	}
	pt := reflect.TypeOf(tools.VolumePolicyValue{})
	for i := 0; i < pt.NumField(); i++ {
		if f := pt.Field(i).Name; f != "Active" && f != "Bindings" {
			t.Errorf("tools.VolumePolicyValue.%s is not recorded in volumeCeilingRecord", f)
		}
	}

	// And what is recorded survives the column's JSON round trip.
	rec := runConfigRecord{Spawn: spawnRecordOf(tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{
		{Name: "docs", Root: "/must/not/persist", ReadOnly: true, Default: true},
	}}, 3)}
	raw := rec.marshal()
	if strings.Contains(string(raw), "/must/not/persist") {
		t.Errorf("the record carries a volume root: %s", raw)
	}
	back, ok := decodeRunConfig(raw)
	want := &spawnRecord{
		Volumes:   volumeCeilingRecord{Active: true, Bindings: []volumeCeilingBinding{{Name: "docs", ReadOnly: true, Default: true}}},
		FanoutCap: 3,
	}
	if !ok || !reflect.DeepEqual(back.Spawn, want) {
		t.Errorf("round trip = %+v, want %+v", back.Spawn, want)
	}
}
