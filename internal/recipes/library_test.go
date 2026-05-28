package recipes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadLibrary_BundledOnly(t *testing.T) {
	lib, err := LoadLibrary("")
	if err != nil {
		t.Fatalf("LoadLibrary: %v", err)
	}
	names := lib.Enabled()
	if len(names) < 10 {
		t.Errorf("expected at least 10 bundled recipes, got %d: %v", len(names), names)
	}
	// Spot-check that a few known recipes are present.
	for _, want := range []string{"github", "slack", "telegram", "tavily"} {
		rec, enabled, ok := lib.Get(want)
		if !ok {
			t.Errorf("missing bundled recipe %q", want)
			continue
		}
		if !enabled {
			t.Errorf("recipe %q should be enabled by default", want)
		}
		if rec.Source != "bundled" {
			t.Errorf("recipe %q Source = %q, want bundled", want, rec.Source)
		}
	}
}

func TestLoadLibrary_BundledRecipesAreValid(t *testing.T) {
	lib, err := LoadLibrary("")
	if err != nil {
		t.Fatalf("LoadLibrary: %v", err)
	}
	for _, name := range lib.All() {
		rec, _, _ := lib.Get(name)
		if rec.Loomcycle == nil {
			t.Errorf("bundled recipe %q missing _loomcycle metadata block", name)
			continue
		}
		if rec.Loomcycle.Description == "" {
			t.Errorf("bundled recipe %q has empty description", name)
		}
		if rec.HasTransport() == "" {
			t.Errorf("bundled recipe %q has no resolvable transport", name)
		}
		// PoolSize default sanity.
		if rec.Loomcycle.PoolSize < 0 || rec.Loomcycle.PoolSize > 16 {
			t.Errorf("bundled recipe %q has implausible pool_size %d", name, rec.Loomcycle.PoolSize)
		}
	}
}

func TestLoadLibrary_OverlayOverridesBundled(t *testing.T) {
	root := t.TempDir()
	// Override the bundled `slack` with a custom version that uses
	// a different package + pool size.
	overlayJSON := []byte(`{
  "command": "node",
  "args": ["/opt/my-custom-slack/dist/index.js"],
  "env": {"SLACK_TOKEN": "${LOOMCYCLE_CUSTOM_SLACK_TOKEN}"},
  "_loomcycle": {
    "description": "Custom Slack fork.",
    "transport": "stdio",
    "pool_size": 8,
    "env_vars_required": ["LOOMCYCLE_CUSTOM_SLACK_TOKEN"],
    "credentials": [],
    "schedule_compatible": true,
    "agent_prompt_hint": "Custom"
  }
}`)
	if err := os.WriteFile(filepath.Join(root, "slack.json"), overlayJSON, 0o644); err != nil {
		t.Fatal(err)
	}

	lib, err := LoadLibrary(root)
	if err != nil {
		t.Fatalf("LoadLibrary: %v", err)
	}
	rec, _, ok := lib.Get("slack")
	if !ok {
		t.Fatal("slack missing after overlay")
	}
	if rec.Source != "overlay" {
		t.Errorf("Source = %q, want overlay", rec.Source)
	}
	if rec.Loomcycle.PoolSize != 8 {
		t.Errorf("PoolSize = %d, want 8 (overlay's value, not bundled)", rec.Loomcycle.PoolSize)
	}
}

func TestLoadLibrary_OverlayAddsNewRecipe(t *testing.T) {
	root := t.TempDir()
	overlay := []byte(`{
  "url": "https://my-internal-mcp.example.com/api",
  "headers": {"Authorization": "Bearer ${LOOMCYCLE_INTERNAL_TOKEN}"},
  "_loomcycle": {
    "description": "Internal-only MCP.",
    "transport": "http",
    "pool_size": 2,
    "env_vars_required": ["LOOMCYCLE_INTERNAL_TOKEN"],
    "credentials": [],
    "schedule_compatible": false,
    "agent_prompt_hint": "Internal use only."
  }
}`)
	if err := os.WriteFile(filepath.Join(root, "internal-mcp.json"), overlay, 0o644); err != nil {
		t.Fatal(err)
	}

	lib, err := LoadLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	rec, _, ok := lib.Get("internal-mcp")
	if !ok {
		t.Fatal("internal-mcp not loaded from overlay")
	}
	if rec.HasTransport() != "http" {
		t.Errorf("transport = %q, want http", rec.HasTransport())
	}
}

