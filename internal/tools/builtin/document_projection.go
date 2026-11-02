package builtin

// document_projection.go — the 3D layout of a scope's chunks by meaning (RFC FQ P2).
//
// Each chunk's stored embedding is placed on the scope's top three principal
// components, fitted from a sample of the scope's own chunk vectors, and the three
// coordinates are cached per chunk in chunk_projection. graph_snapshot serves them
// as its projection block and search can place its query on the same map. Vectors
// never leave the runtime: the pass reads them in-process and stores only x, y, z.
//
// The basis (chunk_projection_basis) is per scope and per embedder model, versioned:
// a refit writes a new version and re-places every chunk on it; a coordinate of an
// older version, or of another model, is never served — it counts as missing until
// the pass catches up — so one map never mixes two bases.
//
// It runs only when the operator turns it on (memory.projection). Off, nothing
// here reads a table: graph_snapshot answers the empty block, search leaves
// query_xyz out, and the operator route refuses.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/projection"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// chunkProjectionDDL is in docSchemaDDL. DOUBLE PRECISION, not REAL: postgres REAL
// is float4, so the two tiers would store different coordinates for one chunk (see
// chunk_memory_meta.confidence). No foreign key — cascade is explicit in Go, at
// every site that deletes a chunk row.
const chunkProjectionDDL = `CREATE TABLE IF NOT EXISTS chunk_projection (
		chunk_id TEXT PRIMARY KEY, model TEXT NOT NULL, basis_version INTEGER NOT NULL,
		x DOUBLE PRECISION NOT NULL, y DOUBLE PRECISION NOT NULL, z DOUBLE PRECISION NOT NULL,
		updated_at BIGINT NOT NULL)`

// chunkProjectionBasisDDL is built per tier, like chunk_assets: mean and components
// are binary (float32 little-endian), BLOB on sqlite and BYTEA on postgres.
func chunkProjectionBasisDDL(binType string) string {
	return `CREATE TABLE IF NOT EXISTS chunk_projection_basis (
		model TEXT NOT NULL, version INTEGER NOT NULL, dimension INTEGER NOT NULL,
		mean ` + binType + ` NOT NULL, components ` + binType + ` NOT NULL,
		explained_variance TEXT NOT NULL, sample_n INTEGER NOT NULL, fitted_at BIGINT NOT NULL,
		PRIMARY KEY (model, version))`
}

const (
	// projectionPage is the vectors read per store call (the store's cap).
	projectionPage = 1000
	// projectionDefaultPlaceLimit bounds the chunks one pass places.
	projectionDefaultPlaceLimit = 5000
	// projectionSampleBytesMax caps the fit sample's memory, on top of sample_max:
	// 20000 vectors of 3072 float32 would hold 245 MB for the length of a fit. At
	// this cap a 3072-dimension sample keeps 5461 vectors and a 768-dimension one
	// 21845 — sample_max governs below that.
	projectionSampleBytesMax = 64 << 20
	// projectionReadBatch is the chunk ids per IN list: under the SQL Memory row cap.
	projectionReadBatch = 500
	// projectionInsertBatch is the rows per multi-row INSERT (7 binds each).
	projectionInsertBatch = 200
)

// errProjectionDisabled is the operator pass's refusal when memory.projection is off.
var errProjectionDisabled = errors.New("document projection is off (memory.projection.enabled)")

// ErrProjectionDisabled reports the operator pass refused because the projection is off.
func ErrProjectionDisabled(err error) bool { return errors.Is(err, errProjectionDisabled) }

// ProjectionOptions steers one pass. The rest of its settings are the operator's
// memory.projection block.
type ProjectionOptions struct {
	// Refit forces a new basis even when the current one is fresh.
	Refit bool
	// DryRun reports what the pass would do and writes nothing.
	DryRun bool
	// PlaceLimit bounds the chunks placed; <= 0 is the default (5000).
	PlaceLimit int
}

