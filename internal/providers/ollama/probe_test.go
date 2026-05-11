package ollama

import (
	"context"
	"fmt"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeTagsServer serves a canned /api/tags response. Ollama doesn't
// require auth, so no header assertions.
func fakeTagsServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("path = %q, want /api/tags", r.URL.Path)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
}

func TestListModels_HappyPath(t *testing.T) {
	body := `{
		"models": [
			{"name": "qwen3:14b",   "modified_at": "2026-05-08T09:06:12Z", "size": 9276198565},
			{"name": "gemma4:9b",   "modified_at": "2026-05-01T00:00:00Z", "size": 5500000000},
			{"name": "kimi-k2.6",   "modified_at": "2026-04-30T00:00:00Z", "size": 14000000000}
		]
	}`
	srv := fakeTagsServer(t, http.StatusOK, body)
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	models, err := d.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 3 || models[0] != "qwen3:14b" {
		t.Errorf("models = %v, want [qwen3:14b, gemma4:9b, kimi-k2.6]", models)
	}
}

func TestListModels_NoModelsPulled(t *testing.T) {
	// Fresh Ollama install with no models pulled — empty array, NOT
	// an error. The resolver treats this as "provider reachable, no
	// candidates available" — distinct from probe failure.
	srv := fakeTagsServer(t, http.StatusOK, `{"models": []}`)
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	models, err := d.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels (empty): %v", err)
	}
	if len(models) != 0 {
		t.Errorf("models = %v, want empty slice", models)
	}
}

func TestProbe_HappyPath(t *testing.T) {
	srv := fakeTagsServer(t, http.StatusOK, `{"models": []}`)
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	if err := d.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestProbe_ServerDown(t *testing.T) {
	// 500 from Ollama — daemon stuck or restarting. Probe must
	// fail so the resolver flips reachability.
	srv := fakeTagsServer(t, http.StatusInternalServerError, "ollama: server error")
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	if err := d.Probe(context.Background()); err == nil {
		t.Fatal("Probe should error on 500")
	}
}
