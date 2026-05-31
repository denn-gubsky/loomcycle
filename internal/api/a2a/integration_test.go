package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	bridgesign "github.com/denn-gubsky/loomcycle/internal/a2a/sign"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// scriptedBackend is the full server-side fake for the end-to-end tests:
// it is BOTH a runner.Runner and the CardAndRunStore the A2A server
// reads run rows from. Because the real SDK client generates the task id
// (which the executor uses as the loomcycle agent_id), the run row can't
// be pre-keyed by a known id; instead RunOnce records a terminal run row
// keyed by the agent_id it was driven with, so the executor's
// finalOutcome resolves it via GetRunByAgentID after the run returns.
//
// scriptFor maps a resolved loomcycle agent name to the provider-event
// sequence + terminal RunStatus that agent should produce, so a test can
// assert routing (which agent ran) and outcome (the terminal A2A state).
type scriptedBackend struct {
	scriptFor func(agent string) ([]providers.Event, store.RunStatus, string)

	mu         sync.Mutex
	runs       map[string]store.Run     // keyed by agent_id (== A2A task id)
	sessions   map[string]store.Session // keyed by session_id; carries the run's tenant
	ranAgents  []string                 // ordered record of which agents ran
	ranTenants []string                 // ordered record of the tenant each run was attributed to
}

func newScriptedBackend(scriptFor func(agent string) ([]providers.Event, store.RunStatus, string)) *scriptedBackend {
	return &scriptedBackend{scriptFor: scriptFor, runs: map[string]store.Run{}, sessions: map[string]store.Session{}}
}

// RunOnce drives the scripted event stream for the routed agent, then
// records the terminal run row the executor reads back.
func (b *scriptedBackend) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	if cb.OnRegistered != nil {
		cb.OnRegistered(in.AgentID, "run-"+in.AgentID, "sess-"+in.AgentID, "")
	}
	events, status, detail := b.scriptFor(in.Agent)
	for _, ev := range events {
		if cb.OnEvent != nil {
			cb.OnEvent(ev)
		}
	}
	b.mu.Lock()
	b.ranAgents = append(b.ranAgents, in.Agent)
	b.ranTenants = append(b.ranTenants, in.TenantID)
	row := store.Run{
		ID:         "run-" + in.AgentID,
		AgentID:    in.AgentID,
		SessionID:  "sess-" + in.AgentID,
		Status:     status,
		StopReason: detail,
	}
	if status == store.RunFailed {
		row.ErrorMsg = detail
	}
	b.runs[in.AgentID] = row
	// Record the run's owning session with the tenant it was attributed to,
	// so the tenant-scoping path (GetSession) sees the same tenant the run
	// was created under — mirroring the real store where tenant lives on
	// the session row, not the run.
	b.sessions["sess-"+in.AgentID] = store.Session{ID: "sess-" + in.AgentID, TenantID: in.TenantID}
	b.mu.Unlock()
	return nil
}

func (b *scriptedBackend) GetRun(ctx context.Context, runID string) (store.Run, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range b.runs {
		if r.ID == runID {
			return r, nil
		}
	}
	return store.Run{}, &store.ErrNotFound{Kind: "run", ID: runID}
}

func (b *scriptedBackend) GetRunByAgentID(ctx context.Context, agentID string) (store.Run, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.runs[agentID]; ok {
		return r, nil
	}
	return store.Run{}, &store.ErrNotFound{Kind: "run", ID: agentID}
}

func (b *scriptedBackend) GetSession(ctx context.Context, sessionID string) (store.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sessions[sessionID]; ok {
		return s, nil
	}
	return store.Session{}, &store.ErrNotFound{Kind: "session", ID: sessionID}
}

// The card-resolution + interrupt surfaces are unused by these tests
// (the card is served from cfg, and these scenarios don't park), so they
// return not-found / no-op to satisfy CardAndRunStore.
func (b *scriptedBackend) A2AServerCardDefGetActive(ctx context.Context, name string) (store.A2AServerCardDefRow, error) {
	return store.A2AServerCardDefRow{}, &store.ErrNotFound{Kind: "a2a_server_card_def", ID: name}
}
func (b *scriptedBackend) InterruptListByRun(ctx context.Context, runID, statusFilter string) ([]store.InterruptRow, error) {
	return nil, nil
}
func (b *scriptedBackend) InterruptResolve(ctx context.Context, interruptID, answer, resolvedBy string, answerMeta json.RawMessage) error {
	return nil
}

// integrationConnector embeds the interface; these scenarios never
// cancel, so the embedded nil is never dialled.
type integrationConnector struct{ connector.Connector }

