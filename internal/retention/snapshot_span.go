package retention

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// snapshotSpanMaxChars caps a snapshotted span. A span is EVIDENCE, not an
// archive: it exists so an operator can see why the store believes a fact, and
// the export already keeps the full transcript for anyone who needs all of it.
const snapshotSpanMaxChars = 4000

// snapshotSpansBeforeArchive copies each fact's source span onto the fact
// itself, for facts derived from a session that is about to be deleted.
//
// WHY IT EXISTS. A fact keeps a live reference to the turn it was distilled
// from, and retrieval can hand that turn back verbatim — for exactly as long as
// the turn exists. When the chats sweeper archives the session, that reference
// becomes dangling (the cascade deletes runs and events with it), and a fact
// whose reference dangles and whose source_quote is empty can no longer answer
// "why does the store believe this" at all. So the span is copied onto the fact
// AS THE SOURCE LEAVES.
//
// This is not the duplication the one-chunk-store work removes. For the whole
// life of the source there is ONE copy, in the transcript, reached by the
// reference; the copy onto the fact is made only when the source is going away.
// At every moment the span exists exactly once.
//
// ⚠️ ORDERING IS THE POINT, not an implementation detail. Snapshot BEFORE
// archive, so a crash between the two steps leaves a live source and a fact
// that can be snapshotted again next tick. The other order leaves a fact with
// neither the reference nor the span, which is unrecoverable.
//
// KEYED ON run_id, NOT session_id, and that is a measurement rather than a
// preference: across 1,096 fact-provenance rows on a live store, run_id was
// 100% populated and session_id 0%. The queue enqueues with the ingesting run's
// id and no session (there is no session-id helper on the tools context), so
// run_id is the pointer that actually carries provenance, and runs.session_id is
// one hop from it. RunsForSession is already called here for the export, so the
// run set costs nothing extra.
func (s *Sweeper) snapshotSpansBeforeArchive(ctx context.Context, sessionID string) {
	if s.sqlMem == nil {
		return // no SQL Memory: no chunk provenance to snapshot
	}
	runs, err := s.store.RunsForSession(ctx, sessionID)
	if err != nil || len(runs) == 0 {
		if err != nil {
			s.logf("retention: snapshot spans: runs for session %s: %v", sessionID, err)
		}
		return
	}
	runIDs := make([]string, 0, len(runs))
	for _, r := range runs {
		if r.ID != "" {
			runIDs = append(runIDs, r.ID)
		}
	}
	if len(runIDs) == 0 {
		return
	}

	events, err := s.store.GetTranscript(ctx, sessionID)
	if err != nil {
		s.logf("retention: snapshot spans: transcript for session %s: %v", sessionID, err)
		return
	}
	// The whole conversation, rendered once. A fact points at a RUN rather than
	// a turn (event_seq is unpopulated on the queue path), so the honest span is
	// the conversation the fact was distilled from — trimmed to the same cap the
	// write path applies, because a span is evidence and not an archive.
	span := renderTranscriptSpan(events)
	if span == "" {
		return
	}

	scopes, err := s.sqlMem.ListScopes(ctx)
	if err != nil {
		s.logf("retention: snapshot spans: list scopes: %v", err)
		return
	}
	filled := 0
	for _, key := range scopes {
		n, err := s.fillEmptySpans(ctx, key, runIDs, span)
		if err != nil {
			// A scope that has no chunk_memory_meta table is the common case, not
			// a fault: only scopes carrying entity-tier content have one. Logged at
			// debug volume would drown the tick, so an error here is skipped
			// silently and the NEXT scope still gets its chance.
			continue
		}
		filled += n
	}
	if filled > 0 {
		s.logf("retention: snapshot spans: session %s leaving, %d fact(s) kept their source span", sessionID, filled)
	}
}

// fillEmptySpans writes the span onto facts in one scope that came from these
// runs and carry no span yet.
//
// ONLY THE EMPTY ONES. A fact that already has a source_quote got it at write
// time from the text the extractor actually saw, which is a tighter span than a
// whole conversation — overwriting it would trade precise evidence for coarse.
func (s *Sweeper) fillEmptySpans(ctx context.Context, key sqlmem.ScopeKey, runIDs []string, span string) (int, error) {
	ph := make([]string, len(runIDs))
	args := make([]any, 0, len(runIDs)+1)
	args = append(args, span)
	for i := range runIDs {
		ph[i] = "?"
		args = append(args, runIDs[i])
	}
	stmt := "UPDATE chunk_memory_meta SET source_quote = ? WHERE run_id IN (" +
		strings.Join(ph, ",") + ") AND (source_quote IS NULL OR source_quote = '')"
	res, err := s.sqlMem.Exec(ctx, key, stmt, args, 0)
	if err != nil {
		return 0, err
	}
	return int(res.RowsAffected), nil
}

// renderTranscriptSpan turns a session's events into the text a fact can carry
// as its evidence, capped.
//
// Reuses the shape the extractor was shown — "### role" sections — so a span
// snapshotted at archival reads like the spans written at extraction time
// rather than like a different format that happens to live in the same column.
func renderTranscriptSpan(events []store.Event) string {
	var b strings.Builder
	for _, ev := range events {
		var text string
		switch ev.Type {
		case "user_input":
			text = firstUserText(ev.Payload)
		case "text":
			var pe struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(ev.Payload, &pe) == nil {
				text = pe.Text
			}
		default:
			continue
		}
		if text = strings.TrimSpace(text); text == "" {
			continue
		}
		if b.Len()+len(text) > snapshotSpanMaxChars {
			break
		}
		fmt.Fprintf(&b, "%s\n\n", text)
	}
	return strings.TrimSpace(b.String())
}

// firstUserText pulls the human words out of a persisted user_input payload
// (a []PromptSegment), dropping system framing and image bytes.
func firstUserText(payload []byte) string {
	var segs []struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(payload, &segs) != nil {
		return ""
	}
	var parts []string
	for _, seg := range segs {
		if seg.Role != "user" {
			continue
		}
		for _, c := range seg.Content {
			if c.Type == "image" || strings.TrimSpace(c.Text) == "" {
				continue
			}
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}
