package a2a

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bridge "github.com/denn-gubsky/loomcycle/internal/a2a"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The server main wires as the A2A runner must hold the run rate, or every
// A2A start would go uncounted.
var _ bridge.StartAdmitter = (*lchttp.Server)(nil)

// countingProvider answers every call "done" and counts the calls: one per
// run that reached the model.
type countingProvider struct{ calls atomic.Int64 }

func (p *countingProvider) ID() string                  { return "stub" }
func (p *countingProvider) Probe(context.Context) error { return nil }
func (p *countingProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *countingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *countingProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	p.calls.Add(1)
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "done"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	close(ch)
	return ch, nil
}

type oneProvider struct{ p providers.Provider }

func (r oneProvider) Get(string) (providers.Provider, error) { return r.p, nil }

const rateTestPepper = "pep-a2a-rate"

// rateA2AServer is the A2A surface over a real loomcycle server — the runner,
// the connector and the token resolver main wires — with one token per entry
// of tokens (bearer → restrictions record) and the given default run rate.
func rateA2AServer(t *testing.T, defaultRate auth.RateLimit, tokens map[string]struct {
	scopes       []string
	restrictions string
}) (*httptest.Server, *countingProvider) {
	t.Helper()
	cfg := twoAgentCardCfg()
	cfg.Defaults = config.Defaults{Provider: "stub", Model: "stub-model"}
	cfg.Concurrency = config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4}
	cfg.Agents = map[string]config.AgentDef{}
	for _, a := range cfg.A2AServerCards["loomcycle-fleet"].ExposedAgents {
		cfg.Agents[a.AgentName] = config.AgentDef{Model: "stub-model", SystemPrompt: "hi", Tools: []string{}}
	}
	cfg.Env.AuthToken = "legacy-admin-secret"
	cfg.Env.OperatorTokenPepper = rateTestPepper
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "a2a-rate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for bearer, tok := range tokens {
		if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
			DefID: "def_" + bearer, Name: "n-" + bearer, TenantID: "acme", Subject: "s-" + bearer,
			TokenHash: auth.HashToken(rateTestPepper, bearer), AllowedScopes: tok.scopes,
			RestrictionsJSON: tok.restrictions,
		}); err != nil {
			t.Fatal(err)
		}
	}
	prov := &countingProvider{}
	srv := lchttp.New(cfg, oneProvider{prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	srv.SetDefaultRunRate(defaultRate)

	var real atomic.Pointer[http.Handler]
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := real.Load(); h != nil {
			(*h).ServeHTTP(w, r)
			return
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	t.Cleanup(ts.Close)
	cfg.Env.A2APublicBaseURL = ts.URL
	a2aSrv, err := New(context.Background(), Deps{
		Cfg: cfg, Store: st, Conn: srv, Run: srv,
		Auth: FrontierAuthenticator(srv.AuthConfigured, srv.ResolvePrincipal, false),
	})
	if err != nil || a2aSrv == nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	a2aSrv.Mount(mux, nil)
	var h http.Handler = mux
	real.Store(&h)
	return ts, prov
}

func sendA2A(t *testing.T, ts *httptest.Server, bearer string) string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":{"messageId":"m","role":"user","parts":[{"kind":"text","text":"hi"}],"metadata":{"skillId":"research"}}}}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/a2a/jsonrpc", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out)
}

// A message that starts a run over A2A takes a start from its caller's run
// rate, as one over HTTP, gRPC or MCP does: the token's own rate, or the
// server's default per-user rate for a caller that is not an operator. The
// refusal starts nothing and says it is a rate limit.
func TestA2A_AStartTakesFromTheCallersRunRate(t *testing.T) {
	member := []string{auth.ScopeRunsCreate}
	type tok = struct {
		scopes       []string
		restrictions string
	}
	for name, c := range map[string]struct {
		def    auth.RateLimit
		bearer string
		tokens map[string]tok
	}{
		"the server's default rate": {
			def: auth.RateLimit{PerMinute: 1, Burst: 1}, bearer: "member",
			tokens: map[string]tok{"member": {member, ""}},
		},
		"the token's own rate": {
			bearer: "rated",
			tokens: map[string]tok{"rated": {member, `{"rate":{"per_minute":1,"burst":1}}`}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			ts, prov := rateA2AServer(t, c.def, c.tokens)
			if out := sendA2A(t, ts, c.bearer); strings.Contains(out, `"error"`) || prov.calls.Load() != 1 {
				t.Fatalf("the first start: %d model call(s), answer %s", prov.calls.Load(), out)
			}
			out := sendA2A(t, ts, c.bearer)
			if !strings.Contains(out, "rate_limited") {
				t.Errorf("the second start past a burst of 1 was not refused as a rate limit: %s", out)
			}
			// A protocol error, not a task: the refused start made nothing.
			if !strings.Contains(out, `"error"`) || strings.Contains(out, `"task"`) {
				t.Errorf("the refusal is not a bare protocol error: %s", out)
			}
			t.Logf("refusal on the wire: %s", out)
			if got := prov.calls.Load(); got != 1 {
				t.Errorf("the refused start reached the model: %d calls, want 1", got)
			}
		})
	}
}

// A runner with no run-rate limiter (anything but the loomcycle server) cannot
// count a start, so a token that carries its own rate is refused rather than
// let through uncounted; a token without one still runs.
func TestA2A_WithNoLimiterARateBoundTokenIsRefused(t *testing.T) {
	backend := newScriptedBackend(completeWithText("done"))
	ts := startA2AServerWithAuth(t, backend, twoAgentCardCfg(), map[string]auth.Principal{
		"rated": {TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate},
			Restrictions: auth.Restrictions{Rate: &auth.RateLimit{PerMinute: 100}}},
		"plain": {TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate}},
	})
	if out := sendA2A(t, ts, "rated"); !strings.Contains(out, "rate_limited") {
		t.Errorf("a rate-bound token ran with no limiter to count it: %s", out)
	}
	if out := sendA2A(t, ts, "plain"); strings.Contains(out, `"error"`) {
		t.Errorf("a token with no rate was refused: %s", out)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.ranTenants) != 1 {
		t.Errorf("%d runs, want only the plain token's", len(backend.ranTenants))
	}
}

// The default rate bounds end users, not the deployment's operators, here as
// on the other transports.
func TestA2A_TheDefaultRateDoesNotBindAnOperator(t *testing.T) {
	ts, prov := rateA2AServer(t, auth.RateLimit{PerMinute: 1, Burst: 1}, map[string]struct {
		scopes       []string
		restrictions string
	}{"op": {[]string{auth.ScopeTenant}, ""}})
	for i := 0; i < 3; i++ {
		if out := sendA2A(t, ts, "op"); strings.Contains(out, "rate_limited") {
			t.Fatalf("start %d of a tenant operator was rate limited: %s", i+1, out)
		}
	}
	if got := prov.calls.Load(); got != 3 {
		t.Errorf("%d model calls, want 3", got)
	}
}
