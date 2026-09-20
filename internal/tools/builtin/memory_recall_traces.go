package builtin

// memory_recall_traces.go — the QUESTION-anchored turns, attached by the runtime.
//
// WHY A SECOND RETRIEVAL AND NOT A WIDER FIRST ONE. Its sibling
// (memory_recall_turns.go) attaches the turn a recalled FACT was distilled from.
// That is fact-anchored: the query finds facts, and each fact drags its own turn
// along. Measured on LoCoMo conv-26, that route reaches 0.5473 on a cloud reader
// while searching the turns DIRECTLY with the question reaches 0.7877 — 24 points
// that fact-anchoring cannot recover, because it can only ever return turns some
// fact was already extracted from. The turns that answer the remaining questions are
// the ones the extractor passed over.
//
// So this runs the trace search the reader will not run for itself. The uptake
// numbers are the whole argument: handed the tool and told when to use it, deepseek
// issued 214 trace retrievals across 150 questions, qwen3.6 issued 7, and
// ornith-1.5:35b — agentic-tuned — issued 17. Mandating it in the prompt took
// compliance to 100% and accuracy to 0.0034. An operator who wants these turns
// cannot get them by asking the model.
//
// ⚠️ A SEPARATE BLOCK, NOT FUSED INTO `memories`. This is expansion, not ranking.
// Scoring a raw turn against the fact extracted from it in one ranked list is what
// ErrTracesNotCombinable refuses, and for the same reason it is refused there: the
// two answer different questions ("what was said" vs "what is known") and a fused
// list lets the more numerous, lexically-overlapping turns crowd out the facts. The
// reader gets both and is told which is which.
//
// ⚠️ THE INDEX IS A PRECONDITION, AND AN EMPTY ONE IS SILENT. The trace index is
// forward-only and off by default; a store ingested before the flag has nothing in
// it, and this retrieval then returns zero turns while looking like it worked. The
// response says how many it found for exactly that reason.

import (
	"context"
	"encoding/json"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	// recallTraceTopK bounds how many turns the question-anchored search returns.
	//
	// ⚠️ 24, NOT 6, AND 6 WAS THE ONLY BOUND THAT EVER BIT. Measured across 300
	// recalls on the reference store: every single one returned exactly 6 turns while
	// the block averaged 1271 chars against a 6000-char budget — 4x headroom the
	// budget never used. The two bounds are not interchangeable. The budget is
	// content-aware and cuts when turns are long; a fixed count cuts a short-turn
	// corpus off regardless of cost. LoCoMo turns average ~212 chars, so k=6 spent
	// 21% of the budget and discarded the rest.
	//
	// WHAT 24 BUYS, two draws per cell on two conversations:
	//
	//   conv-26  k=6  0.6976 / strict 0.6216   ->  k=24  0.7457 / 0.6826   +4.8pp
	//   conv-30  k=6  0.7593 / strict 0.7160   ->  k=24  0.8013 / 0.7454   +4.2pp
	//
	// The magnitudes agree within 0.6pp across corpora with different question mixes
	// and turn lengths, and the within-condition spreads are 0.0018-0.0123. On
	// conv-26 three of four cross-pairings are significant (p=0.017-0.041); on
	// conv-30 none are (p=0.18-0.73), because 81 questions and a high baseline leave
	// only 5-7 moving and the paired test has no power there. The claim rests on the
	// agreeing magnitudes, not on conv-30's p-values.
	//
	// ⚠️ WHY NOT HIGHER. k=48 was measured and is a plateau: +11/-11 (p=1.0) and
	// +11/-13 (p=0.839) against k=24, for +1.1s of latency. At 48 the BUDGET finally
	// binds (1 of 150 recalls reached the count; blocks ran 6427 chars) and buys
	// nothing — so ~24 turns is where this corpus saturates, and 24 is the smaller of
	// two values that perform identically.
	//
	// ⚠️ IT IS NOT FREE. k=24 loses 5-6 questions that k=6 answered correctly: more
	// material dilutes attention, which is the documented failure mode here (the
	// coerced-prompt arm collapsed to 0.0034 from added instructions). The net is
	// clearly positive on both corpora; a corpus of long turns would pay more of that
	// cost and collect less of the benefit, and the budget is what protects it.
	recallTraceTopK = 24
	// recallTraceMaxChars bounds ONE turn, matching the fact-anchored cap so a reader
	// sees the two blocks in the same units.
	recallTraceMaxChars = 1200
	// recallTracesBudget bounds the whole block, SEPARATE from the fact-anchored
	// budget. Sharing one budget would let whichever retrieval ran first starve the
	// other, and which one that is would be an accident of ordering.
	recallTracesBudget = 6000
)

