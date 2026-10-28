package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// systemSeenProvider is roleProvider plus the system prompt of every call,
// per role.
type systemSeenProvider struct {
	*roleProvider
	mu   sync.Mutex
	seen map[string][]string
}

func (p *systemSeenProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	sys := systemText(req)
	role := ""
	for _, r := range p.roles {
		if strings.Contains(sys, "role:"+r+".") {
			role = r
		}
	}
	p.mu.Lock()
	p.seen[role] = append(p.seen[role], sys)
	p.mu.Unlock()
	return p.roleProvider.Call(ctx, req)
}

func (p *systemSeenProvider) systemsOf(role string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen[role]...)
}

// {{identity}} is rendered on every path that assembles a prompt — a run
// started over HTTP, the sub-agent it spawns, and the run after a resume —
// from the originating caller's set: dev's name, and Bash, which dev's list
// withholds.
func TestIdentityInjection_ReachesTheRunItsSubAgentAndItsResume(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Env.AuthToken = "legacy-admin-secret"
	cfg.Env.OperatorTokenPepper = testTokenPepper
	cfg.Agents = map[string]config.AgentDef{
		"worker": {Model: "stub-model", Tools: []string{"Bash", "Agent"}, SystemPrompt: "role:worker.\n{{identity}}"},
		"child":  {Model: "stub-model", Tools: []string{"Bash"}, SystemPrompt: "role:child.\n{{identity}}"},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "identity_run.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "def_worker", Name: "u-def_worker", TenantID: "acme", Subject: "dev",
		TokenHash: auth.HashToken(testTokenPepper, workerBearer), AllowedScopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "dev", DisplayName: "Dev Person", AccessMode: "tenant", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "no-bash", Rules: json.RawMessage(`[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "no-bash", SubjectKind: "user", SubjectID: "dev"}); err != nil {
		t.Fatal(err)
	}
	prov := &systemSeenProvider{
		roleProvider: &roleProvider{roles: []string{"worker", "child"}, calls: map[string]int{}, scripts: map[string][][]providers.Event{
			"worker": {toolCallTurn("tu_child", "Agent", `{"name":"child","prompt":"work"}`), endTurn(), endTurn()},
		}},
		seen: map[string][]string{},
	}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{namedStubTool{"Bash"}}, concurrency.New(8, 8, 5*time.Second), st)
	srv.SetACLResolver(aclres.New(st, authz.ModeCompat, 0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	run := (&aclResumeEnv{srv: srv, st: st, ts: ts}).startAs(t, "worker", "a_identity")
	resumeAndFinish(t, srv, run)

	check := func(where, sys string) {
		t.Helper()
		for _, want := range []string{"- name: Dev Person\n", "- tools withheld by access rules: Bash\n"} {
			if !strings.Contains(sys, want) {
				t.Errorf("%s: missing %q in the system prompt:\n%s", where, want, sys)
			}
		}
	}
	workers := prov.systemsOf("worker")
	if len(workers) < 3 {
		t.Fatalf("worker calls = %d, want 3 (live turn, after the child, resumed)", len(workers))
	}
	check("the live run", workers[0])
	check("the resumed run", workers[len(workers)-1])
	children := prov.systemsOf("child")
	if len(children) == 0 {
		t.Fatal("the child never ran")
	}
	check("the sub-agent", children[0])
}
