package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The opt-in search rerank: one listwise model call over the first N candidates
// of a search, whose reply is a JSON array of candidate numbers.
//
// WHY A LISTWISE CALL AND NOT A CROSS-ENCODER: it runs on any chat model the
// operator already has, local or hosted, with no second model family to deploy.
// Measured on Natural Questions (911 questions, bare queries, a local 36B reader)
// it moved R@1 from 0.515 to 0.754 and R@5 from 0.935 to 0.959, both significant.
//
// THE REPLY IS REPAIRED, NEVER TRUSTED. Unknown and repeated numbers are dropped,
// candidates the model left out keep their original relative order after the
// ranked ones, and any failure — a timeout, a transport error, a reply with no
// array in it — returns search's OWN order. A rerank can only reorder what search
// already found, so failing open costs the gain and nothing else; failing closed
// would turn a model outage into a retrieval outage.

// Rerank defaults — the measured configuration. 20 candidates of 1,200 characters
// is about 6,000 prompt tokens and five seconds on a local 35B model.
const (
	DefaultRerankCandidates = 20
	DefaultRerankMaxChars   = 1200
)

// RerankModel is the model a rerank calls: one prompt in, the reply text out.
// Building the prompt and reading the reply stay here, so every model is asked
// the same measured question and every reply is repaired the same way.
type RerankModel interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// RerankOptions is what an agent's configuration asks of a search. The zero value
// asks for nothing, so every caller that never mentions a rerank is unchanged.
//
// OPERATOR-SET ONLY. It is resolved from the agent definition and has no tool
// parameter: a rerank costs a model call per search, so neither the model nor the
// caller of a tool may switch it on, widen its budget, or switch it off.
type RerankOptions struct {
	Enabled    bool
	Candidates int // 0 = DefaultRerankCandidates
	MaxChars   int // per-candidate truncation; 0 = DefaultRerankMaxChars
}

// EffectiveCandidates is how many of the fused pool the model is shown.
func (o RerankOptions) EffectiveCandidates() int {
	if o.Candidates > 0 {
		return o.Candidates
	}
	return DefaultRerankCandidates
}

// EffectiveMaxChars is the per-candidate character budget.
func (o RerankOptions) EffectiveMaxChars() int {
	if o.MaxChars > 0 {
		return o.MaxChars
	}
	return DefaultRerankMaxChars
}

// Rerank reasons — why a requested rerank did NOT change the order. Stable
// strings: they reach the caller as `rerank_reason`.
const (
	// RerankNotConfigured: the server declares no memory.reranker.
	RerankNotConfigured = "not_configured"
	// RerankNotDocumentSearch: the search could not return documents (a source
	// selector without `documents`). The rerank was measured on document
	// retrieval, and a recall of the agent's own facts on every turn is not what
	// an operator enabling it paid for.
	RerankNotDocumentSearch = "not_a_document_search"
	// RerankTooFewCandidates: fewer than two candidates — nothing to reorder.
	RerankTooFewCandidates = "too_few_candidates"
	// RerankTimeout: the model did not answer within the reranker's timeout.
	RerankTimeout = "timeout"
	// RerankCallFailed: the call itself failed (transport, provider, key).
	RerankCallFailed = "call_failed"
	// RerankUnparseable: the model answered without a usable ranking.
	RerankUnparseable = "unparseable"
	// RerankBackendUnsupported: the agent's memory backend does not rerank.
	RerankBackendUnsupported = "not_supported_by_memory_backend"
)

// RerankReport is what a requested rerank did. nil on a result means none was
// requested.
type RerankReport struct {
	// Applied: the order is the reranker's.
	Applied bool
	// Reason says why not, when !Applied — one of the Rerank* reasons.
	Reason string
	// Candidates is how many results the model was shown (0 when no call ran).
	Candidates int
}

