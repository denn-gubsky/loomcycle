package builtin

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/projection"
)

// umapOn switches d to the UMAP method, with mod applied after.
func umapOn(d *Document, mod func(*config.ProjectionConfig)) {
	d.Cfg = projectionOn(func(p *config.ProjectionConfig) {
		p.Method = config.ProjectionMethodUMAP
		if mod != nil {
			mod(p)
		}
	})
}

// clusterVecs draws docs groups of per 16-dimension vectors, each group around
// its own random centre: chunks of one document are near each other.
func clusterVecs(docs, per int, seed uint64) [][][]float32 {
	rng := rand.New(rand.NewPCG(seed, 3))
	out := make([][][]float32, docs)
	for g := range out {
		centre := make([]float64, 16)
		for j := range centre {
			centre[j] = rng.NormFloat64()
		}
		for range per {
			v := make([]float32, 16)
			for j := range v {
				v[j] = float32(centre[j] + 0.3*rng.NormFloat64())
			}
			out[g] = append(out[g], v)
		}
	}
	return out
}

// umapDocs makes one document per group and returns each chunk's group.
func umapDocs(t *testing.T, d *Document, vs *vectorStore, ctx context.Context, groups [][][]float32) map[string]int {
	t.Helper()
	label := map[string]int{}
	for g, vecs := range groups {
		for _, id := range projDoc(t, d, vs, ctx, fmt.Sprintf("Doc %d", g), vecs) {
			label[id] = g
		}
	}
	return label
}

