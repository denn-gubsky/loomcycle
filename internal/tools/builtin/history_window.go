package builtin

import (
	"context"
	"strings"
	"unicode"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// history_window.go — RFC CV P1: reach through from a fact to the turn it came from.
//
// A distilled fact is a sentence. The question it was distilled from often is not:
// "we moved the release to the 14th because Maria was out" becomes "the release moved",
// and the date and the reason are gone. Measured on the paired corpus, the answerer
// declines ~two thirds of questions because the fact it retrieved does not carry the
// specific the question asks for — while the turn it came from does, and is still in
// the database.
//
// Every memory system that scores on long-conversation QA keeps that reachable:
// Graphiti's facts point at their source episode, MemGPT's recall memory IS the
// transcript. loomcycle writes the pointer already (`recall` returns the span and the
// session it came from); what was missing is a way to FOLLOW it that does not mean
// reading the whole chat back. On the benchmark corpus a conversation is 419 turns —
// a reach-through that returns all of them is not a reach-through.
//
// SO IT IS A WINDOW, ANCHORED ON THE SPAN. The fact carries the exact text it was
// derived from, so the turn containing that text is the turn to return, plus a few
// neighbours for the context a single turn usually lacks ("the 14th" means nothing
// without the message before it).
//
// WHY NOT event_seq, which is what the plan named. The sidecar reserves the column and
// NOTHING WRITES IT — verified against the store: session_id is populated, run_id is
// populated, event_seq is only ever carried forward from a previous revision, so it is
// null on every fact. The span is what actually exists, and anchoring on it needs no
// migration and works on every fact already stored.

// windowDefaultContext is how many turns either side of the match come back. Two is
// enough for a pronoun or a bare date to resolve and small enough that a handful of
// reach-throughs still fit a context window.
const windowDefaultContext = 2

// windowMaxContext bounds what a caller can ask for. A window wide enough to be the
// whole chat defeats the point of not returning the whole chat.
const windowMaxContext = 10

// window returns the conversation turns around the one a fact was distilled from.
func (h *History) window(ctx context.Context, scope string, in historyInput) (tools.Result, error) {
	quote := strings.TrimSpace(in.Quote)
	if quote == "" {
		return errResult("history: window requires quote — the fact's source span, which is what " +
			"anchors it to a turn (recall returns it as `source`)"), nil
	}
	sess, err := h.loadSessionInScope(ctx, scope, "window", in.SessionID)
	if err != nil {
		// An ARCHIVED OR ERASED SOURCE IS NOT A FAULT. Retention snapshots the span onto
		// the fact before it removes the session, so the caller still holds the evidence;
		// what it has lost is the surrounding context. Saying which of the two happened
		// is the difference between "look elsewhere" and "this is broken".
		return errResult("history: window: " + err.Error() + " — the fact's span survives on " +
			"the fact itself; only the surrounding turns are gone"), nil
	}
	events, err := h.Store.GetTranscript(ctx, sess.ID)
	if err != nil {
		return errResult("history: window: transcript: " + err.Error()), nil
	}
	turns := conversationTurns(events)

	idx := matchTurn(turns, quote)
	if idx < 0 {
		// REPORTED, NOT GUESSED. Returning the first turns of the chat when the span
		// cannot be located would look like a successful reach-through and quietly
		// answer from the wrong place.
		return okJSON(map[string]any{
			"scope": scope, "session_id": sess.ID, "turns": []any{}, "matched": false,
			"total_turns": len(turns),
			"note": "the span was not found in this conversation — it may have been reworded " +
				"before it was stored, or the turn it came from has been redacted",
		})
	}

	n := windowDefaultContext
	if in.Context != nil {
		n = *in.Context
		if n < 0 {
			n = 0
		}
		if n > windowMaxContext {
			n = windowMaxContext
		}
	}
	lo, hi := idx-n, idx+n
	if lo < 0 {
		lo = 0
	}
	if hi >= len(turns) {
		hi = len(turns) - 1
	}
	out := turns[lo : hi+1]
	var md strings.Builder
	for _, t := range out {
		md.WriteString("### " + t.Speaker + "\n\n" + t.Text + "\n\n")
	}
	return okJSON(map[string]any{
		"scope": scope, "session_id": sess.ID,
		"matched": true, "matched_turn": idx, "total_turns": len(turns),
		"first_turn": lo, "turns": out, "markdown": md.String(),
	})
}

// matchTurn finds the turn a span came from, or -1.
//
// Exact containment first, because a span is copied verbatim at write time and should
// match exactly. The fallback is containment after whitespace and case folding, which
// covers the ways a span picks up cosmetic differences on its way through a model and
// two stores — a collapsed newline, a normalised quote mark. Nothing looser: a fuzzy
// match that lands on the wrong turn is worse than no match, because the caller cannot
// tell it happened.
func matchTurn(turns []conversationTurn, quote string) int {
	for i, t := range turns {
		if strings.Contains(t.Text, quote) {
			return i
		}
	}
	nq := foldForMatch(quote)
	if nq == "" {
		return -1
	}
	for i, t := range turns {
		if strings.Contains(foldForMatch(t.Text), nq) {
			return i
		}
	}
	return -1
}

// foldForMatch collapses a string to lower case with runs of whitespace reduced to one
// space, so a span that differs only cosmetically still finds its turn.
func foldForMatch(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}
