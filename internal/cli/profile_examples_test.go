package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/embedded"
)

// The two profile examples under examples/profiles are what an operator
// copies. These tests hold them to the code: every variable is one the runtime
// reads, nothing in them loosens a gate or carries a secret, and each yaml
// loads over the preset its env file selects.

// Absolute, taken while the working directory is still the package's: the
// load tests change it.
var profileExampleRoot = func() string {
	abs, err := filepath.Abs(filepath.Join("..", "..", "examples", "profiles"))
	if err != nil {
		panic(err)
	}
	return abs
}()

var profileNames = []string{"personal", "server"}

// envAssignment matches a variable assignment, active or commented out. A
// commented line is an instruction to uncomment, so its name is checked too.
var envAssignment = regexp.MustCompile(`^(#\s*)?([A-Z][A-Z0-9_]*)=(.*)$`)

type envLine struct {
	name, value string
	active      bool
}

func readEnvExample(t *testing.T, profile, file string) []envLine {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(profileExampleRoot, profile, file))
	if err != nil {
		t.Fatalf("read %s/%s: %v", profile, file, err)
	}
	var out []envLine
	for _, l := range strings.Split(string(data), "\n") {
		if m := envAssignment.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			out = append(out, envLine{name: m[2], value: strings.TrimSpace(m[3]), active: m[1] == ""})
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s/%s: no variable lines found; the test is not reading the file", profile, file)
	}
	return out
}

func exampleFile(t *testing.T, profile, file string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(profileExampleRoot, profile, file))
	if err != nil {
		t.Fatalf("read %s/%s: %v", profile, file, err)
	}
	return string(data)
}

// Every variable an example names is one the runtime still reads: it is not
// in the retired table, and it is documented in the env catalogue or the
// secrets template, or it is a name the example's own yaml or compose file
// introduces (a token_env, a gate's secret, the bundled database's password).
func TestProfileExamples_EveryVariableIsOneTheRuntimeReads(t *testing.T) {
	secretsTemplate, err := os.ReadFile(filepath.Join(profileExampleRoot, "..", "..", ".env.local.example"))
	if err != nil {
		t.Fatalf("read .env.local.example: %v", err)
	}
	known := string(embedded.EnvTemplate()) + "\n" + string(secretsTemplate)
	// The check below must be able to fail: a name the runtime retired is
	// reported, and a made-up one is in no catalogue.
	if len(config.RetiredEnvAdvisories([]string{"LOOMCYCLE_SCHEDULER_ENABLED=1"})) == 0 {
		t.Fatal("RetiredEnvAdvisories reports nothing for a retired name; this test would pass anything")
	}
	if strings.Contains(known, "LOOMCYCLE_NO_SUCH_SETTING") {
		t.Fatal("the catalogue matches a made-up name")
	}
	names := 0
	for _, profile := range profileNames {
		own := exampleFile(t, profile, "loomcycle.yaml")
		if profile == "server" {
			own += exampleFile(t, profile, "docker-compose.yaml")
		}
		for _, file := range []string{"env.insecure.example", "env.local.example"} {
			for _, l := range readEnvExample(t, profile, file) {
				names++
				if adv := config.RetiredEnvAdvisories([]string{l.name + "=1"}); len(adv) > 0 {
					t.Errorf("%s/%s: %s is retired: %s", profile, file, l.name, strings.Join(adv, "; "))
				}
				word := regexp.MustCompile(`\b` + regexp.QuoteMeta(l.name) + `\b`)
				if !word.MatchString(known) && !word.MatchString(own) {
					t.Errorf("%s/%s: %s is in neither the env catalogue, the secrets template, nor this profile's own yaml or compose file", profile, file, l.name)
				}
			}
		}
	}
	if names < 20 {
		t.Fatalf("checked %d names; the examples have more", names)
	}
}

// loosening are the variables that widen what an agent or a caller may do.
// A profile example may show one only as a commented line that says so.
var loosening = []string{
	"LOOMCYCLE_BASH_ENABLED", "LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS", "LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_ENV",
	"LOOMCYCLE_HTTP_HOST_ALLOWLIST", "LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST", "LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE",
	"LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS", "LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO",
	"LOOMCYCLE_PUBLIC_CONFIG", "LOOMCYCLE_UI_INSECURE_COOKIES", "LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED",
	"LOOMCYCLE_MOCK_ENABLED",
}

func TestProfileExamples_ALooseningVariableIsOnlyACommentedOptIn(t *testing.T) {
	isLoosening := map[string]bool{}
	for _, n := range loosening {
		isLoosening[n] = true
	}
	shown := 0
	for _, profile := range profileNames {
		lines := strings.Split(exampleFile(t, profile, "env.insecure.example"), "\n")
		for i, raw := range lines {
			m := envAssignment.FindStringSubmatch(strings.TrimSpace(raw))
			if m == nil || !isLoosening[m[2]] {
				continue
			}
			shown++
			if m[1] == "" {
				t.Errorf("%s: %s is set; a profile may show it only commented out", profile, m[2])
			}
			if i == 0 || !strings.Contains(lines[i-1], "opt-in;") {
				t.Errorf("%s: %s has no `opt-in; …` line above it saying what it loosens", profile, m[2])
			}
		}
	}
	if shown == 0 {
		t.Fatal("no loosening variable found in either example; the test is not reading them")
	}
}

