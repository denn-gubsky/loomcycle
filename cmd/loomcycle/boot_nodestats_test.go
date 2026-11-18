package main

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// TestNodeStatsSweeper_OffByDefault — with no memory.node_stats block nothing
// starts; enabled without an interval, an interval without enabled, or no store
// start nothing either. Enabled with an interval starts it, with or without an
// embedder or a vector store (it then keeps counts only).
func TestNodeStatsSweeper_OffByDefault(t *testing.T) {
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

	on := func(enabled bool, interval string) *config.Config {
		c := &config.Config{}
		c.Memory.NodeStats = config.NodeStatsConfig{Enabled: enabled, Interval: interval}
		return c
	}
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		st   store.Store
		want bool
	}{
		{"no block", &config.Config{}, base, false},
		{"nil config", nil, base, false},
		{"enabled, no interval", on(true, ""), base, false},
		{"interval, not enabled", on(false, "1h"), base, false},
		{"no store", on(true, "1h"), nil, false},
		{"enabled with interval, no vectors", on(true, "1h"), base, true},
	} {
		if got := startNodeStatsSweeper(ctx, tc.cfg, tc.st, mgr, nil, nil, nil); got != tc.want {
			t.Errorf("%s: started = %v, want %v", tc.name, got, tc.want)
		}
	}
}
