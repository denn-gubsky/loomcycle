// Package reranker is the model the opt-in search rerank calls. internal/wiring
// builds it from the operator's memory.reranker block.
//
// It is the model half only: the prompt the model is asked and the repair of its
// reply live in internal/memory (RerankTexts), so every configured model is asked
// the same measured question.
package reranker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

const (
	defaultTimeout       = 30 * time.Second
	defaultContextTokens = 16384
	// A ranking of twenty numbers is well under a hundred tokens; this is a
	// ceiling against a model that keeps talking, not a budget.
	maxOutputTokens = 256
)

// Model is one configured reranker. Safe for concurrent use: every field is set
// at boot, before the server serves, and only read afterwards.
type Model struct {
	provider      providers.Provider
	providerID    string
	model         string
	timeout       time.Duration
	effort        string
	contextTokens int

	// A RERANK IS A CHAT CALL ON ITS PROVIDER'S LANE. Each one is a ~6,000-token
	// prompt, and they arrive per SEARCH: a fan-out of agents, each searching
	// several times a turn, sends many at once. The reranker makes its calls on
	// a driver of its own (providerbuild.ServiceProvider), so internal/wiring
	// wraps that driver in the provider's chat lane (lanegate.WrapWithFloor): the
	// provider's max_concurrent counts a rerank like any other chat call. While
	// the operator has set no cap on that provider, the wrapper's built-in floor
	// (lanegate.FloorCap in flight) keeps a fan-out from sending every rerank at
	// once. The wait for a slot is inside the rerank's timeout, and a rerank
	// that times out waiting keeps search's order like any other timeout.

	// OnUsage records the call's tokens against the run whose context it is. The
	// rerank is spent on that run's behalf, so it belongs in that run's cost.
	// nil = not recorded.
	OnUsage func(ctx context.Context, u *providers.Usage)
}

// Reranker is a built reranker of either kind: what a search ranks with, plus
// what logs and the usage ledger need to know about it.
type Reranker interface {
	memory.Ranker
	ProviderID() string
	ModelID() string
	Kind() string
	SetOnUsage(func(ctx context.Context, u *providers.Usage))
}

// New wraps an already-constructed provider with the reranker settings. In
// production internal/wiring builds the provider, in its chat lane, and rc with
// the provider and model the block resolved to; a test supplies a stub.
func New(p providers.Provider, rc config.RerankerConfig) *Model {
	m := &Model{
		provider:      p,
		providerID:    rc.Provider,
		model:         rc.Model,
		timeout:       time.Duration(rc.TimeoutMs) * time.Millisecond,
		effort:        rc.Effort,
		contextTokens: rc.ContextTokens,
	}
	if m.providerID == "" && p != nil {
		m.providerID = p.ID()
	}
	if m.timeout <= 0 {
		m.timeout = defaultTimeout
	}
	if m.contextTokens <= 0 {
		m.contextTokens = defaultContextTokens
	}
	return m
}

// Rank serves the listwise kind through the measured prompt and reply repair.
func (m *Model) Rank(ctx context.Context, query string, texts []string, maxChars int) ([]int, memory.RerankReport) {
	return memory.RerankTexts(ctx, m, query, texts, maxChars)
}

// Kind is the listwise kind.
func (m *Model) Kind() string { return config.RerankerKindListwise }

// DefaultCandidates is the listwise kind's measured pool.
func (m *Model) DefaultCandidates() int { return memory.DefaultListwiseRerankCandidates }

// SetOnUsage records each call's tokens against the run whose context it is.
func (m *Model) SetOnUsage(f func(ctx context.Context, u *providers.Usage)) { m.OnUsage = f }

// ProviderID and ModelID name what serves the rerank, for logs and reports.
func (m *Model) ProviderID() string { return m.providerID }
func (m *Model) ModelID() string    { return m.model }

// Complete sends one prompt and returns the reply text. A deadline hit wraps
// context.DeadlineExceeded so the caller can report it as a timeout.
func (m *Model) Complete(ctx context.Context, prompt string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	zero := 0.0
	req := providers.Request{
		Model:            m.model,
		MaxTokens:        maxOutputTokens,
		Temperature:      &zero, // the ranking should be a function of the candidates, not of a draw
		Effort:           m.effort,
		MaxContextTokens: m.contextTokens,
		Messages: []providers.Message{{
			Role:    "user",
			Content: []providers.ContentBlock{{Type: "text", Text: prompt}},
		}},
	}
	timer := providers.StartCallTimer(nil)
	ch, err := m.provider.Call(callCtx, req)
	if err != nil {
		return "", deadlineAware(callCtx, err)
	}
	var b strings.Builder
	var callErr error
	var usage *providers.Usage
	done := false
	// Drain to completion even after an error: abandoning the channel leaves the
	// driver's goroutine blocked on a send.
	for ev := range ch {
		timer.Observe(ev)
		if ev.Usage != nil {
			usage = ev.Usage
		}
		switch ev.Type {
		case providers.EventText:
			b.WriteString(ev.Text)
		case providers.EventDone:
			done = true
		case providers.EventError:
			if callErr == nil {
				callErr = errors.New(ev.Error)
			}
		}
	}
	// A REPLY THAT NEVER FINISHED IS NOT A REPLY. A driver's send gives up once
	// its context ends, so a stream cut off by the timeout can close with neither
	// an error nor a done event — and the text so far ("[3, 1, 7,", or an early
	// "passage [4]") would otherwise be parsed as the whole answer. Every driver
	// ends a completed reply with EventDone.
	if callErr == nil && !done {
		if err := callCtx.Err(); err != nil {
			callErr = err
		} else {
			callErr = errors.New("the reply stream ended before it completed")
		}
	}
	// Recorded even for a failed call: tokens a provider reports were spent.
	if usage != nil && m.OnUsage != nil {
		u := *usage
		if u.Provider == "" {
			u.Provider = m.providerID
		}
		if u.Model == "" {
			u.Model = m.model
		}
		timer.Stamp(&u) // a call that failed or was cut off stays unstamped
		m.OnUsage(ctx, &u)
	}
	if callErr != nil {
		return "", deadlineAware(callCtx, callErr)
	}
	return b.String(), nil
}

// deadlineAware reports a call cut off by the reranker's own timeout as
// context.DeadlineExceeded, whatever text the driver wrapped it in.
func deadlineAware(callCtx context.Context, err error) error {
	if errors.Is(callCtx.Err(), context.DeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%v: %w", err, context.DeadlineExceeded)
	}
	return err
}
