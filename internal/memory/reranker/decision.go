package reranker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/providerbuild"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// The decision kind (memory.reranker.kind: decision): one typed choice over the
// candidates, asked of a decision model through the shared decision driver
// (internal/decision), and the candidates ordered by the probability it returns
// for each.
//
// WHY NOT THE LISTWISE PROMPT: a decision model returns no text. The answer is a
// probability per named option, so the order is the answer: nothing to repair,
// and no way to name a passage that is not in the pool. Measured on ConditionalQA
// (800 questions) it matched the listwise rerank at R@5 (0.944 vs 0.935) and on
// LoCoMo memory recall (0.739 vs 0.741 recall@5), at about a third of the latency.
// It is worse at the very top (R@1 0.776 vs 0.813), which is why it is opt-in.

const (
	// decisionMaxOptions is the most candidates one choice may name: the options are
	// the letters A–Z, and nimble takes 2 to 26. A larger pool is shown its first
	// 26 and the rest keep their places below them.
	decisionMaxOptions = 26
	// decisionInstruction is the question the probe measured, verbatim.
	decisionInstruction = "Which passage best answers the question?"
	// decisionQuestion is the name the one question is asked, and answered, under.
	decisionQuestion = "best"
	letters          = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

// Decision is one configured decision-kind reranker. Safe for concurrent use:
// every field is set at boot and only read afterwards.
type Decision struct {
	// driver makes the call: the endpoint, the key rule, the timeout and the
	// bound on calls in flight are its own.
	driver     decision.Driver
	model      string
	providerID string
	// baseURL and keyEnvName are what the block resolved to: the endpoint called,
	// and the credential name a tenant's own key would be stored under. The driver
	// holds its own copies; these are kept so the resolution can be read back.
	baseURL    string
	keyEnvName string
	onUsage    func(ctx context.Context, u *providers.Usage)
}

// buildDecision builds the decision reranker rc declares. The endpoint and key
// resolve exactly as a listwise reranker's (providerbuild.ServiceDriverOptions);
// the provider must be an Ollama one: the option letters, the 26-option clamp
// and the shrink-and-retry below were measured against Ollama's decision models.
func buildDecision(cfg *config.Config, rc config.RerankerConfig) (*Decision, error) {
	opts, provider, model, driver, err := providerbuild.ServiceDriverOptions(cfg, "memory.reranker", providerbuild.ServiceEndpoint{
		Provider: rc.Provider, Model: rc.Model, BaseURL: rc.BaseURL, APIKeyEnv: rc.APIKeyEnv,
	}, rerankerKeyEnvName)
	if err != nil {
		return nil, err
	}
	if driver != "ollama" {
		return nil, fmt.Errorf("memory.reranker: kind: decision needs an Ollama provider (Ollama serves /v1/systemone); %q uses the %q driver", provider, driver)
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, fmt.Errorf("memory.reranker: provider %q has no base URL", provider)
	}
	timeout := time.Duration(rc.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	n := rc.MaxConcurrent
	if n <= 0 {
		n = defaultMaxConcurrent
	}
	baseURL := strings.TrimRight(opts.BaseURL, "/")
	drv, err := decision.New(driver, decision.Options{
		ProviderID: provider, BaseURL: baseURL,
		APIKey: opts.APIKey, KeyEnvName: opts.KeyEnvName,
		Timeout: timeout, MaxConcurrent: n,
	})
	if err != nil {
		return nil, fmt.Errorf("memory.reranker: %w", err)
	}
	return &Decision{
		driver: drv, model: model, providerID: provider,
		baseURL: baseURL, keyEnvName: opts.KeyEnvName,
	}, nil
}

func (d *Decision) ProviderID() string { return d.providerID }
func (d *Decision) ModelID() string    { return d.model }
func (d *Decision) Kind() string       { return config.RerankerKindDecision }

// SetOnUsage records each call's tokens against the run whose context it is.
func (d *Decision) SetOnUsage(f func(ctx context.Context, u *providers.Usage)) { d.onUsage = f }

// Rank orders texts for query. It never fails: every fault keeps the identity
// order, the search's own, and says why.
func (d *Decision) Rank(ctx context.Context, query string, texts []string, maxChars int) ([]int, memory.RerankReport) {
	n := len(texts)
	identity := make([]int, n)
	for i := range identity {
		identity[i] = i
	}
	if n < 2 {
		return identity, memory.RerankReport{Reason: memory.RerankTooFewCandidates}
	}
	// More candidates than the model has options: the first decisionMaxOptions are
	// ranked and the rest keep their places (operator decision: clamp and report).
	shown := n
	if shown > decisionMaxOptions {
		shown = decisionMaxOptions
	}
	if maxChars <= 0 {
		maxChars = memory.DefaultRerankMaxChars
	}
	// A prompt the model refuses as too large is retried with every candidate cut
	// to half, then a quarter, of its characters: nimble never truncates input, so
	// the alternative to a smaller prompt is no rerank at all. The shortening is
	// this caller's choice; the driver itself refuses rather than trims.
	var probs map[string]float64
	var err error
	for _, chars := range []int{maxChars, maxChars / 2, maxChars / 4} {
		probs, err = d.choose(ctx, query, texts[:shown], chars)
		if decision.CodeOf(err) != decision.CodePromptTooLarge {
			break
		}
	}
	if err != nil {
		switch {
		case decision.CodeOf(err) == decision.CodePromptTooLarge:
			return identity, memory.RerankReport{Reason: memory.RerankPromptTooLarge, Candidates: shown}
		case errors.Is(err, context.DeadlineExceeded):
			return identity, memory.RerankReport{Reason: memory.RerankTimeout, Candidates: shown}
		default:
			return identity, memory.RerankReport{Reason: memory.RerankCallFailed, Candidates: shown}
		}
	}
	ranked := make([]int, shown)
	for i := range ranked {
		ranked[i] = i
	}
	// Ties keep pool order: a candidate the model scored no higher than an earlier
	// one never overtakes it.
	sort.SliceStable(ranked, func(a, b int) bool {
		return probs[string(letters[ranked[a]])] > probs[string(letters[ranked[b]])]
	})
	return append(ranked, identity[shown:]...), memory.RerankReport{Applied: true, Candidates: shown}
}

// choose asks one choice over texts, each cut to chars runes, and returns the
// probability per option letter.
func (d *Decision) choose(ctx context.Context, query string, texts []string, chars int) (map[string]float64, error) {
	options := make(map[string]string, len(texts))
	for i, t := range texts {
		options[string(letters[i])] = truncateRunes(t, chars)
	}
	criteria, err := json.Marshal(options)
	if err != nil {
		return nil, err
	}
	resp, err := d.driver.Decide(ctx, decision.Request{
		Model: d.model,
		State: map[string]any{"question": query},
		Questions: map[string]decision.Question{decisionQuestion: {
			Type: decision.TypeChoice, Instructions: decisionInstruction, Criteria: criteria,
		}},
	})
	if err != nil {
		return nil, err
	}
	if d.onUsage != nil && (resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0) {
		u := resp.Usage
		d.onUsage(ctx, &u)
	}
	best, ok := resp.Answers[decisionQuestion]
	if !ok || len(best.Probabilities) == 0 {
		return nil, errors.New("the decision reply carries no probabilities")
	}
	return best.Probabilities, nil
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}