// ProjectionReport is one pass over one scope.
type ProjectionReport struct {
	Scope  string `json:"scope"`
	DryRun bool   `json:"dry_run"`
	// Model is the embedder whose vectors were placed, "<provider>/<model>".
	Model string `json:"model"`
	// BasisVersion is the basis the scope's coordinates are on after the pass (in a
	// dry run, the one they would be on); 0 = none yet.
	BasisVersion int `json:"basis_version"`
	// Placed chunks were given coordinates on the basis — or, in a dry run, would be.
	Placed int `json:"placed"`
	// Refitted is true when the pass fitted a new basis (or would).
	Refitted bool `json:"refitted"`
	// Skipped vectors needed nothing: already placed, another model's, another
	// dimension, or a body whose chunk is gone.
	Skipped int `json:"skipped"`
	// Unsupported is true when the store has no vector plane: nothing to place.
	Unsupported bool `json:"unsupported"`
	// More is true when the place limit stopped the pass with chunks still to place.
	More bool `json:"more"`
}

// projectionSettings is the operator's resolved block; nil Cfg is off.
func (d *Document) projectionSettings() config.ProjectionSettings {
	return d.Cfg.DocumentProjection()
}

// projectionModel is the "<provider>/<model>" a vector's producer is keyed by, or
// "" with no embedder.
func (d *Document) projectionModel() string {
	if d.Embedder == nil {
		return ""
	}
	return d.Embedder.Provider() + "/" + d.Embedder.Model()
}

func rowModel(r store.MemoryEmbedRow) string { return r.Provider + "/" + r.Model }

// ProjectScope runs one pass over the caller's scope (the operator route): the
// tenant and subject come from ctx, as for the re-index.
func (d *Document) ProjectScope(ctx context.Context, scope string, opts ProjectionOptions) (ProjectionReport, error) {
	rep := ProjectionReport{Scope: scope, DryRun: opts.DryRun}
	if d.Store == nil || d.SqlMem == nil {
		return rep, fmt.Errorf("document projection: SQL Memory is not configured")
	}
	if !d.projectionSettings().Enabled {
		return rep, errProjectionDisabled
	}
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return rep, err
	}
	return d.projectPass(ctx, key, mscope, direntTenant(ctx), opts, rep)
}

// ProjectScopeKey runs one pass over a scope the sweeper found. The memory tenant
// is whichever spelling of the scope's tenant holds its chunk vectors (see
// bodyTenantsFor); a scope with no chunk vectors is left untouched — its SQL is
// not even provisioned.
func (d *Document) ProjectScopeKey(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, opts ProjectionOptions) (ProjectionReport, error) {
	rep := ProjectionReport{Scope: key.Scope, DryRun: opts.DryRun}
	if d.Store == nil || d.SqlMem == nil || !d.projectionSettings().Enabled {
		return rep, errProjectionDisabled
	}
	if !d.Store.SupportsVectors() {
		rep.Unsupported = true
		return rep, nil
	}
	for _, tenant := range bodyTenantsFor(key.Tenant) {
		page, err := d.Store.MemoryEmbedListVectors(ctx, tenant, mscope, docMemScopeID(mscope, key), chunkBodyKeyPrefix, "", 1)
		if errors.Is(err, store.ErrVectorUnsupported) {
			rep.Unsupported = true
			return rep, nil
		}
		if err != nil {
			return rep, err
		}
		if len(page) > 0 {
			return d.projectPass(ctx, key, mscope, tenant, opts, rep)
		}
	}
	return rep, nil
}

