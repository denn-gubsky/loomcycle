// layer.go adds the RFC K MemoryLayer capability to the Mem9 Backend.
//
// The Mem9 backend genuinely serves BOTH shapes (RFC K §5): the flat KV
// surface (mem9.go) AND the memory-layer add/recall paradigm (here). The
// two live in separate files so mem9.go stays focused on the six-op
// Backend contract; this file is ONLY the two MemoryLayer methods plus
// Capabilities, and it reuses mem9.go's verified plumbing (do() for the
// authenticated/size-capped round-trip, resolveKey, the tenancy helpers,
// joinPath). No HTTP/auth/tenancy logic is duplicated.
//
// ⚠ WIRE-SHAPE HONESTY ⚠ — same discipline as mem9.go. The credential
// (X-API-Key), the size cap, and the tenancy scoping are the VERIFIED,
// load-bearing parts (they flow through do() + scopedPrefix). The exact
// JSON field names and the two paths below are an ASSUMED CONTRACT, each
// tagged with the banner. They are a best-effort reconstruction of Mem9's
// v1alpha2 smart-mode write + q= recall and are NOT verified against a
// live server. The httptest stub in layer_test.go IS the contract under
// test, not real Mem9.
package mem9

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// =====================================================================
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
//
// Everything between this banner and the next "END ASSUMED WIRE SHAPE"
// banner is the unverified REST contract for the memory-layer surface.
// To adapt to the real Mem9 API, change ONLY this block. The Add/Recall
// methods below call these and stay shape-agnostic.
// =====================================================================

// layerCollectionPath is the smart-mode write + the q= recall endpoint:
// POST/GET {base}/{api_version}/mem9s/memories. Per the RFC K research
// (doc-internal/research/external-memory-products-2026-05-31.md, Mem9
// section): write is 202-async with body {messages, mode, session_id};
// recall is GET ?q=&limit= returning scored memory objects.
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
func (b *Backend) layerCollectionPath() string {
	return b.joinPath("mem9s", "memories")
}

