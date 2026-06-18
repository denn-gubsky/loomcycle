package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const volTestPreamble = `defaults: { provider: anthropic, model: claude-sonnet-4-6 }
`

func writeVolConfig(t *testing.T, body string) string {
	t.Helper()
	tmp := t.TempDir()
	yamlPath := filepath.Join(tmp, "c.yaml")
	if err := os.WriteFile(yamlPath, []byte(volTestPreamble+body), 0o600); err != nil {
		t.Fatal(err)
	}
	return yamlPath
}

// A volumes entry whose path does not exist is a config-load error (static
// volumes map existing infrastructure; the runtime never creates them).
func TestLoadVolumes_MissingPathErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	yamlPath := writeVolConfig(t, `
volumes:
  work: { path: `+missing+`, mode: rw }
agents:
  a: { model: claude-sonnet-4-6 }
`)
	_, err := Load(yamlPath)
	if err == nil || !strings.Contains(err.Error(), "must already exist") {
		t.Fatalf("expected missing-path error, got %v", err)
	}
}

// A volumes entry pointing at a file (not a directory) is a config-load error.
func TestLoadVolumes_NonDirPathErrors(t *testing.T) {
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	yamlPath := writeVolConfig(t, `
volumes:
  work: { path: `+f+`, mode: rw }
agents:
  a: { model: claude-sonnet-4-6 }
`)
	_, err := Load(yamlPath)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("expected not-a-directory error, got %v", err)
	}
}

// Two volumes marked default:true is a config-load error.
func TestLoadVolumes_TwoDefaultsErrors(t *testing.T) {
	d := t.TempDir()
	yamlPath := writeVolConfig(t, `
volumes:
  a: { path: `+d+`, mode: rw, default: true }
  b: { path: `+d+`, mode: ro, default: true }
agents:
  ag: { model: claude-sonnet-4-6 }
`)
	_, err := Load(yamlPath)
	if err == nil || !strings.Contains(err.Error(), "at most one volume may be default") {
		t.Fatalf("expected two-defaults error, got %v", err)
	}
}

// An agent binding to a volume not declared in the top-level map errors.
func TestLoadVolumes_AgentBindsUndeclaredErrors(t *testing.T) {
	d := t.TempDir()
	yamlPath := writeVolConfig(t, `
volumes:
  work: { path: `+d+`, mode: rw, default: true }
agents:
  a:
    model: claude-sonnet-4-6
    volumes: [nonesuch]
`)
	_, err := Load(yamlPath)
	if err == nil || !strings.Contains(err.Error(), "unknown volume") {
		t.Fatalf("expected unknown-volume binding error, got %v", err)
	}
}

// An agent binding only to DECLARED volumes loads cleanly and the absolute
// path is normalised in place.
func TestLoadVolumes_AgentBindsDeclaredLoads(t *testing.T) {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	yamlPath := writeVolConfig(t, `
volumes:
  work: { path: `+d+`, mode: rw, default: true }
  ref:  { path: `+d+`, mode: ro }
agents:
  a:
    model: claude-sonnet-4-6
    volumes: [work, ref]
`)
	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Volumes["work"].Path; got != d {
		t.Errorf("volume path = %q, want absolute %q", got, d)
	}
	if cfg.Volumes["ref"].Mode != "ro" {
		t.Errorf("ref volume should be ro; got %q", cfg.Volumes["ref"].Mode)
	}
}

// Backward-compat: with legacy ReadRoot/WriteRoot env and NO `volumes:` block,
// NOTHING is synthesized — there is no `default` volume. Unbound agents then
// run with an inactive policy and each file tool uses its own legacy root
// (Read←ReadRoot, Write←WriteRoot, Bash←BashCwd), byte-identical to a
// pre-feature deployment. We deliberately do NOT collapse the three legacy
// roots into one synthesized `default`: a single root can't reproduce three
// distinct ones, and a ReadRoot-only "writes disabled" deploy must not silently
// gain write access on upgrade.
func TestLoadVolumes_NoSynthesisFromLegacyEnv(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeDir := filepath.Join(base, "out")
	if err := os.MkdirAll(writeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOMCYCLE_READ_ROOT", base)
	t.Setenv("LOOMCYCLE_WRITE_ROOT", writeDir)
	yamlPath := writeVolConfig(t, `
agents:
  a: { model: claude-sonnet-4-6 }
`)
	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := cfg.Volumes["default"]; ok {
		t.Error("no `volumes:` block must NOT synthesize a `default` volume from legacy env")
	}
	if _, ok := cfg.Volumes["default-read"]; ok {
		t.Error("no `default-read` companion should be synthesized either")
	}
}

// An explicit `default` volume in yaml loads and is honored verbatim (path /
// mode / default flag); unbound agents bind to it.
func TestLoadVolumes_ExplicitDefaultLoads(t *testing.T) {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	yamlPath := writeVolConfig(t, `
volumes:
  default: { path: `+d+`, mode: ro, default: true }
agents:
  a: { model: claude-sonnet-4-6 }
`)
	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	v := cfg.Volumes["default"]
	if v.Path != d || !v.ReadOnly() || !v.Default {
		t.Errorf("explicit default = %+v, want {path:%q, ro, default:true}", v, d)
	}
}

// An unknown mode (not ro / rw / "") is a config-load error.
func TestLoadVolumes_InvalidModeErrors(t *testing.T) {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	yamlPath := writeVolConfig(t, `
volumes:
  data: { path: `+d+`, mode: readwrite }
agents:
  a: { model: claude-sonnet-4-6 }
`)
	if _, err := Load(yamlPath); err == nil {
		t.Fatal("an invalid volume mode must be a config-load error")
	}
}

// RFC AH Phase 2a: two volumes marked dynamic_root:true is a config-load
// error — there can be exactly one operator-blessed parent the VolumeDef
// substrate provisions dynamic volumes inside.
func TestLoadVolumes_TwoDynamicRootsErrors(t *testing.T) {
	d := t.TempDir()
	yamlPath := writeVolConfig(t, `
volumes:
  pool-a: { path: `+d+`, mode: rw, dynamic_root: true }
  pool-b: { path: `+d+`, mode: rw, dynamic_root: true }
agents:
  ag: { model: claude-sonnet-4-6 }
`)
	_, err := Load(yamlPath)
	if err == nil || !strings.Contains(err.Error(), "at most one volume may be dynamic_root") {
		t.Fatalf("expected two-dynamic-roots error, got %v", err)
	}
}

// A single dynamic_root:true volume loads + carries the flag through.
func TestLoadVolumes_DynamicRootLoads(t *testing.T) {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	yamlPath := writeVolConfig(t, `
volumes:
  pool: { path: `+d+`, mode: rw, dynamic_root: true }
agents:
  a: { model: claude-sonnet-4-6 }
`)
	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Volumes["pool"].DynamicRoot {
		t.Errorf("dynamic_root flag dropped: %+v", cfg.Volumes["pool"])
	}
}

// An invalid volume_def_scopes entry (not "any" / "named:<x>") errors.
func TestLoadVolumes_InvalidVolumeDefScopeErrors(t *testing.T) {
	d := t.TempDir()
	yamlPath := writeVolConfig(t, `
volumes:
  pool: { path: `+d+`, mode: rw, dynamic_root: true }
agents:
  a: { model: claude-sonnet-4-6, volume_def_scopes: [self] }
`)
	_, err := Load(yamlPath)
	if err == nil || !strings.Contains(err.Error(), "volume_def_scopes") {
		t.Fatalf("expected volume_def_scopes validation error, got %v", err)
	}
}
