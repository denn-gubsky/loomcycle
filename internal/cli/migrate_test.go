package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// migrate up/down/status without any DSN source — should fail with a
// clear "no DSN" message rather than panicking on nil pgxpool.
func TestRunMigrate_UpNoDSN(t *testing.T) {
	t.Setenv("LOOMCYCLE_PG_DSN", "")
	var stdout, stderr bytes.Buffer
	rc := RunMigrate([]string{"up"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "no DSN") {
		t.Errorf("stderr missing 'no DSN': %q", stderr.String())
	}
}

// Unknown migrate verb → rc=2 + helpful message.
func TestRunMigrate_UnknownVerb(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunMigrate([]string{"sideways"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), `unknown migrate verb "sideways"`) {
		t.Errorf("stderr missing diagnostic: %q", stderr.String())
	}
}

func TestRunMigrate_NoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunMigrate(nil, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("stderr missing usage: %q", stderr.String())
	}
}

// migrate down without --yes refuses (destructive guardrail).
func TestRunMigrate_DownRequiresYes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunMigrate([]string{"down", "--dsn", "postgres://nowhere"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "--yes") {
		t.Errorf("stderr missing --yes hint: %q", stderr.String())
	}
}

// loadPgDSN: --dsn wins over env, env wins over yaml; yaml needs
// backend=postgres + non-empty DSN.
func TestLoadPgDSN_Precedence(t *testing.T) {
	// 1) explicit DSN beats env.
	t.Setenv("LOOMCYCLE_PG_DSN", "from-env")
	got, err := loadPgDSN(nil, "from-flag", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-flag" {
		t.Errorf("explicit-flag precedence: got %q", got)
	}

	// 2) env beats yaml.
	got, err = loadPgDSN(nil, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-env" {
		t.Errorf("env precedence: got %q", got)
	}

	// 3) yaml-only path returns the yaml's DSN.
	t.Setenv("LOOMCYCLE_PG_DSN", "")
	yamlPath := writeTempConfig(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  default:
    system_prompt: "x"
    tools: []
storage:
  backend: postgres
  pg_dsn: from-yaml
`)
	got, err = loadPgDSN([]string{yamlPath}, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-yaml" {
		t.Errorf("yaml precedence: got %q", got)
	}

	// 4) yaml with sqlite backend errors with a pointed message.
	yamlPath2 := writeTempConfig(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  default:
    system_prompt: "x"
    tools: []
`)
	_, err = loadPgDSN([]string{yamlPath2}, "", io.Discard)
	if err == nil {
		t.Fatal("expected error for sqlite-backend yaml; got nil")
	}
	if !strings.Contains(err.Error(), "want \"postgres\"") {
		t.Errorf("error doesn't name the fix: %v", err)
	}
}

