package providers

import (
	"context"
	"sync"
)

// EmbedCall is what one Embed or EmbedQuery call through an ObservedEmbedder
// consumed, handed to its call hook so the server can write a usage row.
//
// Chars is always set (the runtime counts what it sent). InputTokens is what
// the provider reported, and TokensReported says whether it reported anything:
// Gemini's batch endpoint and a Cohere model's body carry no count, and a row
// for such a call is recorded by its characters alone rather than with a guess.
type EmbedCall struct {
	Provider       string
	Model          string
	Texts          int
	Chars          int64
	InputTokens    int
	TokensReported bool
}

// EmbedTokens accumulates the token counts a driver reports while serving one
// embed call. A driver may send several batches, possibly at once, so adds are
// locked.
type EmbedTokens struct {
	mu       sync.Mutex
	n        int
	reported bool
}

type embedTokensKey struct{}

// WithEmbedTokens returns ctx carrying a fresh accumulator for the embed call
// about to be made under it.
func WithEmbedTokens(ctx context.Context) (context.Context, *EmbedTokens) {
	acc := &EmbedTokens{}
	return context.WithValue(ctx, embedTokensKey{}, acc), acc
}

// ReportEmbedTokens adds n tokens to the accumulator on ctx, if any. A driver
// calls it once per batch the provider answered with a count; a ctx with no
// accumulator (a test, the CLI) makes it a no-op.
func ReportEmbedTokens(ctx context.Context, n int) {
	acc, ok := ctx.Value(embedTokensKey{}).(*EmbedTokens)
	if !ok || acc == nil {
		return
	}
	acc.mu.Lock()
	acc.n += n
	acc.reported = true
	acc.mu.Unlock()
}

// Total returns the tokens reported so far and whether any batch reported.
func (e *EmbedTokens) Total() (int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n, e.reported
}

// charsOf counts the characters (runes) of texts, the unit an embedding
// without a token count is recorded and priced by.
func charsOf(texts []string) int64 {
	var n int64
	for _, t := range texts {
		for range t {
			n++
		}
	}
	return n
}
