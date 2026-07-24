package memory

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// MemoryLayer is the OPTIONAL capability for LLM-extract memory products
// (mem0, Zep-style) — RFC K. It is a DIFFERENT paradigm
// from Backend: conversation messages go in, the backend (optionally) runs
// an LLM to extract/reconcile durable facts, and recall is a
// natural-language semantic search returning those facts.
//
// It deliberately has NO key-addressed Get/Set: identity is server-assigned
// (a UUID, not a caller key) and fidelity is not guaranteed — in `infer`
// mode the server may rewrite, merge, or delete facts. That is the whole
// value of a memory layer, and the reason it cannot satisfy Backend's
// faithful KV contract. The two interfaces are intentionally disjoint; a
// backend implements Backend, MemoryLayer, or both.
//
// The Memory tool routes its `add` / `recall` ops here. When the resolved
// backend does not implement MemoryLayer, the tool refuses with
// *store.MemoryError{Code: "capability_unsupported"} — the same fail-closed
// posture as the existing vector_unsupported / embedder_not_configured
// refusals, never a panic or a silent no-op.
type MemoryLayer interface {
	// Add ingests conversation messages under (scope, scopeID). The backend
	// MAY run an LLM to extract/reconcile durable facts (ADD/UPDATE/DELETE/
	// NOOP against existing memories) when opts.Infer is set, or store them
	// verbatim when it is not. Ingestion is often ASYNCHRONOUS (the server
	// returns before extraction completes): AddResult.Status reports whether
	// the work is done (AddDone) or still pending (AddPending, with an
	// EventID the operator can correlate). A synchronous backend returns
	// AddDone. Read-after-write is NOT guaranteed for a pending add.
	Add(ctx context.Context, scope store.MemoryScope, scopeID string, msgs []LayerMessage, opts AddOptions) (AddResult, error)

	// Recall runs a natural-language semantic search over the extracted
	// facts under (scope, scopeID). Distinct from Backend.Search: results are
	// derived facts with server-assigned IDs, not caller-keyed entries, and
	// carry a 0..1 relevance score.
	Recall(ctx context.Context, scope store.MemoryScope, scopeID string, q RecallQuery) (RecallResult, error)
}

// Capabilities lets the Memory tool probe a backend ONCE at op-routing time
// and decide what to offer, instead of re-asserting types on every call. A
// backend advertises it via the optional Capable interface below.
//
//   - KV:           satisfies the flat Backend contract faithfully
//     (Get/Set round-trip, caller keys).
//   - VectorSearch: Backend.Search works (not vector_unsupported).
//   - Stats:        Backend.Stats returns real per-(provider,model) rows.
//   - MemoryLayer:  implements MemoryLayer (the add/recall paradigm).
type Capabilities struct {
	KV           bool
	VectorSearch bool
	Stats        bool
	MemoryLayer  bool
}

// Capable is OPTIONAL. A backend that does NOT implement it is assumed to be
// a full flat Backend (KV + VectorSearch + Stats, no MemoryLayer) — the
// zero-config default, so the in-process backend needs no change. A backend
// that implements MemoryLayer (or that declines part of the flat contract)
// SHOULD implement Capable so the tool can route + degrade correctly.
type Capable interface {
	Capabilities() Capabilities
}

// CapabilitiesOf returns a backend's advertised capabilities. A backend that
// implements Capable reports them directly; otherwise the conservative
// default for a plain Backend is assumed (KV + VectorSearch + Stats, no
// MemoryLayer) — matching the pre-RFC-K behavior of every existing backend.
func CapabilitiesOf(b Backend) Capabilities {
	if c, ok := b.(Capable); ok {
		return c.Capabilities()
	}
	caps := Capabilities{KV: true, VectorSearch: true, Stats: true}
	// A backend may implement MemoryLayer without bothering with Capable;
	// detect that too so add/recall route correctly regardless.
	if _, ok := b.(MemoryLayer); ok {
		caps.MemoryLayer = true
	}
	return caps
}

// AsMemoryLayer returns the backend as a MemoryLayer when it implements the
// capability, or (nil, false) when it does not. The tool uses this to route
// add/recall and to produce the capability_unsupported refusal when false.
func AsMemoryLayer(b Backend) (MemoryLayer, bool) {
	ml, ok := b.(MemoryLayer)
	return ml, ok
}

// LayerMessage is one conversation turn handed to MemoryLayer.Add.
type LayerMessage struct {
	Role    string `json:"role"` // "user" | "assistant" | "system"
	Content string `json:"content"`
}

// AddStatus reports whether a MemoryLayer.Add ingestion has completed.
type AddStatus int

const (
	// AddPending — the backend accepted the messages but extraction/indexing
	// is still running (async). EventID, when set, is the correlation handle.
	AddPending AddStatus = iota
	// AddDone — the ingestion completed synchronously (or was already
	// reconciled).
	AddDone
)

// String renders the status for tool output.
func (s AddStatus) String() string {
	switch s {
	case AddDone:
		return "done"
	default:
		return "pending"
	}
}

// AddOptions carries the write-side knobs for MemoryLayer.Add.
type AddOptions struct {
	// Infer requests server-side LLM fact extraction (the layer paradigm).
	// When false the backend stores messages verbatim with no extraction.
	// Memory-layer backends default this to true; the tool surfaces it so an
	// operator can opt into raw storage.
	Infer bool
	// Metadata is opaque key/value context attached to the ingestion (e.g.
	// loomcycle scope/scopeID hints, a source tag). Backends that support
	// metadata filtering on recall persist it.
	Metadata map[string]string
}

// AddResult is the outcome of MemoryLayer.Add.
type AddResult struct {
	Status  AddStatus
	EventID string // poll/correlation handle for async backends (e.g. mem0 event_id)
}

// RecallQuery is the input to MemoryLayer.Recall.
type RecallQuery struct {
	Query     string
	TopK      int
	Threshold float64 // 0..1 relevance floor; 0 = backend default
}

// RecallFact is one extracted fact returned by Recall. ID is server-assigned
// (a UUID, NOT a caller key) — it is opaque to loomcycle and only meaningful
// to the backend.
type RecallFact struct {
	ID       string            `json:"id"`
	Memory   string            `json:"memory"`
	Score    float64           `json:"score"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// RecallResult is the ranked output of Recall, trimmed to the query's TopK by
// the backend.
type RecallResult struct {
	Facts []RecallFact
}