// `migrate --config X` opens the database the server would open, and that
// can be one X does not name: a section file's storage block, a later
// --config, an auth.env beside the file, the environment. Before acting,
// every verb says which layers it read and where the DSN came from, and
// never the DSN.
func TestLoadPgDSN_SaysWhereTheDSNCameFrom(t *testing.T) {
	const secret = "postgres://u:s3cr3tmarker@db.example.com/x"
	base := "defaults: { provider: anthropic, model: claude-sonnet-4-6 }\nstorage:\n  backend: postgres\n"
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, cwd string) (paths []string, wantSource string)
	}{
		{"the named file", func(t *testing.T, cwd string) ([]string, string) {
			p := writeYAML(t, filepath.Join(cwd, "a.yaml"), base+"  pg_dsn: "+secret+"\n")
			return []string{p}, "storage.pg_dsn in " + p
		}},
		{"a later --config", func(t *testing.T, cwd string) ([]string, string) {
			a := writeYAML(t, filepath.Join(cwd, "a.yaml"), base+"  pg_dsn: postgres://other.example.com/y\n")
			b := writeYAML(t, filepath.Join(cwd, "b.yaml"), "storage:\n  pg_dsn: "+secret+"\n")
			return []string{a, b}, "storage.pg_dsn in " + b
		}},
		{"a section file beside the named loomcycle.yaml", func(t *testing.T, cwd string) ([]string, string) {
			p := writeYAML(t, filepath.Join(cwd, "site", "loomcycle.yaml"), base)
			sib := writeYAML(t, filepath.Join(cwd, "site", "loomcycle.storage.yaml"), "storage:\n  pg_dsn: "+secret+"\n")
			return []string{p}, "storage.pg_dsn in " + sib
		}},
		{"the environment", func(t *testing.T, cwd string) ([]string, string) {
			p := writeYAML(t, filepath.Join(cwd, "a.yaml"), base+"  pg_dsn: postgres://other.example.com/y\n")
			t.Setenv("LOOMCYCLE_PG_DSN", secret)
			return []string{p}, "the environment (LOOMCYCLE_PG_DSN)"
		}},
		{"an auth.env beside the last file", func(t *testing.T, cwd string) ([]string, string) {
			p := writeYAML(t, filepath.Join(cwd, "site", "loomcycle.yaml"), base)
			env := writeYAML(t, filepath.Join(cwd, "site", "auth.env"), "LOOMCYCLE_PG_DSN="+secret+"\n")
			return []string{p}, "auth.env (" + env + ")"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cwd := isolateLayerEnv(t)
			// Restored by t.Setenv's cleanup; unset so an auth.env may set it.
			t.Setenv("LOOMCYCLE_PG_DSN", "")
			os.Unsetenv("LOOMCYCLE_PG_DSN")
			paths, wantSource := tc.setup(t, cwd)
			var stderr bytes.Buffer
			got, err := loadPgDSN(paths, "", &stderr)
			if err != nil {
				t.Fatal(err)
			}
			if got != secret {
				t.Fatalf("the DSN resolved is not the one the server would open")
			}
			out := stderr.String()
			if !strings.Contains(out, "Postgres DSN taken from: "+wantSource+"\n") {
				t.Errorf("stderr does not name the source %q:\n%s", wantSource, out)
			}
			for _, p := range paths {
				if !strings.Contains(out, p) {
					t.Errorf("stderr does not list the layer %s:\n%s", p, out)
				}
			}
			if strings.Contains(out, "s3cr3tmarker") || strings.Contains(out, "db.example.com") {
				t.Errorf("stderr carries part of the DSN:\n%s", out)
			}
		})
	}
}

// The value may come from any layer, so the errors name the layers read and
// not the one file that was passed.
func TestLoadPgDSN_ErrorsNameTheLayersRead(t *testing.T) {
	_, cwd := isolateLayerEnv(t)
	t.Setenv("LOOMCYCLE_PG_DSN", "")
	a := writeYAML(t, filepath.Join(cwd, "a.yaml"), "defaults: { provider: anthropic, model: claude-sonnet-4-6 }\n")
	b := writeYAML(t, filepath.Join(cwd, "b.yaml"), "storage:\n  backend: sqlite\n")
	_, err := loadPgDSN([]string{a, b}, "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "want \"postgres\"") ||
		!strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) {
		t.Errorf("err = %v, want it to name both layers", err)
	}
}