// attachQuestionTurns runs the trace search for the same query and returns the turns
// as their own block.
//
// Best-effort: no index, no embedder, or no match returns an empty slice and no
// error. A missing turn is not a failed recall — the facts are still the answer to
// the question that was asked.
func (m *Memory) attachQuestionTurns(ctx context.Context, scope store.MemoryScope,
	scopeID, query string) (turns []map[string]any, found int) {
	if query == "" {
		return nil, 0
	}
	// ⚠️ HISTORY REACH IS GOVERNED BY history_scope, exactly as it is for the
	// fact-anchored turns. A turn is chat transcript however the runtime reached it,
	// and letting the memory path serve one an agent's History grant forbids would
	// make the grant decorative.
	histScopes := tools.EffectiveHistoryScopes(ctx, tools.HistoryPolicy(ctx).Scopes)
	if !historyScopeAllowsOwnChats(histScopes) {
		return nil, 0
	}
	// Default ranking, as the `search` op uses when the caller names no rank block:
	// this retrieval is the one the model would have issued, so it should behave the
	// way that call behaves rather than acquire a private tuning.
	res, err := m.backend(ctx).Search(ctx, scope, scopeID, memrank.SearchQuery{
		QueryText: query,
		Sources:   []memrank.Source{memrank.SourceTraces},
		TopK:      recallTraceTopK,
	}, memrank.DefaultRankConfig(), memrank.DedupConfig{})
	if err != nil {
		// Swallowed on purpose: an operator grant must not turn a working recall into
		// a failed one because the index is off or the embedder is down. The reported
		// count is what distinguishes "nothing indexed" from "nothing matched".
		return nil, 0
	}
	used := 0
	for _, hit := range res.Entries {
		text := trimTraceText(TraceTurnText(hit.Value))
		if text == "" {
			continue
		}
		if used+len(text) > recallTracesBudget {
			break
		}
		used += len(text)
		found++
		turns = append(turns, map[string]any{"text": text})
	}
	return turns, found
}

// TraceTurnText pulls the rendered turn out of a stored trace row.
//
// EXPORTED because the prompt-injection path renders the same rows and must parse
// them the same way. A second copy of this is the drift that has cost this codebase
// a silent bug more than once.
//
// The row is the index's own JSON ({text, speaker, session_id, at}), and `text`
// already opens with the turn's own "[date] Speaker:" stamp — which is the half that
// matters here, since the distilled fact it sits beside is tenseless. A row that will
// not parse yields "" and is skipped rather than handed to the reader as raw JSON:
// a model shown a serialised object tends to answer with one.
func TraceTurnText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var row struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		return ""
	}
	return row.Text
}

// trimTraceText caps one turn. Separate so the bound is testable without a store.
func trimTraceText(s string) string {
	if len(s) <= recallTraceMaxChars {
		return s
	}
	return s[:recallTraceMaxChars]
}

// sourcesIncludeTraces reports whether the caller explicitly asked for raw turns.
//
// Used to suppress the question-anchored block on a search that is ALREADY a trace
// search: those turns arrive as the result's entries, and appending the same rows a
// second time under a different key is duplication the reader has to reconcile.
func sourcesIncludeTraces(sources []memrank.Source) bool {
	for _, s := range sources {
		if s == memrank.SourceTraces {
			return true
		}
	}
	return false
}