// docAgreement is the share of each placed chunk's k nearest placed chunks on
// the snapshot that are in its own document.
func docAgreement(t *testing.T, s projSnap, label map[string]int, k int) float64 {
	t.Helper()
	var ids []string
	for id := range label {
		if _, ok := s.at(t, id); ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var total float64
	for _, id := range ids {
		p, _ := s.at(t, id)
		type nd struct {
			id string
			d  float64
		}
		var ds []nd
		for _, o := range ids {
			if o != id {
				q, _ := s.at(t, o)
				ds = append(ds, nd{o, dist3(p, q)})
			}
		}
		slices.SortFunc(ds, func(a, b nd) int { return cmp.Compare(a.d, b.d) })
		same := 0
		for _, n := range ds[:k] {
			if label[n.id] == label[id] {
				same++
			}
		}
		total += float64(same) / float64(k)
	}
	return total / float64(len(ids))
}

func dist3(a, b [3]float64) float64 {
	return math.Sqrt((a[0]-b[0])*(a[0]-b[0]) + (a[1]-b[1])*(a[1]-b[1]) + (a[2]-b[2])*(a[2]-b[2]))
}

func anchorCount(t *testing.T, d *Document, ctx context.Context) int {
	t.Helper()
	return countRows(t, d, ctx, `SELECT COUNT(*) FROM chunk_projection_anchor`)
}

// TestUMAPProjection_FitsServesAndKeepsDocumentsTogether — with method umap, a pass
// fits a UMAP map: every chunk is placed, the sample is stored as anchors, the
// snapshot names the method, and a chunk's nearest chunks on the map are its own
// document's. A second pass over unchanged data moves nothing.
func TestUMAPProjection_FitsServesAndKeepsDocumentsTogether(t *testing.T) {
	projectionBothTiers(t, func(t *testing.T, d *Document, vs *vectorStore, ctx context.Context) {
		umapOn(d, nil)
		label := umapDocs(t, d, vs, ctx, clusterVecs(3, 20, 1))
		rep := project(t, d, ctx, ProjectionOptions{})
		if !rep.Refitted || rep.Method != "umap" || rep.BasisVersion != 1 || rep.Placed != 60 {
			t.Fatalf("first pass = %+v, want a umap fit (v1) with 60 chunks placed", rep)
		}
		if n := anchorCount(t, d, ctx); n != 60 {
			t.Fatalf("anchors = %d, want the 60-chunk sample", n)
		}
		s1 := snapProjection(t, d, ctx)
		if s1.method != "umap" || s1.basisVersion != 1 || len(s1.missing) != 3 {
			t.Fatalf("snapshot = %s v%d missing %v, want umap v1 with only the 3 roots missing", s1.method, s1.basisVersion, s1.missing)
		}
		if a := docAgreement(t, s1, label, 10); a < 0.9 {
			t.Fatalf("10-NN document agreement on the map = %.3f, want ≥ 0.9", a)
		}

		second := project(t, d, ctx, ProjectionOptions{})
		if second.Refitted || second.Placed != 0 || second.Method != "umap" {
			t.Fatalf("second pass = %+v, want nothing to do", second)
		}
		if s2 := snapProjection(t, d, ctx); !bytes.Equal(s2.rawXYZ, s1.rawXYZ) {
			t.Fatalf("xyz moved on a pass over unchanged data:\n%s\n%s", s1.rawXYZ, s2.rawXYZ)
		}
	})
}

// TestUMAPProjection_NewChunkPlacedWithoutRefit — chunks outside the sample, and a
// chunk written after the fit, are placed among the anchors (read back from the
// store on the later pass) without a refit; nothing already placed moves.
func TestUMAPProjection_NewChunkPlacedWithoutRefit(t *testing.T) {
	d, vs, ctx := projectionFixture(t)
	umapOn(d, func(p *config.ProjectionConfig) { p.UMAPSampleMax = 30 })
	groups := clusterVecs(3, 21, 2)
	late := groups[0][20]
	for g := range groups {
		groups[g] = groups[g][:20]
	}
	label := umapDocs(t, d, vs, ctx, groups)
	rep := project(t, d, ctx, ProjectionOptions{})
	if rep.Placed != 60 || anchorCount(t, d, ctx) != 30 {
		t.Fatalf("pass = %+v with %d anchors, want 60 placed from a 30-chunk sample", rep, anchorCount(t, d, ctx))
	}
	before := snapProjection(t, d, ctx)
	if a := docAgreement(t, before, label, 10); a < 0.9 {
		t.Fatalf("10-NN document agreement with half the chunks placed among anchors = %.3f, want ≥ 0.9", a)
	}

	lateID := projDoc(t, d, vs, ctx, "Late", [][]float32{late})[0]
	label[lateID] = 0
	rep = project(t, d, ctx, ProjectionOptions{})
	if rep.Refitted || rep.Placed != 1 || rep.BasisVersion != 1 {
		t.Fatalf("pass after one new chunk = %+v, want it placed on v1 without a refit", rep)
	}
	after := snapProjection(t, d, ctx)
	p, ok := after.at(t, lateID)
	if !ok {
		t.Fatalf("the new chunk is still missing")
	}
	for id := range label {
		if id == lateID {
			continue
		}
		was, _ := before.at(t, id)
		if now, _ := after.at(t, id); now != was {
			t.Fatalf("chunk %s moved from %v to %v", id, was, now)
		}
	}
	// Its 5 nearest chunks on the map are its group's.
	type nd struct {
		id string
		d  float64
	}
	var ds []nd
	for id := range label {
		if id != lateID {
			q, _ := after.at(t, id)
			ds = append(ds, nd{id, dist3(p, q)})
		}
	}
	slices.SortFunc(ds, func(a, b nd) int { return cmp.Compare(a.d, b.d) })
	for _, n := range ds[:5] {
		if label[n.id] != 0 {
			t.Fatalf("the new chunk sits next to group %d's chunk %s, want its own group's", label[n.id], n.id)
		}
	}
}

// TestUMAPProjection_RefitKeepsTheArrangement — a forced refit of unchanged data
// starts from the previous map and is aligned to it: chunks move under a tenth of
// the map's RMS radius on average.
func TestUMAPProjection_RefitKeepsTheArrangement(t *testing.T) {
	projectionBothTiers(t, func(t *testing.T, d *Document, vs *vectorStore, ctx context.Context) {
		umapOn(d, nil)
		label := umapDocs(t, d, vs, ctx, clusterVecs(4, 15, 3))
		project(t, d, ctx, ProjectionOptions{})
		v1 := snapProjection(t, d, ctx)
		if rep := project(t, d, ctx, ProjectionOptions{Refit: true}); !rep.Refitted || rep.BasisVersion != 2 || rep.Placed != 60 {
			t.Fatalf("forced refit = %+v, want v2 with every chunk re-placed", rep)
		}
		v2 := snapProjection(t, d, ctx)
		var a, b [][3]float64
		for id := range label {
			p, _ := v1.at(t, id)
			q, _ := v2.at(t, id)
			a, b = append(a, p), append(b, q)
		}
		var centre [3]float64
		for _, p := range a {
			for k := range 3 {
				centre[k] += p[k] / float64(len(a))
			}
		}
		var disp, radius float64
		for i := range a {
			disp += dist3(a[i], b[i])
			radius += dist3(a[i], centre) * dist3(a[i], centre)
		}
		disp /= float64(len(a))
		radius = math.Sqrt(radius / float64(len(a)))
		t.Logf("refit displacement %.3f of the RMS radius", disp/radius)
		if disp/radius >= 0.1 {
			t.Fatalf("a refit of unchanged data moved chunks %.3f of the radius on average, want < 0.1", disp/radius)
		}
	})
}

// TestUMAPProjection_MethodSwitchRefitsAndPrunesAnchors — a map of the other method
// is refitted (a side-by-side fit by Method, then the configured method again), a
// dry run reports the method it would fit with, and a PCA refit prunes the anchors
// of versions it no longer keeps.
func TestUMAPProjection_MethodSwitchRefitsAndPrunesAnchors(t *testing.T) {
	d, vs, ctx := projectionFixture(t) // configured: pca
	umapDocs(t, d, vs, ctx, clusterVecs(2, 10, 4))
	if rep := project(t, d, ctx, ProjectionOptions{}); rep.Method != "pca" || rep.BasisVersion != 1 {
		t.Fatalf("first pass = %+v, want pca v1", rep)
	}
	if rep := project(t, d, ctx, ProjectionOptions{Method: "umap", DryRun: true}); !rep.Refitted || rep.Method != "umap" || rep.BasisVersion != 2 {
		t.Fatalf("dry run with method umap = %+v, want a umap v2 refit reported", rep)
	}
	if n := anchorCount(t, d, ctx); n != 0 {
		t.Fatalf("a dry run wrote %d anchors", n)
	}
	if rep := project(t, d, ctx, ProjectionOptions{Method: "umap"}); !rep.Refitted || rep.Method != "umap" || rep.BasisVersion != 2 {
		t.Fatalf("side-by-side umap pass = %+v, want a umap v2 refit", rep)
	}
	if s := snapProjection(t, d, ctx); s.method != "umap" || s.basisVersion != 2 {
		t.Fatalf("snapshot = %s v%d, want umap v2", s.method, s.basisVersion)
	}
	if rep := project(t, d, ctx, ProjectionOptions{}); !rep.Refitted || rep.Method != "pca" || rep.BasisVersion != 3 {
		t.Fatalf("pass under the configured method = %+v, want a pca v3 refit", rep)
	}
	if n := anchorCount(t, d, ctx); n != 20 {
		t.Fatalf("anchors after one pca refit = %d, want v2's 20 kept for one refit", n)
	}
	project(t, d, ctx, ProjectionOptions{Refit: true})
	if n := anchorCount(t, d, ctx); n != 0 {
		t.Fatalf("anchors after the version holding them was pruned = %d, want 0", n)
	}
	if _, err := d.ProjectScope(ctx, "user", ProjectionOptions{Method: "tsne"}); err == nil {
		t.Fatal("an unknown method was accepted")
	}
}

// TestProjection_MethodColumnMigratesAnOldBasisTable — a basis table from before
// UMAP gains method, and its rows read as pca.
func TestProjection_MethodColumnMigratesAnOldBasisTable(t *testing.T) {
	projectionBothTiers(t, migratesAnOldBasisTable)
}

func migratesAnOldBasisTable(t *testing.T, d *Document, vs *vectorStore, ctx context.Context) {
	projDoc(t, d, vs, ctx, "Map", synthVectors(6, 5))
	binType := "BLOB"
	if d.docSQL().Tier() == "postgres" {
		binType = "BYTEA"
	}
	project(t, d, ctx, ProjectionOptions{})
	key := sidecarScope(t, d, ctx)
	before, err := d.loadBasis(ctx, key, "fake/m1")
	if err != nil || before == nil {
		t.Fatalf("loadBasis: %v", err)
	}
	// Rebuild the table in its pre-UMAP shape, keeping the row.
	for _, stmt := range []string{
		`CREATE TABLE old_basis AS SELECT model, version, dimension, mean, components, explained_variance, sample_n, fitted_at FROM chunk_projection_basis`,
		`DROP TABLE chunk_projection_basis`,
		`CREATE TABLE chunk_projection_basis (model TEXT NOT NULL, version INTEGER NOT NULL, dimension INTEGER NOT NULL,
			mean ` + binType + ` NOT NULL, components ` + binType + ` NOT NULL, explained_variance TEXT NOT NULL, sample_n INTEGER NOT NULL,
			fitted_at BIGINT NOT NULL, PRIMARY KEY (model, version))`,
		`INSERT INTO chunk_projection_basis SELECT * FROM old_basis`,
		`DROP TABLE old_basis`,
	} {
		if err := d.exec(ctx, key, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if d.tableHasColumn(ctx, key, "chunk_projection_basis", "method") {
		t.Fatal("precondition: the rebuilt table already has method")
	}
	if err := d.migrateProjectionMethod(ctx, key); err != nil {
		t.Fatal(err)
	}
	after, err := d.loadBasis(ctx, key, "fake/m1")
	if err != nil || after == nil || after.methodName() != "pca" || after.version != before.version {
		t.Fatalf("after the migration loadBasis = %+v, %v; want the pca v%d basis", after, err, before.version)
	}
	if !slices.Equal(after.Components[0], before.Components[0]) {
		t.Fatal("the migration changed the basis")
	}
	// The migrated table takes a UMAP version.
	d.Cfg = projectionOn(func(p *config.ProjectionConfig) { p.Method = config.ProjectionMethodUMAP })
	if rep := project(t, d, ctx, ProjectionOptions{}); rep.Method != "umap" || rep.BasisVersion != before.version+1 {
		t.Fatalf("umap pass on the migrated table = %+v", rep)
	}
}

// TestSearch_WithProjectionOnAUMAPMapPlacesTheQueryAmongReadableHits — a UMAP map
// has no formula for a query, so search places it among its hits; a hit in a
// document the caller may not read does not count, so the answer says nothing
// about where the unreadable chunks are.
func TestSearch_WithProjectionOnAUMAPMapPlacesTheQueryAmongReadableHits(t *testing.T) {
	d, _, base := mermaidDocFixture(t, "savepoint", "nesting", "secret", "public")
	umapOn(d, nil)
	open := tenantDocCtx(base, nil)
	s := seedACLDocs(t, d, open)
	rep, err := d.ProjectScope(open, "tenant", ProjectionOptions{})
	if err != nil || rep.Method != "umap" || rep.Placed != 2 {
		t.Fatalf("ProjectScope = %+v, %v; want both chunks placed on a umap map", rep, err)
	}
	key, _, err := d.resolveScope(open, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	places, err := d.projectionXYZ(open, key, "fake/m1", rep.BasisVersion, []string{s.secretChunk, s.publicChunk})
	if err != nil || len(places) != 2 {
		t.Fatalf("places = %v, %v", places, err)
	}
	query := func(ctx context.Context) []float64 {
		t.Helper()
		out, r := docExec(t, d, ctx, `{"op":"search","scope":"tenant","query":"savepoint nesting secret","with_projection":true}`)
		if r.IsError {
			t.Fatalf("search: %s", r.Text)
		}
		raw, ok := out["query_xyz"].([]any)
		if !ok || len(raw) != 3 {
			t.Fatalf("query_xyz = %v, want three coordinates", out["query_xyz"])
		}
		got := make([]float64, 3)
		for k := range got {
			got[k] = raw[k].(float64)
		}
		return got
	}
	round := func(p [3]float64) []float64 {
		return []float64{projection.Round4(p[0]), projection.Round4(p[1]), projection.Round4(p[2])}
	}
	if got := query(open); slices.Equal(got, round(places[s.publicChunk])) {
		t.Fatalf("with both hits readable the query sits exactly on the public chunk (%v): the secret one did not count, so the test proves nothing", got)
	}
	denied := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}}))
	if got, want := query(denied), round(places[s.publicChunk]); !slices.Equal(got, want) {
		t.Fatalf("with the secret document unreadable query_xyz = %v, want the public chunk's place %v alone", got, want)
	}
}