// The verbs print the lines before they touch the database: here the
// connection fails, and the lines are already out.
func TestRunMigrate_PrintsTheTargetBeforeActing(t *testing.T) {
	for _, verb := range [][]string{{"status"}, {"up"}, {"down", "--yes"}} {
		t.Run(verb[0], func(t *testing.T) {
			_, cwd := isolateLayerEnv(t)
			t.Setenv("LOOMCYCLE_PG_DSN", "")
			// Port 1 on loopback refuses at once.
			p := writeYAML(t, filepath.Join(cwd, "a.yaml"),
				"defaults: { provider: anthropic, model: claude-sonnet-4-6 }\nstorage:\n  backend: postgres\n  pg_dsn: postgres://u:s3cr3tmarker@127.0.0.1:1/x?connect_timeout=2\n")
			var stdout, stderr bytes.Buffer
			rc := RunMigrate(append(append([]string{}, verb...), "--config", p), &stdout, &stderr)
			if rc != 1 {
				t.Fatalf("rc=%d, want 1 (the database is unreachable); stderr=%q", rc, stderr.String())
			}
			out := stderr.String()
			if !strings.Contains(out, "migrate: config layers: ") || !strings.Contains(out, "migrate: Postgres DSN taken from: storage.pg_dsn in "+p) {
				t.Errorf("stderr does not say what was loaded and where the DSN came from:\n%s", out)
			}
			if strings.Contains(out, "s3cr3tmarker") {
				t.Errorf("stderr carries the DSN's password:\n%s", out)
			}
		})
	}
}

// migrate is what an operator runs when the schema is the problem. A value
// the server refuses, in a variable migrate never reads, is a warning here:
// the verb goes on to the database. The commands that describe the server
// (validate, agents list) keep failing on the same environment.
func TestRunMigrate_RunsUnderAnEnvironmentTheServerRefuses(t *testing.T) {
	setup := func(t *testing.T) string {
		_, cwd := isolateLayerEnv(t)
		t.Setenv("LOOMCYCLE_PG_DSN", "")
		t.Setenv("LOOMCYCLE_LIMITS_ENFORCE", "zz-strictt")
		t.Setenv("LOOMCYCLE_SCHEDULER_ENABLED", "0")
		t.Setenv("LOOMCYCLE_CONFIG_FILES", "/zz-nowhere.yaml")
		return writeYAML(t, filepath.Join(cwd, "a.yaml"),
			"defaults: { provider: anthropic, model: claude-sonnet-4-6 }\nstorage:\n  backend: postgres\n  pg_dsn: postgres://u@127.0.0.1:1/x?connect_timeout=2\n")
	}
	for _, verb := range [][]string{{"status"}, {"up"}, {"down", "--yes"}} {
		t.Run(verb[0], func(t *testing.T) {
			p := setup(t)
			var stdout, stderr bytes.Buffer
			rc := RunMigrate(append(append([]string{}, verb...), "--config", p), &stdout, &stderr)
			if rc != 1 {
				t.Fatalf("rc=%d, want 1 (it reached the unreachable database); stderr=%q", rc, stderr.String())
			}
			out := stderr.String()
			for _, name := range []string{"LOOMCYCLE_LIMITS_ENFORCE", "LOOMCYCLE_SCHEDULER_ENABLED", "LOOMCYCLE_CONFIG_FILES"} {
				if !strings.Contains(out, "loomcycle: warning: ") || !strings.Contains(out, name) {
					t.Errorf("stderr does not warn about %s:\n%s", name, out)
				}
			}
			if strings.Contains(out, "zz-") {
				t.Errorf("stderr carries a value:\n%s", out)
			}
		})
	}
	t.Run("a store disagreement still stops it", func(t *testing.T) {
		p := setup(t)
		t.Setenv("LOOMCYCLE_STORAGE_BACKEND", "sqlite")
		var stdout, stderr bytes.Buffer
		if rc := RunMigrate([]string{"status", "--config", p}, &stdout, &stderr); rc != 2 || !strings.Contains(stderr.String(), "LOOMCYCLE_STORAGE_BACKEND") {
			t.Errorf("rc=%d, stderr=%q", rc, stderr.String())
		}
	})
	t.Run("validate and agents list still fail", func(t *testing.T) {
		p := setup(t)
		var stdout, stderr bytes.Buffer
		if rc := RunValidate([]string{p}, &stdout, &stderr); rc != 2 {
			t.Errorf("validate rc=%d, want 2", rc)
		}
		if rc := RunAgents([]string{"list", "--config", p}, &stdout, &stderr); rc != 2 {
			t.Errorf("agents list rc=%d, want 2", rc)
		}
	})
}
