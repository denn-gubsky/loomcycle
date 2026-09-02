package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestAgentOverride_ReDeclaringABundleAgentKeepsItsBody is the operator story that broke a
// live deployment: an operator adds a few fields to a BUNDLED agent — the documented way to
// grant it a scope — and the agent must keep everything the bundle gave it.
//
// The consolidator is the worst case and the one that failed: it is a code-js agent whose
// whole behaviour IS its code_body (~139 KB), so silently losing that yields an agent with a
// provider and nothing to run.
func TestAgentOverride_ReDeclaringABundleAgentKeepsItsBody(t *testing.T) {
	dir := t.TempDir()
	overlay := filepath.Join(dir, "grant.yaml")
	if err := os.WriteFile(overlay, []byte(
		"agents:\n  memory/consolidator:\n    memory_scopes: [agent, user, tenant]\n    sql_scopes: [tenant]\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	base, err := config.LoadLayers(layersFor(t, "base", "memory")...)
	if err != nil {
		t.Fatalf("baseline load: %v", err)
	}
	bAgent, ok := base.Agents["memory/consolidator"]
	if !ok {
		t.Fatal("baseline: the bundle declares no memory/consolidator")
	}
	if len(bAgent.Code) == 0 {
		t.Fatal("baseline: the bundled consolidator has no code_body — fixture is wrong")
	}

	layers := append(layersFor(t, "base", "memory"), config.Layer{Name: "operator-overlay", Data: mustRead(t, overlay)})
	got, err := config.LoadLayers(layers...)
	if err != nil {
		t.Fatalf("load with the operator overlay: %v", err)
	}
	a, ok := got.Agents["memory/consolidator"]
	if !ok {
		t.Fatal("the overlay removed the agent entirely")
	}

	// The grants the operator asked for.
	if len(a.MemoryScopes) != 3 || a.MemoryScopes[2] != "tenant" {
		t.Errorf("memory_scopes = %v, want the overlay's three", a.MemoryScopes)
	}
	if len(a.SqlScopes) != 1 || a.SqlScopes[0] != "tenant" {
		t.Errorf("sql_scopes = %v, want [tenant]", a.SqlScopes)
	}

	// Everything the bundle gave it must survive. code_body FIRST: it is the whole agent.
	if len(a.Code) != len(bAgent.Code) {
		t.Errorf("code_body went from %d bytes to %d — an operator adding a grant must not "+
			"erase the agent's behaviour", len(bAgent.Code), len(a.Code))
	}
	if a.Provider != bAgent.Provider {
		t.Errorf("provider %q -> %q", bAgent.Provider, a.Provider)
	}
	if len(a.Tools) != len(bAgent.Tools) {
		t.Errorf("tools %v -> %v", bAgent.Tools, a.Tools)
	}
	if a.MemoryConsolidation != bAgent.MemoryConsolidation {
		t.Errorf("memory_consolidation %v -> %v — the consolidation ops would refuse",
			bAgent.MemoryConsolidation, a.MemoryConsolidation)
	}
	if a.HistoryScope == nil && bAgent.HistoryScope != nil {
		t.Errorf("history_scope %v -> nil", bAgent.HistoryScope)
	}
}

// The same overlay, layered onto a stack WITHOUT the bundle it means to widen. The merge
// cannot know the difference, so it builds a half agent — capability grants and nothing to
// run — and config validation refuses it. What the refusal must do is point at the missing
// bundle, because the reader's eye goes to the overlay they just wrote.
func TestAgentOverride_OverlayWithoutItsBundleNamesTheMissingLayer(t *testing.T) {
	dir := t.TempDir()
	overlay := filepath.Join(dir, "grant.yaml")
	if err := os.WriteFile(overlay, []byte(
		"agents:\n  memory/consolidator:\n    memory_scopes: [agent, user, tenant]\n    sql_scopes: [tenant]\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	// "base" only — the memory bundle, which defines the agent, is absent.
	layers := append(layersFor(t, "base"), config.Layer{Name: "operator-overlay", Data: mustRead(t, overlay)})
	_, err := config.LoadLayers(layers...)
	if err == nil {
		t.Fatal("an agent with grants but no provider/model/tier must be refused")
	}
	for _, want := range []string{"LOOMCYCLE_PRESETS", "memory/consolidator", "capability grants"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q so the reader looks at the layer stack; got: %v", want, err)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