// projectPass places the scope's chunks on the current basis, refitting first when
// the basis is missing, stale enough, or the caller forces it.
func (d *Document) projectPass(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, tenant string,
	opts ProjectionOptions, rep ProjectionReport) (ProjectionReport, error) {
	if d.Embedder == nil {
		return rep, fmt.Errorf("document projection: no embedder is configured, so there is no model to place vectors of")
	}
	if !d.Store.SupportsVectors() {
		rep.Unsupported = true
		return rep, nil
	}
	settings := d.projectionSettings()
	model := d.projectionModel()
	rep.Model = model
	limit := opts.PlaceLimit
	if limit <= 0 {
		limit = projectionDefaultPlaceLimit
	}
	scopeID := docMemScopeID(mscope, key)
	list := func(after string) ([]store.MemoryEmbedRow, error) {
		return d.Store.MemoryEmbedListVectors(ctx, tenant, mscope, scopeID, chunkBodyKeyPrefix, after, projectionPage)
	}
	// Vectors first: a scope with none is answered before its SQL is touched.
	first, err := list("")
	if errors.Is(err, store.ErrVectorUnsupported) {
		rep.Unsupported = true
		return rep, nil
	}
	if err != nil {
		return rep, err
	}
	if len(first) == 0 {
		return rep, nil
	}
	if err := d.ensureSchema(ctx, key); err != nil {
		return rep, err
	}

	now := time.Now().UnixNano()
	cur, err := d.loadBasis(ctx, key, model)
	if err != nil {
		return rep, err
	}
	refit := opts.Refit || cur == nil
	if !refit && time.Duration(now-cur.fittedAt) >= settings.RefitAfter {
		fresh, err := d.placedSince(ctx, key, model, cur.version, cur.fittedAt)
		if err != nil {
			return rep, err
		}
		refit = fresh >= settings.RefitMinNew
	}

	basis := cur
	if refit {
		rep.Refitted = true
		next := 1
		if cur != nil {
			next = cur.version + 1
		}
		if opts.DryRun {
			// What a refit would do is re-place every chunk; fitting to count that
			// would be work a dry run exists to avoid.
			rep.BasisVersion = next
			return d.placeChunks(ctx, key, list, first, model, &storedBasis{version: next, fittedAt: now}, true, now, limit, rep)
		}
		sample, err := d.projectionSample(list, first, model, settings.SampleMax)
		if err != nil {
			return rep, err
		}
		if len(sample) == 0 {
			// Vectors exist, none of this model: nothing to fit.
			rep.Refitted = false
			if cur != nil {
				rep.BasisVersion = cur.version
			}
			return rep, nil
		}
		var prev *projection.Basis
		if cur != nil {
			prev = &cur.Basis
		}
		fitted, err := projection.Fit(sample, prev)
		if err != nil {
			return rep, err
		}
		basis = &storedBasis{Basis: fitted, version: next, fittedAt: now}
		if err := d.saveBasis(ctx, key, model, basis); err != nil {
			return rep, err
		}
	}
	rep.BasisVersion = basis.version
	return d.placeChunks(ctx, key, list, first, model, basis, refit, now, limit, rep)
}

// projectionSample streams the scope's vectors of model into a reservoir. The
// sample's dimension is the first matching vector's; another dimension under the
// same model name is not fitted.
func (d *Document) projectionSample(list func(string) ([]store.MemoryEmbedRow, error), first []store.MemoryEmbedRow,
	model string, sampleMax int) ([][]float32, error) {
	var res *projection.Reservoir
	dim := 0
	page := first
	for {
		for _, r := range page {
			if rowModel(r) != model || len(r.Vector) == 0 {
				continue
			}
			if res == nil {
				dim = len(r.Vector)
				res = projection.NewReservoir(max(1, min(sampleMax, projectionSampleBytesMax/(4*dim))))
			}
			if len(r.Vector) == dim {
				res.Offer(r.Vector)
			}
		}
		if len(page) < projectionPage {
			break
		}
		var err error
		if page, err = list(page[len(page)-1].Key); err != nil {
			return nil, err
		}
	}
	if res == nil {
		return nil, nil
	}
	return res.Sample(), nil
}