// wireAddRequest is the POST mem9s/memories body. mode="smart" runs the
// 2-phase LLM extract+reconcile (opts.Infer); mode="raw" stores verbatim.
// session_id scopes the ingestion so recall can be narrowed to the same
// (scope, scopeID) slice — we put loomcycle's tenant-prefixed scope key
// there. Metadata carries the opts.Metadata hints.
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
type wireAddRequest struct {
	Messages  []wireAddMessage  `json:"messages"`
	Mode      string            `json:"mode"`
	SessionID string            `json:"session_id,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// wireAddMessage is one conversation turn in the smart-mode write body.
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
type wireAddMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// wireAddResponse is the 202 body. The research says Mem9 echoes NO object
// on the async write, so this is best-effort: if a future/real server does
// return a correlation handle we accept any of these field names and pass
// it through as AddResult.EventID. When absent (the documented case),
// EventID stays "" and the status is still AddPending.
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
type wireAddResponse struct {
	EventID string `json:"event_id,omitempty"`
	ID      string `json:"id,omitempty"`
}

// wireRecallResponse is the GET ?q= response. The research describes a bare
// JSON array of memory objects (id, content, tags, type, state, version,
// relevance score). We decode into a slice of wireRecallItem.
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
type wireRecallItem struct {
	ID       string            `json:"id"`
	Content  string            `json:"content"`
	Score    float64           `json:"relevance"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// toRecallFact maps one assumed wire item onto memory.RecallFact. The
// server-assigned id is opaque to loomcycle; content is the extracted
// fact; relevance is the 0..1 score (1−distance per the research).
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
func toRecallFact(w wireRecallItem) memory.RecallFact {
	return memory.RecallFact{
		ID:       w.ID,
		Memory:   w.Content,
		Score:    w.Score,
		Metadata: w.Metadata,
	}
}

// =====================================================================
// END ASSUMED WIRE SHAPE. Everything below is verified loomcycle-side
// logic: the capability advertisement, tenancy scoping, and the mapping
// onto the MemoryLayer interface — all reusing mem9.go's do()/tenancy.
// =====================================================================

// Capabilities advertises that the Mem9 backend serves both the flat KV
// shape (Get/Set/Delete/List/Search/Stats) AND the memory-layer add/recall
// shape. This makes *Backend satisfy memory.Capable so the Memory tool can
// route add/recall here instead of refusing capability_unsupported.
func (b *Backend) Capabilities() memory.Capabilities {
	return memory.Capabilities{
		KV:           true,
		VectorSearch: true,
		Stats:        true,
		MemoryLayer:  true,
	}
}

// Add ingests conversation messages via Mem9's smart-mode write. The write
// is 202-async (the research: "returns 202 Accepted, async, no object
// echoed"), so we always report AddPending — being honest that extraction
// has not necessarily completed and read-after-write is not guaranteed. A
// correlation handle, if the server returns one, is surfaced as EventID;
// the documented case (no body) yields EventID == "".
//
// Tenancy + scope: loomcycle's (scope, scopeID) is namespaced under the
// tenant prefix (scopedPrefix + scopeKey, the SAME isolation boundary the
// KV path uses) and sent as session_id, so a shared_key_with_prefix tenant
// cannot write into another tenant's space and recall can scope to it.
func (b *Backend) Add(ctx context.Context, scope store.MemoryScope, scopeID string, msgs []memory.LayerMessage, opts memory.AddOptions) (memory.AddResult, error) {
	mode := "raw"
	if opts.Infer {
		mode = "smart"
	}

	wireMsgs := make([]wireAddMessage, 0, len(msgs))
	for _, m := range msgs {
		wireMsgs = append(wireMsgs, wireAddMessage{Role: m.Role, Content: m.Content})
	}

	req := wireAddRequest{
		Messages: wireMsgs,
		Mode:     mode,
		// Tenant-prefixed scope key as the session id — same isolation
		// boundary as the KV path's scopedKey. An empty key segment is fine;
		// the prefix is what enforces tenant + scope separation.
		SessionID: b.scopedPrefix(scopeKey(scope, scopeID, "")),
		Metadata:  opts.Metadata,
	}

	respBody, err := b.do(ctx, http.MethodPost, b.layerCollectionPath(), req)
	if err != nil {
		return memory.AddResult{}, err
	}

	// The async write echoes no object in the documented case; decode is
	// best-effort so a present correlation handle is surfaced, an empty body
	// is fine, and a malformed body does not fail an accepted (2xx) write.
	var w wireAddResponse
	_ = json.Unmarshal(respBody, &w)
	eventID := w.EventID
	if eventID == "" {
		eventID = w.ID
	}

	// Mem9's write is async: be honest with AddPending, never AddDone.
	return memory.AddResult{Status: memory.AddPending, EventID: eventID}, nil
}

// Recall runs Mem9's natural-language hybrid search (GET ?q=&limit=) and
// maps the returned memory objects onto RecallFacts. The tenant-prefixed
// scope is sent so the query stays within the tenant + scope slice (mirrors
// the KV Search's scopedPrefix usage). Mem9 returns a relevance score; we
// pass it through, honor q.Threshold client-side (the research lists no
// threshold param on the q= recall), and trim to q.TopK.
func (b *Backend) Recall(ctx context.Context, scope store.MemoryScope, scopeID string, q memory.RecallQuery) (memory.RecallResult, error) {
	vals := url.Values{}
	vals.Set("q", q.Query)
	if q.TopK > 0 {
		vals.Set("limit", strconv.Itoa(q.TopK))
	}
	// Scope the query to this tenant + (scope, scopeID). Same isolation
	// rationale as KV Search: without the prefix, one tenant's recall could
	// reach another's facts in Mem9's flat space.
	vals.Set("session_id", b.scopedPrefix(scopeKey(scope, scopeID, "")))

	urlStr := b.layerCollectionPath() + "?" + vals.Encode()
	respBody, err := b.do(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return memory.RecallResult{}, err
	}

	var items []wireRecallItem
	if uerr := json.Unmarshal(respBody, &items); uerr != nil {
		return memory.RecallResult{}, fmt.Errorf("mem9: decode recall response: %w", uerr)
	}

	facts := make([]memory.RecallFact, 0, len(items))
	for _, it := range items {
		f := toRecallFact(it)
		// Threshold is a relevance floor; Mem9's q= recall has no documented
		// threshold param, so filter client-side. Threshold==0 means "backend
		// default" → keep everything.
		if q.Threshold > 0 && f.Score < q.Threshold {
			continue
		}
		facts = append(facts, f)
	}

	// Trim to TopK defensively in case the server returned more than limit
	// (or we requested no limit) — the contract is "ranked top_k".
	if q.TopK > 0 && len(facts) > q.TopK {
		facts = facts[:q.TopK]
	}

	return memory.RecallResult{Facts: facts}, nil
}

// Compile-time assertions: the Mem9 Backend implements both the optional
// MemoryLayer capability and the Capable advertiser.
var (
	_ memory.MemoryLayer = (*Backend)(nil)
	_ memory.Capable     = (*Backend)(nil)
)
