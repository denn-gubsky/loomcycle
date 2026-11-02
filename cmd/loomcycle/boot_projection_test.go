package main

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

type projVecStore struct{ store.Store }

func (projVecStore) SupportsVectors() bool { return true }

type projEmbedder struct{}

func (projEmbedder) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }
func (projEmbedder) Model() string                                        { return "m" }
func (projEmbedder) Provider() string                                     { return "p" }
func (projEmbedder) Dimension() int                                       { return 4 }

// TestProjectionSweeper_OffByDefault — with no memory.projection block nothing
// starts; enabling without an interval, or without a store, embedder or vector
// index, starts nothing either. Only the full set starts the sweeper.
func TestProjectionSweeper_OffByDefault(t *testing.T) {
	t.Setenv("LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	vs := projVecStore{base}

	on := func(interval string) *config.Config {
		c := &config.Config{}
		c.Memory.Projection = config.ProjectionConfig{Enabled: true, Interval: interval}
		return c
	}
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		st   store.Store
		emb  bool
		want bool
	}{
		{"no block", &config.Config{}, vs, true, false},
		{"enabled, no interval", on(""), vs, true, false},
		{"interval, not enabled", func() *config.Config {
			c := on("1h")
			c.Memory.Projection.Enabled = false
			return c
		}(), vs, true, false},
		{"no store", on("1h"), nil, true, false},
		{"no embedder", on("1h"), vs, false, false},
		{"no vector index", on("1h"), base, true, false},
		{"everything", on("1h"), vs, true, true},
	} {
		var emb projEmbedder
		var got bool
		if tc.emb {
			got = startProjectionSweeper(ctx, tc.cfg, tc.st, mgr, emb, nil)
		} else {
			got = startProjectionSweeper(ctx, tc.cfg, tc.st, mgr, nil, nil)
		}
		if got != tc.want {
			t.Errorf("%s: started = %v, want %v", tc.name, got, tc.want)
		}
	}
}
