package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// routedServer wires a tiered agent over a request-recording provider, so what
// a routing override actually did is observable where it counts: the model the
// provider was asked for.
func routedServer(t *testing.T) (*Server, *httptest.Server, *recordingScriptedProvider, store.Store) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"router": {Tier: "middle", Tools: []string{}, SystemPrompt: "you route"},
	}
	cfg.Tiers = map[string][]config.TierCandidate{
		"middle": {{Provider: "primary", Model: "model-a"}, {Provider: "secondary", Model: "model-b"}},
	}
	prov := &recordingScriptedProvider{defaultS: endTurn()}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "routing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	res := resolve.NewResolver([]string{"primary", "secondary"}, map[string][]resolve.Candidate{
		"middle": {{Provider: "primary", Model: "model-a"}, {Provider: "secondary", Model: "model-b"}},
	})
	res.SetReachable("primary", true, []string{"model-a"}, "")
	res.SetReachable("secondary", true, []string{"model-b"}, "")
	srv.SetResolver(res)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return srv, ts, prov, st
}

func postRoutedRun(t *testing.T, ts *httptest.Server, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const routedSegments = `"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]`

// THE CROSSING. The override is threaded from the wire, through validation,
// into the definition copy, into resolution, and finally into the provider
// Request. Every one of those is a hand-off that a unit test on either side
// leaves uncovered, and the only place the answer is observable is the model
// the provider was actually asked for.
func TestRoutingOverride_WireToProviderRequest(t *testing.T) {
	_, ts, prov, _ := routedServer(t)

	// Sanity: the tier's own answer is model-a, so "overridden" and
	// "re-derived" cannot produce the same string.
	if code, body := postRoutedRun(t, ts, `{"agent":"router",`+routedSegments+`}`); code != 200 {
		t.Fatalf("baseline run: %d %s", code, body)
	}
	if got := prov.waitForRequests(t, 1)[0].Model; got != "model-a" {
		t.Fatalf("fixture drifted: the tier resolves %q, expected model-a", got)
	}

	if code, body := postRoutedRun(t, ts, `{"agent":"router","model":"model-b",`+routedSegments+`}`); code != 200 {
		t.Fatalf("overridden run: %d %s", code, body)
	}
	if got := prov.waitForRequests(t, 2)[1].Model; got != "model-b" {
		t.Errorf("provider was asked for %q, want model-b — the override did not reach the request", got)
	}
}

// RFC DC V5 at the wire: a model outside what the deployment can reach is a
// 400, not a silent fallback to the tier's own pick.
func TestRoutingOverride_UnreachableModelIsRefusedAtTheWire(t *testing.T) {
	_, ts, prov, _ := routedServer(t)

	code, body := postRoutedRun(t, ts, `{"agent":"router","model":"model-nowhere",`+routedSegments+`}`)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d (%s), want 400", code, strings.TrimSpace(body))
	}
	if got := prov.requests(); len(got) != 0 {
		t.Errorf("the provider was called %d time(s) for a refused override", len(got))
	}
}

// The override survives a pause. This is the whole reason it lives in the run's
// configuration record rather than beside the request that carried it.
func TestRoutingOverride_SurvivesResume(t *testing.T) {
	srv, ts, prov, st := routedServer(t)
	srv.SetSteerRegistry(steer.NewRegistry(0))

	code, body := postRoutedRun(t, ts, `{"agent":"router","model":"model-b",`+routedSegments+`}`)
	if code != 200 {
		t.Fatalf("run: %d %s", code, body)
	}
	prov.waitForRequests(t, 1)

	run := onlyRun(t, st, extractSessionID(body))
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok || rec.Routing == nil || rec.Routing.Model != "model-b" {
		t.Fatalf("the run row records no routing override (%+v); nothing can be restored", rec.Routing)
	}

	parkForResume(t, srv, run.ID)
	if n, warns := srv.ResumePausedRuns(context.Background()); n == 0 {
		t.Fatalf("nothing resumed (warnings: %v)", warns)
	}
	if got := prov.waitForRequests(t, 2)[1].Model; got != "model-b" {
		t.Errorf("the resumed turn ran on %q, want model-b — the run silently reverted to "+
			"the tier's own pick mid-conversation", got)
	}
}