// startA2AServer builds an enabled A2A server over the given backend +
// card config and mounts it on an httptest.Server, returning the live
// server. The mount mirrors main.go: the A2A routes on a mux, wrapped in
// PathTenantWrapper.
//
// The served AgentCard's interface URLs must be ABSOLUTE and point back
// at this server so the SDK client can dial them — but the httptest URL
// is only known after the listener starts, which is after New() captured
// the base URL. We resolve this chicken-and-egg by deferring the handler
// behind a one-shot indirection: start the server with a stub, learn its
// URL, rebuild the A2A server with that URL as A2APublicBaseURL, then
// install the real handler.
func startA2AServer(t *testing.T, backend *scriptedBackend, cfg *config.Config) *httptest.Server {
	t.Helper()
	// atomic so the handler-goroutine read of the deferred handler is
	// race-clean against the test-goroutine write below (the -race
	// detector needs the synchronization edge even though no request can
	// arrive before the SDK client is built).
	var real atomic.Pointer[http.Handler]
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := real.Load(); h != nil {
			(*h).ServeHTTP(w, r)
			return
		}
		http.Error(w, "server not ready", http.StatusServiceUnavailable)
	}))
	t.Cleanup(httpSrv.Close)

	cfg.Env.A2APublicBaseURL = httpSrv.URL
	srv, err := New(context.Background(), Deps{
		Cfg:   cfg,
		Store: backend,
		Conn:  integrationConnector{},
		Run:   backend,
		Auth:  nil, // open mode — these tests assert routing/outcome, not auth
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv == nil {
		t.Fatal("New returned nil for an enabled config")
	}
	mux := http.NewServeMux()
	srv.Mount(mux, nil)
	h := srv.PathTenantWrapper(mux)
	real.Store(&h)
	return httpSrv
}

// twoAgentCardCfg is an enabled, none-tenancy config whose card exposes
// the researcher + writer agents. The public base URL is filled in by
// startA2AServer once the live httptest URL is known.
func twoAgentCardCfg() *config.Config {
	cfg := &config.Config{
		A2AServerCards: map[string]config.A2AServerCard{
			"loomcycle-fleet": fixtureCard(),
		},
	}
	cfg.Env.A2AServerEnabled = true
	cfg.Env.A2AServerCardName = "loomcycle-fleet"
	cfg.Env.A2ATenancyRouting = "none"
	return cfg
}

// completeWithText scripts a run that emits one text artifact and
// completes, for the named-agent happy path.
func completeWithText(text string) func(string) ([]providers.Event, store.RunStatus, string) {
	return func(agent string) ([]providers.Event, store.RunStatus, string) {
		return []providers.Event{
			{Type: providers.EventStarted},
			{Type: providers.EventText, Text: text + " [" + agent + "]"},
			{Type: providers.EventDone, StopReason: "end_turn"},
		}, store.RunCompleted, "end_turn"
	}
}

// newSDKClient resolves the card from the well-known URI and builds a
// real SDK client against it — the same discovery path an external A2A
// consumer uses.
func newSDKClient(t *testing.T, base string) *a2aclient.Client {
	t.Helper()
	card, err := agentcard.DefaultResolver.Resolve(context.Background(), base)
	if err != nil {
		t.Fatalf("resolve agent card: %v", err)
	}
	client, err := a2aclient.NewFromCard(context.Background(), card)
	if err != nil {
		t.Fatalf("client from card: %v", err)
	}
	t.Cleanup(func() { _ = client.Destroy() })
	return client
}

// sendToSkill sends a message targeting skillID via the real SDK client.
func sendToSkill(t *testing.T, client *a2aclient.Client, skillID, text string) a2asdk.SendMessageResult {
	t.Helper()
	msg := a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart(text))
	msg.Metadata = map[string]any{"skillId": skillID}
	res, err := client.SendMessage(context.Background(), &a2asdk.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatalf("SendMessage(skill=%s): %v", skillID, err)
	}
	return res
}

// TestIntegration_WellKnownCardSkillsMatchExposedAgents fetches the
// AgentCard from the well-known URI through the SDK resolver and asserts
// the advertised skills are exactly the card's exposed_agents.
func TestIntegration_WellKnownCardSkillsMatchExposedAgents(t *testing.T) {
	backend := newScriptedBackend(completeWithText("ok"))
	httpSrv := startA2AServer(t, backend, twoAgentCardCfg())

	card, err := agentcard.DefaultResolver.Resolve(context.Background(), httpSrv.URL)
	if err != nil {
		t.Fatalf("resolve card: %v", err)
	}
	if card.Name != "loomcycle-fleet" {
		t.Errorf("card name = %q, want loomcycle-fleet", card.Name)
	}
	gotSkills := map[string]bool{}
	for _, s := range card.Skills {
		gotSkills[s.ID] = true
	}
	for _, want := range []string{"research", "write"} {
		if !gotSkills[want] {
			t.Errorf("served card missing skill %q (got %v)", want, gotSkills)
		}
	}
	if len(card.Skills) != 2 {
		t.Errorf("card advertises %d skills, want 2 (one per exposed agent)", len(card.Skills))
	}
}

