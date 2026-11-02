package builtin

import (
	"context"
	"errors"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The reader side of the member-safe document feed (RFC FV §3): which of a
// batch of change rows one caller may see, as frames. The HTTP route owns the
// stream, the cursor and the timing; this owns the scope and the access rule,
// so the feed can never answer differently from the snapshot and the search.
//
// A frame names only a document the caller can list, in the scope it named:
// rows of other scopes, subjects and tenants, rows with no document, and plain
// memory rows are dropped before anything is sent. There is no cursor in a frame
// — a seq would count, by its gaps, what the caller cannot see.

// ErrDocumentFeedScope refuses a scope the reader feed does not serve: an agent
// scope is run-private, and anything else is not a scope.
var ErrDocumentFeedScope = errors.New("the document feed serves scope user or tenant; agent scopes are private to their runs")

// DocumentFeedFrame is one `change` frame of the reader feed.
type DocumentFeedFrame struct {
	Type       store.MemoryChangeType `json:"type"`
	ChunkID    string                 `json:"chunk_id"`
	DocumentID string                 `json:"document_id"`
	At         time.Time              `json:"at"`
}

// DocumentFeedView is one caller's view of one scope's change rows. Its
// answers are cached for its lifetime, so the route opens a new one at least
// every RFC FV §3 TTL (30 s) from the caller's CURRENT rules: that is the
// window in which a revoked grant still lets a frame through.
type DocumentFeedView struct {
	d       *Document
	key     sqlmem.ScopeKey
	mscope  store.MemoryScope
	tenant  string // the Memory-plane tenant the rows were written under
	scopeID string // the Memory-plane scope id (docMemScopeID)
	// identity is the run identity the view was opened under. Frames runs under
	// the stream's own ctx, which carries none, and the Path-tree names an access
	// answer depends on are keyed by its tenant: re-stamped there, so a name
	// lookup can never fall back to the shared tenant and answer by the scope.
	identity tools.RunIdentityValue
	acl      *docACL
	// unconditional: nothing the caller holds restricts reading this scope, so
	// every document — a deleted one included — is readable and nothing is asked.
	unconditional bool
	readable      map[string]bool
}

// OpenDocumentFeedView resolves scope for the caller on ctx exactly as a
// Document op does (resolveScope: the grants, the isolated-member confinement,
// the user id a user scope needs) and takes the caller's access-control set.
// ctx is the off-run browse ctx the POST /v1/_document route builds.
func (d *Document) OpenDocumentFeedView(ctx context.Context, scope string) (*DocumentFeedView, error) {
	if d == nil || d.Store == nil || d.SqlMem == nil {
		return nil, errors.New("the document feed requires SQL Memory (LOOMCYCLE_SQLMEM_ENABLED=1)")
	}
	if scope != "user" && scope != "tenant" {
		return nil, ErrDocumentFeedScope
	}
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	v := &DocumentFeedView{
		d: d, key: key, mscope: mscope,
		tenant: direntTenant(ctx), scopeID: docMemScopeID(mscope, key),
		identity: tools.RunIdentity(ctx),
		acl:      d.newDocACL(ctx, key), readable: map[string]bool{},
	}
	// Names come from the reverse index, per batch, as graph_snapshot reads them:
	// a feed asks about a few documents at a time, never the whole tree.
	v.acl.useIndex()
	limited, err := v.acl.limitsReading(ctx)
	if err != nil {
		return nil, err
	}
	v.unconditional = !limited
	return v, nil
}

// Tenant is the tenant whose change rows this view reads.
func (v *DocumentFeedView) Tenant() string { return v.tenant }

// isDocumentFeedChange is the change types the reader feed carries.
func isDocumentFeedChange(t store.MemoryChangeType) bool {
	return t == store.DocumentChangeUpdated || t == store.DocumentChangeDeleted || t == store.DocumentEdgesChanged
}

// Frames turns one poll batch of change rows into the frames the caller may
// see: this scope's document rows, of documents it may read, with frames for
// the same (type, document) collapsed into one carrying the latest chunk and
// time. On an error nothing is returned — the caller must not send a frame it
// could not check.
func (v *DocumentFeedView) Frames(ctx context.Context, rows []store.MemoryChange) ([]DocumentFeedFrame, error) {
	type coalesceKey struct {
		t   store.MemoryChangeType
		doc string
	}
	var out []DocumentFeedFrame
	at := map[coalesceKey]int{}
	var docs []string
	for _, r := range rows {
		if !isDocumentFeedChange(r.Type) || r.DocumentID == "" || r.TenantID != v.tenant ||
			r.Scope != v.mscope || r.ScopeID != v.scopeID {
			continue
		}
		k := coalesceKey{r.Type, r.DocumentID}
		if i, ok := at[k]; ok {
			out[i].ChunkID, out[i].At = r.ChunkID, r.At
			continue
		}
		at[k] = len(out)
		out = append(out, DocumentFeedFrame{Type: r.Type, ChunkID: r.ChunkID, DocumentID: r.DocumentID, At: r.At})
		docs = append(docs, r.DocumentID)
	}
	if len(out) == 0 {
		return nil, nil
	}
	if err := v.decide(tools.WithRunIdentity(ctx, v.identity), docs); err != nil {
		return nil, err
	}
	kept := out[:0]
	for _, f := range out {
		if v.readable[f.DocumentID] {
			kept = append(kept, f)
		}
	}
	return kept, nil
}

// decide answers, once per view, whether each document is readable: under the
// listing rule (docACL.readable) when it exists; for one that no longer exists,
// only when the caller's access to the scope is unconditional (RFC FV G5) —
// with rules in play a deleted document has no names left to judge it by, and
// the scope-wide answer is not its answer.
func (v *DocumentFeedView) decide(ctx context.Context, docs []string) error {
	var ask []string
	for _, id := range docs {
		if _, known := v.readable[id]; !known {
			ask = append(ask, id)
		}
	}
	if len(ask) == 0 {
		return nil
	}
	if v.unconditional {
		for _, id := range ask {
			v.readable[id] = true
		}
		return nil
	}
	live, err := v.d.existingDocuments(ctx, v.key, ask)
	if err != nil {
		return err
	}
	var present []string
	for _, id := range ask {
		if live[id] {
			present = append(present, id)
		} else {
			v.readable[id] = false
		}
	}
	if err := v.acl.primeNames(ctx, present); err != nil {
		return err
	}
	for _, id := range present {
		ok, err := v.acl.readable(ctx, id)
		if err != nil {
			return err
		}
		v.readable[id] = ok
	}
	return nil
}

// existingDocuments is the subset of ids that are documents of this scope.
func (d *Document) existingDocuments(ctx context.Context, key sqlmem.ScopeKey, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, batch := range batchIDs(ids, graphIDBatch) {
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		res, err := d.query(ctx, key, `SELECT id FROM documents WHERE id IN (`+placeholders(len(batch))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Rows {
			out[asStr(r[0])] = true
		}
	}
	return out, nil
}
