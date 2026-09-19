package http

// trace_index.go — RFC CI P1: index the raw user turn so the evidence a fact was
// distilled FROM is findable, not only the fact.
//
// loomcycle keeps every transcript and indexes none of them. The derived layers —
// facts, notes, document bodies — are embedded, hybrid-ranked and filterable; the
// layer they were abstracted from is reachable only by opening the chat, or by a
// case-insensitive match on an auto-generated title. That inverts the finding the
// whole memory design rests on: preserve evidence first, structure second.
//
// OFF BY DEFAULT. Indexing raw turns copies personal data into a second place, which
// is a change in posture rather than an implementation detail, so an operator opts in.

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// traceTurnMaxBytes caps one indexed turn.
//
// A cap rather than a refusal: a pasted 200KB log is a real user turn, and the
// searchable part of it is the first paragraph. Truncating keeps the turn findable
// while bounding what the embedder is asked to read — the embedder is the dominant
// cost on a slow local deployment, which is exactly where this feature has to not
// hurt.
const traceTurnMaxBytes = 8192

// defaultTraceMaxAge is how long an indexed turn stays searchable when the operator
// sets no window.
//
// SHORT ON PURPOSE. An index over the last month and an index over everything are
// different storage commitments, and only one of them is reversible cheaply: widening
// it later is a backfill, narrowing it is a prune, and committing to "everything" up
// front is neither.
const defaultTraceMaxAge = 30 * 24 * time.Hour

// traceTurnValue is what one indexed turn stores.
//
// The session and run travel WITH the row rather than being parsed back out of the
// key, because a hit nobody can attribute is a hit nobody can use — and because the
// key's shape is this file's business, not a reader's.
type traceTurnValue struct {
	Text      string `json:"text"`
	Speaker   string `json:"speaker"`
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	At        string `json:"at"`
}

// persistUserInput records the caller's input segments on the transcript and, when
// trace indexing is on, indexes the turn.
//
// ONE WRITER FOR BOTH, and that is the point of the function existing. The transcript
// append happens at three call sites — a fresh run, an interactive run, and a
// continuation — and a trace index hooked into each of them separately is three
// places to keep in step plus a fourth that gets added later and indexes nothing.
// This project has already shipped that bug once, with four of six writers bypassing
// the publisher they were supposed to go through.
//
// The transcript append is unchanged and stays authoritative: its failure is logged
// exactly as before, and the index never blocks it.
func (s *Server) persistUserInput(ctx context.Context, runID, sessionID, tenantID, userID string, segments []loop.PromptSegment) {
	if s.store == nil || runID == "" {
		return
	}
	inputJSON, err := json.Marshal(segments)
	if err != nil {
		return
	}
	if err := s.store.AppendEvent(ctx, runID, "user_input", inputJSON); err != nil {
		log.Printf("store: AppendEvent(user_input) failed: %v", err)
	}
	s.indexUserTurn(ctx, runID, sessionID, tenantID, userID, segments)
}

