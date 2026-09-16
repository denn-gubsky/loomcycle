package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// recordingScriptedProvider is scriptedProvider plus the Requests it received.
// The request is the only place a resumed run's configuration becomes
// observable — everything upstream of it is a value being copied around.
type recordingScriptedProvider struct {
	mu       sync.Mutex
	reqs     []providers.Request
	scripts  [][]providers.Event
	defaultS []providers.Event
}

func (p *recordingScriptedProvider) ID() string                    { return "scripted" }
func (p *recordingScriptedProvider) Probe(_ context.Context) error { return nil }
func (p *recordingScriptedProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *recordingScriptedProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *recordingScriptedProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	idx := len(p.reqs)
	p.reqs = append(p.reqs, req)
	events := p.defaultS
	if idx < len(p.scripts) {
		events = p.scripts[idx]
	}
	p.mu.Unlock()

	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (p *recordingScriptedProvider) requests() []providers.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providers.Request(nil), p.reqs...)
}

// waitForRequests blocks until the provider has seen n calls, or fails.
func (p *recordingScriptedProvider) waitForRequests(t *testing.T, n int) []providers.Request {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := p.requests(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("provider saw %d calls, wanted %d", len(p.requests()), n)
	return nil
}

func endTurn() []providers.Event {
	return []providers.Event{
		{Type: providers.EventText, Text: "ok"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
}

// parkForResume makes a finished run look like one that was paused mid-
// conversation: a pending operator turn at the end of the transcript, and
// pause_state='paused' so ResumePausedRuns picks it up.
func parkForResume(t *testing.T, srv *Server, runID string) {
	t.Helper()
	appendResumeEvent(t, srv, runID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "carry on"}}},
	})
	if err := srv.store.SetRunPauseState(context.Background(), runID, store.PauseStatePaused); err != nil {
		t.Fatalf("SetRunPauseState: %v", err)
	}
}

// onlyRun returns the single run of a session, failing if there isn't exactly one.
func onlyRun(t *testing.T, st store.Store, sessionID string) store.Run {
	t.Helper()
	runs, err := st.RunsForSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("RunsForSession: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("session %s has %d runs, want 1", sessionID, len(runs))
	}
	return runs[0]
}

// --- RFC DD Gap 1: the run's own configuration survives a resume ---

// TestResumedRun_KeepsItsOwnSampling crosses every seam at once: a per-run
// temperature is merged at run start, written to the runs row, read back on
// resume, decoded, and handed to the loop — and the assertion is on the
// provider REQUEST, which is the only place the value becomes observable.
//
// Testing the record's encode/decode on its own would prove nothing here: the
// value is THREADED, not computed, so both ends stay green with the carry
// deleted. Verified by probe — see the fail-before note on the PR.
//
// The definition deliberately sets a DIFFERENT temperature, so "restored" and
// "re-derived from the def" cannot produce the same number.
func TestResumedRun_KeepsItsOwnSampling(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"tuner": {
				Provider:     "scripted",
				Model:        "stub-model",
				SystemPrompt: "you are tuned",
				Tools:        []string{},
				Sampling:     &config.Sampling{Temperature: f64(0.9)},
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""

	prov := &recordingScriptedProvider{defaultS: endTurn()}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "runcfg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"tuner","sampling":{"temperature":0.11},"segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}

	first := prov.waitForRequests(t, 1)[0]
	if first.Temperature == nil || *first.Temperature != 0.11 {
		t.Fatalf("fixture drifted: the ORIGINAL run did not use the per-run temperature (got %v)", first.Temperature)
	}

	sessionID := extractSessionID(string(body))
	if sessionID == "" {
		t.Fatalf("no session frame in SSE body:\n%s", body)
	}
	run := onlyRun(t, st, sessionID)

	// The write seam: the row carries the merged record, not the def's.
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok {
		t.Fatal("the run row carries no run_config; nothing can be restored from it")
	}
	if rec.Sampling == nil || rec.Sampling.Temperature == nil || *rec.Sampling.Temperature != 0.11 {
		t.Errorf("persisted sampling = %+v, want temperature 0.11", rec.Sampling)
	}

	// The read seam: resume the run and watch which temperature reaches the model.
	parkForResume(t, srv, run.ID)
	n, warns := srv.ResumePausedRuns(context.Background())
	if n == 0 {
		t.Fatalf("nothing resumed (warnings: %v)", warns)
	}
	resumed := prov.waitForRequests(t, 2)[1]
	if resumed.Temperature == nil {
		t.Fatal("the resumed turn sent no temperature at all")
	}
	if *resumed.Temperature != 0.11 {
		t.Errorf("resumed turn ran at temperature %v, want 0.11 — the run silently "+
			"reverted to its definition's %v mid-conversation", *resumed.Temperature, 0.9)
	}
}