// TestIntegration_SendMessageRoutesSkillToAgentAndCompletes drives the
// FULL stack through the real SDK client: discover the card, send a
// message targeting the "research" skill, and assert (a) it routed to
// the "researcher" agent and (b) the scripted run came back as an A2A
// Task terminating COMPLETED with the run's artifact text.
func TestIntegration_SendMessageRoutesSkillToAgentAndCompletes(t *testing.T) {
	backend := newScriptedBackend(completeWithText("done"))
	httpSrv := startA2AServer(t, backend, twoAgentCardCfg())

	client := newSDKClient(t, httpSrv.URL)
	res := sendToSkill(t, client, "research", "research Acme Corp")

	task, ok := res.(*a2asdk.Task)
	if !ok {
		t.Fatalf("result = %T, want *a2a.Task", res)
	}
	if task.Status.State != a2asdk.TaskStateCompleted {
		t.Fatalf("task state = %q, want COMPLETED", task.Status.State)
	}
	if len(task.Artifacts) == 0 {
		t.Fatal("completed task carried no artifacts (the run's text was lost)")
	}
	if got := task.Artifacts[0].Parts[0].Text(); got != "done [researcher]" {
		t.Errorf("artifact text = %q, want 'done [researcher]'", got)
	}
	if len(backend.ranAgents) != 1 || backend.ranAgents[0] != "researcher" {
		t.Errorf("ran agents = %v, want [researcher] (skill 'research' must route to 'researcher')", backend.ranAgents)
	}
}

// TestIntegration_DistinctSkillsRouteToDistinctAgents asserts the two
// exposed skills dispatch to their two distinct agents over the wire.
func TestIntegration_DistinctSkillsRouteToDistinctAgents(t *testing.T) {
	backend := newScriptedBackend(completeWithText("done"))
	httpSrv := startA2AServer(t, backend, twoAgentCardCfg())
	client := newSDKClient(t, httpSrv.URL)

	writeRes := sendToSkill(t, client, "write", "write a poem")
	task, ok := writeRes.(*a2asdk.Task)
	if !ok || task.Status.State != a2asdk.TaskStateCompleted {
		t.Fatalf("write result = %#v, want completed task", writeRes)
	}
	if got := task.Artifacts[0].Parts[0].Text(); got != "done [writer]" {
		t.Errorf("write artifact = %q, want 'done [writer]' (skill 'write' must route to 'writer')", got)
	}
}

// TestIntegration_UnknownSkillTerminatesFailed asserts a message naming
// a skill the card does not expose terminates FAILED (the peer cannot
// reach an unexposed agent) — driven through the real SDK client.
func TestIntegration_UnknownSkillTerminatesFailed(t *testing.T) {
	backend := newScriptedBackend(completeWithText("should-not-run"))
	httpSrv := startA2AServer(t, backend, twoAgentCardCfg())
	client := newSDKClient(t, httpSrv.URL)

	res := sendToSkill(t, client, "nonexistent", "do something")
	task, ok := res.(*a2asdk.Task)
	if !ok {
		t.Fatalf("result = %T, want *a2a.Task (a FAILED task)", res)
	}
	if task.Status.State != a2asdk.TaskStateFailed {
		t.Fatalf("task state = %q, want FAILED for an unknown skill", task.Status.State)
	}
	if len(backend.ranAgents) != 0 {
		t.Errorf("an unexposed skill must not drive any agent; ran %v", backend.ranAgents)
	}
}

// TestIntegration_StreamingRoundTripYieldsIntermediateTaskEvents drives
// SendStreamingMessage through the real SDK client and asserts the
// scripted run's intermediate events arrive as A2A Task events ending in
// a COMPLETED status update — exercising the streaming binding end-to-end.
func TestIntegration_StreamingRoundTripYieldsIntermediateTaskEvents(t *testing.T) {
	backend := newScriptedBackend(completeWithText("streamed"))
	httpSrv := startA2AServer(t, backend, twoAgentCardCfg())
	client := newSDKClient(t, httpSrv.URL)

	msg := a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("stream it"))
	msg.Metadata = map[string]any{"skillId": "research"}

	var states []a2asdk.TaskState
	var sawArtifact bool
	for ev, err := range client.SendStreamingMessage(context.Background(), &a2asdk.SendMessageRequest{Message: msg}) {
		if err != nil {
			t.Fatalf("streaming event error: %v", err)
		}
		switch e := ev.(type) {
		case *a2asdk.TaskStatusUpdateEvent:
			states = append(states, e.Status.State)
		case *a2asdk.TaskArtifactUpdateEvent:
			sawArtifact = true
		case *a2asdk.Task:
			states = append(states, e.Status.State)
		}
	}

	if !sawArtifact {
		t.Error("streaming round-trip never delivered the text artifact")
	}
	if len(states) == 0 || states[len(states)-1] != a2asdk.TaskStateCompleted {
		t.Errorf("streamed states = %v, want terminal COMPLETED", states)
	}
	// The WORKING intermediate state must appear before terminal — proof
	// the stream carried interim progress, not just the final snapshot.
	var sawWorking bool
	for _, s := range states {
		if s == a2asdk.TaskStateWorking {
			sawWorking = true
		}
	}
	if !sawWorking {
		t.Errorf("streamed states = %v, want an intermediate WORKING", states)
	}
}