func TestLoadLibrary_MalformedOverlayRecipeIsSkipped(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "bad.json"), []byte(`{not valid json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "good.json"), []byte(`{
  "command": "echo",
  "args": ["hi"],
  "_loomcycle": {
    "description": "ok",
    "transport": "stdio",
    "pool_size": 1
  }
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, err := LoadLibrary(root)
	if err != nil {
		t.Fatalf("LoadLibrary should not fatal on malformed overlay: %v", err)
	}
	if _, _, ok := lib.Get("good"); !ok {
		t.Error("good.json should be loaded despite bad.json being malformed")
	}
	if _, _, ok := lib.Get("bad"); ok {
		t.Error("bad.json should have been skipped")
	}
}

func TestLoadLibrary_DisabledFileFiltersFromEnabled(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".disabled"), []byte("slack\ntelegram\n# this is a comment\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, err := LoadLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	if !lib.IsDisabled("slack") {
		t.Error("slack should be disabled")
	}
	if !lib.IsDisabled("telegram") {
		t.Error("telegram should be disabled")
	}
	for _, n := range lib.Enabled() {
		if n == "slack" || n == "telegram" {
			t.Errorf("disabled recipe %q appeared in Enabled()", n)
		}
	}
	// All() includes disabled.
	found := map[string]bool{}
	for _, n := range lib.All() {
		found[n] = true
	}
	if !found["slack"] || !found["telegram"] {
		t.Error("All() should include disabled recipes")
	}
}

func TestLoadLibrary_SymlinkIsRefused(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "evil.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	lib, err := LoadLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := lib.Get("evil"); ok {
		t.Error("symlink recipe should be refused")
	}
}

func TestLoadLibrary_MissingOverlayRootIsFatal(t *testing.T) {
	_, err := LoadLibrary("/nonexistent/path/that/does/not/exist/0123456789")
	if err == nil {
		t.Fatal("missing overlay root should be fatal")
	}
}

func TestAddOverlay_ValidatesInput(t *testing.T) {
	root := t.TempDir()
	lib, _ := LoadLibrary(root)
	// Malformed JSON.
	_, err := lib.AddOverlay("new", []byte(`{bad`), false)
	if err == nil {
		t.Error("expected error on malformed JSON")
	}
	// Neither command nor url.
	_, err = lib.AddOverlay("empty", []byte(`{"_loomcycle":{"description":"x","transport":"stdio","pool_size":1}}`), false)
	if err == nil {
		t.Error("expected error on missing transport-defining fields")
	}
	// Invalid name.
	_, err = lib.AddOverlay("bad name", []byte(`{"command":"x","_loomcycle":{"description":"x","transport":"stdio","pool_size":1}}`), false)
	if err == nil {
		t.Error("expected error on invalid recipe name (space)")
	}
}

func TestAddOverlay_RefusesWithoutOverlayRoot(t *testing.T) {
	lib, err := LoadLibrary("")
	if err != nil {
		t.Fatal(err)
	}
	_, err = lib.AddOverlay("test", []byte(`{"command":"x","_loomcycle":{"description":"x","transport":"stdio","pool_size":1}}`), false)
	if err == nil {
		t.Error("expected error when overlay root is unset")
	}
	if !strings.Contains(err.Error(), "LOOMCYCLE_MCP_RECIPES_ROOT") {
		t.Errorf("error should mention env var name; got %v", err)
	}
}

func TestAddOverlay_RefusesClobberWithoutForce(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, _ := LoadLibrary(root)
	_, err := lib.AddOverlay("existing", []byte(`{"command":"x","_loomcycle":{"description":"x","transport":"stdio","pool_size":1}}`), false)
	if err == nil {
		t.Error("expected refusal on collision without force")
	}
}