// indexUserTurn writes one trace row for a user turn. Best-effort throughout: the
// transcript is the system of record and this index is derived from it, so losing a
// row costs searchability and never the turn.
func (s *Server) indexUserTurn(ctx context.Context, runID, sessionID, tenantID, userID string, segments []loop.PromptSegment) {
	conf := s.cfg()
	if conf == nil || s.store == nil || !conf.Env.MemoryTraceIndexEnabled {
		return
	}
	// A turn with no user to file it under has no scope to live in. Skipping is right
	// rather than falling back to some shared scope: a trace is one person's words,
	// and a scope chosen by default is a disclosure chosen by default.
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return
	}
	text := userTurnText(segments)
	if text == "" {
		return
	}
	// REDACTED BEFORE IT IS STORED. The secret-redaction transform runs on context,
	// not on this path, so a turn containing a pasted credential would otherwise be
	// embedded verbatim — and an embedding is not something you can un-say. Nil-safe.
	text = s.redactor.String(text)
	if len(text) > traceTurnMaxBytes {
		text = text[:traceTurnMaxBytes]
	}

	// now() IS the turn's instant here, and correctly so: this path indexes a user
	// turn AS IT ARRIVES. That is not true of the backfill or of the assistant path
	// below, which both replay turns said earlier — they take the turn's own stamp.
	value, err := json.Marshal(traceTurnValue{
		Text: text, Speaker: "user", SessionID: sessionID, RunID: runID,
		At: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return
	}
	key := traceTurnKey(sessionID)

	// THE WINDOW IS A TTL, not a retention family, and that is the whole retention
	// story for this class. An expired row reads as missing before any sweeper touches
	// it, MemorySweep deletes it on its own schedule, and memory_embeddings cascades
	// on that delete. A fourth retention family could have drifted from the class it
	// prunes; a TTL cannot.
	ttl := conf.Env.MemoryTraceMaxAge
	if ttl <= 0 {
		ttl = defaultTraceMaxAge
	}
	if err := s.store.MemorySet(ctx, tenantID, store.MemoryScopeUser, userID, key, value, ttl); err != nil {
		log.Printf("trace index: set %s failed: %v", key, err)
		return
	}
	// The embedding is a SEPARATE, best-effort step, exactly as document bodies treat
	// it: an unembedded turn is unsearchable, which backfill can fix, where a failed
	// write would be a turn the index never hears about again.
	s.embedTraceTurn(ctx, tenantID, userID, key, text)
}