// TestIntegration_SignedCardFetchVerifiesClientSide fetches a signed
// card through the SDK resolver and verifies its signature client-side
// via the self-contained (embedded-jwk) path the outbound client uses,
// proving the full sign-on-serve → fetch → verify round-trip.
func TestIntegration_SignedCardFetchVerifiesClientSide(t *testing.T) {
	const envName = "LOOMCYCLE_A2A_SIGNING_KEY"
	_, pemStr := ecKeyPEM(t)
	t.Setenv(envName, pemStr)

	cardCfg := fixtureCard()
	cardCfg.SignWithKeyEnv = envName
	cfg := &config.Config{
		A2AServerCards: map[string]config.A2AServerCard{"loomcycle-fleet": cardCfg},
	}
	cfg.Env.A2AServerEnabled = true
	cfg.Env.A2AServerCardName = "loomcycle-fleet"
	cfg.Env.A2ATenancyRouting = "none"
	cfg.Env.SchedulerEnvAllowlist = []string{envName}

	backend := newScriptedBackend(completeWithText("ok"))
	httpSrv := startA2AServer(t, backend, cfg)

	card, err := agentcard.DefaultResolver.Resolve(context.Background(), httpSrv.URL)
	if err != nil {
		t.Fatalf("resolve signed card: %v", err)
	}
	if len(card.Signatures) != 1 {
		t.Fatalf("fetched card has %d signatures, want 1 (signed)", len(card.Signatures))
	}
	if err := bridgesign.VerifyCardSelfContained(card); err != nil {
		t.Fatalf("fetched card signature does not verify client-side: %v", err)
	}
}

// TestIntegration_HostTenancyAttributesRunsToTheirTenant exercises
// host-mode multi-tenancy: two requests carrying distinct tenant-<id>
// Host headers each attribute their run to their OWN tenant, and a bare
// host falls back to the single-tenant (empty) attribution.
//
// This is driven at the binding level (raw JSON-RPC POSTs with distinct
// Host headers) rather than through the real SDK client: the SDK dials
// the absolute interface URL baked into the fetched card, so one
// httptest server can't present two tenant subdomains to it. The wire
// shape (JSON-RPC body) is identical to what the SDK sends — the raw
// requests below mirror the SDK's "SendMessage" call exactly — so the
// host-routing trust boundary is still exercised end-to-end through the
// real mounted handler.
func TestIntegration_HostTenancyAttributesRunsToTheirTenant(t *testing.T) {
	backend := newScriptedBackend(completeWithText("done"))
	cfg := &config.Config{
		A2AServerCards: map[string]config.A2AServerCard{"loomcycle-fleet": fixtureCard()},
	}
	cfg.Env.A2AServerEnabled = true
	cfg.Env.A2AServerCardName = "loomcycle-fleet"
	cfg.Env.A2ATenancyRouting = "host"
	httpSrv := startA2AServer(t, backend, cfg)

	sendWithHost := func(host string) {
		body := `{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":{"messageId":"m","role":"user","parts":[{"kind":"text","text":"hi"}],"metadata":{"skillId":"research"}}}}`
		req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/a2a/jsonrpc", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("send (host=%s): %v", host, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("send (host=%s) status=%d body=%s", host, resp.StatusCode, b)
		}
	}

	sendWithHost("tenant-acme.agents.example")
	sendWithHost("tenant-globex.agents.example")
	sendWithHost("agents.example") // bare root → single-tenant fallback

	backend.mu.Lock()
	defer backend.mu.Unlock()
	want := []string{"acme", "globex", ""}
	if len(backend.ranTenants) != len(want) {
		t.Fatalf("ran tenants = %v, want %v", backend.ranTenants, want)
	}
	for i, w := range want {
		if backend.ranTenants[i] != w {
			t.Errorf("run %d attributed to tenant %q, want %q (host routing is the trust boundary)", i, backend.ranTenants[i], w)
		}
	}
}