// placeChunks walks the scope's vectors and gives every chunk that needs one a
// coordinate on basis: a chunk with no row, a row of another model or an older
// basis, or a row older than its chunk's current vector (the body was re-embedded).
//
// updated_at says WHEN a row's position became news, which is what a refit counts:
// a row placed because its vector is new is stamped now; one placed only because
// the basis moved (every row of a refit, and the rows a refit's place limit left
// to later passes) is stamped with the basis's fit time, so it never counts as new
// content toward the next refit.
func (d *Document) placeChunks(ctx context.Context, key sqlmem.ScopeKey, list func(string) ([]store.MemoryEmbedRow, error),
	first []store.MemoryEmbedRow, model string, basis *storedBasis, refitRun bool, now int64, limit int,
	rep ProjectionReport) (ProjectionReport, error) {
	page := first
	for {
		var cands []store.MemoryEmbedRow
		ids := make([]string, 0, len(page))
		for _, r := range page {
			id := ChunkIDFromBodyKey(r.Key)
			if id == "" || rowModel(r) != model || (basis.Dimension > 0 && len(r.Vector) != basis.Dimension) {
				rep.Skipped++
				continue
			}
			cands = append(cands, r)
			ids = append(ids, id)
		}
		live, err := d.existingChunkIDs(ctx, key, ids)
		if err != nil {
			return rep, err
		}
		held, err := d.projectionRows(ctx, key, ids)
		if err != nil {
			return rep, err
		}
		var batch []placedRow
		for _, r := range cands {
			id := ChunkIDFromBodyKey(r.Key)
			if !live[id] {
				rep.Skipped++ // a body whose chunk row is gone: the dead-link GC's
				continue
			}
			var embeddedAt int64 // an embedding with no write time is never "newer"
			if !r.CreatedAt.IsZero() {
				embeddedAt = r.CreatedAt.UnixNano()
			}
			h, has := held[id]
			if has && h.model == model && h.version == basis.version && h.updatedAt >= embeddedAt {
				rep.Skipped++
				continue
			}
			if rep.Placed >= limit {
				rep.More = true
				return rep, d.writeProjection(ctx, key, batch, writeFor(rep))
			}
			stamp := now
			if refitRun || (has && h.model == model && h.updatedAt >= embeddedAt) {
				stamp = basis.fittedAt
			}
			row := placedRow{chunkID: id, updatedAt: stamp}
			if !rep.DryRun {
				xyz, ok := basis.Project(r.Vector)
				if !ok {
					rep.Skipped++
					continue
				}
				row.xyz = xyz
			}
			batch = append(batch, row)
			rep.Placed++
		}
		if err := d.writeProjection(ctx, key, batch, writeFor(rep)); err != nil {
			return rep, err
		}
		if len(page) < projectionPage {
			return rep, nil
		}
		if page, err = list(page[len(page)-1].Key); err != nil {
			return rep, err
		}
	}
}

// writeFor carries what writeProjection needs from the report: the model and
// version it writes under, and whether it writes at all.
func writeFor(rep ProjectionReport) projectionWrite {
	return projectionWrite{model: rep.Model, version: rep.BasisVersion, dryRun: rep.DryRun}
}

type projectionWrite struct {
	model   string
	version int
	dryRun  bool
}

type placedRow struct {
	chunkID   string
	xyz       [projection.Components]float64
	updatedAt int64
}

// writeProjection replaces the batch's rows in one transaction, so a reader never
// sees a chunk between its old row and its new one. Portable upsert: delete, then
// insert, as the rest of this schema does.
func (d *Document) writeProjection(ctx context.Context, key sqlmem.ScopeKey, rows []placedRow, w projectionWrite) error {
	if w.dryRun || len(rows) == 0 {
		return nil
	}
	return d.withSqlTxn(ctx, key, func(txnID string) error {
		for start := 0; start < len(rows); start += projectionInsertBatch {
			part := rows[start:min(start+projectionInsertBatch, len(rows))]
			ids := make([]string, len(part))
			for i, r := range part {
				ids[i] = r.chunkID
			}
			ph, args := inPlaceholders(ids)
			if err := d.execTxn(ctx, txnID, `DELETE FROM chunk_projection WHERE chunk_id IN (`+ph+`)`, args...); err != nil {
				return err
			}
			values := make([]string, len(part))
			ins := make([]any, 0, 7*len(part))
			for i, r := range part {
				values[i] = "(?, ?, ?, ?, ?, ?, ?)"
				ins = append(ins, r.chunkID, w.model, w.version, r.xyz[0], r.xyz[1], r.xyz[2], r.updatedAt)
			}
			if err := d.execTxn(ctx, txnID, `INSERT INTO chunk_projection (chunk_id, model, basis_version, x, y, z, updated_at) VALUES `+
				strings.Join(values, ", "), ins...); err != nil {
				return err
			}
		}
		return nil
	})
}

// storedBasis is a basis as chunk_projection_basis holds it.
type storedBasis struct {
	projection.Basis
	version  int
	fittedAt int64
}

