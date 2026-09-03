package builtin

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/recall"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Recall is the read side of recall-augmented distillation: a free-text query
// over the conversation spans this run's context distillation evicted (the
// run-scoped index the loop harvested), with a silent fallback to the agent's
// durable memory. ONE tool searches both, so the model need not know where a fact
// lives — it just asks for what it needs.
//
// The design choice that makes it work is free text, not identifiers: models
// barely reproduce fixed ids or exact prior wording but query fluently in plain
// language, which also makes the tool far more likely to actually be invoked. The
// query is embedded and matched against the (per-message) evicted spans + the
// agent's memory facts; the top few originals come back verbatim.
type Recall struct {
	// Memory backs the silent persistent-memory fallback. When nil, Recall
	// searches only the run-scoped index (still useful — recall of this run's own
	// evicted spans). When the run has no index either (recall disabled), the tool
	// degrades to a durable-memory free-text search.
	Memory *Memory
}

func (r *Recall) Name() string { return "Recall" }

func (r *Recall) Description() string {
	return strings.TrimSpace(`
Fetch details from earlier in this conversation that have since been summarized away.

As a conversation grows, older turns are distilled into a short summary and their
specifics — exact values, names, numbers, IDs, file paths, quoted lines — are dropped
from what you can currently see. Recall searches the ORIGINAL turns by meaning and
returns the most relevant ones verbatim.

Use it whenever you are about to state or rely on a specific detail from earlier that
you are not certain is still in front of you. Ask in plain language for what you need
(for example "the deployment token the user gave" or "the revenue figure from the Q2
report") — you do not need to remember the exact wording. It also transparently
searches your durable memory, so a fact learned in an earlier session may surface too.
Prefer recalling a specific value over guessing it.`)
}

// UsageHint surfaces via Context op=guide for the agent's highest-error tools.
func (r *Recall) UsageHint() string {
	return "Unsure of an exact value, name, or number from earlier in the conversation? Recall it in plain language instead of guessing."
}

func (r *Recall) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Plain-language description of the detail you need from earlier in the conversation."},
    "top_k": {"type": "integer", "description": "How many original spans to return (default 3, max 10)."}
  },
  "required": ["query"],
  "additionalProperties": false
}`)
}

type recallInput struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k,omitempty"`
}

func (r *Recall) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var in recallInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult("Recall: invalid input: " + err.Error()), nil
	}
	if strings.TrimSpace(in.Query) == "" {
		return errResult("Recall: missing required field: query"), nil
	}
	k := in.TopK
	if k <= 0 {
		k = recall.DefaultTopK
	}
	if k > recall.MaxTopK {
		k = recall.MaxTopK
	}

	var hits []recall.Hit
	// Run-scoped index leg: this run's own evicted spans (nil when recall is off
	// for the run or no embedder is configured — Search returns nothing).
	if ix := recall.FromContext(ctx); ix != nil {
		if found, err := ix.Search(ctx, in.Query, k); err == nil {
			hits = append(hits, found...)
		}
	}
	// Silent persistent-memory fallback: the agent's durable facts. Never errors.
	if r.Memory != nil {
		for _, f := range r.Memory.RecallFallback(ctx, in.Query, k) {
			hits = append(hits, recall.Hit{Text: f.Memory, Score: f.Score, Source: "memory"})
		}
	}
	if len(hits) == 0 {
		return tools.Result{Text: "No earlier details matched that query."}, nil
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > k {
		hits = hits[:k]
	}

	recalled := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		recalled = append(recalled, map[string]any{"text": h.Text, "score": h.Score, "source": h.Source})
	}
	return okJSON(map[string]any{"recalled": recalled})
}
