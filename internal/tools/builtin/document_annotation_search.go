package builtin

// document_annotation_search.go — annotations as a search leg.
//
// Each annotation record has an index row: its brief and abstract under a
// header naming the level, the class and the place, embedded once. A question
// about the corpus as a whole ("what do we have on X", "what does this folder
// hold") matches a folder's or a document's abstract far better than any one
// passage, so Document op=search can rank annotations beside the chunks
// (annotations: "include") or alone ("only"). A hit resolves to its target —
// the section, the document's root, the folder's _annotation document — taking
// one slot per target as a derived unit does, and says which annotation found it.
//
// The rows live in their own memory scope id beside the documents' (never in the
// documents' keyspace), so no other search, listing, quota or fact surface of
// Memory ever sees them, and nothing but this leg reads them. The record in SQL
// stays authoritative: a hit whose record is gone resolves to nothing.

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	annotationKeyPrefix = "doc.annotation:"
	// annotationMemScope names the memory scope id the index rows live under,
	// beside the documents' own.
	annotationMemScope = "doc.annotations"
	// annotationSearchDocuments is how many annotation hits carry their abstract.
	annotationSearchAbstracts = 5
	// rrfK is the reciprocal-rank constant the include leg fuses with.
	rrfK = 60
)

func annotationMemScopeID(mscope store.MemoryScope, key sqlmem.ScopeKey) string {
	if base := docMemScopeID(mscope, key); base != "" {
		return base + "#" + annotationMemScope
	}
	return annotationMemScope
}

func annotationKey(chunkID, level string) string { return annotationKeyPrefix + chunkID + ":" + level }

// annotationIndexHeader is the line the index text starts with, so a question
// that names a class or a place finds its annotations too.
func (d *Document) annotationIndexHeader(ctx context.Context, key sqlmem.ScopeKey, rec annotationRecord) string {
	if rec.Level == annotationLevelPath {
		return "[annotation · path] " + rec.Folder
	}
	h := "[annotation · " + rec.Level
	if rec.Class != "" {
		h += " · " + rec.Class
	}
	h += "]"
	if titles, _, ok := d.chunkLineage(ctx, key, rec.ChunkID); ok && len(titles) > 0 {
		h += " " + strings.Join(titles, " › ")
	}
	return h
}

// indexAnnotation writes the index row of one record. Best-effort like every
// document embed: a row that cannot be embedded is left out of the leg, and
// rewriting the annotation writes it again.
//
// Tenant scope only, as annotations are: a user's or an agent's memory is deleted
// scope-wide (erasure, retention) by its own scope id, which a row kept beside it
// would outlive. A record copied into such a scope keeps no row.
func (d *Document) indexAnnotation(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, rec annotationRecord) {
	if d.Store == nil || mscope != store.MemoryScopeTenant {
		return
	}
	tenant, scopeID := direntTenant(ctx), annotationMemScopeID(mscope, key)
	k := annotationKey(rec.ChunkID, rec.Level)
	text := d.annotationIndexHeader(ctx, key, rec) + "\n" + rec.Brief + "\n\n" + rec.Abstract
	v, _ := json.Marshal(map[string]string{"chunk_id": rec.ChunkID, "level": rec.Level, "text": rec.Brief + "\n\n" + rec.Abstract})
	if err := d.Store.MemorySet(ctx, tenant, mscope, scopeID, k, v, 0); err != nil {
		slog.WarnContext(ctx, "document: annotation index row not written", "chunk_id", rec.ChunkID, "level", rec.Level, "err", err)
		return
	}
	if d.Embedder == nil {
		return
	}
	vec, err := d.Embedder.Embed(ctx, []string{text})
	if err != nil || len(vec) == 0 {
		_ = d.Store.MemoryEmbedDelete(ctx, tenant, mscope, scopeID, k)
		slog.WarnContext(ctx, "document: annotation not embedded", "chunk_id", rec.ChunkID, "level", rec.Level, "err", err)
		return
	}
	if err := d.Store.MemoryEmbedSet(ctx, tenant, mscope, scopeID, k, store.MemoryEmbedding{
		Provider: d.Embedder.Provider(), Model: d.Embedder.Model(), Dimension: len(vec[0]),
		Vector: vec[0], EmbedText: text, CreatedAt: time.Now().UTC(),
	}); err != nil {
		slog.WarnContext(ctx, "document: annotation embedding not stored", "chunk_id", rec.ChunkID, "level", rec.Level, "err", err)
	}
}

// unindexAnnotations removes the index rows of every level of the given chunks.
// Best-effort: a row left behind resolves to no record and is never shown.
func (d *Document) unindexAnnotations(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, chunkIDs []string, levels ...string) {
	if d.Store == nil {
		return
	}
	if len(levels) == 0 {
		levels = []string{annotationLevelBranch, annotationLevelDocument, annotationLevelPath}
	}
	tenant, scopeID := direntTenant(ctx), annotationMemScopeID(mscope, key)
	for _, c := range chunkIDs {
		for _, l := range levels {
			_, _ = d.Store.MemoryDelete(ctx, tenant, mscope, scopeID, annotationKey(c, l))
		}
	}
}

// annotationHit is one annotation the leg found, resolved to its record.
type annotationHit struct {
	rec   annotationRecord
	score float64
}

