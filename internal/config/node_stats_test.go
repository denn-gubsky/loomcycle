package config

import (
	"strings"
	"testing"
	"time"
)

// TestDocumentNodeStats_OffByDefault — no block: the sweeper is off with no
// interval, and a nil config resolves to off.
func TestDocumentNodeStats_OffByDefault(t *testing.T) {
	cfg, err := loadYAMLString(t, "agents: {}\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.DocumentNodeStats(); got.Enabled || got.Interval != 0 {
		t.Fatalf("default node_stats = %+v, want off with no interval", got)
	}
	var nilCfg *Config
	if nilCfg.DocumentNodeStats().Enabled {
		t.Error("a nil config resolved to enabled")
	}
}

func TestDocumentNodeStats_YAMLBlockDecodes(t *testing.T) {
	cfg, err := loadYAMLString(t, "memory:\n  node_stats:\n    enabled: true\n    interval: 6h\n")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.DocumentNodeStats(), (NodeStatsSettings{Enabled: true, Interval: 6 * time.Hour}); got != want {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
}

func TestDocumentNodeStats_LoadRefusesABadInterval(t *testing.T) {
	for _, y := range []string{
		"memory:\n  node_stats:\n    interval: soon\n",
		"memory:\n  node_stats:\n    interval: -1h\n",
	} {
		_, err := loadYAMLString(t, y)
		if err == nil || !strings.Contains(err.Error(), "memory.node_stats.interval") {
			t.Errorf("%q: want an error naming memory.node_stats.interval, got %v", y, err)
		}
	}
}
