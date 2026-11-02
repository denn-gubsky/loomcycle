package builtin

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestUMAPProjection_ALosingRefitPlacesOnTheWinnersBasis — two refits of one scope
// read the same basis and both allocate the next version (the sweeper and the
// operator route, or two replicas). The primary key decides: the loser writes no
// basis row and no anchors, and places the chunks on the winner's version.
func TestUMAPProjection_ALosingRefitPlacesOnTheWinnersBasis(t *testing.T) {
	projectionBothTiers(t, func(t *testing.T, d *Document, vs *vectorStore, ctx context.Context) {
		umapOn(d, nil)
		umapDocs(t, d, vs, ctx, clusterVecs(2, 12, 6))
		project(t, d, ctx, ProjectionOptions{})
		key := sidecarScope(t, d, ctx)
		stale, err := d.loadBasis(ctx, key, "fake/m1") // what both racers read: v1
		if err != nil || stale == nil {
			t.Fatalf("loadBasis: %v", err)
		}
		// The winner saves v2 first.
		if rep := project(t, d, ctx, ProjectionOptions{Refit: true}); rep.BasisVersion != 2 {
			t.Fatalf("winner = %+v, want v2", rep)
		}
		winners := countRows(t, d, ctx, `SELECT COUNT(*) FROM chunk_projection_anchor WHERE version = 2`)
		wantXYZ := snapProjection(t, d, ctx).rawXYZ

		// The loser, still holding v1, refits to v2 as well.
		list := func(after string) ([]store.MemoryEmbedRow, error) {
			return d.Store.MemoryEmbedListVectors(ctx, "", store.MemoryScopeUser, "u1", chunkBodyKeyPrefix, after, projectionPage)
		}
		first, err := list("")
		if err != nil {
			t.Fatal(err)
		}
		rep, err := d.refitUMAP(ctx, key, list, first, "fake/m1", d.projectionSettings(), stale, 2, time.Now().UnixNano(),
			projectionDefaultPlaceLimit, ProjectionReport{Scope: "user", Model: "fake/m1", Refitted: true})
		if err != nil {
			t.Fatalf("losing refit: %v", err)
		}
		if rep.Refitted || rep.BasisVersion != 2 || rep.Method != "umap" {
			t.Fatalf("losing refit = %+v, want not refitted, on the winner's umap v2", rep)
		}
		if n := countRows(t, d, ctx, `SELECT COUNT(*) FROM chunk_projection_basis WHERE version = 2`); n != 1 {
			t.Fatalf("basis rows for v2 = %d, want the winner's alone", n)
		}
		if n := countRows(t, d, ctx, `SELECT COUNT(*) FROM chunk_projection_anchor WHERE version = 2`); n != winners {
			t.Fatalf("anchors for v2 = %d, want the winner's %d", n, winners)
		}
		if got := snapProjection(t, d, ctx).rawXYZ; string(got) != string(wantXYZ) {
			t.Fatalf("the loser moved the winner's map:\n%s\n%s", wantXYZ, got)
		}
	})
}
