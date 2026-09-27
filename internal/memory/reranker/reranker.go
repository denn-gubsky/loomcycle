// Package reranker builds the model the opt-in search rerank calls, from the
// operator's memory.reranker block.
//
// It is the model half only: the prompt the model is asked and the repair of its
// reply live in internal/memory (RerankTexts), so every configured model is asked
// the same measured question.
package reranker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providerbuild"
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
// at construction and only read afterwards.
type Model struct {
	provider      providers.Provider
	providerID    string
	model         string
	timeout       time.Duration
	effort        string
	contextTokens int

	// Acquire holds one of the provider's concurrency slots for the duration of
	// a call, so a burst of reranked searches cannot starve the runs sharing that
	// provider. nil = no gate.
	Acquire func(ctx context.Context, providerID string) (release func(), err error)
	// OnUsage records the call's tokens against the run whose context it is. The
	// rerank is spent on that run's behalf, so it belongs in that run's cost.
	// nil = not recorded.
	OnUsage func(ctx context.Context, u *providers.Usage)
}

// Build constructs the reranker declared in cfg.Memory.Reranker, or returns nil
// when none is declared. A declared reranker that cannot be built is an error:
// an operator who configured one expects it to run, and would otherwise learn
// otherwise only from `reranked: false` on every search.
func Build(cfg *config.Config) (*Model, error) {
	rc := cfg.Memory.Reranker
	if !rc.Configured() {
		return nil, nil
	}
	pc, ok := cfg.Providers[rc.Provider]
	if !ok {
		known := make([]string, 0, len(cfg.Providers))
		for k := range cfg.Providers {
			known = append(known, k)
		}
		sort.Strings(known)
		return nil, fmt.Errorf("memory.reranker.provider: %q is not declared in providers (declared: %v)", rc.Provider, known)
	}
	opts := providerbuild.DriverOptions(rc.Provider, pc, cfg)
	// The reranker's own block wins over the provider's endpoint and key, as the
	// embedder's does — the point of the two knobs is to diverge on purpose.
	if rc.BaseURL != "" {
		opts.BaseURL = rc.BaseURL
	}
	if rc.APIKeyEnv != "" {
		opts.APIKey = os.Getenv(rc.APIKeyEnv)
		opts.KeyEnvName = rc.APIKeyEnv
		if opts.APIKey == "" {
			log.Printf("memory.reranker: api_key_env=%s is set but empty — the reranker will call %s unauthenticated",
				rc.APIKeyEnv, rc.Provider)
		}
	}
	p, err := providers.NewDriver(pc.Driver, opts)
	if err != nil {
		return nil, fmt.Errorf("memory.reranker: provider %q: %w", rc.Provider, err)
	}
	return New(p, rc), nil
}

// New wraps an already-constructed provider with the reranker settings. Build
// is the production path; New is how a test supplies a stub provider.
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

// ProviderID and ModelID name what serves the rerank, for logs and reports.
func (m *Model) ProviderID() string { return m.providerID }
func (m *Model) ModelID() string    { return m.model }

// Complete sends one prompt and returns the reply text. A deadline hit wraps
// context.DeadlineExceeded so the caller can report it as a timeout.
func (m *Model) Complete(ctx context.Context, prompt string) (string, error) {
	if m.Acquire != nil {
		release, err := m.Acquire(ctx, m.providerID)
		if err != nil {
			return "", err
		}
		defer release()
	}
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
	ch, err := m.provider.Call(callCtx, req)
	if err != nil {
		return "", deadlineAware(callCtx, err)
	}
	var b strings.Builder
	var callErr error
	var usage *providers.Usage
	// Drain to completion even after an error: abandoning the channel leaves the
	// driver's goroutine blocked on a send.
	for ev := range ch {
		if ev.Usage != nil {
			usage = ev.Usage
		}
		switch ev.Type {
		case providers.EventText:
			b.WriteString(ev.Text)
		case providers.EventError:
			if callErr == nil {
				callErr = errors.New(ev.Error)
			}
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