// The secrets templates carry names only.
func TestProfileExamples_SecretsTemplatesHaveNoValues(t *testing.T) {
	for _, profile := range profileNames {
		for _, l := range readEnvExample(t, profile, "env.local.example") {
			if l.value != "" {
				t.Errorf("%s/env.local.example: %s has a value; the template carries names only", profile, l.name)
			}
		}
	}
}

// Each example yaml is enough by itself: it names its preset, its env file
// selects none, and it loads with only what the example tells the operator to
// supply.
func TestProfileExamples_YAMLAloneSelectsItsPresetAndLoads(t *testing.T) {
	for _, profile := range profileNames {
		t.Run(profile, func(t *testing.T) {
			isolateLayerEnv(t)
			for _, k := range []string{"LOOMCYCLE_PG_DSN", "LOOMCYCLE_SQLMEM_PG_DSN", "LOOMCYCLE_LISTEN_ADDR", "LOOMCYCLE_AUTH_MODE", "LOOMCYCLE_PUBLIC_URL"} {
				t.Setenv(k, "")
			}
			for _, l := range readEnvExample(t, profile, "env.insecure.example") {
				if l.active && l.name == "LOOMCYCLE_PRESETS" {
					t.Errorf("env.insecure.example sets LOOMCYCLE_PRESETS: it would override the yaml's own presets: key; keep it a commented alternative")
				}
			}

			// The server yaml names the container's dynamic root; a static
			// volume's path must exist, so point it at a directory that does.
			body := exampleFile(t, profile, "loomcycle.yaml")
			if profile == "server" {
				if !strings.Contains(body, "/mnt/dynamic") {
					t.Fatal("the server yaml no longer names /mnt/dynamic; update this test and the compose mount together")
				}
				body = strings.ReplaceAll(body, "/mnt/dynamic", t.TempDir())
				t.Setenv("LOOMCYCLE_AUTH_TOKEN", "test-token-not-a-secret")
				// A sign-in gate turns on the signed audit stream, which
				// needs the key; the compose file requires it too.
				t.Setenv("LOOMCYCLE_SECRET_KEY", "test-key-not-a-secret-test-key-not-a-secret")
			}
			// The server yaml lives in the config directory, with no file
			// named; the personal one is named or discovered.
			dir := t.TempDir()
			path := writeYAML(t, filepath.Join(dir, "loomcycle.yaml"), body)
			paths := []string{path}
			if profile == "server" {
				t.Setenv("LOOMCYCLE_CONFIG_DIR", dir)
				paths = nil
			}

			stack, err := AssembleConfigStack(nil, paths, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(stack.Presets) == 0 || stack.Presets[len(stack.Presets)-1] != profile {
				t.Fatalf("presets layered = %v, want the %s stack, selected by the yaml alone", stack.Presets, profile)
			}
			cfg, err := config.LoadLayers(stack.Layers...)
			if err != nil {
				t.Fatalf("LoadLayers: %v", err)
			}
			if _, ok := cfg.Agents["chat/medium"]; !ok {
				t.Error("chat/medium missing: the preset was not layered under the example yaml")
			}
			if profile == "server" {
				if g, ok := cfg.Auth.Gates["token"]; !ok || g.Kind != "token" {
					t.Errorf("auth.gates = %v, want the token gate", cfg.Auth.Gates)
				}
				if len(cfg.Auth.Gates) != 1 {
					t.Errorf("%d gates are active; the interactive and bridge gates ship commented out", len(cfg.Auth.Gates))
				}
				if cfg.Retention.Enabled {
					t.Error("retention is on in the server example; it ships as a commented opt-in")
				}
			}
		})
	}
}

// The server compose file mounts what the yaml and the env file name, and
// requires each secret the runtime cannot start without.
func TestProfileExamples_ServerComposeMatchesItsYAMLAndEnv(t *testing.T) {
	compose := exampleFile(t, "server", "docker-compose.yaml")
	for _, want := range []string{
		"./config:/config:ro", // LOOMCYCLE_CONFIG_DIR, read-only
		"./data:/data",        // LOOMCYCLE_DATA_DIR
		":/mnt/dynamic",       // volumes.dynamic.path in the yaml
		"${LOOMCYCLE_AUTH_TOKEN:?",
		"${LOOMCYCLE_SECRET_KEY:?",
		"${LOOMCYCLE_PG_DSN:?",
		"${LOOMCYCLE_SQLMEM_PG_DSN:?",
		"${POSTGRES_PASSWORD:?",
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("docker-compose.yaml lacks %q", want)
		}
	}
	env := map[string]string{}
	for _, l := range readEnvExample(t, "server", "env.insecure.example") {
		if l.active {
			env[l.name] = l.value
		}
	}
	if env["LOOMCYCLE_CONFIG_DIR"] != "/config" || env["LOOMCYCLE_DATA_DIR"] != "/data" {
		t.Errorf("env.insecure.example config/data dirs = %q, %q; the compose file mounts /config and /data", env["LOOMCYCLE_CONFIG_DIR"], env["LOOMCYCLE_DATA_DIR"])
	}
}