// loadBasis reads model's latest basis, or nil when it has none.
func (d *Document) loadBasis(ctx context.Context, key sqlmem.ScopeKey, model string) (*storedBasis, error) {
	res, err := d.query(ctx, key, `SELECT version, dimension, mean, components, sample_n, fitted_at FROM chunk_projection_basis
		WHERE model = ? ORDER BY version DESC LIMIT 1`, model)
	if err != nil {
		return nil, err
	}
	if len(res.Rows) == 0 {
		return nil, nil
	}
	r := res.Rows[0]
	b := &storedBasis{version: asInt(r[0]), fittedAt: int64OrZero(r[5])}
	b.Dimension, b.SampleN = asInt(r[1]), asInt(r[4])
	if b.Mean, err = projection.DecodeFloat32s(asBytes(r[2]), b.Dimension); err != nil {
		return nil, fmt.Errorf("chunk_projection_basis %s v%d mean: %w", model, b.version, err)
	}
	comps, err := projection.DecodeFloat32s(asBytes(r[3]), projection.Components*b.Dimension)
	if err != nil {
		return nil, fmt.Errorf("chunk_projection_basis %s v%d components: %w", model, b.version, err)
	}
	for k := range projection.Components {
		b.Components[k] = comps[k*b.Dimension : (k+1)*b.Dimension]
	}
	return b, nil
}

// latestBasisVersion is model's latest basis version, 0 for none, without reading
// the basis itself.
func (d *Document) latestBasisVersion(ctx context.Context, key sqlmem.ScopeKey, model string) (int, error) {
	res, err := d.query(ctx, key, `SELECT max(version) FROM chunk_projection_basis WHERE model = ?`, model)
	if err != nil || len(res.Rows) == 0 {
		return 0, err
	}
	return asInt(res.Rows[0][0]), nil
}

