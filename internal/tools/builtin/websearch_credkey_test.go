package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// A tenant/user credential named BRAVE_API_KEY overrides the operator host key
// on the actual outbound request; without one the host key is used (RFC AR).
func TestWebSearch_BraveKeyOverride(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Subscription-Token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"web":{"results":[{"title":"t","url":"https://e.com","description":"d"}]}}`))
	}))
	defer srv.Close()

	input := json.RawMessage(`{"query":"hello"}`)

	// (1) No override → host key on the wire.
	ws := &WebSearch{APIKey: "host-key", Endpoint: srv.URL}
	if res, err := ws.Execute(context.Background(), input); err != nil || res.IsError {
		t.Fatalf("host-key call failed: err=%v res=%+v", err, res)
	}
	if gotToken != "host-key" {
		t.Errorf("no override: X-Subscription-Token = %q, want host-key", gotToken)
	}

	// (2) Tenant override → the tenant key is sent instead of the host key.
	ctx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (string, bool) {
		return "tenant-brave", name == "BRAVE_API_KEY"
	})
	if res, err := ws.Execute(ctx, input); err != nil || res.IsError {
		t.Fatalf("override call failed: err=%v res=%+v", err, res)
	}
	if gotToken != "tenant-brave" {
		t.Errorf("override: X-Subscription-Token = %q, want tenant-brave", gotToken)
	}
}

// A tenant may search on its own Brave quota even when the operator set no host
// key: the tenant BRAVE_API_KEY both enables the call and is what's sent.
func TestWebSearch_TenantKeyEnablesWithNoHostKey(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Subscription-Token")
		_, _ = w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer srv.Close()

	ws := &WebSearch{APIKey: "", Endpoint: srv.URL} // operator set no key

	// Without a tenant key the tool refuses (unchanged behavior).
	if res, _ := ws.Execute(context.Background(), json.RawMessage(`{"query":"x"}`)); !res.IsError {
		t.Errorf("no host key + no override should refuse, got %+v", res)
	}

	ctx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (string, bool) {
		return "tenant-brave", name == "BRAVE_API_KEY"
	})
	if res, err := ws.Execute(ctx, json.RawMessage(`{"query":"x"}`)); err != nil || res.IsError {
		t.Fatalf("tenant key should enable the call: err=%v res=%+v", err, res)
	}
	if gotToken != "tenant-brave" {
		t.Errorf("X-Subscription-Token = %q, want tenant-brave", gotToken)
	}
}
