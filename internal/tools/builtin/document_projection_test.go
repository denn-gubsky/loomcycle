package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/projection"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// projectionOn is an operator config with the projection enabled and no sweeper.
func projectionOn(mod func(*config.ProjectionConfig)) *config.Config {
	cfg := &config.Config{}
	cfg.Memory.Projection.Enabled = true
	if mod != nil {
		mod(&cfg.Memory.Projection)
	}
	return cfg
}

// projectionFixture is a document tool over a vector-capable fake store, with the
// projection on and the embedder fake/m1.
func projectionFixture(t *testing.T, vocab ...string) (*Document, *vectorStore, context.Context) {
	t.Helper()
	d, vs, ctx := mermaidDocFixture(t, vocab...)
	d.Cfg = projectionOn(nil)
	return d, vs, ctx
}

// projectionBothTiers runs fn on the sqlite SQL Memory tier and, with
// LOOMCYCLE_TEST_SQLMEM_PG_DSN, on the postgres tier: the projection tables hold
// binary columns and doubles, which is where the tiers can differ.
func projectionBothTiers(t *testing.T, fn func(*testing.T, *Document, *vectorStore, context.Context)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		d, vs, ctx := projectionFixture(t)
		fn(t, d, vs, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		vs := newVectorStore(d.Store)
		d.Store = vs
		d.Embedder = newFakeEmbedder("fake", "m1")
		d.Cfg = projectionOn(nil)
		fn(t, d, vs, ctx)
	})
}

// synthVectors draws n 8-dimension vectors that vary along three fixed directions
// with distinct spreads, so the fit has a well-defined answer.
func synthVectors(n int, seed uint64) [][]float32 {
	rng := rand.New(rand.NewPCG(seed, 99))
	dirs := [3][]float32{{1, 0, 0, 0, 0, 0, 0, 0}, {0, 1, 1, 0, 0, 0, 0, 0}, {0, 0, 0, 0, 1, 0, 0, 1}}
	sd := [3]float64{5, 3, 1}
	out := make([][]float32, n)
	for i := range out {
		v := make([]float32, 8)
		for j := range v {
			v[j] = float32(2 + rng.NormFloat64()*0.01)
		}
		for k, dir := range dirs {
			c := float32(rng.NormFloat64() * sd[k])
			for j := range v {
				v[j] += c * dir[j]
			}
		}
		out[i] = v
	}
	return out
}

// setVec overwrites a chunk's stored embedding.
func setVec(t *testing.T, vs *vectorStore, ctx context.Context, chunkID, model string, vec []float32, at time.Time) {
	t.Helper()
	if err := vs.MemoryEmbedSet(ctx, "", store.MemoryScopeUser, "u1", chunkBodyKey(chunkID), store.MemoryEmbedding{
		Provider: "fake", Model: model, Dimension: len(vec), Vector: vec, EmbedText: chunkID, CreatedAt: at,
	}); err != nil {
		t.Fatalf("MemoryEmbedSet: %v", err)
	}
}

// projDoc makes a document with one chunk per vector and gives each chunk that
// vector under fake/m1.
func projDoc(t *testing.T, d *Document, vs *vectorStore, ctx context.Context, title string, vecs [][]float32) []string {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"user","title":%q}`, title))
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	doc := out["document_id"].(string)
	ids := make([]string, len(vecs))
	for i, v := range vecs {
		o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Part %d","body":"text %d"}`, doc, i, i))
		if r.IsError {
			t.Fatalf("create_chunk: %s", r.Text)
		}
		ids[i] = o["id"].(string)
		setVec(t, vs, ctx, ids[i], "m1", v, time.Now())
	}
	return ids
}

func project(t *testing.T, d *Document, ctx context.Context, opts ProjectionOptions) ProjectionReport {
	t.Helper()
	rep, err := d.ProjectScope(ctx, "user", opts)
	if err != nil {
		t.Fatalf("ProjectScope: %v", err)
	}
	return rep
}

type projSnap struct {
	ids          []string
	model        string
	basisVersion int
	xyz          []float64
	rawXYZ       json.RawMessage
	missing      []int
}

