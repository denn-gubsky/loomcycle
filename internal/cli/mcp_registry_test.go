package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPRegistry_NoArgsShowsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunMCPRegistry(nil, &stdout, &stderr)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "Usage") {
		t.Errorf("stderr should contain Usage; got %q", stderr.String())
	}
}

func TestMCPRegistry_UnknownVerb(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunMCPRegistry([]string{"frobnicate"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "frobnicate") {
		t.Errorf("stderr should name the bad verb")
	}
}

func TestMCPRegistryList_Table(t *testing.T) {
	t.Setenv(envOverlayRoot, "")
	var stdout, stderr bytes.Buffer
	code := RunMCPRegistry([]string{"list"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"NAME", "TRANSPORT", "SOURCE", "github", "slack", "tavily"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q\n%s", want, out)
		}
	}
}

func TestMCPRegistryList_JSON(t *testing.T) {
	t.Setenv(envOverlayRoot, "")
	var stdout, stderr bytes.Buffer
	code := RunMCPRegistry([]string{"list", "--format=json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr=%s", code, stderr.String())
	}
	var env struct {
		Recipes []map[string]any `json:"recipes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	if len(env.Recipes) < 10 {
		t.Errorf("expected ≥10 recipes; got %d", len(env.Recipes))
	}
}

func TestMCPRegistryList_IncludesDisabled(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".disabled"), []byte("github\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envOverlayRoot, root)

	// Without --include-disabled, github is hidden.
	var stdout, stderr bytes.Buffer
	RunMCPRegistry([]string{"list"}, &stdout, &stderr)
	if strings.Contains(stdout.String(), "github ") {
		// Use a trailing space to avoid matching " github (disabled)".
		t.Errorf("github should be hidden by default when disabled")
	}

	// With --include-disabled, it shows up.
	stdout.Reset()
	stderr.Reset()
	RunMCPRegistry([]string{"list", "--include-disabled"}, &stdout, &stderr)
	if !strings.Contains(stdout.String(), "github") {
		t.Errorf("github should appear with --include-disabled")
	}
	if !strings.Contains(stdout.String(), "(disabled)") {
		t.Errorf("disabled tag missing\n%s", stdout.String())
	}
}

func TestMCPRegistryShow_Bundled(t *testing.T) {
	t.Setenv(envOverlayRoot, "")
	var stdout, stderr bytes.Buffer
	code := RunMCPRegistry([]string{"show", "github"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr=%s", code, stderr.String())
	}
	// Output should be valid JSON.
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if _, ok := got["_loomcycle"]; !ok {
		t.Errorf("output missing _loomcycle block:\n%s", stdout.String())
	}
}

func TestMCPRegistryShow_UnknownRefuses(t *testing.T) {
	t.Setenv(envOverlayRoot, "")
	var stdout, stderr bytes.Buffer
	code := RunMCPRegistry([]string{"show", "nonexistent-recipe-xyz"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}

func TestMCPRegistryShow_BundledFlagSkipsOverlay(t *testing.T) {
	root := t.TempDir()
	overlay := []byte(`{"command":"custom","_loomcycle":{"description":"OVERLAY","transport":"stdio","pool_size":1}}`)
	if err := os.WriteFile(filepath.Join(root, "github.json"), overlay, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envOverlayRoot, root)

	// Default: see overlay version.
	var stdout, stderr bytes.Buffer
	RunMCPRegistry([]string{"show", "github"}, &stdout, &stderr)
	if !strings.Contains(stdout.String(), "OVERLAY") {
		t.Errorf("default show should see overlay version\n%s", stdout.String())
	}

	// With --bundled: see bundled version.
	stdout.Reset()
	stderr.Reset()
	RunMCPRegistry([]string{"show", "github", "--bundled"}, &stdout, &stderr)
	if strings.Contains(stdout.String(), "OVERLAY") {
		t.Errorf("--bundled should ignore overlay\n%s", stdout.String())
	}
}

func TestMCPRegistryAppendToConfig_HappyPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "loomcycle.yaml")
	if err := os.WriteFile(target, []byte("defaults:\n  provider: anthropic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envOverlayRoot, "")

	var stdout, stderr bytes.Buffer
	// Natural RFC-documented form: positional first, flags after.
	// reorderFlagsFirst handles the reshuffle for Go's stdlib flag.
	code := RunMCPRegistry([]string{"append-to-config", "tavily", "--to=" + target, "--skip-env-check"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr=%s", code, stderr.String())
	}
	// File should now have mcp_servers.tavily.
	final, _ := os.ReadFile(target)
	if !strings.Contains(string(final), "tavily:") {
		t.Errorf("target missing tavily: entry\n%s", string(final))
	}
}

func TestMCPRegistryAppendToConfig_MissingTo(t *testing.T) {
	t.Setenv(envOverlayRoot, "")
	var stdout, stderr bytes.Buffer
	code := RunMCPRegistry([]string{"append-to-config", "tavily"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--to") {
		t.Errorf("stderr should mention --to; got %q", stderr.String())
	}
}

func TestMCPRegistryAdd(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "new.json")
	body := `{
  "command": "myserver",
  "args": [],
  "_loomcycle": {
    "description": "test",
    "transport": "stdio",
    "pool_size": 1
  }
}`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envOverlayRoot, root)

	var stdout, stderr bytes.Buffer
	code := RunMCPRegistry([]string{"add", src}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr=%s", code, stderr.String())
	}
	// File should now exist in overlay root.
	if _, err := os.Stat(filepath.Join(root, "new.json")); err != nil {
		t.Errorf("expected file in overlay root: %v", err)
	}
}

func TestMCPRegistryAdd_NameOverride(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "weird-name-from-disk.json")
	body := `{"command":"x","_loomcycle":{"description":"t","transport":"stdio","pool_size":1}}`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envOverlayRoot, root)

	var stdout, stderr bytes.Buffer
	code := RunMCPRegistry([]string{"add", src, "--name=clean-name"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "clean-name.json")); err != nil {
		t.Errorf("expected clean-name.json: %v", err)
	}
}

func TestMCPRegistryAdd_WithoutOverlayRootRefuses(t *testing.T) {
	t.Setenv(envOverlayRoot, "")
	src := filepath.Join(t.TempDir(), "x.json")
	if err := os.WriteFile(src, []byte(`{"command":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := RunMCPRegistry([]string{"add", src}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit when overlay root is unset")
	}
}

func TestMCPRegistryRemove_RefusesBundled(t *testing.T) {
	root := t.TempDir()
	t.Setenv(envOverlayRoot, root)
	var stdout, stderr bytes.Buffer
	code := RunMCPRegistry([]string{"remove", "github"}, &stdout, &stderr)
	if code == 0 {
		t.Error("expected non-zero exit when removing a bundled recipe")
	}
	if !strings.Contains(stderr.String(), "bundled") {
		t.Errorf("error should mention bundled; got %q", stderr.String())
	}
}

func TestMCPRegistryEnableDisableRoundTrip(t *testing.T) {
	root := t.TempDir()
	t.Setenv(envOverlayRoot, root)

	var stdout, stderr bytes.Buffer
	if code := RunMCPRegistry([]string{"disable", "github"}, &stdout, &stderr); code != 0 {
		t.Fatalf("disable: code=%d stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunMCPRegistry([]string{"enable", "github"}, &stdout, &stderr); code != 0 {
		t.Fatalf("enable: code=%d stderr=%s", code, stderr.String())
	}
}

func TestReadEnvAllowlist(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "x.yaml")
	body := `defaults:
  provider: anthropic
env:
  allowlist:
    - LOOMCYCLE_FOO_KEY
    - "LOOMCYCLE_BAR_TOKEN"
    # comment
storage:
  driver: sqlite
`
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readEnvAllowlist(target)
	if !got["LOOMCYCLE_FOO_KEY"] || !got["LOOMCYCLE_BAR_TOKEN"] {
		t.Errorf("allowlist = %v", got)
	}
	if got["storage:"] != false {
		t.Errorf("should not capture storage key")
	}
}

func TestReadEnvAllowlist_MissingFileReturnsNil(t *testing.T) {
	got := readEnvAllowlist(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if got != nil {
		t.Errorf("missing file should return nil; got %v", got)
	}
}

func TestPathBaseWithoutExt(t *testing.T) {
	tests := []struct{ in, want string }{
		{"foo.json", "foo"},
		{"/a/b/c.json", "c"},
		{"deep/nested/x.json", "x"},
		{"noext", "noext"},
	}
	for _, tt := range tests {
		if got := pathBaseWithoutExt(tt.in); got != tt.want {
			t.Errorf("pathBaseWithoutExt(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
