package builtin

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/projection"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// gateCtxKey marks the one pass the gated store parks.
type gateCtxKey struct{}

// gatedVectorStore parks the marked pass the first time it reads a vector page past
// the first. In a refit that read is the fit sample's second page: after the pass
// read the current basis and before it saves the next one — the window two refits
// of one scope race in.
type gatedVectorStore struct {
	*vectorStore
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func (g *gatedVectorStore) MemoryEmbedListVectors(ctx context.Context, tenant string, scope store.MemoryScope,
	scopeID, keyPrefix, afterKey string, limit int) ([]store.MemoryEmbedRow, error) {
	if afterKey != "" && ctx.Value(gateCtxKey{}) != nil {
		g.once.Do(func() {
			close(g.reached)
			<-g.release
		})
	}
	return g.vectorStore.MemoryEmbedListVectors(ctx, tenant, scope, scopeID, keyPrefix, afterKey, limit)
}

// TestProjection_RefitThatLosesTheRaceReportsRacedAndPlacesOnTheWinner — two refits
// of one scope read the same basis and both mean to write the next version. The
// one that saves second must not fail: it reports that it did not refit (another
// pass did), and the scope's chunks end up on the winner's basis.
func TestProjection_RefitThatLosesTheRaceReportsRacedAndPlacesOnTheWinner(t *testing.T) {
	projectionBothTiers(t, func(t *testing.T, d *Document, vs *vectorStore, ctx context.Context) {
		projDoc(t, d, vs, ctx, "Map", synthVectors(4, 7))
		// A full first page of vectors with no chunk row behind them, so the fit
		// sample reads a second page — where the gate parks the pass. The placement
		// skips them (no chunk), as it skips any dead body.
		for i, v := range synthVectors(projectionPage, 8) {
			setVec(t, vs, ctx, fmt.Sprintf("ghost%04d", i), "m1", v, time.Now())
		}
		if first := project(t, d, ctx, ProjectionOptions{}); first.BasisVersion != 1 {
			t.Fatalf("first pass = %+v, want basis v1", first)
		}

		g := &gatedVectorStore{vectorStore: vs, reached: make(chan struct{}), release: make(chan struct{})}
		d.Store = g
		type result struct {
			rep ProjectionReport
			err error
		}
		parked := make(chan result, 1)
		go func() {
			rep, err := d.ProjectScope(context.WithValue(ctx, gateCtxKey{}, true), "user", ProjectionOptions{Refit: true})
			parked <- result{rep, err}
		}()
		select {
		case <-g.reached:
		case <-time.After(30 * time.Second):
			t.Fatal("the gated refit never reached its fit sample")
		}

		winner := project(t, d, ctx, ProjectionOptions{Refit: true})
		if !winner.Refitted || winner.BasisVersion != 2 {
			t.Fatalf("winning refit = %+v, want a fit of v2", winner)
		}
		close(g.release)
		var loser result
		select {
		case loser = <-parked:
		case <-time.After(30 * time.Second):
			t.Fatal("the parked refit never finished")
		}
		if loser.err != nil {
			t.Fatalf("the refit that lost the race failed: %v", loser.err)
		}
		if loser.rep.Refitted || !loser.rep.Raced || loser.rep.BasisVersion != 2 {
			t.Fatalf("losing refit = %+v, want refitted=false raced=true on the winner's v2", loser.rep)
		}
		if s := snapProjection(t, d, ctx); s.basisVersion != 2 || len(s.missing) != 1 {
			t.Fatalf("snapshot = v%d missing %v, want v2 with only the bodyless root missing", s.basisVersion, s.missing)
		}
	})
}

// TestProjection_ConcurrentRefitsOfOneScopeAllSucceed — several refits of one scope
// started together (the sweeper, operator calls, replicas) all finish without an
// error, exactly the passes that wrote a basis report a refit, and every chunk ends
// on the scope's latest basis.
func TestProjection_ConcurrentRefitsOfOneScopeAllSucceed(t *testing.T) {
	projectionBothTiers(t, func(t *testing.T, d *Document, vs *vectorStore, ctx context.Context) {
		projDoc(t, d, vs, ctx, "Map", synthVectors(40, 3))
		project(t, d, ctx, ProjectionOptions{})

		const passes = 6
		start := make(chan struct{})
		reps := make([]ProjectionReport, passes)
		errs := make([]error, passes)
		var wg sync.WaitGroup
		for i := range passes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				reps[i], errs[i] = d.ProjectScope(ctx, "user", ProjectionOptions{Refit: true})
			}()
		}
		close(start)
		wg.Wait()

		refits := map[int]int{}
		for i, err := range errs {
			if err != nil {
				t.Fatalf("pass %d failed: %v", i, err)
			}
			if reps[i].Refitted == reps[i].Raced {
				t.Fatalf("pass %d = %+v, want exactly one of refitted and raced", i, reps[i])
			}
			if reps[i].Refitted {
				refits[reps[i].BasisVersion]++
			}
		}
		for v, n := range refits {
			if n != 1 {
				t.Fatalf("%d passes report fitting v%d, want one", n, v)
			}
		}
		s := snapProjection(t, d, ctx)
		if refits[s.basisVersion] != 1 || len(s.missing) != 1 {
			t.Fatalf("snapshot = v%d missing %v (refits %v), want the latest fit with only the root missing",
				s.basisVersion, s.missing, refits)
		}
	})
}

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