// embedTraceTurn embeds one indexed turn. Mirrors how a document body is embedded,
// including the failure rule: one log line, never a returned error. An unembedded
// turn is unsearchable and recoverable by a backfill; a write refused because the
// embedder was cold is a turn the index never hears about again.
func (s *Server) embedTraceTurn(ctx context.Context, tenantID, userID, key, text string) {
	if s.embedder == nil {
		return
	}
	vec, err := s.embedder.Embed(ctx, []string{text})
	if err != nil || len(vec) == 0 {
		log.Printf("trace index: embed %s failed (searchable after a backfill): %v", key, err)
		return
	}
	if err := s.store.MemoryEmbedSet(ctx, tenantID, store.MemoryScopeUser, userID, key, store.MemoryEmbedding{
		Provider:  s.embedder.Provider(),
		Model:     s.embedder.Model(),
		Dimension: len(vec[0]),
		Vector:    vec[0],
		EmbedText: text,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		log.Printf("trace index: store embedding for %s failed: %v", key, err)
	}
}

// traceTurnKey names one turn: `trace.turn:<session_id>:<nanos>`.
//
// ⚠️ NANOS RATHER THAN THE EVENT'S SEQ, which is what RFC CI §3 specifies. The seq
// would cost a second query on the write path — AppendEvent does not return it — to
// buy an ordering the timestamp already gives, within one session, monotonically. The
// row carries session_id and run_id in its VALUE, so attribution does not depend on
// the key being parsed.
func traceTurnKey(sessionID string) string {
	return store.TraceTurnKeyPrefix + sessionID + ":" +
		strconv.FormatInt(time.Now().UnixNano(), 10) + "-" +
		strconv.FormatUint(traceTurnSeq.Add(1), 10)
}

// traceTurnSeq disambiguates two turns that land in the same nanosecond.
//
// ⚠️ NOT HYPOTHETICAL. The first version keyed on the timestamp alone and its own
// uniqueness test failed under -race: the clock's resolution is coarser than a
// nanosecond on some platforms, so two turns in quick succession — a steer following
// a prompt, a client retrying — produced the SAME key and the later one silently
// overwrote the earlier. A turn that disappears because another arrived too fast is
// the one failure this index must not have.
var traceTurnSeq atomic.Uint64

// userTurnText flattens a turn's segments to the text a person would search for.
//
// USER-ROLE TEXT ONLY. A system segment is operator configuration rather than
// something anybody said, and an image block is bytes — indexing its base64 would
// embed noise and pay the embedder to read it.
func userTurnText(segments []loop.PromptSegment) string {
	var b strings.Builder
	for _, seg := range segments {
		if seg.Role != "" && !strings.EqualFold(seg.Role, "user") {
			continue
		}
		for _, blk := range seg.Content {
			if blk.Type == "image" || strings.TrimSpace(blk.Text) == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// maxAssistantTurnsPerRun bounds what one completed run contributes to the index.
//
// A long tool-using run produces many short assistant turns — narration between
// calls — and indexing all of them buys little while paying the embedder for each.
// The cap is generous enough that an ordinary conversation is complete and tight
// enough that a runaway loop cannot flood a scope.
const maxAssistantTurnsPerRun = 20

// indexAssistantTurns indexes a completed run's assistant turns.
//
// ⚠️ SEPARATELY FLAGGED, and off even when the trace index itself is on. Assistant
// text is the bulk of the volume and the most redundant with the fact layer — a fact
// distilled from a reply is already searchable — so a deployment that wants its own
// words findable should not be made to pay for the model's as well.
//
// IT RUNS AT COMPLETION, not per event, because a turn is not an event. Assistant
// text is persisted one row per streamed delta, so anything reading raw rows sees
// fragments with no speaker. The boundaries come from ConversationTurns — the same
// rule the transcript renders with, shared rather than re-derived.
//
// The transcript read is paid ONLY when the flag is on: a default deployment does no
// extra work at run end.
func (s *Server) indexAssistantTurns(ctx context.Context, runID string, meta runStateMeta) {
	// NIL-SAFE ON THE CONFIG, because this runs from finishRun — the one path that is
	// reached by a Server built without a config holder at all. `cfg()` returns nil
	// there by design, and reading .Env off it panics inside the run-completion path,
	// which is the worst place to panic: the run has already done its work.
	conf := s.cfg()
	if conf == nil || s.store == nil {
		return
	}
	cfg := conf.Env
	if !cfg.MemoryTraceIndexEnabled || !cfg.MemoryTraceAssistantTurns {
		return
	}
	if runID == "" || strings.TrimSpace(meta.UserID) == "" {
		return
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil || strings.TrimSpace(run.SessionID) == "" {
		return
	}
	// THIS RUN'S events, not the session's. A session-wide read would re-derive every
	// earlier run's turns on every completion — the keys are deterministic so nothing
	// would duplicate, but the embedder would be asked to re-read the whole
	// conversation each time a turn was added to it.
	events, err := s.store.GetRunEventsSince(ctx, runID, 0, 0)
	if err != nil {
		return
	}
	indexed := 0
	for _, turn := range builtin.ConversationTurns(events) {
		if turn.Speaker != "assistant" {
			continue
		}
		if indexed >= maxAssistantTurnsPerRun {
			break
		}
		text := s.redactor.String(turn.Text)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if len(text) > traceTurnMaxBytes {
			text = text[:traceTurnMaxBytes]
		}
		// KEYED ON THE TURN'S SEQ, unlike a user turn's timestamp. finishRun can run
		// again — a resumed run completes twice — and a deterministic key makes the
		// second pass overwrite the first instead of filing the same words twice.
		// The user path cannot do this: it has no seq in hand at write time.
		key := store.TraceTurnKeyPrefix + run.SessionID + ":a" + strconv.FormatInt(turn.Seq, 10)
		// THE TURN'S OWN INSTANT. finishRun replays turns from a completed run, and a
		// long or resumed run can close well after the assistant actually spoke — so
		// now() would stamp every turn of that run with the same closing moment.
		at := turn.At
		if at.IsZero() {
			at = time.Now()
		}
		value, err := json.Marshal(traceTurnValue{
			Text: text, Speaker: "assistant", SessionID: run.SessionID, RunID: runID,
			At: at.UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			continue
		}
		ttl := cfg.MemoryTraceMaxAge
		if ttl <= 0 {
			ttl = defaultTraceMaxAge
		}
		if err := s.store.MemorySet(ctx, meta.TenantID, store.MemoryScopeUser, meta.UserID, key, value, ttl); err != nil {
			log.Printf("trace index: assistant turn %s: %v", key, err)
			continue
		}
		s.embedTraceTurn(ctx, meta.TenantID, meta.UserID, key, text)
		indexed++
	}
}
