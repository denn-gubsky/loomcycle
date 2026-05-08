package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeModelsServer serves a canned /v1/models response. Asserts the
// request carries the Anthropic auth headers we wrote in the driver.
func fakeModelsServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("x-api-key = %q, want test-key", got)
		}
		if got := r.Header.Get("anthropic-version"); got != apiVersion {
			t.Errorf("anthropic-version = %q, want %s", got, apiVersion)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
}

func TestListModels_HappyPath(t *testing.T) {
	body := `{
		"data": [
			{"id": "claude-haiku-4-5",  "type": "model", "display_name": "Claude Haiku 4.5"},
			{"id": "claude-sonnet-4-6", "type": "model", "display_name": "Claude Sonnet 4.6"},
			{"id": "claude-opus-4-7",   "type": "model", "display_name": "Claude Opus 4.7"}
		],
		"first_id": "claude-haiku-4-5",
		"last_id": "claude-opus-4-7",
		"has_more": false
	}`
	srv := fakeModelsServer(t, http.StatusOK, body)
	defer srv.Close()

	d := New("test-key", srv.URL, nil)
	models, err := d.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("got %d models, want 3", len(models))
	}
	if models[0] != "claude-haiku-4-5" {
		t.Errorf("models[0] = %q, want claude-haiku-4-5", models[0])
	}
}

func TestProbe_HappyPath(t *testing.T) {
	srv := fakeModelsServer(t, http.StatusOK, `{"data": [{"id": "claude-opus-4-7"}]}`)
	defer srv.Close()

	d := New("test-key", srv.URL, nil)
	if err := d.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestProbe_AuthFailure(t *testing.T) {
	// 401 from Anthropic — bad/missing API key. Probe must surface
	// this as an error so the resolver flips Reachable=false.
	srv := fakeModelsServer(t, http.StatusUnauthorized,
		`{"type": "error", "error": {"type": "authentication_error", "message": "invalid x-api-key"}}`)
	defer srv.Close()

	d := New("test-key", srv.URL, nil)
	err := d.Probe(context.Background())
	if err == nil {
		t.Fatal("Probe should error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want it to mention status 401", err)
	}
}

func TestProbe_NetworkFailure(t *testing.T) {
	// Server immediately closes — driver must surface the network
	// error rather than silently treat the provider as healthy.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	d := New("test-key", srv.URL, nil)
	if err := d.Probe(context.Background()); err == nil {
		t.Fatal("Probe should error on dropped connection")
	}
}
