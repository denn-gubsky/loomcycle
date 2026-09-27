package builtin

// document_reindex.go — the operator pass that brings a scope's document index up to date.
//
// The write path indexes each chunk under chunkIndexText as it is written, and a rename
// or move re-indexes the subtree it touches. Neither reaches chunks written BEFORE the
// index text changed: a store from before headers keeps its old vectors until something
// rewrites them. This pass is that something, and it is explicit because re-embedding a
// large store is a decision about embedder load that belongs to the operator — nothing
// migrates automatically.
//
// It is idempotent and resumable by construction: a chunk whose stored text already
// equals what chunkIndexText derives is left alone (no embedding call), so re-running
// costs a read per chunk and nothing more, and a keyset cursor steps a large scope in
// pages.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ReindexReport is one page of a re-index, or of its dry run.
type ReindexReport struct {
	Scope    string `json:"scope"`
	DryRun   bool   `json:"dry_run"`
	Examined int    `json:"examined"`
	// UpToDate chunks already carry the text chunkIndexText derives (or, for a chunk
	// that must not be indexed, have no vector). They cost no embedding call.
	UpToDate int `json:"up_to_date"`
	// Reindexed chunks were embedded under their current index text — or, in a dry
	// run, would be.
	Reindexed int `json:"reindexed"`
	// Unindexed chunks derive to nothing (a bodyless document root, a letterless
	// heading) but still had a vector; it was removed — or, in a dry run, would be.
	Unindexed  int      `json:"unindexed"`
	Failed     int      `json:"failed"`
	FailedKeys []string `json:"failed_keys,omitempty"`
	// NextCursor resumes the walk; pass it back as `after`. More is true while chunks
	// beyond this page remain unexamined.
	NextCursor string `json:"next_cursor,omitempty"`
	More       bool   `json:"more"`
}

const (
	reindexPage          = 200
	reindexMaxExamined   = 5000
	reindexFailedKeysCap = 50
)

// ReindexScope walks the scope's chunk bodies from `after` and makes each one's stored
// embedding match chunkIndexText. `limit` bounds how many chunks are CHANGED
// (re-embedded or un-indexed), not how many are examined — up-to-date chunks are
// stepped over, so a mostly-current store does not stall a page — and the walk also
// stops after reindexMaxExamined chunks so one request stays bounded.
func (d *Document) ReindexScope(ctx context.Context, scope, after string, limit int, dryRun bool) (ReindexReport, error) {
	rep := ReindexReport{Scope: scope, DryRun: dryRun}
	if d.Store == nil || d.SqlMem == nil {
		return rep, fmt.Errorf("document reindex: SQL Memory is not configured")
	}
	if !dryRun && d.Embedder == nil {
		return rep, fmt.Errorf("document reindex: no embedder is configured")
	}
	if limit <= 0 {
		limit = 200
	}
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return rep, err
	}
	tenant := direntTenant(ctx)
	cursor := after
	for {
		page, err := d.Store.MemoryListAfter(ctx, tenant, mscope, key.ScopeID, chunkBodyKeyPrefix, cursor, reindexPage)
		if err != nil {
			return rep, err
		}
		for _, row := range page {
			cursor = row.Key
			rep.Examined++
			chunkID := ChunkIDFromBodyKey(row.Key)
			body := bodyFromValue(row.Value)
			want := d.chunkIndexText(ctx, key, chunkID, "", body)
			stored, gerr := d.Store.MemoryEmbedGet(ctx, tenant, mscope, key.ScopeID, row.Key)
			hasStored := gerr == nil
			if gerr != nil && !isNotFound(gerr) {
				// A store that cannot answer (no vector support) cannot be re-indexed at
				// all; say so rather than report every chunk as changed.
				return rep, gerr
			}
			switch {
			case want == "" && !hasStored, hasStored && stored.EmbedText == want:
				rep.UpToDate++
			case want == "":
				rep.Unindexed++
				if !dryRun {
					if err := d.Store.MemoryEmbedDelete(ctx, tenant, mscope, key.ScopeID, row.Key); err != nil {
						rep.fail(row.Key)
					}
				}
			default:
				rep.Reindexed++
				if !dryRun && !d.embedText(ctx, tenant, mscope, key.ScopeID, row.Key, want) {
					rep.Reindexed--
					rep.fail(row.Key)
				}
			}
			if rep.Reindexed+rep.Unindexed+rep.Failed >= limit || rep.Examined >= reindexMaxExamined {
				rep.NextCursor, rep.More = cursor, true
				return rep, nil
			}
		}
		if len(page) < reindexPage {
			return rep, nil // the walk reached the end of the scope
		}
	}
}

func (r *ReindexReport) fail(key string) {
	r.Failed++
	if len(r.FailedKeys) < reindexFailedKeysCap {
		r.FailedKeys = append(r.FailedKeys, key)
	}
}

// embedText embeds one precomputed index text and stores it, reporting success.
func (d *Document) embedText(ctx context.Context, tenant string, mscope store.MemoryScope, scopeID, bodyKey, text string) bool {
	vec, err := d.Embedder.Embed(ctx, []string{text})
	if err != nil || len(vec) == 0 {
		return false
	}
	return d.Store.MemoryEmbedSet(ctx, tenant, mscope, scopeID, bodyKey, store.MemoryEmbedding{
		Provider:  d.Embedder.Provider(),
		Model:     d.Embedder.Model(),
		Dimension: len(vec[0]),
		Vector:    vec[0],
		EmbedText: text,
		CreatedAt: time.Now().UTC(),
	}) == nil
}

// bodyFromValue reads the body out of a chunk-body row's JSON envelope ("" when the
// value is not one).
func bodyFromValue(v json.RawMessage) string {
	var env struct {
		Body string `json:"body"`
	}
	_ = json.Unmarshal(v, &env)
	return env.Body
}

// isNotFound reports a store miss — for an embedding, "this chunk has no vector".
func isNotFound(err error) bool {
	var nf *store.ErrNotFound
	return errors.As(err, &nf)
}
