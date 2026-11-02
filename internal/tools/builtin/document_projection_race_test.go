package builtin

import (
	"context"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/projection"
)

// TestProjection_ConcurrentPassesPlacingTheSameChunksAllSucceed — passes that find
// the same chunks unplaced (no refit) write their rows at once. Every pass must
// finish, and every chunk be placed.
func TestProjection_ConcurrentPassesPlacingTheSameChunksAllSucceed(t *testing.T) {
	projectionBothTiers(t, func(t *testing.T, d *Document, vs *vectorStore, ctx context.Context) {
		projDoc(t, d, vs, ctx, "Map", synthVectors(24, 5))
		project(t, d, ctx, ProjectionOptions{})
		projDoc(t, d, vs, ctx, "More", synthVectors(60, 6))

		const passes = 6
		start := make(chan struct{})
		errs := make([]error, passes)
		var wg sync.WaitGroup
		for i := range passes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = d.ProjectScope(ctx, "user", ProjectionOptions{})
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("pass %d failed: %v", i, err)
			}
		}
		if s := snapProjection(t, d, ctx); s.basisVersion != 1 || len(s.missing) != 2 {
			t.Fatalf("snapshot = v%d missing %v, want v1 with only the two bodyless roots missing", s.basisVersion, s.missing)
		}
	})
}

// TestProjection_WriteNeverMovesARowBackToAnOlderBasis — a slow pass still placing
// on an older basis does not overwrite a row a newer refit already placed; a newer
// basis, or another model's, does replace it.
func TestProjection_WriteNeverMovesARowBackToAnOlderBasis(t *testing.T) {
	projectionBothTiers(t, func(t *testing.T, d *Document, vs *vectorStore, ctx context.Context) {
		id := projDoc(t, d, vs, ctx, "Map", synthVectors(1, 9))[0]
		key := sidecarScope(t, d, ctx)
		write := func(model string, version int) {
			t.Helper()
			row := placedRow{chunkID: id, xyz: [projection.Components]float64{float64(version)}, updatedAt: 1}
			if err := d.writeProjection(ctx, key, []placedRow{row}, projectionWrite{model: model, version: version}); err != nil {
				t.Fatalf("writeProjection %s v%d: %v", model, version, err)
			}
		}
		held := func() heldProjection {
			t.Helper()
			rows, err := d.projectionRows(ctx, key, []string{id})
			if err != nil {
				t.Fatalf("projectionRows: %v", err)
			}
			return rows[id]
		}
		for _, step := range []struct {
			model       string
			version     int
			wantModel   string
			wantVersion int
		}{
			{"fake/m1", 2, "fake/m1", 2},
			{"fake/m1", 1, "fake/m1", 2}, // older basis: kept
			{"fake/m1", 3, "fake/m1", 3}, // newer basis: replaced
			{"fake/m2", 1, "fake/m2", 1}, // another model: replaced
		} {
			write(step.model, step.version)
			if h := held(); h.model != step.wantModel || h.version != step.wantVersion {
				t.Fatalf("after writing %s v%d the row is %s v%d, want %s v%d",
					step.model, step.version, h.model, h.version, step.wantModel, step.wantVersion)
			}
		}
	})
}