// TestResumedRun_KeepsItsContextWindow is the same crossing for RFC CJ's
// per-run max_context_tokens, which reaches the driver on its own field.
func TestResumedRun_KeepsItsContextWindow(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"sized": {
				Provider:         "scripted",
				Model:            "stub-model",
				SystemPrompt:     "you are sized",
				Tools:            []string{},
				MaxContextTokens: 8192,
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""

	prov := &recordingScriptedProvider{defaultS: endTurn()}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "runcfg_ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"sized","max_context_tokens":65536,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if got := prov.waitForRequests(t, 1)[0].MaxContextTokens; got != 65536 {
		t.Fatalf("fixture drifted: original run used window %d, want 65536", got)
	}

	run := onlyRun(t, st, extractSessionID(string(body)))
	parkForResume(t, srv, run.ID)
	if n, warns := srv.ResumePausedRuns(context.Background()); n == 0 {
		t.Fatalf("nothing resumed (warnings: %v)", warns)
	}
	if got := prov.waitForRequests(t, 2)[1].MaxContextTokens; got != 65536 {
		t.Errorf("resumed turn used window %d, want 65536 — it reverted to the "+
			"definition's 8192", got)
	}
}

// --- RFC DD Gap 2: a resumed run is never WIDER than the original ---

// TestResumedRun_KeepsItsCallerHostNarrowing asserts on the CONSUMER's view:
// whether the resumed run's HTTP tool can actually reach the host the original
// was narrowed to. The operator's static list is empty, so a resumed run that
// lost its narrowing falls back to a floor that reaches nothing.
func TestResumedRun_KeepsItsCallerHostNarrowing(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("resumed-run-reached-it"))
	}))
	defer target.Close()

	cfg := makeBaseConfig()
	cfg.Env.HTTPCallerAuthoritative = true
	cfg.Env.HTTPHostAllowlist = nil                          // operator floor reaches nothing
	cfg.Env.HTTPPrivateHostAllowlist = []string{"127.0.0.1"} // dial-layer loopback exemption
	cfg.Agents = map[string]config.AgentDef{
		"fetcher": {Model: "stub-model", Tools: []string{"HTTP"}, SystemPrompt: "you fetch"},
	}

	prov := &recordingScriptedProvider{
		scripts: [][]providers.Event{
			endTurn(), // the original run: nothing to do
			{ // the RESUMED turn: try the host the caller narrowed to
				{
					Type: providers.EventToolCall,
					ToolUse: &providers.ToolUse{
						ID:    "tu_resumed_1",
						Name:  "HTTP",
						Input: json.RawMessage(`{"method":"GET","url":"` + target.URL + `"}`),
					},
				},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}},
			},
		},
		defaultS: endTurn(),
	}

	httpTool := &builtin.HTTP{
		HostAllowlist:        cfg.Env.HTTPHostAllowlist,
		PrivateHostAllowlist: cfg.Env.HTTPPrivateHostAllowlist,
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "runcfg_hosts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{httpTool}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"fetcher","allowed_hosts":["127.0.0.1"],"segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	run := onlyRun(t, st, extractSessionID(string(body)))

	parkForResume(t, srv, run.ID)
	if n, warns := srv.ResumePausedRuns(context.Background()); n == 0 {
		t.Fatalf("nothing resumed (warnings: %v)", warns)
	}
	// Three calls: the original turn, the resumed turn's tool_use, and the
	// follow-up the loop makes after the tool_result.
	prov.waitForRequests(t, 3)

	transcript := runTranscriptText(t, st, run.SessionID, run.ID)
	if !strings.Contains(transcript, "resumed-run-reached-it") {
		t.Errorf("the resumed run could not reach the host the original was narrowed to; "+
			"its narrowing was dropped and the empty operator floor applied.\ntranscript:\n%s",
			transcript)
	}
}

