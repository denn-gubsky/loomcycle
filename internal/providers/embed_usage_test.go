package providers

import (
	"context"
	"errors"
	"testing"
)

// tokenEmbedder reports tokens per text the way a driver does, once per call.
type tokenEmbedder struct {
	perText int // 0 = the provider reports no count
	err     error
}

func (e *tokenEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if e.perText > 0 {
		ReportEmbedTokens(ctx, e.perText*len(texts))
	}
	if e.err != nil {
		return nil, e.err
	}
	return make([][]float32, len(texts)), nil
}
func (e *tokenEmbedder) Model() string    { return "m" }
func (e *tokenEmbedder) Provider() string { return "p" }
func (e *tokenEmbedder) Dimension() int   { return 1 }

// TestObservedEmbedder_ReportsEachCall — every call reaches the hook with its
// characters (runes, not bytes) and the tokens the driver reported; a call
// whose provider counted nothing is reported with its characters alone.
func TestObservedEmbedder_ReportsEachCall(t *testing.T) {
	inner := &tokenEmbedder{perText: 5}
	o := ObserveEmbedder(inner)
	var got []EmbedCall
	o.SetOnCall(func(_ context.Context, c EmbedCall) { got = append(got, c) })

	if _, err := o.Embed(context.Background(), []string{"привіт", "hi"}); err != nil {
		t.Fatal(err)
	}
	inner.perText = 0
	if _, err := o.EmbedQuery(context.Background(), []string{"query"}); err != nil {
		t.Fatal(err)
	}
	want := []EmbedCall{
		{Provider: "p", Model: "m", Texts: 2, Chars: 8, InputTokens: 10, TokensReported: true},
		{Provider: "p", Model: "m", Texts: 1, Chars: 5},
	}
	if len(got) != len(want) {
		t.Fatalf("hook saw %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestObservedEmbedder_AFailedCallIsReportedOnlyIfCounted — a call that never
// reached the model consumed nothing worth a row; one the provider counted
// before failing did.
func TestObservedEmbedder_AFailedCallIsReportedOnlyIfCounted(t *testing.T) {
	inner := &tokenEmbedder{err: errors.New("down")}
	o := ObserveEmbedder(inner)
	calls := 0
	o.SetOnCall(func(context.Context, EmbedCall) { calls++ })
	_, _ = o.Embed(context.Background(), []string{"a"})
	if calls != 0 {
		t.Errorf("an uncounted failed call was reported")
	}
	inner.perText = 3
	_, _ = o.Embed(context.Background(), []string{"a"})
	if calls != 1 {
		t.Errorf("a counted failed call was not reported")
	}
}

// TestObservedEmbedder_AddsToAnOuterAccumulator — the embeddings gateway puts
// its own accumulator on ctx to learn whether the provider counted the call;
// the observer's per-call accumulator must hand the count on to it.
func TestObservedEmbedder_AddsToAnOuterAccumulator(t *testing.T) {
	o := ObserveEmbedder(&tokenEmbedder{perText: 4})
	ctx, outer := WithEmbedTokens(context.Background())
	if _, err := o.Embed(ctx, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if n, ok := outer.Total(); !ok || n != 8 {
		t.Errorf("outer accumulator = %d (%v), want 8", n, ok)
	}
}
