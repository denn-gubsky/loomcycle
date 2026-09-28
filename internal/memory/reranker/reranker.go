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
	// defaultMaxConcurrent bounds reranks in flight across the process. Each one
	// is a ~6,000-token prompt, and they arrive per SEARCH: a fan-out of agents,
	// each searching several times a turn, would otherwise send every one at
	// once, whatever the operator set for the provider.
	defaultMaxConcurrent = 4
	// rerankerKeyEnvName is the credential name a reranker answers to when the
	// operator pointed it at its own endpoint without naming a key. See Build.
	rerankerKeyEnvName = "LOOMCYCLE_RERANKER_API_KEY"
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
	// slots bounds reranks in flight (memory.reranker.max_concurrent). A rerank
	// waiting for one spends its own timeout doing so, and one that times out
	// waiting keeps search's order like any other timeout.
	slots chan struct{}

	// NO PROVIDER CONCURRENCY SLOT IS TAKEN, deliberately. The per-provider gates
	// cap in-flight RUNS, and a rerank runs inside a run that already holds a slot
	// on its own provider: with the reranker on that same provider and a cap of 1
	// (the usual setting for a local model host), the rerank would wait on the
	// slot its own run holds until the gate timed out. It is part of the run's
	// work, like a tool call, and is admitted by the run's admission.

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
	// The model may be a models: alias (e.g. local-medium); resolve it, and the
	// provider it carries when the block names none, as an agent's model resolves.
	provider, model, err := cfg.ExpandServiceModel("memory.reranker", rc.Provider, rc.Model)
	if err != nil {
		return nil, err
	}
	rc.Provider, rc.Model = provider, model
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
	} else if rc.BaseURL != "" {
		// A TENANT'S PROVIDER KEY MUST NOT TRAVEL TO AN ENDPOINT THAT PROVIDER DOES
		// NOT RUN. The driver resolves a tenant's own stored credential by its key
		// name (OPENAI_API_KEY for the openai driver) before falling back to the
		// operator's, so a reranker pointed at an operator's own endpoint would hand
		// every such tenant's real vendor key to that endpoint — and book the call
		// as tenant-paid for an account it never touched. A name no provider uses
		// means only a credential stored FOR the reranker can override.
		opts.KeyEnvName = rerankerKeyEnvName
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
	n := rc.MaxConcurrent
	if n <= 0 {
		n = defaultMaxConcurrent
	}
	m.slots = make(chan struct{}, n)
	return m
}

// ProviderID and ModelID name what serves the rerank, for logs and reports.
func (m *Model) ProviderID() string { return m.providerID }
func (m *Model) ModelID() string    { return m.model }

// Complete sends one prompt and returns the reply text. A deadline hit wraps
// context.DeadlineExceeded so the caller can report it as a timeout.
func (m *Model) Complete(ctx context.Context, prompt string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-callCtx.Done():
		return "", deadlineAware(callCtx, callCtx.Err())
	}

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
	done := false
	// Drain to completion even after an error: abandoning the channel leaves the
	// driver's goroutine blocked on a send.
	for ev := range ch {
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
