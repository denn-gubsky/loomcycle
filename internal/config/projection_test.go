package config

import (
	"strings"
	"testing"
	"time"
)

// TestDocumentProjection_OffByDefault — no block, no env: the projection is off
// and has no sweeper interval, so nothing runs.
func TestDocumentProjection_OffByDefault(t *testing.T) {
	t.Setenv("LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL", "")
	cfg, err := loadYAMLString(t, "agents: {}\n")
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.DocumentProjection()
	if got.Enabled || got.Interval != 0 {
		t.Fatalf("default projection = %+v, want off with no interval", got)
	}
	if got.RefitAfter != 7*24*time.Hour || got.RefitMinNew != 500 || got.SampleMax != 20000 ||
		got.Method != ProjectionMethodPCA || got.UMAPSampleMax != 10000 {
		t.Errorf("defaults = %+v, want refit_after 168h, refit_min_new 500, sample_max 20000, method pca, umap_sample_max 10000", got)
	}
	var nilCfg *Config
	if nilCfg.DocumentProjection().Enabled {
		t.Error("a nil config resolved to enabled")
	}
}

func TestDocumentProjection_YAMLBlockDecodes(t *testing.T) {
	t.Setenv("LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL", "")
	cfg, err := loadYAMLString(t, "memory:\n  projection:\n    enabled: true\n    interval: 30m\n    refit_after: 48h\n    refit_min_new: 50\n    sample_max: 1000\n    method: umap\n    umap_sample_max: 4000\n")
	if err != nil {
		t.Fatal(err)
	}
	want := ProjectionSettings{Enabled: true, Interval: 30 * time.Minute, RefitAfter: 48 * time.Hour, RefitMinNew: 50, SampleMax: 1000,
		Method: ProjectionMethodUMAP, UMAPSampleMax: 4000}
	if got := cfg.DocumentProjection(); got != want {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
}

// TestDocumentProjection_EnvIntervalOverridesAndEnables — the env var replaces
// the yaml interval; a positive one turns the projection on, "0" stops only the
// sweeper.
func TestDocumentProjection_EnvIntervalOverridesAndEnables(t *testing.T) {
	t.Setenv("LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL", "2h")
	cfg, err := loadYAMLString(t, "memory:\n  projection:\n    interval: 30m\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.DocumentProjection(); !got.Enabled || got.Interval != 2*time.Hour {
		t.Fatalf("settings = %+v, want enabled with the env's 2h", got)
	}
	t.Setenv("LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL", "0")
	cfg, err = loadYAMLString(t, "memory:\n  projection:\n    enabled: true\n    interval: 30m\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.DocumentProjection(); !got.Enabled || got.Interval != 0 {
		t.Fatalf("settings = %+v, want enabled with the sweeper stopped", got)
	}
}

func TestDocumentProjection_LoadRefusesABadValue(t *testing.T) {
	for _, tc := range []struct{ env, yaml, want string }{
		{"", "memory:\n  projection:\n    interval: soon\n", "memory.projection.interval"},
		{"", "memory:\n  projection:\n    refit_after: -1h\n", "memory.projection.refit_after"},
		{"", "memory:\n  projection:\n    sample_max: -1\n", "memory.projection.sample_max"},
		{"", "memory:\n  projection:\n    method: tsne\n", "memory.projection.method"},
		{"", "memory:\n  projection:\n    method: UMAP\n", "memory.projection.method"},
		{"", "memory:\n  projection:\n    umap_sample_max: -5\n", "memory.projection.umap_sample_max"},
		{"hourly", "agents: {}\n", "LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL"},
	} {
		t.Setenv("LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL", tc.env)
		_, err := loadYAMLString(t, tc.yaml)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q / %q: want an error naming %s, got %v", tc.env, tc.yaml, tc.want, err)
		}
	}
}