// runTranscriptText concatenates a run's event payloads for substring assertions.
func runTranscriptText(t *testing.T, st store.Store, sessionID, runID string) string {
	t.Helper()
	events, err := st.GetTranscript(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetTranscript: %v", err)
	}
	var b strings.Builder
	for _, e := range events {
		if e.RunID != runID {
			continue
		}
		b.WriteString(e.Type)
		b.WriteString(": ")
		b.Write(e.Payload)
		b.WriteString("\n")
	}
	return b.String()
}

// --- the record itself ---

// An EMPTY caller list means "allow nothing"; no list at all means "no
// narrowing". JSON's omitempty erases that difference on the wire, which is
// what HasList is carried for — assert the distinction survives a round trip
// rather than trusting the tag.
func TestRunConfigRecord_EmptyCallerListIsNotNoList(t *testing.T) {
	empty := runConfigRecord{Hosts: hostRecordOf(tools.HostPolicyValue{
		AllowedHosts: []string{},
		HasList:      true,
	})}
	back, ok := decodeRunConfig(empty.marshal())
	if !ok {
		t.Fatal("record did not round-trip")
	}
	if got := back.callerHosts(); got == nil {
		t.Error("an empty caller list decoded as NO list — the run would resume " +
			"with the full operator floor instead of reaching nothing")
	} else if len(got) != 0 {
		t.Errorf("callerHosts() = %v, want an empty non-nil list", got)
	}

	none, ok := decodeRunConfig(runConfigRecord{}.marshal())
	if !ok {
		t.Fatal("empty record did not round-trip")
	}
	if got := none.callerHosts(); got != nil {
		t.Errorf("callerHosts() = %v for a run that sent no list, want nil", got)
	}
}

// A run with no record resumes exactly the way every run did before the column
// existed. Guards the legacy path, which is the one nothing else exercises.
func TestDecodeRunConfig_AbsentAndCorruptBothFallBack(t *testing.T) {
	if _, ok := decodeRunConfig(nil); ok {
		t.Error("a run with no record reported one")
	}
	if _, ok := decodeRunConfig(json.RawMessage(`{"sampling":`)); ok {
		t.Error("a corrupt record was accepted; the run would resume on garbage")
	}
}

// --- the hoist this change relies on ---

// prepareSubRunValues merges the sub-run's tuning BEFORE the two def rewrites
// (resolveSkillBodiesForRun / applyMemoryInjection), because the run row is
// written between them. That is only correct while those rewrites touch nothing
// but SystemPrompt. Pin it here rather than in a comment.
func TestSubRunConfig_DefRewritesLeaveTuningUntouched(t *testing.T) {
	cfg := makeBaseConfig()
	srv, _ := makeServer(t, completingProvider(), cfg)

	def := config.AgentDef{
		SystemPrompt:     "before",
		Skills:           []string{"anything"},
		Sampling:         &config.Sampling{Temperature: f64(0.33)},
		Compaction:       &config.Compaction{KeepLastN: intPtr(7)},
		Context:          &config.Context{Mode: strPtr("recap")},
		MaxContextTokens: 4242,
	}
	ctx := context.Background()

	after, _ := srv.resolveSkillBodiesForRun(ctx, "", def)
	after, _ = srv.applyMemoryInjection(ctx, after, memInject{Tenant: "", UserID: "u", AgentName: "a"})

	if after.Sampling != def.Sampling {
		t.Error("a def rewrite replaced Sampling; merging it before the rewrites is no longer safe")
	}
	if after.Compaction != def.Compaction {
		t.Error("a def rewrite replaced Compaction; merging it before the rewrites is no longer safe")
	}
	if after.Context != def.Context {
		t.Error("a def rewrite replaced Context; merging it before the rewrites is no longer safe")
	}
	if after.MaxContextTokens != def.MaxContextTokens {
		t.Errorf("a def rewrite changed MaxContextTokens %d → %d", def.MaxContextTokens, after.MaxContextTokens)
	}
}

func intPtr(v int) *int { return &v }
