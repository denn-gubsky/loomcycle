package providers

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeEmbedder struct {
	err   error
	calls int
}

func (f *fakeEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return [][]float32{{0.1}}, nil
}
func (f *fakeEmbedder) Model() string    { return "embeddinggemma:latest" }
func (f *fakeEmbedder) Provider() string { return "ollama-local" }
func (f *fakeEmbedder) Dimension() int   { return 1 }

// TestObserveEmbedder_UntriedIsNotHealthy.
//
// A configured-but-never-called embedder must NOT report ok. Claiming health that
// has not been observed is the same class of lie as a disabled change feed reading
// as a quiet one — and it is the state a freshly booted deployment is in, so it is
// the state a reader sees most often.
func TestObserveEmbedder_UntriedIsNotHealthy(t *testing.T) {
	o := ObserveEmbedder(&fakeEmbedder{})
	h := o.Health()
	if h.State != EmbedderUntried {
		t.Errorf("state = %q, want %q", h.State, EmbedderUntried)
	}
	if h.Calls != 0 || h.Failures != 0 {
		t.Errorf("counts should be zero before any call, got %d/%d", h.Calls, h.Failures)
	}
	// Provider/model are knowable without a call and are what capabilities already
	// discloses, so they belong even in the untried state.
	if h.Provider != "ollama-local" || h.Model != "embeddinggemma:latest" {
		t.Errorf("provider/model missing: %+v", h)
	}
}

// TestObserveEmbedder_TracksOutcomesAndRecovers.
//
// The count is the actionable part — it is how many rows need re-embedding — and
// recovery must flip the state back, or an operator who fixed the embedder still
// sees "failing" and re-embeds forever.
func TestObserveEmbedder_TracksOutcomesAndRecovers(t *testing.T) {
	f := &fakeEmbedder{}
	o := ObserveEmbedder(f)
	ctx := context.Background()

	if _, err := o.Embed(ctx, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if got := o.Health().State; got != EmbedderOK {
		t.Errorf("after a success, state = %q, want ok", got)
	}

	f.err = context.DeadlineExceeded
	for i := 0; i < 3; i++ {
		if _, err := o.Embed(ctx, []string{"a"}); err == nil {
			t.Fatal("expected the injected failure")
		}
	}
	h := o.Health()
	if h.State != EmbedderFailing {
		t.Errorf("state = %q, want failing", h.State)
	}
	if h.Failures != 3 || h.Calls != 4 {
		t.Errorf("calls/failures = %d/%d, want 4/3", h.Calls, h.Failures)
	}
	if h.LastFailKind != "timeout" {
		t.Errorf("last_failure_kind = %q, want timeout", h.LastFailKind)
	}

	f.err = nil
	if _, err := o.Embed(ctx, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if got := o.Health().State; got != EmbedderOK {
		t.Errorf("after recovery, state = %q, want ok — a stuck `failing` sends an "+
			"operator re-embedding a scope that is already fine", got)
	}
}

// TestObserveEmbedder_NeverLeaksTheErrorText.
//
// The error text is the one thing that cannot be forwarded: a transport failure
// reads `dial tcp 192.168.0.77:11434: connect: connection refused`, which is a map
// of the operator's network — the same reason the capabilities report withholds the
// embedder's base URL. Only a classified kind may escape.
func TestObserveEmbedder_NeverLeaksTheErrorText(t *testing.T) {
	secret := "dial tcp 192.168.0.77:11434: connect: connection refused"
	o := ObserveEmbedder(&fakeEmbedder{err: errors.New(secret)})
	if _, err := o.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected the injected failure")
	}
	h := o.Health()
	if h.LastFailKind != "other" {
		t.Errorf("kind = %q, want other (no sentinel matched)", h.LastFailKind)
	}
	for _, needle := range []string{"192.168", "11434", "dial", "refused"} {
		if strings.Contains(h.LastFailKind, needle) {
			t.Errorf("health leaked %q from the error text: %+v", needle, h)
		}
	}
}

// TestObserveEmbedder_NilStaysNil.
//
// Every consumer gates on `if Embedder == nil`. Wrapping nil into a non-nil
// interface holding a nil pointer is exactly how that check silently stops firing,
// and the symptom would be a nil dereference on the first write of a deployment
// that configured no embedder at all.
func TestObserveEmbedder_NilStaysNil(t *testing.T) {
	if o := ObserveEmbedder(nil); o != nil {
		t.Fatalf("ObserveEmbedder(nil) = %v, want nil", o)
	}
	var e Embedder = ObserveEmbedder(nil)
	_ = e // documents the hazard: this assignment is why the nil check above exists
}

// TestObserveEmbedder_PassesThroughTheDriverIdentity.
//
// Dimension in particular is load-bearing: the Memory tool refuses a search whose
// dimension does not match the stored rows, so a decorator that reported its own
// zero would break every vector search on a wrapped deployment.
func TestObserveEmbedder_PassesThroughTheDriverIdentity(t *testing.T) {
	o := ObserveEmbedder(&fakeEmbedder{})
	if o.Dimension() != 1 || o.Provider() != "ollama-local" || o.Model() != "embeddinggemma:latest" {
		t.Errorf("identity not passed through: dim=%d provider=%q model=%q",
			o.Dimension(), o.Provider(), o.Model())
	}
	if _, ok := o.Unwrap().(*fakeEmbedder); !ok {
		t.Error("Unwrap should return the wrapped driver")
	}
}
