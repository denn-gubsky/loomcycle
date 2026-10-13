package ollama

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// TestModelIdentity_IsTheModelAtTheContextSizeItWillSend — to an Ollama host
// the same model at another num_ctx is a different model: it is reloaded and
// its prompt cache is lost. The identity a provider's models-in-flight cap
// counts must therefore carry the num_ctx the request will actually SEND —
// whichever of the three sources it comes from — and be just the model name
// when the request sends none.
func TestModelIdentity_IsTheModelAtTheContextSizeItWillSend(t *testing.T) {
	var captured []byte
	var psHits int32

	// Pinned on the provider.
	pinnedSrv := numCtxServer(`{"models":[]}`, &captured, &psHits)
	defer pinnedSrv.Close()
	pinned := New("ollama-local", "", pinnedSrv.URL, streamhttp.Options{}, nil).WithNumCtx(65536)
	if got := pinned.ModelIdentity(providers.Request{Model: "qwen3.6:latest"}); got != "qwen3.6:latest@65536" {
		t.Errorf("pinned: identity = %q, want qwen3.6:latest@65536", got)
	}
	// The request's own size wins over the pin, exactly as it does on the wire.
	if got := pinned.ModelIdentity(providers.Request{Model: "qwen3.6:latest", MaxContextTokens: 32768}); got != "qwen3.6:latest@32768" {
		t.Errorf("per-request: identity = %q, want qwen3.6:latest@32768", got)
	}

	// Neither pinned nor requested: the size Ollama reports the model loaded at.
	loadedSrv := numCtxServer(`{"models":[{"name":"qwen3.6:latest","context_length":16384}]}`, &captured, &psHits)
	defer loadedSrv.Close()
	loaded := New("ollama-local", "", loadedSrv.URL, streamhttp.Options{}, nil)
	if got := loaded.ModelIdentity(providers.Request{Model: "qwen3.6:latest"}); got != "qwen3.6:latest@16384" {
		t.Errorf("loaded: identity = %q, want qwen3.6:latest@16384", got)
	}

	// Nothing known: no num_ctx goes on the wire, so the identity is the model.
	unknownSrv := numCtxServer(`{"models":[]}`, &captured, &psHits)
	defer unknownSrv.Close()
	unknown := New("ollama-local", "", unknownSrv.URL, streamhttp.Options{}, nil)
	if got := unknown.ModelIdentity(providers.Request{Model: "qwen3.6:latest"}); got != "qwen3.6:latest" {
		t.Errorf("unknown: identity = %q, want the bare model name", got)
	}

	// The identity and the wire agree: what was named is what is sent.
	if _, err := drain(pinned.Call(context.Background(), providers.Request{Model: "qwen3.6:latest", MaxContextTokens: 32768})); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if n, ok := wireNumCtx(t, captured); !ok || n != 32768 {
		t.Errorf("wire num_ctx = %d (sent=%v), want 32768 — the identity named a size the request did not send", n, ok)
	}

	// It is reachable the way the scheduler reaches it.
	if got := providers.ModelIdentity(pinned, providers.Request{Model: "m"}); got != "m@65536" {
		t.Errorf("providers.ModelIdentity = %q, want m@65536", got)
	}
}

func drain(ch <-chan providers.Event, err error) (int, error) {
	if err != nil {
		return 0, err
	}
	n := 0
	for range ch {
		n++
	}
	return n, nil
}

// TestCall_WarnsOncePerContextSizeMismatch — an agent that asks for its own
// context size on a provider pinned to another makes Ollama swap the model
// between the two sizes, which shows up only as slow calls. The driver says so
// once per (model, requested, pinned): the line names both numbers and both
// knobs, a new combination warns again, and matching sizes never warn.
func TestCall_WarnsOncePerContextSizeMismatch(t *testing.T) {
	var captured []byte
	var psHits int32
	srv := numCtxServer(`{"models":[]}`, &captured, &psHits)
	defer srv.Close()
	d := New("ollama-local", "", srv.URL, streamhttp.Options{}, nil).WithNumCtx(65536)
	call := func(model string, requested int) {
		t.Helper()
		if _, err := drain(d.Call(context.Background(), providers.Request{Model: model, MaxContextTokens: requested})); err != nil {
			t.Fatalf("Call: %v", err)
		}
	}
	const marker = "reloads the model"

	out := captureLog(func() {
		call("qwen3.6:latest", 32768)
		call("qwen3.6:latest", 32768)
		call("qwen3.6:latest", 32768)
	})
	if n := strings.Count(out, marker); n != 1 {
		t.Fatalf("three calls with one mismatch warned %d times, want once:\n%s", n, out)
	}
	for _, want := range []string{"qwen3.6:latest", "32768", "65536", "max_context_tokens", "LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX", "ollama-local"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing %q; got:\n%s", want, out)
		}
	}

	// Another requested size, and another model, are each a new combination.
	out = captureLog(func() {
		call("qwen3.6:latest", 16384)
		call("glm-4.7-flash", 32768)
		call("qwen3.6:latest", 32768) // already said
	})
	if n := strings.Count(out, marker); n != 2 {
		t.Errorf("two new combinations warned %d times, want 2:\n%s", n, out)
	}

	// No mismatch, no warning: the same size, no per-request size, or no pin.
	unpinned := New("ollama-local", "", srv.URL, streamhttp.Options{}, nil)
	out = captureLog(func() {
		call("other", 65536)
		call("other", 0)
		if _, err := drain(unpinned.Call(context.Background(), providers.Request{Model: "other", MaxContextTokens: 32768})); err != nil {
			t.Fatalf("Call: %v", err)
		}
	})
	if strings.Contains(out, marker) {
		t.Errorf("warned without a mismatch:\n%s", out)
	}
}
