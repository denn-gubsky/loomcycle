package http

import (
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
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A permitted pre hook's allow_hosts widens the host list for the ONE tool
// call it judged. When that call is the Agent tool, the grant must not become
// the spawned child's for its whole run: the child's own HTTP calls, on its
// own iterations, are judged by its own hooks or not widened at all.
func TestSubAgent_HookHostGrantOnTheSpawnDoesNotReachTheChild(t *testing.T) {
	var (
		mu     sync.Mutex
		judged []string
	)
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		if strings.Contains(string(b), `"tool_call":{"id":"a1","name":"Agent"`) {
			judged = append(judged, "Agent")
		} else {
			judged = append(judged, string(b))
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"allow_hosts":["widen.invalid"]}`))
	}))
	t.Cleanup(gate.Close)

	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"net": {Model: "stub-model", Tools: []string{"HTTP"}, SystemPrompt: "net"},
		"launch-net": {
			Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "launcher:net",
			// An operator's definition, with its hook on the permit list: the
			// grant is a real one for the call it judges.
			OperatorAuthored: true,
			ToolHooks: hooks.ToolHooks{"Agent": {
				hooks.PhasePre: {{Inline: &hooks.Inline{Name: "widen-gate", URL: gate.URL}}},
			}},
		},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grant.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	prov := &hostProbeProvider{}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{&builtin.HTTP{HostAllowlist: []string{"operator.example"}}},
		concurrency.New(8, 8, 5*time.Second), st)
	srv.hookPermits = hooks.NewPermits([]string{"widen-gate"})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	body := `{"agent":"launch-net","segments":[{"role":"user","content":[{"type":"trusted-text","text":"fetch https://widen.invalid/"}]}]}`
	if code, out := bearerCall(t, ts, "", "POST", "/v1/runs", body); code != 200 {
		t.Fatalf("run: status %d: %s", code, out)
	}

	mu.Lock()
	seen := append([]string(nil), judged...)
	mu.Unlock()
	if len(seen) != 1 || seen[0] != "Agent" {
		t.Fatalf("the gate judged %q; want exactly the parent's Agent call", seen)
	}
	if res := prov.last(t); !hostRefused(res) {
		t.Errorf("the child reached a host granted only to its parent's spawn call: the HTTP tool answered %q", res)
	}
}
