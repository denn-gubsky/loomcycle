package builtin

// document_projection_umap.go — the UMAP method of the document projection (RFC
// FQ §11). See document_projection.go for the pass and the PCA method it extends.

import (
	"cmp"
	"context"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/projection"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const (
	// projectionAnchorBatch is the anchor rows per multi-row INSERT (6 binds each).
	projectionAnchorBatch = 200
	// projectionAnchorPage is the anchor rows per read: under the SQL Memory row
	// cap, which a 10,000-chunk sample would reach in one statement.
	projectionAnchorPage = 5000
)

// umapAnchors is a UMAP basis in memory: every anchor's place by chunk id, and
// the anchors whose vectors were found, for k-NN placement of everything else.
type umapAnchors struct {
	pos map[string][3]float64
	// knn is nil when no anchor's vector is still the one it was fitted with:
	// then only the anchors themselves can be placed.
	knn *projection.Anchors
}

// umapSeed is the fit's seed: the scope and model, so a fit is reproducible on
// the same data and two scopes do not share their negative-sampling stream.
func umapSeed(key sqlmem.ScopeKey, model string) uint64 {
	h := fnv.New64a()
	for _, s := range []string{key.Tenant, key.Scope, key.ScopeID, model} {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// embeddedAt is when r's vector was written, 0 when the store did not say — the
// placeChunks rule: an embedding with no write time is never newer than a fit.
func embeddedAt(r store.MemoryEmbedRow) int64 {
	if r.CreatedAt.IsZero() {
		return 0
	}
	return r.CreatedAt.UnixNano()
}

// refitUMAP fits version next of model's map with UMAP, stores it, and places the
// scope's chunks on it.
//
// The sample is a reservoir of at most umap_sample_max live chunks. When the
// current basis is a UMAP one, every sample chunk it placed starts where it was
// (the rest k-NN placed among them) and the fit is aligned to those places; a PCA
// or missing current basis starts the fit cold, from the sample's PCA layout.
func (d *Document) refitUMAP(ctx context.Context, key sqlmem.ScopeKey, list func(string) ([]store.MemoryEmbedRow, error),
	first []store.MemoryEmbedRow, model string, settings config.ProjectionSettings, cur *storedBasis, next int, now int64,
	limit int, rep ProjectionReport) (ProjectionReport, error) {
	vecs, ids, err := d.projectionSample(list, first, model, settings.UMAPSampleMax)
	if err != nil {
		return rep, err
	}
	// A body whose chunk is gone is the dead-link GC's; as an anchor it would be
	// a row no chunk delete ever reaches.
	live, err := d.existingChunkIDs(ctx, key, ids)
	if err != nil {
		return rep, err
	}
	keep := 0
	for i, id := range ids {
		if live[id] {
			vecs[keep], ids[keep] = vecs[i], id
			keep++
		}
	}
	vecs, ids = vecs[:keep], ids[:keep]
	if len(vecs) == 0 {
		// Vectors exist, none of this model on a live chunk: nothing to fit.
		rep.Refitted = false
		if cur != nil {
			rep.BasisVersion, rep.Method = cur.version, cur.methodName()
		}
		return rep, nil
	}

	var prev [][3]float64
	var has []bool
	if cur != nil && cur.methodName() == config.ProjectionMethodUMAP {
		held, err := d.projectionXYZ(ctx, key, model, cur.version, ids)
		if err != nil {
			return rep, err
		}
		prev, has = make([][3]float64, len(ids)), make([]bool, len(ids))
		for i, id := range ids {
			prev[i], has[i] = held[id]
		}
	}
	pos, err := projection.LayoutUMAP(vecs, prev, has, projection.UMAPOptions{Seed: umapSeed(key, model)})
	if err != nil {
		return rep, err
	}
	knn, err := projection.NewAnchors(vecs, pos)
	if err != nil {
		return rep, err
	}
	basis := &storedBasis{version: next, fittedAt: now, method: config.ProjectionMethodUMAP,
		umap: &umapAnchors{pos: make(map[string][3]float64, len(ids)), knn: knn}}
	basis.Dimension, basis.SampleN = len(vecs[0]), len(vecs)
	for i, id := range ids {
		basis.umap.pos[id] = pos[i]
	}
	saved, err := d.saveUMAPBasis(ctx, key, model, basis, ids, pos)
	if err != nil {
		return rep, err
	}
	if !saved {
		// Another pass over this scope (the sweeper, an operator call, another
		// replica) read the same basis and saved version next first. Its fit
		// stands, whatever its method; this one is dropped and the chunks go on
		// the latest basis.
		rep.Refitted = false
		if basis, err = d.loadBasis(ctx, key, model); err != nil {
			return rep, err
		}
		if basis == nil {
			return rep, fmt.Errorf("chunk_projection_basis %s: version %d was taken but no basis is stored", model, next)
		}
	}
	rep.BasisVersion, rep.Method = basis.version, basis.methodName()
	return d.placeChunks(ctx, key, list, first, model, basis, now, limit, rep)
}

// saveUMAPBasis writes a UMAP version: its basis row (no mean or components) and
// its anchors, in one transaction with the prune saveBasis does. It reports false,
// having written nothing, when (model, b.version) is already stored: the primary
// key decides which of two concurrent refits of one scope wins, on both tiers and
// across replicas, and the loser's anchors are never written. A version's anchors
// are written only by the transaction that inserted its basis row, so they cannot
// collide.
func (d *Document) saveUMAPBasis(ctx context.Context, key sqlmem.ScopeKey, model string, b *storedBasis, ids []string, pos [][3]float64) (bool, error) {
	saved := false
	err := d.withSqlTxn(ctx, key, func(txnID string) error {
		res, err := d.docSQL().ExecTxn(ctx, txnID, d.docSQL().Rebind(`INSERT INTO chunk_projection_basis
			(model, version, dimension, mean, components, explained_variance, sample_n, fitted_at, method) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (model, version) DO NOTHING`),
			[]any{model, b.version, b.Dimension, []byte{}, []byte{}, "[]", b.SampleN, b.fittedAt, config.ProjectionMethodUMAP}, 0)
		if err != nil || res.RowsAffected == 0 {
			return err
		}
		saved = true
		for start := 0; start < len(ids); start += projectionAnchorBatch {
			end := min(start+projectionAnchorBatch, len(ids))
			values := make([]string, 0, end-start)
			args := make([]any, 0, 6*(end-start))
			for i := start; i < end; i++ {
				values = append(values, "(?, ?, ?, ?, ?, ?)")
				args = append(args, model, b.version, ids[i], pos[i][0], pos[i][1], pos[i][2])
			}
			if err := d.execTxn(ctx, txnID, `INSERT INTO chunk_projection_anchor (model, version, chunk_id, x, y, z) VALUES `+
				strings.Join(values, ", "), args...); err != nil {
				return err
			}
		}
		return d.pruneBases(ctx, txnID, model, b.version)
	})
	return saved && err == nil, err
}

// projectionXYZ reads the places of ids on (model, version).
func (d *Document) projectionXYZ(ctx context.Context, key sqlmem.ScopeKey, model string, version int, ids []string) (map[string][3]float64, error) {
	out := make(map[string][3]float64, len(ids))
	for start := 0; start < len(ids); start += projectionReadBatch {
		ph, args := inPlaceholders(ids[start:min(start+projectionReadBatch, len(ids))])
		res, err := d.query(ctx, key, `SELECT chunk_id, x, y, z FROM chunk_projection WHERE model = ? AND basis_version = ? AND chunk_id IN (`+ph+`)`,
			append([]any{model, version}, args...)...)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Rows {
			out[asStr(r[0])] = [3]float64{asF64(r[1]), asF64(r[2]), asF64(r[3])}
		}
	}
	return out, nil
}

// loadUMAPAnchors fills b.umap from the store: the anchors' places from
// chunk_projection_anchor, and their vectors by streaming the scope's vectors as
// the pass does. An anchor re-embedded since the fit is left out of the k-NN set —
// its place belongs to the old vector.
func (d *Document) loadUMAPAnchors(ctx context.Context, key sqlmem.ScopeKey, list func(string) ([]store.MemoryEmbedRow, error),
	model string, b *storedBasis) error {
	if b.umap != nil {
		return nil
	}
	a := &umapAnchors{pos: make(map[string][3]float64, b.SampleN)}
	after := ""
	for {
		res, err := d.query(ctx, key, `SELECT chunk_id, x, y, z FROM chunk_projection_anchor
			WHERE model = ? AND version = ? AND chunk_id > ? ORDER BY chunk_id LIMIT ?`, model, b.version, after, projectionAnchorPage)
		if err != nil {
			return err
		}
		for _, r := range res.Rows {
			after = asStr(r[0])
			a.pos[after] = [3]float64{asF64(r[1]), asF64(r[2]), asF64(r[3])}
		}
		if len(res.Rows) < projectionAnchorPage {
			break
		}
	}
	var vecs [][]float32
	var places [][3]float64
	page, err := list("")
	for ; err == nil; page, err = list(page[len(page)-1].Key) {
		for _, r := range page {
			p, ok := a.pos[ChunkIDFromBodyKey(r.Key)]
			if !ok || rowModel(r) != model || len(r.Vector) != b.Dimension || embeddedAt(r) > b.fittedAt {
				continue
			}
			vecs, places = append(vecs, r.Vector), append(places, p)
		}
		if len(page) < projectionPage {
			break
		}
	}
	if err != nil {
		return err
	}
	if len(vecs) > 0 {
		if a.knn, err = projection.NewAnchors(vecs, places); err != nil {
			return err
		}
	}
	b.umap = a
	return nil
}

// placeBatch gives each row of batch its place on basis, from its vector at the
// same index; a row that cannot be placed is dropped and counted skipped. On a
// UMAP basis an anchor keeps its own place (unless it was re-embedded since the
// fit) and the rest are k-NN placed among the anchors, the batch at once.
func (d *Document) placeBatch(ctx context.Context, key sqlmem.ScopeKey, list func(string) ([]store.MemoryEmbedRow, error),
	model string, basis *storedBasis, batch []placedRow, vectors []store.MemoryEmbedRow, rep *ProjectionReport) ([]placedRow, error) {
	xyz := make([][3]float64, len(batch))
	ok := make([]bool, len(batch))
	if basis.methodName() == config.ProjectionMethodUMAP {
		if err := d.loadUMAPAnchors(ctx, key, list, model, basis); err != nil {
			return nil, err
		}
		var q [][]float32
		var at []int
		for i, r := range vectors {
			if p, anchor := basis.umap.pos[batch[i].chunkID]; anchor && embeddedAt(r) <= basis.fittedAt {
				xyz[i], ok[i] = p, true
				continue
			}
			q, at = append(q, r.Vector), append(at, i)
		}
		if len(q) > 0 && basis.umap.knn != nil {
			placed, pok := basis.umap.knn.Place(q, projection.DefaultNeighbors, 0)
			for j, i := range at {
				xyz[i], ok[i] = placed[j], pok[j]
			}
		}
	} else {
		for i, r := range vectors {
			xyz[i], ok[i] = basis.Project(r.Vector)
		}
	}
	kept := batch[:0]
	for i := range batch {
		if !ok[i] {
			rep.Skipped++
			rep.Placed--
			continue
		}
		batch[i].xyz = xyz[i]
		kept = append(kept, batch[i])
	}
	return kept, nil
}

// queryAmongHits is queryProjection on a UMAP map: see there. hits are already
// only chunks the caller may read — search restricts its candidates to readable
// documents before ranking (resolveSearchACL) — which is what keeps the answer
// from pointing at unreadable ones.
func (d *Document) queryAmongHits(ctx context.Context, key sqlmem.ScopeKey, model string, version int, hits []projectionHit) ([]float64, bool, error) {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.chunkID
	}
	held, err := d.projectionXYZ(ctx, key, model, version, ids)
	if err != nil {
		return nil, false, err
	}
	placed := make([]projectionHit, 0, len(hits))
	for _, h := range hits {
		if _, ok := held[h.chunkID]; ok {
			placed = append(placed, h)
		}
	}
	if len(placed) == 0 {
		return nil, false, nil
	}
	// Nearest first; the chunk id breaks a tie so the answer does not depend on
	// the order the hits were ranked in.
	slices.SortFunc(placed, func(a, b projectionHit) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return strings.Compare(a.chunkID, b.chunkID)
	})
	placed = placed[:min(len(placed), projection.DefaultNeighbors)]
	dist := make([]float32, len(placed))
	pos := make([][3]float64, len(placed))
	for i, h := range placed {
		dist[i] = float32(max(0, 1-h.score))
		pos[i] = held[h.chunkID]
	}
	xyz := projection.PlaceAmong(dist, pos)
	return []float64{projection.Round4(xyz[0]), projection.Round4(xyz[1]), projection.Round4(xyz[2])}, true, nil
}
