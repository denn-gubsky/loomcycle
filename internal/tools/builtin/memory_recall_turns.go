package builtin

// memory_recall_turns.go — RFC DF P1: the turn a recalled fact was distilled from,
// attached to the hit rather than fetched by a second call.
//
// WHY THIS EXISTS, AND WHY IT IS NOT A PROMPT. A distilled fact is tenseless
// ("Caroline went to a support group") while the turn it came from opens with
// `[1:56 pm on 8 May, 2023]`. Measured on LoCoMo conv-26, reaching the turns was
// worth +39pp (0.3960 -> 0.7877, McNemar +61/-3, p=4.7e-15), concentrated in
// temporal questions (0.176 -> 0.797).
//
// The problem is who makes the call. Handed the same tool and the same prompt,
// deepseek issued 214 trace retrievals across 150 questions; qwen3.6 issued 7 and
// ornith-1.5:35b — agentic-tuned, declaring `tools` in its manifest — issued 17 on
// 11 questions. Forcing it in the PROMPT instead took compliance to 100% and
// accuracy to 0.0034 of 1.0: on a small model a mandatory protocol competes with the
// task for attention, and it began emitting tool-call JSON into the answer field.
//
// So the retrieval happens here, in the runtime, where no model has to elect it.
//
// RESOLVED VIA source_session_id, NOT THE TRACE INDEX. The trace index is
// forward-only and empty unless an operator enabled and backfilled it;
// source_session_id is populated on 87% of facts in the reference store and needs no
// flag. Same path History op=window already uses.

import (
	"context"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	// recallTurnMaxChars bounds ONE attached turn. A turn is raw conversation and can
	// be long; the fact it supports is one sentence.
	recallTurnMaxChars = 1200
	// recallTurnsBudget bounds the WHOLE response's attached turns. Without it a
	// 10-hit recall over a chatty corpus returns more transcript than context.
	recallTurnsBudget = 6000
)

// attachSourceTurns fills in each hit's originating turn, in place.
//
// Best-effort per hit: a fact whose session is gone, whose span never matched, or
// which never carried a pointer is returned exactly as it is today. A missing turn is
// not an error — the fact is still the answer to the question that was asked.
//
// Returns how many turns were attached and how many were dropped for budget, so the
// caller can say so rather than leave a reader to infer a thin result from a short
// list.
func (m *Memory) attachSourceTurns(ctx context.Context, scope store.MemoryScope,
	memories []map[string]any, spans map[string]FactSource) (attached, dropped int) {
	if m.Store == nil || len(memories) == 0 || len(spans) == 0 {
		return 0, 0
	}
	ident := tools.RunIdentity(ctx)
	// ONE TRANSCRIPT LOAD PER SESSION, not per hit. Ten hits from one chat is the
	// common shape, and loading that chat ten times is the N+1 this cache exists to
	// avoid.
	type loaded struct {
		turns []conversationTurn
		ok    bool
	}
	cache := map[string]loaded{}
	used := 0

	for _, mem := range memories {
		id, _ := mem["id"].(string)
		sp, ok := spans[id]
		if !ok || sp.SessionID == "" || sp.Span == "" {
			continue
		}
		got, seen := cache[sp.SessionID]
		if !seen {
			got = loaded{}
			// SCOPE GATE, mirroring History's: the session must belong to this tenant,
			// and for a user-scoped recall to this user. A source_session_id is a
			// COORDINATE, not an authorisation — a fact pointing at a chat does not
			// entitle its reader to that chat.
			if sess, err := m.Store.GetSession(ctx, sp.SessionID); err == nil &&
				sess.TenantID == ident.TenantID &&
				(scope != store.MemoryScopeUser || sess.UserID == ident.UserID) {
				if events, terr := m.Store.GetTranscript(ctx, sess.ID); terr == nil {
					got = loaded{turns: conversationTurns(events), ok: true}
				}
			}
			cache[sp.SessionID] = got
		}
		if !got.ok || len(got.turns) == 0 {
			continue
		}
		idx := matchTurn(got.turns, sp.Span)
		if idx < 0 {
			// NOT GUESSED. Returning the first turn of the chat when the span cannot be
			// located would look like a successful reach-through and answer from the
			// wrong place — the same reason History op=window reports `matched: false`
			// rather than falling back.
			continue
		}
		text := got.turns[idx].Text
		if len(text) > recallTurnMaxChars {
			text = text[:recallTurnMaxChars]
		}
		if used+len(text) > recallTurnsBudget {
			dropped++
			continue
		}
		used += len(text)
		mem["turn"] = map[string]any{
			"text":    text,
			"speaker": got.turns[idx].Speaker,
		}
		attached++
	}
	return attached, dropped
}

// trimTurnText is separate so the cap is testable without a store.
func trimTurnText(s string) string {
	if len(s) <= recallTurnMaxChars {
		return s
	}
	return strings.TrimSpace(s[:recallTurnMaxChars])
}