// saveBasis writes b as model's newest version and prunes all but the version
// before it: that one is kept for one refit (its rows still name it until they are
// re-placed), anything older is no basis any row can need.
func (d *Document) saveBasis(ctx context.Context, key sqlmem.ScopeKey, model string, b *storedBasis) error {
	comps := make([]float32, 0, projection.Components*b.Dimension)
	for k := range projection.Components {
		comps = append(comps, b.Components[k]...)
	}
	ev := fmt.Sprintf("[%s,%s,%s]", fmtFloat(b.ExplainedVariance[0]), fmtFloat(b.ExplainedVariance[1]), fmtFloat(b.ExplainedVariance[2]))
	return d.withSqlTxn(ctx, key, func(txnID string) error {
		if err := d.execTxn(ctx, txnID, `INSERT INTO chunk_projection_basis
			(model, version, dimension, mean, components, explained_variance, sample_n, fitted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			model, b.version, b.Dimension, projection.EncodeFloat32s(b.Mean), projection.EncodeFloat32s(comps), ev, b.SampleN, b.fittedAt); err != nil {
			return err
		}
		return d.execTxn(ctx, txnID, `DELETE FROM chunk_projection_basis WHERE model = ? AND version < ?`, model, b.version-1)
	})
}

func fmtFloat(f float64) string { return fmt.Sprintf("%.6g", f) }

func int64OrZero(v any) int64 {
	n, _ := asInt64(v)
	return n
}

// placedSince counts the rows of (model, version) whose position is newer than the
// fit — what refit_min_new is measured against (see placeChunks on the stamp).
func (d *Document) placedSince(ctx context.Context, key sqlmem.ScopeKey, model string, version int, fittedAt int64) (int, error) {
	res, err := d.query(ctx, key, `SELECT count(*) FROM chunk_projection WHERE model = ? AND basis_version = ? AND updated_at > ?`,
		model, version, fittedAt)
	if err != nil || len(res.Rows) == 0 {
		return 0, err
	}
	return asInt(res.Rows[0][0]), nil
}

type heldProjection struct {
	model     string
	version   int
	updatedAt int64
}

// projectionRows reads the stored projection rows of ids.
func (d *Document) projectionRows(ctx context.Context, key sqlmem.ScopeKey, ids []string) (map[string]heldProjection, error) {
	out := make(map[string]heldProjection, len(ids))
	for start := 0; start < len(ids); start += projectionReadBatch {
		ph, args := inPlaceholders(ids[start:min(start+projectionReadBatch, len(ids))])
		res, err := d.query(ctx, key, `SELECT chunk_id, model, basis_version, updated_at FROM chunk_projection WHERE chunk_id IN (`+ph+`)`, args...)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Rows {
			out[asStr(r[0])] = heldProjection{model: asStr(r[1]), version: asInt(r[2]), updatedAt: int64OrZero(r[3])}
		}
	}
	return out, nil
}

// existingChunkIDs is the subset of ids that are chunk rows.
func (d *Document) existingChunkIDs(ctx context.Context, key sqlmem.ScopeKey, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	for start := 0; start < len(ids); start += projectionReadBatch {
		ph, args := inPlaceholders(ids[start:min(start+projectionReadBatch, len(ids))])
		res, err := d.query(ctx, key, `SELECT id FROM chunks WHERE id IN (`+ph+`)`, args...)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Rows {
			out[asStr(r[0])] = true
		}
	}
	return out, nil
}

// emptyProjectionBlock is graph_snapshot's projection block with nothing placed:
// every chunk on the page missing.
func emptyProjectionBlock(model string, n int) map[string]any {
	missing := make([]int, n)
	for i := range missing {
		missing[i] = i
	}
	return map[string]any{"model": model, "basis_version": 0, "xyz": []float64{}, "missing": missing}
}

// snapshotProjection is graph_snapshot's projection block for the page's chunks,
// ids in the chunks section's order: the current model's latest basis, one rounded triple per chunk,
// 0,0,0 and an entry in missing for a chunk with no coordinate on that basis (none
// yet, another model's, or an older basis's). Off, without an embedder or on a
// store with no vectors, it is the empty block and no table is read; with no basis
// yet it is the empty block under the model's name.
func (d *Document) snapshotProjection(ctx context.Context, key sqlmem.ScopeKey, ids []string) (map[string]any, error) {
	model := d.projectionModel()
	if !d.projectionSettings().Enabled || model == "" || !d.Store.SupportsVectors() {
		return emptyProjectionBlock("", len(ids)), nil
	}
	version, err := d.latestBasisVersion(ctx, key, model)
	if err != nil {
		return nil, err
	}
	if version == 0 {
		return emptyProjectionBlock(model, len(ids)), nil
	}
	at := make(map[string][3]float64, len(ids))
	for start := 0; start < len(ids); start += projectionReadBatch {
		ph, args := inPlaceholders(ids[start:min(start+projectionReadBatch, len(ids))])
		res, err := d.query(ctx, key, `SELECT chunk_id, x, y, z FROM chunk_projection WHERE model = ? AND basis_version = ? AND chunk_id IN (`+ph+`)`,
			append([]any{model, version}, args...)...)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Rows {
			at[asStr(r[0])] = [3]float64{asF64(r[1]), asF64(r[2]), asF64(r[3])}
		}
	}
	xyz := make([]float64, 0, 3*len(ids))
	missing := []int{}
	for i, id := range ids {
		p, ok := at[id]
		if !ok {
			missing = append(missing, i)
		}
		xyz = append(xyz, projection.Round4(p[0]), projection.Round4(p[1]), projection.Round4(p[2]))
	}
	return map[string]any{"model": model, "basis_version": version, "xyz": xyz, "missing": missing}, nil
}

// queryProjection places a search's query vector on the current basis, or reports
// false: off, no embedder, no basis, or a vector of another dimension.
func (d *Document) queryProjection(ctx context.Context, key sqlmem.ScopeKey, vec []float32) ([]float64, bool, error) {
	model := d.projectionModel()
	if len(vec) == 0 || model == "" || !d.projectionSettings().Enabled {
		return nil, false, nil
	}
	b, err := d.loadBasis(ctx, key, model)
	if err != nil || b == nil {
		return nil, false, err
	}
	xyz, ok := b.Project(vec)
	if !ok {
		return nil, false, nil
	}
	return []float64{projection.Round4(xyz[0]), projection.Round4(xyz[1]), projection.Round4(xyz[2])}, true, nil
}

// asF64 reads a numeric cell; a NOT NULL column, so a miss is 0.
func asF64(v any) float64 {
	if p := asFloat64Ptr(v); p != nil {
		return *p
	}
	return 0
}
