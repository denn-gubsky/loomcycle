package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// PendingPayload is the wire shape of a `memory_pending` row's payload:
// `{messages:[{role,content}], metadata}`. The consolidator bundle parses exactly
// this, so it has ONE definition — the in-process backend aliases it rather than
// declaring its own, because two structs describing one wire shape is a drift
// waiting to happen and a mismatch here fails silently (the pass reads a payload
// it cannot interpret and extracts nothing).
type PendingPayload struct {
	Messages []LayerMessage    `json:"messages"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// BankSpanMaxBytes bounds a single banked payload. It is a runaway guard, not a
// quality knob: an oversized span is REFUSED and named rather than truncated,
// because silently dropping part of a conversation is the failure mode this line
// has spent several releases eliminating, and a refusal is visible in the marker
// while a truncation is not. Extraction is bounded downstream anyway — the
// consolidator splits a payload into at most four 12,000-char parts.
const BankSpanMaxBytes = 1 << 20

// BankSpanRequest is one span to bank. Every field is server-supplied; nothing
// here is reachable from model input.
type BankSpanRequest struct {
	TenantID string
	Scope    store.MemoryScope
	ScopeID  string
	// RunID / SessionID become the fact's origin link — what `include_provenance`
	// later resolves to answer "can I still read the conversation this came from".
	RunID     string
	SessionID string
	Messages  []LayerMessage
	Metadata  map[string]string
}

// BankSpan enqueues a compaction-discarded span onto the consolidation queue so
// the pass extracts durable facts from it later (RFC BL P3).
//
// It BANKS, it does not flush: nothing is extracted here. That is deliberate —
// asking the summarizer's call to also emit facts is two objectives on one reply,
// which is the shape that had the LLM consolidator at 0-for-5 before v1.36.0
// reduced the model's job to one narrow tool-less call.
//
// It writes the queue row DIRECTLY rather than through MemoryLayer.Add, because
// Add hardcodes `origin=agent_explicit` — correctly, since an agent reaching it
// IS an explicit agent write. A compaction is not, and telling those apart is the
// entire point of the origin column. The cost of bypassing the tool path is its
// size cap, which is why BankSpanMaxBytes exists here.
//
// Returns the pending row's id. Every error is the CALLER's to swallow: banking
// memory is strictly secondary to keeping a run alive, so a compaction must
// complete whether or not this succeeds.
func BankSpan(ctx context.Context, st store.Store, req BankSpanRequest) (string, error) {
	if st == nil {
		return "", fmt.Errorf("bank span: no store")
	}
	if len(req.Messages) == 0 {
		// Not an error: a span with no conversational content (all tool traffic,
		// say) is a legitimate nothing-to-bank rather than a failure.
		return "", nil
	}
	if req.ScopeID == "" {
		return "", fmt.Errorf("bank span: no writable memory scope")
	}
	payload, err := json.Marshal(PendingPayload{Messages: req.Messages, Metadata: req.Metadata})
	if err != nil {
		return "", fmt.Errorf("bank span: marshal payload: %w", err)
	}
	if len(payload) > BankSpanMaxBytes {
		return "", fmt.Errorf("bank span: payload %d bytes exceeds the %d-byte guard; not banked (the compaction itself is unaffected)",
			len(payload), BankSpanMaxBytes)
	}
	// The store mints an id when the row carries none but does not return it, so
	// mint here to hand back a correlation handle for the transcript marker.
	row := store.MemoryPendingRow{
		ID:              "mp_" + bankRandHex(),
		TenantID:        req.TenantID,
		Scope:           req.Scope,
		ScopeID:         req.ScopeID,
		Payload:         payload,
		Origin:          store.PendingOriginCompaction, // server-set; the reason this exists
		SourceSessionID: req.SessionID,
		SourceRunID:     req.RunID,
	}
	if err := st.MemoryPendingEnqueue(ctx, row); err != nil {
		return "", fmt.Errorf("bank span: enqueue: %w", err)
	}
	return row.ID, nil
}

func bankRandHex() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// ConversationFromMessages renders provider messages as the turns worth banking,
// keeping ONLY dialogue and dropping everything that is loop plumbing.
//
// Tool calls and tool results are excluded deliberately. v1.36.5 measured what
// happens when the extractor is fed loomcycle's own scaffolding: handed a
// markdown export that dumped tool_call / tool_result / usage rows as raw JSON, a
// model whose single instruction is "a durable fact is never a fact ABOUT the
// conversation" extracted the transcript's own metadata as its first fact. The
// fix there was to filter by event TYPE rather than by pattern, and this is the
// same filter one layer down — a user turn that happens to contain JSON is
// content and survives verbatim.
//
// A message whose blocks yield no text is dropped entirely rather than banked as
// an empty turn, so a span of pure tool traffic returns nothing and BankSpan
// treats that as a legitimate nothing-to-bank.
func ConversationFromMessages(msgs []providers.Message) []LayerMessage {
	out := make([]LayerMessage, 0, len(msgs))
	for _, m := range msgs {
		var b strings.Builder
		for _, c := range m.Content {
			// "text" only. tool_use / tool_result / thinking are the run's
			// mechanics, not what was said.
			if c.Type == "text" && c.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(c.Text)
			}
		}
		if text := strings.TrimSpace(b.String()); text != "" {
			out = append(out, LayerMessage{Role: m.Role, Content: text})
		}
	}
	return out
}