func TestAddOverlay_ForceClobbers(t *testing.T) {
	root := t.TempDir()
	lib, _ := LoadLibrary(root)
	body1 := []byte(`{"command":"a","_loomcycle":{"description":"first","transport":"stdio","pool_size":1}}`)
	body2 := []byte(`{"command":"b","_loomcycle":{"description":"second","transport":"stdio","pool_size":2}}`)
	if _, err := lib.AddOverlay("t", body1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.AddOverlay("t", body2, true); err != nil {
		t.Fatalf("force clobber: %v", err)
	}
	rec, _, _ := lib.Get("t")
	if rec.Loomcycle.PoolSize != 2 {
		t.Errorf("after force, pool_size = %d, want 2", rec.Loomcycle.PoolSize)
	}
}

func TestRemoveOverlay_RevealsBundledFallback(t *testing.T) {
	root := t.TempDir()
	// Override bundled slack.
	override := []byte(`{"command":"custom","_loomcycle":{"description":"custom","transport":"stdio","pool_size":99}}`)
	if err := os.WriteFile(filepath.Join(root, "slack.json"), override, 0o644); err != nil {
		t.Fatal(err)
	}
	lib, _ := LoadLibrary(root)
	rec, _, _ := lib.Get("slack")
	if rec.Source != "overlay" {
		t.Fatalf("setup: slack should be overlay-sourced")
	}
	if err := lib.RemoveOverlay("slack"); err != nil {
		t.Fatalf("RemoveOverlay: %v", err)
	}
	// After remove, bundled should be back.
	rec, _, ok := lib.Get("slack")
	if !ok {
		t.Fatal("slack disappeared after remove — should fall back to bundled")
	}
	if rec.Source != "bundled" {
		t.Errorf("after remove, Source = %q, want bundled", rec.Source)
	}
}

func TestRemoveOverlay_RefusesBundledRecipe(t *testing.T) {
	root := t.TempDir()
	lib, _ := LoadLibrary(root)
	err := lib.RemoveOverlay("github")
	if err == nil {
		t.Error("expected error removing bundled recipe")
	}
	if !strings.Contains(err.Error(), "bundled") {
		t.Errorf("error should mention bundled; got %v", err)
	}
}

func TestEnableDisable_PersistsToFile(t *testing.T) {
	root := t.TempDir()
	lib, _ := LoadLibrary(root)
	if err := lib.Disable("github"); err != nil {
		t.Fatal(err)
	}
	// Re-load — disabled state should persist.
	lib2, _ := LoadLibrary(root)
	if !lib2.IsDisabled("github") {
		t.Error("disable should persist across reload")
	}
	if err := lib2.Enable("github"); err != nil {
		t.Fatal(err)
	}
	lib3, _ := LoadLibrary(root)
	if lib3.IsDisabled("github") {
		t.Error("enable should persist across reload")
	}
}

func TestEnableDisable_Idempotent(t *testing.T) {
	root := t.TempDir()
	lib, _ := LoadLibrary(root)
	if err := lib.Disable("github"); err != nil {
		t.Fatal(err)
	}
	if err := lib.Disable("github"); err != nil {
		t.Errorf("double-disable should be no-op; got %v", err)
	}
}

func TestEnableDisable_RefusesWithoutOverlayRoot(t *testing.T) {
	lib, _ := LoadLibrary("")
	if err := lib.Disable("github"); err == nil {
		t.Error("disable should refuse without overlay root")
	}
}

func TestRecipePackageName(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"stdio with -y", `{"command":"npx","args":["-y","@scope/pkg"],"_loomcycle":{"description":"x","transport":"stdio","pool_size":1}}`, "@scope/pkg"},
		{"stdio direct", `{"command":"node","args":["dist/index.js"],"_loomcycle":{"description":"x","transport":"stdio","pool_size":1}}`, "dist/index.js"},
		{"http", `{"url":"https://example.com/api","_loomcycle":{"description":"x","transport":"http","pool_size":1}}`, "https://example.com/api"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rec Recipe
			if err := json.Unmarshal([]byte(tt.body), &rec); err != nil {
				t.Fatal(err)
			}
			if got := rec.PackageName(); got != tt.want {
				t.Errorf("PackageName() = %q, want %q", got, tt.want)
			}
		})
	}
}
