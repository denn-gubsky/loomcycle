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

// Backward-compat: with no volumes: block but legacy ReadRoot==WriteRoot env,
// a single `default` rw volume is synthesized.
func TestLoadVolumes_SynthesizeDefaultFromLegacyEnv(t *testing.T) {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOMCYCLE_READ_ROOT", d)
	t.Setenv("LOOMCYCLE_WRITE_ROOT", d)
	yamlPath := writeVolConfig(t, `
agents:
  a: { model: claude-sonnet-4-6 }
`)
	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	v, ok := cfg.Volumes["default"]
	if !ok {
		t.Fatal("expected a synthesized `default` volume")
	}
	if v.Path != d || v.Mode != "rw" || !v.Default {
		t.Errorf("default = %+v, want {path:%q, mode:rw, default:true}", v, d)
	}
	if _, ok := cfg.Volumes["default-read"]; ok {
		t.Error("ReadRoot==WriteRoot should NOT synthesize a default-read companion")
	}
}

// ReadRoot != WriteRoot (read is a broader parent) synthesizes a companion
// read-only `default-read` volume alongside the rw `default` (= WriteRoot).
func TestLoadVolumes_ReadRootBroaderSynthesizesCompanion(t *testing.T) {
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
	def := cfg.Volumes["default"]
	if def.Path != writeDir || def.ReadOnly() {
		t.Errorf("default should be the rw write root %q; got %+v", writeDir, def)
	}
	dr, ok := cfg.Volumes["default-read"]
	if !ok {
		t.Fatal("ReadRoot != WriteRoot should synthesize a default-read companion")
	}
	if dr.Path != base || !dr.ReadOnly() {
		t.Errorf("default-read should be the ro read root %q; got %+v", base, dr)
	}
}

// An explicit `default` volume in yaml wins over legacy-env synthesis.
func TestLoadVolumes_ExplicitDefaultNotOverridden(t *testing.T) {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir())
	t.Setenv("LOOMCYCLE_READ_ROOT", other)
	t.Setenv("LOOMCYCLE_WRITE_ROOT", other)
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
	if cfg.Volumes["default"].Path != d || !cfg.Volumes["default"].ReadOnly() {
		t.Errorf("explicit default must win over env synthesis; got %+v", cfg.Volumes["default"])
	}
}
