package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// envPeer is an httptest MCP peer that records every Authorization header it
// receives. It answers nothing useful: the tests only ask what reached it.
type envPeer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	auth []string
}

func newEnvPeer(t *testing.T) *envPeer {
	p := &envPeer{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		p.mu.Lock()
		p.auth = append(p.auth, r.Header.Get("Authorization"))
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *envPeer) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.auth...)
}

// warnLog collects the pool's log lines.
type warnLog struct {
	mu    sync.Mutex
	lines []string
}

func (w *warnLog) logf(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, fmt.Sprintf(format, args...))
}

func (w *warnLog) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.lines...)
}

// TestMCPPoolBuild_StrictModeRefusesNonOperatorDefWithEnvRef pins the strict
// dial (LOOMCYCLE_MCP_REFUSE_UNATTRIBUTED_ENV=1): a dynamic def stored without
// the operator_authored bit (a row written before authoring refused it) is not
// dialed when it holds a ${NAME}, so the operator's env value never reaches its
// host. The operator's own def, and a non-operator def with no ${NAME}, still
// dial.
func TestMCPPoolBuild_StrictModeRefusesNonOperatorDefWithEnvRef(t *testing.T) {
	t.Setenv("LOOMCYCLE_PEER_KEY_GLOBEX", "globex-secret-value")
	peer := newEnvPeer(t)
	cfg := &config.Config{Env: config.Env{
		MCPAllowPrivateIPs:       true, // the httptest peer is on loopback
		MCPRefuseUnattributedEnv: true,
	}}
	reg := mcp.NewDynamicRegistry()
	var credSubstitute func(ctx context.Context, s string) (string, []string, error)
	build := mcpPoolBuild(cfg, mcpLookupView{reg}, &credSubstitute, (&warnLog{}).logf)
	keyHeader := map[string]string{"Authorization": "Bearer ${LOOMCYCLE_PEER_KEY_GLOBEX}"}

	for name, hdr := range map[string]map[string]string{
		"leaky-header": keyHeader,
		"leaky-nested": {"Authorization": "Bearer ${run.credentials.x:-${LOOMCYCLE_PEER_KEY_GLOBEX}}"},
	} {
		reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: name, Transport: "http", URL: peer.srv.URL, Headers: hdr})
		c, err := build("acme", name)
		if err == nil {
			_, _ = c.Call(context.Background(), "initialize", map[string]any{})
			t.Fatalf("%s: a non-operator def holding a ${NAME} was dialed", name)
		}
		if strings.Contains(err.Error(), "globex-secret-value") {
			t.Fatalf("%s: refusal carries the env value: %v", name, err)
		}
	}
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "leaky-stdio", Transport: "stdio",
		Command: "true", Env: map[string]string{"TOKEN": "${LOOMCYCLE_PEER_KEY_GLOBEX}"}})
	if c, err := build("acme", "leaky-stdio"); err == nil {
		if cl, ok := c.(interface{ Close() error }); ok {
			_ = cl.Close()
		}
		t.Fatal("a non-operator stdio def holding a ${NAME} was spawned")
	}
	if got := peer.seen(); len(got) != 0 {
		t.Fatalf("the peer received %d request(s) from refused defs: %q", len(got), got)
	}

	// Controls: the same peer is reachable, and the operator's def sends its key.
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "plain", Transport: "http", URL: peer.srv.URL,
		Headers: map[string]string{"Authorization": "Bearer literal"}})
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "blessed", Transport: "http", URL: peer.srv.URL,
		Headers: keyHeader, OperatorAuthored: true})
	for _, name := range []string{"plain", "blessed"} {
		c, err := build("acme", name)
		if err != nil {
			t.Fatalf("%s: not dialed: %v", name, err)
		}
		_, _ = c.Call(context.Background(), "initialize", map[string]any{})
	}
	got := peer.seen()
	if len(got) != 2 || got[0] != "Bearer literal" || got[1] != "Bearer globex-secret-value" {
		t.Fatalf("controls: want the literal then the operator's expanded key, got %d header(s)", len(got))
	}
}

// TestMCPPoolBuild_DefaultDialsUnattributedEnvRefWithOneWarning pins this
// release's default: an unattributed def holding a ${NAME} still dials and
// sends the expanded value, as before the authoring rule, so an upgrade does
// not cut off a server an admin wrote before authority was recorded. It logs
// one WARNING per stored version, however many times the pool rebuilds it,
// naming the def, version, tenant and fields but never the value.
func TestMCPPoolBuild_DefaultDialsUnattributedEnvRefWithOneWarning(t *testing.T) {
	t.Setenv("LOOMCYCLE_PEER_KEY_GLOBEX", "globex-secret-value")
	peer := newEnvPeer(t)
	cfg := &config.Config{Env: config.Env{MCPAllowPrivateIPs: true}} // the switch left at its default
	reg := mcp.NewDynamicRegistry()
	var credSubstitute func(ctx context.Context, s string) (string, []string, error)
	logs := &warnLog{}
	build := mcpPoolBuild(cfg, mcpLookupView{reg}, &credSubstitute, logs.logf)
	keyHeader := map[string]string{"Authorization": "Bearer ${LOOMCYCLE_PEER_KEY_GLOBEX}"}
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "legacy", Transport: "http", URL: peer.srv.URL,
		Headers: keyHeader, DefID: "mdf_legacy", Version: 3})

	const dials = 3
	for i := 0; i < dials; i++ {
		c, err := build("acme", "legacy")
		if err != nil {
			t.Fatalf("dial %d: an unattributed def was refused by default: %v", i, err)
		}
		_, _ = c.Call(context.Background(), "initialize", map[string]any{})
	}
	got := peer.seen()
	if len(got) != dials {
		t.Fatalf("the peer received %d request(s), want %d", len(got), dials)
	}
	for _, a := range got {
		if a != "Bearer globex-secret-value" {
			t.Fatal("the default dial did not send the expanded header")
		}
	}
	lines := logs.all()
	if len(lines) != 1 {
		t.Fatalf("want exactly one warning for %d dials, got %d: %q", dials, len(lines), lines)
	}
	w := lines[0]
	for _, want := range []string{"WARNING", `"legacy"`, "v3", "mdf_legacy", `tenant "acme"`, "headers.Authorization", "LOOMCYCLE_MCP_REFUSE_UNATTRIBUTED_ENV=1"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning lacks %q: %s", want, w)
		}
	}
	if strings.Contains(w, "globex-secret-value") {
		t.Errorf("warning carries the env value: %s", w)
	}

	// A different stored version gets its own warning; the operator's def none.
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "legacy", Transport: "http", URL: peer.srv.URL,
		Headers: keyHeader, DefID: "mdf_legacy4", Version: 4})
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "blessed", Transport: "http", URL: peer.srv.URL,
		Headers: keyHeader, OperatorAuthored: true})
	for _, name := range []string{"legacy", "blessed"} {
		if _, err := build("acme", name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if lines := logs.all(); len(lines) != 2 || !strings.Contains(lines[1], "v4") {
		t.Fatalf("want one more warning, for v4 only, got %q", lines)
	}
}
