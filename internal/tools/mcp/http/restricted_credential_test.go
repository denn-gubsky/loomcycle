package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// keySeen records the X-Api-Key values a server received.
type keySeen struct {
	mu   sync.Mutex
	keys []string
}

func (k *keySeen) add(v string) {
	if v == "" {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = append(k.keys, v)
}

func (k *keySeen) all() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.keys...)
}

// mcpServer is a TLS MCP server that answers initialize and tools/call and
// records the X-Api-Key of every request.
func mcpServer(t *testing.T, seen *keySeen) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.add(r.Header.Get("X-Api-Key"))
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		switch probe.Method {
		case "initialize":
			writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": *probe.ID, "result": map[string]any{
				"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{},
				"serverInfo": map[string]any{"name": "f", "version": "1"}}})
		default:
			writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": *probe.ID, "result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "ok"}}}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A credential restricted by allowed_urls, in an MCP server's header, through
// the real credential engine: sent to a server its list covers, never to one
// it does not, and never carried on by a redirect from a covered server to
// another host. A custom header is the case that matters: Go forwards it to
// any host on a redirect. An unrestricted credential behaves as before.
func TestRestrictedCredential_ReachesOnlyTheURLsItAllows(t *testing.T) {
	sinkSeen, coveredSeen := &keySeen{}, &keySeen{}
	sink := mcpServer(t, sinkSeen)
	covered := mcpServer(t, coveredSeen)
	hop := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(hop.Close)

	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	engine := credential.NewEngine(st, sealer)
	put := func(name, value string, allowed []string) {
		t.Helper()
		var list *[]string
		if allowed != nil {
			if list, err = credential.NormalizeAllowedURLs(&allowed); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := engine.PutInline(context.Background(), credential.Identity{TenantID: "t", Scope: "tenant", Name: name}, value, nil, list, nil); err != nil {
			t.Fatal(err)
		}
	}
	put("key", "restricted-value", []string{covered.URL, hop.URL})
	put("open", "unrestricted-value", nil)

	// As cmd/loomcycle wires it.
	substitute := func(ctx context.Context, s, targetURL string) (string, []string, error) {
		ri := tools.RunIdentity(ctx)
		return engine.Substitute(ctx, ri.TenantID, tools.AgentName(ctx), ri.UserID, s, targetURL, nil)
	}
	call := func(url, ref string) error {
		t.Helper()
		c, err := New(Config{
			URL:            url,
			Headers:        map[string]string{"X-Api-Key": ref},
			HTTPClient:     covered.Client(), // trusts httptest's certificate, which all three share
			CredSubstitute: substitute,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t", AgentID: "a"})
		if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
			return err
		}
		_, err = mcp.CallTool(ctx, c, "search", json.RawMessage(`{}`))
		return err
	}

	// A covered server gets the value: before, a restricted credential was
	// refused in every header.
	if err := call(covered.URL, "$cred:key"); err != nil {
		t.Fatalf("a covered server: %v", err)
	}
	if got := coveredSeen.all(); len(got) == 0 || got[0] != "restricted-value" {
		t.Errorf("the covered server saw %v", got)
	}

	// A server the list does not cover never sees it.
	if err := call(sink.URL, "$cred:key"); err == nil {
		t.Error("a server outside the list was called with the restricted credential")
	}
	// A covered server that redirects elsewhere does not carry it there.
	if err := call(hop.URL, "$cred:key"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("a redirect from a covered server: err=%v, want it refused", err)
	}
	if got := sinkSeen.all(); len(got) != 0 {
		t.Fatalf("the restricted credential reached a host outside its list: %v", got)
	}

	// Unchanged: an unrestricted credential follows the redirect, as it did.
	if err := call(hop.URL, "$cred:open"); err != nil {
		t.Fatalf("an unrestricted credential through a redirect: %v", err)
	}
	if got := sinkSeen.all(); len(got) == 0 || got[0] != "unrestricted-value" {
		t.Errorf("the unrestricted credential's redirect: sink saw %v", got)
	}
}
