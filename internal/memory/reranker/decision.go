package reranker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/providerbuild"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// The decision kind (memory.reranker.kind: decision): one typed choice over the
// candidates, asked of a decision model at Ollama's /v1/systemone, and the
// candidates ordered by the probability it returns for each.
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
	decisionPath        = "/v1/systemone"
	letters             = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

// Decision is one configured decision-kind reranker. Safe for concurrent use:
// every field is set at boot and only read afterwards.
type Decision struct {
	baseURL    string
	model      string
	providerID string
	// apiKey is the operator's key for the endpoint ("" for a keyless local host);
	// keyEnvName is the credential name a tenant's own key would be stored under.
	// Resolved per call through providers.ResolveKeyOrOperator, as a driver does.
	apiKey     string
	keyEnvName string
	timeout    time.Duration
	slots      chan struct{}
	client     *http.Client
	onUsage    func(ctx context.Context, u *providers.Usage)
}

// buildDecision builds the decision reranker rc declares. The endpoint and key
// resolve exactly as a listwise reranker's (providerbuild.ServiceDriverOptions);
// the provider must be an Ollama one, since /v1/systemone is Ollama's.
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
	d := &Decision{
		baseURL: strings.TrimRight(opts.BaseURL, "/"), model: model, providerID: provider,
		apiKey: opts.APIKey, keyEnvName: opts.KeyEnvName,
		timeout: time.Duration(rc.TimeoutMs) * time.Millisecond,
		client:  &http.Client{},
	}
	if d.timeout <= 0 {
		d.timeout = defaultTimeout
	}
	n := rc.MaxConcurrent
	if n <= 0 {
		n = defaultMaxConcurrent
	}
	d.slots = make(chan struct{}, n)
	return d, nil
}

func (d *Decision) ProviderID() string { return d.providerID }
func (d *Decision) ModelID() string    { return d.model }
func (d *Decision) Kind() string       { return config.RerankerKindDecision }

// SetOnUsage records each call's tokens against the run whose context it is.
func (d *Decision) SetOnUsage(f func(ctx context.Context, u *providers.Usage)) { d.onUsage = f }

// errTooLarge is the model refusing a prompt as more than its context holds.
var errTooLarge = errors.New("the candidates do not fit the decision model's context")

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
	// the alternative to a smaller prompt is no rerank at all.
	var probs map[string]float64
	var err error
	for _, chars := range []int{maxChars, maxChars / 2, maxChars / 4} {
		probs, err = d.choose(ctx, query, texts[:shown], chars)
		if !errors.Is(err, errTooLarge) {
			break
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, errTooLarge):
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

type systemOneRequest struct {
	Model     string                       `json:"model"`
	State     map[string]string            `json:"state"`
	Questions map[string]systemOneQuestion `json:"questions"`
}

type systemOneQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type systemOneResponse struct {
	Answers map[string]struct {
		Probabilities map[string]float64 `json:"probabilities"`
	} `json:"answers"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// choose asks one choice over texts, each cut to chars runes, and returns the
// probability per option letter.
func (d *Decision) choose(ctx context.Context, query string, texts []string, chars int) (map[string]float64, error) {
	callCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	select {
	case d.slots <- struct{}{}:
		defer func() { <-d.slots }()
	case <-callCtx.Done():
		return nil, deadlineAware(callCtx, callCtx.Err())
	}
	// The same key rule as the Ollama driver: a keyless endpoint has no operator key
	// to protect and is never restricted; otherwise a tenant's own stored key wins,
	// and a run barred from the operator's key gets no call at all.
	key, source, scopeID := d.apiKey, "operator", ""
	if d.keyEnvName != "" {
		var err error
		if key, source, scopeID, err = providers.ResolveKeyOrOperator(ctx, d.keyEnvName, d.apiKey); err != nil {
			return nil, err
		}
	}
	criteria := make(map[string]string, len(texts))
	for i, t := range texts {
		criteria[string(letters[i])] = truncateRunes(t, chars)
	}
	body, err := json.Marshal(systemOneRequest{
		Model: d.model,
		State: map[string]string{"question": query},
		Questions: map[string]systemOneQuestion{"best": {
			Type: "choice", Instructions: decisionInstruction, Criteria: criteria,
		}},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, d.baseURL+decisionPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, deadlineAware(callCtx, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, deadlineAware(callCtx, err)
	}
	if resp.StatusCode == http.StatusBadRequest && strings.Contains(string(raw), "tokens") {
		// Ollama's refusal of an over-long prompt: "prompt 0 has 10474 tokens;
		// expected 1–8194 (input is never truncated)".
		return nil, errTooLarge
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", decisionPath, resp.StatusCode)
	}
	var out systemOneResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", decisionPath, err)
	}
	if d.onUsage != nil && (out.Usage.InputTokens > 0 || out.Usage.OutputTokens > 0) {
		d.onUsage(ctx, &providers.Usage{
			InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens,
			Model: d.model, Provider: d.providerID,
			CredentialSource: source, CredentialScopeID: scopeID,
		})
	}
	best, ok := out.Answers["best"]
	if !ok || len(best.Probabilities) == 0 {
		return nil, fmt.Errorf("%s: the reply carries no probabilities", decisionPath)
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