// at is a chunk's coordinates on the snapshot, and whether it was placed.
func (s projSnap) at(t *testing.T, id string) ([3]float64, bool) {
	t.Helper()
	i := slices.Index(s.ids, id)
	if i < 0 {
		t.Fatalf("chunk %s is not on the snapshot", id)
	}
	var p [3]float64
	if len(s.xyz) > 0 {
		copy(p[:], s.xyz[3*i:3*i+3])
	}
	return p, !slices.Contains(s.missing, i)
}

func snapProjection(t *testing.T, d *Document, ctx context.Context) projSnap {
	t.Helper()
	_, text := graphSnap(t, d, ctx, `{"op":"graph_snapshot","scope":"user","include":["documents","chunks","projection"]}`)
	var raw struct {
		Chunks struct {
			IDs []string `json:"ids"`
		} `json:"chunks"`
		Projection struct {
			Model        string          `json:"model"`
			BasisVersion int             `json:"basis_version"`
			XYZ          json.RawMessage `json:"xyz"`
			Missing      []int           `json:"missing"`
		} `json:"projection"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		t.Fatalf("decode %s: %v", text, err)
	}
	s := projSnap{ids: raw.Chunks.IDs, model: raw.Projection.Model, basisVersion: raw.Projection.BasisVersion,
		rawXYZ: raw.Projection.XYZ, missing: raw.Projection.Missing}
	if err := json.Unmarshal(raw.Projection.XYZ, &s.xyz); err != nil {
		t.Fatalf("decode xyz: %v", err)
	}
	if s.basisVersion > 0 && len(s.xyz) != 3*len(s.ids) {
		t.Fatalf("xyz holds %d values for %d chunks, want one triple each", len(s.xyz), len(s.ids))
	}
	return s
}

// TestProjection_StableAcrossTwoPassesOnUnchangedData — a second pass over data
// that did not change places nothing and serves byte-equal coordinates, and even
// a forced refit of the same data lands every chunk where it was: the sample, the
// fit and the orientation are all deterministic.
func TestProjection_StableAcrossTwoPassesOnUnchangedData(t *testing.T) {
	projectionBothTiers(t, func(t *testing.T, d *Document, vs *vectorStore, ctx context.Context) {
		projDoc(t, d, vs, ctx, "Map", synthVectors(24, 1))
		first := project(t, d, ctx, ProjectionOptions{})
		if !first.Refitted || first.Placed != 24 || first.BasisVersion != 1 || first.Model != "fake/m1" {
			t.Fatalf("first pass = %+v, want a fit (v1) and 24 chunks placed", first)
		}
		s1 := snapProjection(t, d, ctx)
		if s1.basisVersion != 1 || s1.model != "fake/m1" || len(s1.missing) != 1 {
			t.Fatalf("snapshot = %+v, want v1 with only the bodyless root missing", s1)
		}

		second := project(t, d, ctx, ProjectionOptions{})
		if second.Refitted || second.Placed != 0 || second.Skipped != 24 || second.BasisVersion != 1 {
			t.Fatalf("second pass = %+v, want nothing to do", second)
		}
		if s2 := snapProjection(t, d, ctx); !bytes.Equal(s2.rawXYZ, s1.rawXYZ) {
			t.Fatalf("xyz moved on a pass over unchanged data:\n%s\n%s", s1.rawXYZ, s2.rawXYZ)
		}

		forced := project(t, d, ctx, ProjectionOptions{Refit: true})
		if !forced.Refitted || forced.BasisVersion != 2 || forced.Placed != 24 {
			t.Fatalf("forced refit = %+v, want v2 with every chunk re-placed", forced)
		}
		s3 := snapProjection(t, d, ctx)
		if s3.basisVersion != 2 || !bytes.Equal(s3.rawXYZ, s1.rawXYZ) {
			t.Fatalf("a refit of unchanged data moved the map (v%d):\n%s\n%s", s3.basisVersion, s1.rawXYZ, s3.rawXYZ)
		}
	})
}

// TestProjection_NewChunkPlacedWithoutRefit — a chunk written after the fit is
// placed on the existing basis without a refit, and the chunks already placed stay
// put; a chunk whose vector changed is re-placed.
func TestProjection_NewChunkPlacedWithoutRefit(t *testing.T) {
	d, vs, ctx := projectionFixture(t)
	vecs := synthVectors(25, 2)
	ids := projDoc(t, d, vs, ctx, "Map", vecs[:24])
	project(t, d, ctx, ProjectionOptions{})
	before := snapProjection(t, d, ctx)

	late := projDoc(t, d, vs, ctx, "Late", vecs[24:])[0]
	rep := project(t, d, ctx, ProjectionOptions{})
	if rep.Refitted || rep.Placed != 1 || rep.BasisVersion != 1 {
		t.Fatalf("pass after one new chunk = %+v, want it placed on v1 without a refit", rep)
	}
	after := snapProjection(t, d, ctx)
	if _, ok := after.at(t, late); !ok {
		t.Fatalf("the new chunk %s is still missing (missing=%v)", late, after.missing)
	}
	for _, id := range ids {
		was, _ := before.at(t, id)
		if now, _ := after.at(t, id); now != was {
			t.Fatalf("chunk %s moved from %v to %v though its vector did not change", id, was, now)
		}
	}

	// A re-embedded chunk (newer vector) is re-placed, still on v1.
	setVec(t, vs, ctx, ids[0], "m1", vecs[5], time.Now().Add(time.Second))
	rep = project(t, d, ctx, ProjectionOptions{})
	if rep.Refitted || rep.Placed != 1 {
		t.Fatalf("pass after a re-embed = %+v, want one chunk re-placed", rep)
	}
	moved, _ := snapProjection(t, d, ctx).at(t, ids[0])
	if want, _ := after.at(t, ids[5]); moved != want {
		t.Fatalf("re-embedded chunk sits at %v, want %v (it now has chunk 5's vector)", moved, want)
	}
}

// TestProjection_RefitKeepsOrientation — a refit's axes point the way the previous
// basis's did, so the map does not flip under a viewer. The stored basis is
// negated first: a refit that ignored it would come back with the opposite signs.
func TestProjection_RefitKeepsOrientation(t *testing.T) {
	d, vs, ctx := projectionFixture(t)
	projDoc(t, d, vs, ctx, "Map", synthVectors(24, 3))
	project(t, d, ctx, ProjectionOptions{})
	key := sidecarScope(t, d, ctx)
	v1, err := d.loadBasis(ctx, key, "fake/m1")
	if err != nil || v1 == nil {
		t.Fatalf("loadBasis: %v %v", v1, err)
	}
	var negated []float32
	for k := range projection.Components {
		for _, x := range v1.Components[k] {
			negated = append(negated, -x)
		}
	}
	if err := d.exec(ctx, key, `UPDATE chunk_projection_basis SET components = ? WHERE model = ? AND version = 1`,
		projection.EncodeFloat32s(negated), "fake/m1"); err != nil {
		t.Fatal(err)
	}
	prev, _ := d.loadBasis(ctx, key, "fake/m1")

	projDoc(t, d, vs, ctx, "More", synthVectors(12, 4))
	if rep := project(t, d, ctx, ProjectionOptions{Refit: true}); rep.BasisVersion != 2 {
		t.Fatalf("refit = %+v", rep)
	}
	v2, _ := d.loadBasis(ctx, key, "fake/m1")
	for k := range projection.Components {
		var dot float64
		for j, x := range v2.Components[k] {
			dot += float64(x) * float64(prev.Components[k][j])
		}
		if dot <= 0.9 {
			t.Errorf("component %d: dot with the previous basis = %.4f, want ≈ +1", k, dot)
		}
	}

	// The previous version is kept for one refit, then pruned.
	project(t, d, ctx, ProjectionOptions{Refit: true})
	res, err := d.query(ctx, key, `SELECT version FROM chunk_projection_basis WHERE model = ? ORDER BY version`, "fake/m1")
	if err != nil {
		t.Fatal(err)
	}
	var versions []int
	for _, r := range res.Rows {
		versions = append(versions, asInt(r[0]))
	}
	if !slices.Equal(versions, []int{2, 3}) {
		t.Errorf("basis versions kept = %v, want [2 3]", versions)
	}
}

// TestProjection_RefitsOnlyWhenStaleAndEnoughNewChunks — past refit_after, a refit
// waits for refit_min_new chunks placed since the fit. Rows re-placed by a refit
// itself do not count as new.
func TestProjection_RefitsOnlyWhenStaleAndEnoughNewChunks(t *testing.T) {
	d, vs, ctx := projectionFixture(t)
	d.Cfg = projectionOn(func(p *config.ProjectionConfig) { p.RefitAfter = "1ns"; p.RefitMinNew = 2 })
	vecs := synthVectors(26, 5)
	projDoc(t, d, vs, ctx, "Map", vecs[:24])
	if rep := project(t, d, ctx, ProjectionOptions{}); rep.BasisVersion != 1 {
		t.Fatalf("first pass = %+v", rep)
	}
	if rep := project(t, d, ctx, ProjectionOptions{}); rep.Refitted {
		t.Fatalf("refitted with no new chunks: %+v", rep)
	}
	projDoc(t, d, vs, ctx, "One", vecs[24:25])
	if rep := project(t, d, ctx, ProjectionOptions{}); rep.Refitted || rep.Placed != 1 {
		t.Fatalf("pass with one new = %+v, want it placed, no refit", rep)
	}
	if rep := project(t, d, ctx, ProjectionOptions{}); rep.Refitted {
		t.Fatalf("refitted at one new chunk of two: %+v", rep)
	}
	projDoc(t, d, vs, ctx, "Two", vecs[25:26])
	project(t, d, ctx, ProjectionOptions{})
	if rep := project(t, d, ctx, ProjectionOptions{}); !rep.Refitted || rep.BasisVersion != 2 {
		t.Fatalf("pass after two new chunks = %+v, want a refit to v2", rep)
	}
	if rep := project(t, d, ctx, ProjectionOptions{}); rep.Refitted {
		t.Fatalf("refitted again right after a refit: %+v", rep)
	}
}

// TestProjection_PlaceLimitBoundsAPass — a pass places at most place_limit chunks,
// says more remain, and the next pass picks up where it stopped.
func TestProjection_PlaceLimitBoundsAPass(t *testing.T) {
	d, vs, ctx := projectionFixture(t)
	projDoc(t, d, vs, ctx, "Map", synthVectors(10, 6))
	rep := project(t, d, ctx, ProjectionOptions{PlaceLimit: 4})
	if rep.Placed != 4 || !rep.More {
		t.Fatalf("limited pass = %+v, want 4 placed and more", rep)
	}
	rep = project(t, d, ctx, ProjectionOptions{PlaceLimit: 100})
	if rep.Placed != 6 || rep.More || rep.Refitted {
		t.Fatalf("next pass = %+v, want the other 6 placed on the same basis", rep)
	}
}

// TestProjection_DryRunWritesNothing — the dry run reports the fit and the
// placements it would make, and leaves no basis and no coordinate behind.
func TestProjection_DryRunWritesNothing(t *testing.T) {
	d, vs, ctx := projectionFixture(t)
	projDoc(t, d, vs, ctx, "Map", synthVectors(8, 7))
	rep := project(t, d, ctx, ProjectionOptions{DryRun: true})
	if !rep.DryRun || !rep.Refitted || rep.Placed != 8 || rep.BasisVersion != 1 {
		t.Fatalf("dry run = %+v, want a v1 fit and 8 placements reported", rep)
	}
	if s := snapProjection(t, d, ctx); s.basisVersion != 0 || len(s.missing) != len(s.ids) {
		t.Fatalf("a dry run left a map behind: %+v", s)
	}
}

// TestProjection_PlainSqliteReportsUnsupported — on a store with no vector plane the
// op answers the empty block, never an error, and the pass says unsupported.
func TestProjection_PlainSqliteReportsUnsupported(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	d.Embedder = newFakeEmbedder("fake", "m1", "text")
	d.Cfg = projectionOn(nil)
	snapDoc(t, d, ctx, "user", "/kb/a", "A")
	s := snapProjection(t, d, ctx)
	if s.model != "" || s.basisVersion != 0 || len(s.xyz) != 0 || len(s.missing) != len(s.ids) || len(s.ids) != 2 {
		t.Fatalf("projection on plain sqlite = %+v, want the empty block", s)
	}
	rep, err := d.ProjectScope(ctx, "user", ProjectionOptions{})
	if err != nil || !rep.Unsupported || rep.Placed != 0 {
		t.Fatalf("ProjectScope = %+v, %v; want unsupported, no error", rep, err)
	}
}

// TestProjection_OffRefusesTheOperatorPass — the operator pass is refused while the
// projection is off; the op still answers the empty block.
func TestProjection_OffRefusesTheOperatorPass(t *testing.T) {
	d, vs, ctx := projectionFixture(t)
	projDoc(t, d, vs, ctx, "Map", synthVectors(6, 8))
	d.Cfg = nil
	if _, err := d.ProjectScope(ctx, "user", ProjectionOptions{}); !ErrProjectionDisabled(err) {
		t.Fatalf("ProjectScope while off = %v, want the disabled refusal", err)
	}
	if s := snapProjection(t, d, ctx); s.model != "" || s.basisVersion != 0 {
		t.Fatalf("projection while off = %+v, want the empty block", s)
	}
}

// TestGraphSnapshot_ProjectionBlockServesOnlyTheCurrentBasis — placed chunks carry
// their coordinates (4 decimals); a chunk with no vector, a vector of another
// model, a row of another model or a row of an older basis is missing, with 0,0,0.
func TestGraphSnapshot_ProjectionBlockServesOnlyTheCurrentBasis(t *testing.T) {
	projectionBothTiers(t, func(t *testing.T, d *Document, vs *vectorStore, ctx context.Context) {
		vecs := synthVectors(12, 9)
		ids := projDoc(t, d, vs, ctx, "Map", vecs)
		setVec(t, vs, ctx, ids[11], "m2", vecs[11], time.Now()) // another embedder's vector
		if rep := project(t, d, ctx, ProjectionOptions{}); rep.Placed != 11 {
			t.Fatalf("pass = %+v, want the 11 fake/m1 chunks placed", rep)
		}
		key := sidecarScope(t, d, ctx)
		if err := d.exec(ctx, key, `UPDATE chunk_projection SET model = ? WHERE chunk_id = ?`, "other/x", ids[1]); err != nil {
			t.Fatal(err)
		}
		if err := d.exec(ctx, key, `UPDATE chunk_projection SET basis_version = 0 WHERE chunk_id = ?`, ids[2]); err != nil {
			t.Fatal(err)
		}
		basis, err := d.loadBasis(ctx, key, "fake/m1")
		if err != nil || basis == nil {
			t.Fatalf("loadBasis: %v", err)
		}

		s := snapProjection(t, d, ctx)
		if s.model != "fake/m1" || s.basisVersion != 1 {
			t.Fatalf("block names %s v%d, want fake/m1 v1", s.model, s.basisVersion)
		}
		hidden := map[string]bool{ids[1]: true, ids[2]: true, ids[11]: true}
		for i, id := range s.ids {
			p, placed := s.at(t, id)
			switch {
			case i == 0 || hidden[id]:
				if placed || p != [3]float64{} {
					t.Errorf("chunk %d: placed=%v at %v, want missing at 0,0,0", i, placed, p)
				}
			default:
				want, _ := basis.Project(vecs[slices.Index(ids, id)])
				for k := range want {
					want[k] = projection.Round4(want[k])
				}
				if !placed || p != want {
					t.Errorf("chunk %d: placed=%v at %v, want %v", i, placed, p, want)
				}
			}
		}
		if len(s.missing) != 4 {
			t.Errorf("missing = %v, want the root and the three hidden chunks", s.missing)
		}
	})
}

// TestSearch_WithProjectionReturnsQueryXYZ — with a map, search places its own query
// embedding on it; without one, or without the flag, the key is absent.
func TestSearch_WithProjectionReturnsQueryXYZ(t *testing.T) {
	vocab := []string{"alpha", "beta", "gamma", "delta", "omega"}
	d, _, ctx := projectionFixture(t, vocab...)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Words"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	doc := out["document_id"].(string)
	for i, body := range []string{"alpha beta", "gamma", "delta omega", "alpha gamma", "beta delta", "omega alpha beta"} {
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"c%d","body":%q}`, doc, i, body)); r.IsError {
			t.Fatal(r.Text)
		}
	}
	search := func(withProjection bool) map[string]any {
		t.Helper()
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"search","scope":"user","query":"alpha delta","with_projection":%v}`, withProjection))
		if r.IsError {
			t.Fatalf("search: %s", r.Text)
		}
		return out
	}
	if _, ok := search(true)["query_xyz"]; ok {
		t.Fatal("query_xyz present before any map exists")
	}
	if rep := project(t, d, ctx, ProjectionOptions{}); rep.BasisVersion != 1 {
		t.Fatalf("pass = %+v", rep)
	}
	got, ok := search(true)["query_xyz"].([]any)
	if !ok || len(got) != 3 {
		t.Fatalf("query_xyz = %v, want three coordinates", search(true)["query_xyz"])
	}
	basis, _ := d.loadBasis(ctx, sidecarScope(t, d, ctx), "fake/m1")
	qv, _ := d.Embedder.Embed(ctx, []string{"alpha delta"})
	want, _ := basis.Project(qv[0])
	for k := range want {
		if got[k].(float64) != projection.Round4(want[k]) {
			t.Fatalf("query_xyz = %v, want %v rounded", got, want)
		}
	}
	if _, ok := search(false)["query_xyz"]; ok {
		t.Fatal("query_xyz present without with_projection")
	}
	d.Cfg = nil
	if _, ok := search(true)["query_xyz"]; ok {
		t.Fatal("query_xyz present with the projection off")
	}
}

// TestProjectScopeKey_PlacesOnlyScopesWithVectors — the sweeper's entry: a scope
// holding chunk vectors is placed, one without any is answered without its SQL
// being provisioned.
func TestProjectScopeKey_PlacesOnlyScopesWithVectors(t *testing.T) {
	d, vs, ctx := projectionFixture(t)
	projDoc(t, d, vs, ctx, "Map", synthVectors(6, 11))
	key := sidecarScope(t, d, ctx)
	rep, err := d.ProjectScopeKey(context.Background(), key, store.MemoryScopeUser, ProjectionOptions{})
	if err != nil || rep.Placed != 6 || rep.BasisVersion != 1 {
		t.Fatalf("ProjectScopeKey = %+v, %v; want 6 placed on v1", rep, err)
	}

	empty := key
	empty.ScopeID = "nobody"
	before, err := d.SqlMem.ListScopes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rep, err = d.ProjectScopeKey(context.Background(), empty, store.MemoryScopeUser, ProjectionOptions{})
	if err != nil || rep.Placed != 0 || rep.BasisVersion != 0 {
		t.Fatalf("ProjectScopeKey on an empty scope = %+v, %v", rep, err)
	}
	after, _ := d.SqlMem.ListScopes(context.Background())
	if len(after) != len(before) {
		t.Fatalf("a scope with no vectors was provisioned: %d scopes before, %d after", len(before), len(after))
	}
}

// insertProjectionRow writes a coordinate directly, for the delete tests: they
// check the cascade, not the pass.
func insertProjectionRow(t *testing.T, d *Document, ctx context.Context, chunkID string) {
	t.Helper()
	if err := d.exec(ctx, sidecarScope(t, d, ctx), `INSERT INTO chunk_projection (chunk_id, model, basis_version, x, y, z, updated_at)
		VALUES (?, 'fake/m1', 1, 1.5, 2.5, 3.5, 1)`, chunkID); err != nil {
		t.Fatalf("insert projection row: %v", err)
	}
}

// TestProjection_DeleteChunkDropsItsSubtreesRows — delete_chunk removes the
// coordinates of the chunk and its descendants, and only those.
func TestProjection_DeleteChunkDropsItsSubtreesRows(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc, _, part := snapDoc(t, d, ctx, "user", "/kb/a", "A")
	child := snapChunk(t, d, ctx, "user", doc, part, "Child")
	other := snapChunk(t, d, ctx, "user", doc, "", "Other")
	for _, id := range []string{part, child, other} {
		insertProjectionRow(t, d, ctx, id)
	}
	if _, r := docExec(t, d, ctx, `{"op":"delete_chunk","scope":"user","id":"`+part+`"}`); r.IsError {
		t.Fatalf("delete_chunk: %s", r.Text)
	}
	for id, want := range map[string]int{part: 0, child: 0, other: 1} {
		if n := chunkRowsIn(t, d, ctx, "chunk_projection", id); n != want {
			t.Errorf("chunk_projection rows for %s = %d, want %d", id, n, want)
		}
	}
}

func TestProjection_DeleteDocumentDropsItsRows(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc, root, part := snapDoc(t, d, ctx, "user", "/kb/a", "A")
	_, _, keep := snapDoc(t, d, ctx, "user", "/kb/b", "B")
	for _, id := range []string{root, part, keep} {
		insertProjectionRow(t, d, ctx, id)
	}
	if _, r := docExec(t, d, ctx, `{"op":"delete_document","scope":"user","id":"`+doc+`"}`); r.IsError {
		t.Fatalf("delete_document: %s", r.Text)
	}
	for id, want := range map[string]int{root: 0, part: 0, keep: 1} {
		if n := chunkRowsIn(t, d, ctx, "chunk_projection", id); n != want {
			t.Errorf("chunk_projection rows for %s = %d, want %d", id, n, want)
		}
	}
}

// TestProjection_PruneDropsTheRetiredChunksRow — the retention prune's cascade
// matches delete_chunk's, the coordinate included.
func TestProjection_PruneDropsTheRetiredChunksRow(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newEntityDoc(t, d, ctx)
	key := sidecarScope(t, d, ctx)
	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`",
		"natural_key":"memory/fact/doomed","title":"Doomed","body":"Gone soon.","type":"fact","subject":"Dave"}`)
	if r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}
	id := asStr(out["id"])
	insertProjectionRow(t, d, ctx, id)
	if err := d.exec(ctx, key, `UPDATE chunk_memory_meta SET expired_at = ? WHERE chunk_id = ?`, 1000, id); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err := d.PruneRetiredChunks(ctx, key, store.MemoryScopeUser, 1<<62, false); err != nil {
		t.Fatalf("PruneRetiredChunks: %v", err)
	}
	if n := chunkRowsIn(t, d, ctx, "chunks", id); n != 0 {
		t.Fatalf("the chunk survived the prune — the test proves nothing")
	}
	if n := chunkRowsIn(t, d, ctx, "chunk_projection", id); n != 0 {
		t.Errorf("chunk_projection still holds %d row(s) for the pruned chunk", n)
	}
}

// TestGraphSnapshot_ProjectionFollowsTheChunksSectionOrder — xyz is aligned with the
// chunks section, which lists a document in reading order (a parent, then its
// subtree), not in the order its rows are read (by position). Here a sibling at
// position 1 is read before a child at position 2 but drawn after it.
func TestGraphSnapshot_ProjectionFollowsTheChunksSectionOrder(t *testing.T) {
	d, vs, ctx := projectionFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Tree"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	doc := out["document_id"].(string)
	parent := snapChunk(t, d, ctx, "user", doc, "", "P")
	sibling := snapChunk(t, d, ctx, "user", doc, "", "S")
	kids := []string{snapChunk(t, d, ctx, "user", doc, parent, "C0"), snapChunk(t, d, ctx, "user", doc, parent, "C1"),
		snapChunk(t, d, ctx, "user", doc, parent, "C2")}
	vecs := synthVectors(5, 10)
	all := append([]string{parent, sibling}, kids...)
	for i, id := range all {
		setVec(t, vs, ctx, id, "m1", vecs[i], time.Now())
	}
	project(t, d, ctx, ProjectionOptions{})
	basis, _ := d.loadBasis(ctx, sidecarScope(t, d, ctx), "fake/m1")
	s := snapProjection(t, d, ctx)
	if slices.Index(s.ids, kids[2]) > slices.Index(s.ids, sibling) {
		t.Fatalf("precondition: the chunks section lists C2 after S (%v), so the orders do not differ", s.ids)
	}
	for i, id := range all {
		want, _ := basis.Project(vecs[i])
		for k := range want {
			want[k] = projection.Round4(want[k])
		}
		if got, placed := s.at(t, id); !placed || got != want {
			t.Errorf("chunk %s at %v (placed=%v), want its own coordinates %v", id, got, placed, want)
		}
	}
}

// TestProjection_SubjectHomingDropsThePlaceholderRootsRow — adopting an empty
// document removes its placeholder root, and the root's coordinate with it.
func TestProjection_SubjectHomingDropsThePlaceholderRootsRow(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	seedOldShape(t, d, ctx)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Denn","path":"/facts/denn"}`)
	if r.IsError {
		t.Fatalf("create stray: %s", r.Text)
	}
	stray := out["root_chunk_id"].(string)
	insertProjectionRow(t, d, ctx, stray)
	rep, err := d.HomeFactsUnderSubjects(ctx, "tnt", store.MemoryScopeUser, "u1", false)
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	if rep.DocumentsAdopted != 1 {
		t.Fatalf("report = %+v, want the stray document adopted", rep)
	}
	if n := chunkRowsIn(t, d, ctx, "chunk_projection", stray); n != 0 {
		t.Errorf("chunk_projection still holds %d row(s) for the dropped placeholder root", n)
	}
}