// rerankPrompt is the measured prompt, verbatim. It is fixed on purpose: the
// numbers above are a property of THIS wording, and a reworded prompt is an
// unmeasured one.
const rerankPrompt = `%s

Candidate passages:

%s

Rank the passages by how useful they are for answering the question, most useful first. Answer only with a JSON array of passage numbers, e.g. [3, 1, 7], listing at least the %d most useful.`

// BuildRerankPrompt numbers the candidates from 1, each truncated to maxChars
// characters (runes, so a multi-byte character is never split).
func BuildRerankPrompt(query string, texts []string, maxChars int) string {
	var b strings.Builder
	for i, t := range texts {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "[%d] %s", i+1, truncateRunes(t, maxChars))
	}
	// "at least the 8 most useful" in the measured prompt; a pool smaller than
	// that cannot be asked for eight.
	atLeast := 8
	if len(texts) < atLeast {
		atLeast = len(texts)
	}
	return fmt.Sprintf(rerankPrompt, "Question: "+query, b.String(), atLeast)
}

// ParseRanking reads a reply into a full permutation of 0..n-1: the model's ranked
// candidates first, in its order, then every candidate it left out in their
// original relative order. ok is false when the reply held no usable number —
// the permutation is then the identity, i.e. search's own order.
func ParseRanking(reply string, n int) (order []int, ok bool) {
	identity := func() []int {
		o := make([]int, n)
		for i := range o {
			o[i] = i
		}
		return o
	}
	// A thinking model may reason before it answers, and its reasoning can
	// contain brackets of its own. Only what follows the reasoning is the answer.
	if i := strings.LastIndex(reply, "</think>"); i >= 0 {
		reply = reply[i+len("</think>"):]
	}
	start := strings.Index(reply, "[")
	if start < 0 {
		return identity(), false
	}
	end := strings.Index(reply[start:], "]")
	if end < 0 {
		return identity(), false
	}
	var raw []any
	if err := json.Unmarshal([]byte(reply[start:start+end+1]), &raw); err != nil {
		return identity(), false
	}
	seen := make([]bool, n)
	for _, v := range raw {
		k, isNum := candidateNumber(v)
		if !isNum || k < 1 || k > n || seen[k-1] {
			continue // an unknown or repeated number is dropped, not trusted
		}
		seen[k-1] = true
		order = append(order, k-1)
	}
	if len(order) == 0 {
		return identity(), false
	}
	for i := 0; i < n; i++ {
		if !seen[i] {
			order = append(order, i)
		}
	}
	return order, true
}

// candidateNumber accepts an integral JSON number, or a string holding one — the
// quoting mistake a small model makes, which costs nothing to forgive.
func candidateNumber(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		if x != float64(int(x)) {
			return 0, false
		}
		return int(x), true
	case string:
		k, err := strconv.Atoi(strings.TrimSpace(x))
		return k, err == nil
	}
	return 0, false
}

// RerankTexts asks m to order texts for query. It never fails: every fault is a
// report with Applied false and the identity order, which is search's own.
func RerankTexts(ctx context.Context, m RerankModel, query string, texts []string, maxChars int) ([]int, RerankReport) {
	n := len(texts)
	identity := make([]int, n)
	for i := range identity {
		identity[i] = i
	}
	if m == nil {
		return identity, RerankReport{Reason: RerankNotConfigured}
	}
	if n < 2 {
		return identity, RerankReport{Reason: RerankTooFewCandidates}
	}
	reply, err := m.Complete(ctx, BuildRerankPrompt(query, texts, maxChars))
	if err != nil {
		reason := RerankCallFailed
		if errors.Is(err, context.DeadlineExceeded) {
			reason = RerankTimeout
		}
		return identity, RerankReport{Reason: reason, Candidates: n}
	}
	order, ok := ParseRanking(reply, n)
	if !ok {
		return identity, RerankReport{Reason: RerankUnparseable, Candidates: n}
	}
	return order, RerankReport{Applied: true, Candidates: n}
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
