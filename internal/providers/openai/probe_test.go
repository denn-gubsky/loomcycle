package openai

import (
	"context"
	"fmt"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeModelsServer serves a canned /models response.
func fakeModelsServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
}

func TestListModels_HappyPath(t *testing.T) {
	body := `{
		"object": "list",
		"data": [
			{"id": "gpt-5.4-mini", "object": "model", "created": 1700000000},
			{"id": "gpt-5.4",      "object": "model", "created": 1700000001},
			{"id": "gpt-5.5",      "object": "model", "created": 1700000002}
		]
	}`
	srv := fakeModelsServer(t, http.StatusOK, body)
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	models, err := d.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 3 || models[0] != "gpt-5.4-mini" {
		t.Errorf("models = %v, want [gpt-5.4-mini, gpt-5.4, gpt-5.5]", models)
	}
}

func TestListModels_DeepSeekShape(t *testing.T) {
	// DeepSeek's wrapper delegates to OpenAI's ListModels — verify
	// the parser handles DeepSeek's actual response shape (which is
	// OpenAI-compatible). Same data{} array, deepseek-prefixed IDs.
	body := `{
		"object": "list",
		"data": [
			{"id": "deepseek-v4-flash", "object": "model"},
			{"id": "deepseek-v4-pro",   "object": "model"}
		]
	}`
	srv := fakeModelsServer(t, http.StatusOK, body)
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	models, err := d.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels (DeepSeek shape): %v", err)
	}
	if len(models) != 2 || models[0] != "deepseek-v4-flash" {
		t.Errorf("models = %v, want [deepseek-v4-flash, deepseek-v4-pro]", models)
	}
}

func TestProbe_AuthFailure(t *testing.T) {
	srv := fakeModelsServer(t, http.StatusUnauthorized,
		`{"error": {"message": "Incorrect API key", "type": "invalid_request_error"}}`)
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	if err := d.Probe(context.Background()); err == nil {
		t.Fatal("Probe should error on 401")
	}
}