// searchAnnotations runs the leg: the annotations closest to the query, best
// first, resolved to their records, at the levels asked for and inside the
// call's structural filter and access-control list.
func (d *Document) searchAnnotations(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, query string,
	topK int, levels map[string]bool, filter *searchFilterSet, aclList *searchACLList) ([]annotationHit, error) {
	vec, err := d.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vec) == 0 {
		return nil, nil
	}
	entries, err := d.Store.MemoryEmbedSearch(ctx, direntTenant(ctx), mscope, annotationMemScopeID(mscope, key),
		store.MemorySearchFilter{KeyPrefix: annotationKeyPrefix}, vec[0], topK*3)
	if err != nil {
		return nil, err
	}
	type found struct {
		chunk, level string
		score        float64
	}
	var hits []found
	var chunks []string
	for _, e := range entries {
		rest := strings.TrimPrefix(e.Key, annotationKeyPrefix)
		i := strings.LastIndexByte(rest, ':')
		if i <= 0 || !levels[rest[i+1:]] {
			continue
		}
		hits = append(hits, found{chunk: rest[:i], level: rest[i+1:], score: e.Score})
		chunks = append(chunks, rest[:i])
	}
	recs, err := d.annotationRecordsFor(ctx, key, dedupeStrings(chunks))
	if err != nil {
		return nil, err
	}
	allowed := func(ids, excluded []string) func(string) bool {
		in, out := map[string]bool{}, map[string]bool{}
		for _, id := range ids {
			in[id] = true
		}
		for _, id := range excluded {
			out[id] = true
		}
		return func(c string) bool { return (ids == nil || in[c]) && !out[c] }
	}
	inFilter := func(string) bool { return true }
	if filter != nil {
		inFilter = allowed(filter.ids, filter.excludeIDs)
	}
	inACL := func(string) bool { return true }
	if aclList != nil {
		inACL = allowed(aclList.ids, aclList.excludeIDs)
	}
	var out []annotationHit
	for _, h := range hits {
		rec, ok := recs[h.chunk][h.level]
		if !ok || !inFilter(h.chunk) || !inACL(h.chunk) {
			continue
		}
		out = append(out, annotationHit{rec: rec, score: h.score})
		if len(out) >= topK {
			break
		}
	}
	return out, nil
}

// annotationLevelsOf validates annotation_levels; empty is every level.
func annotationLevelsOf(levels []string) (map[string]bool, *tools.Result) {
	out := map[string]bool{}
	if len(levels) == 0 {
		for _, l := range []string{annotationLevelBranch, annotationLevelDocument, annotationLevelPath} {
			out[l] = true
		}
		return out, nil
	}
	for _, l := range levels {
		switch l {
		case annotationLevelBranch, annotationLevelDocument, annotationLevelPath:
			out[l] = true
		default:
			r := errValidation("search: annotation_levels takes branch, document and path", "")
			return nil, &r
		}
	}
	return out, nil
}

// mergeAnnotationHits folds the leg into the ranked chunk rows: "only" answers
// with the annotation targets alone; "include" fuses the two rankings by
// reciprocal rank, one row per target, the annotation that found a row named on
// it. Rows the leg adds are enriched like the others.
func (d *Document) mergeAnnotationHits(ctx context.Context, key sqlmem.ScopeKey, mode string, rows []map[string]any, hits []annotationHit, topK int) ([]map[string]any, error) {
	byChunk := map[string]map[string]any{}
	fused := map[string]float64{}
	var order []string
	if mode == "include" {
		for i, r := range rows {
			cid, _ := r["chunk_id"].(string)
			byChunk[cid] = r
			fused[cid] += 1 / float64(rrfK+i+1)
			order = append(order, cid)
		}
	}
	var missing []string
	matched := map[string]annotationHit{}
	for i, h := range hits {
		cid := h.rec.ChunkID
		if _, seen := matched[cid]; seen {
			continue // one slot per target: its best annotation names it
		}
		matched[cid] = h
		fused[cid] += 1 / float64(rrfK+i+1)
		if _, ok := byChunk[cid]; !ok {
			missing = append(missing, cid)
			order = append(order, cid)
		}
	}
	meta, err := d.chunkMetaByIDs(ctx, key, missing)
	if err != nil {
		return nil, err
	}
	for _, cid := range missing {
		h := matched[cid]
		m := map[string]any{"chunk_id": cid, "score": h.score, "document_id": h.rec.DocumentID}
		if md, ok := meta[cid]; ok {
			if md.title != "" {
				m["title"] = md.title
			}
			if md.typ != "" {
				m["type"] = md.typ
			}
		}
		byChunk[cid] = m
	}
	sort.SliceStable(order, func(i, j int) bool { return fused[order[i]] > fused[order[j]] })
	if len(order) > topK {
		order = order[:topK]
	}
	out := make([]map[string]any, 0, len(order))
	abstracts := 0
	for _, cid := range order {
		m := byChunk[cid]
		if h, ok := matched[cid]; ok {
			m["matched_annotation"] = map[string]any{"level": h.rec.Level, "text": h.rec.Brief}
			sm := map[string]any{"level": h.rec.Level, "brief": h.rec.Brief}
			if abstracts < annotationSearchAbstracts {
				sm["abstract"] = h.rec.Abstract
				abstracts++
			}
			m["summary"] = sm
			if h.rec.Folder != "" {
				m["folder"] = h.rec.Folder
			}
			if mode == "include" {
				m["rank_score"] = fused[cid]
			}
		}
		out = append(out, m)
	}
	return out, nil
}
