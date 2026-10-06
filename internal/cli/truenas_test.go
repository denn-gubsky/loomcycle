package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const sampleCatalogue = `# loomcycle CONFIG (non-secret).

# ─── Ollama ───────────────────────────────────────
# The local Ollama base URL.
OLLAMA_BASE_URL=http://localhost:11434

# num_ctx pins the input window.
# LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX=32768

# ─── Sidecar listen ───
LOOMCYCLE_LISTEN_ADDR=127.0.0.1:8787

# Enable the Bash tool.
# LOOMCYCLE_BASH_ENABLED=1

# A trailing inline comment must be stripped.
# LOOMCYCLE_GRPC_ADDR=127.0.0.1:8788   # optional

# This secret must NEVER be surfaced as a plain field.
ANTHROPIC_API_KEY=sk-xxx
# LOOMCYCLE_OPERATOR_TOKEN_PEPPER=pep
`

func attrsByName(attrs []tnVar) map[string]tnVar {
	m := map[string]tnVar{}
	for _, a := range attrs {
		m[a.Variable] = a
	}
	return m
}

// TestParseEnvCatalogue_SurfacesKnobs: commented + active knobs both become
// string questions; the preceding prose is the help; the example value is appended.
func TestParseEnvCatalogue_SurfacesKnobs(t *testing.T) {
	attrs := parseEnvCatalogue(sampleCatalogue)
	by := attrsByName(attrs)

	for _, want := range []string{"OLLAMA_BASE_URL", "LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX", "LOOMCYCLE_BASH_ENABLED", "LOOMCYCLE_GRPC_ADDR"} {
		if _, ok := by[want]; !ok {
			t.Errorf("knob %q not surfaced (have %v)", want, attrKeys(by))
		}
	}
	// type=string, default empty (override-only).
	if a := by["LOOMCYCLE_BASH_ENABLED"]; a.Schema.Type != "string" || a.Schema.Default != "" {
		t.Errorf("bash knob schema = %+v, want type=string default=\"\"", a.Schema)
	}
	// help carries the prose + the example value.
	if a := by["LOOMCYCLE_BASH_ENABLED"]; !strings.Contains(a.Description, "Enable the Bash tool") || !strings.Contains(a.Description, "(example: 1)") {
		t.Errorf("bash help = %q, want prose + (example: 1)", a.Description)
	}
	// inline trailing comment stripped from the example.
	if a := by["LOOMCYCLE_GRPC_ADDR"]; !strings.Contains(a.Description, "(example: 127.0.0.1:8788)") || strings.Contains(a.Description, "optional") {
		t.Errorf("grpc help = %q, want example without the inline comment", a.Description)
	}
}

// TestParseEnvCatalogue_SkipsSecretsAndCovered: secret-suffix keys and the
// already-covered/fixed knobs never appear as plain fields.
func TestParseEnvCatalogue_SkipsSecretsAndCovered(t *testing.T) {
	by := attrsByName(parseEnvCatalogue(sampleCatalogue))
	for _, bad := range []string{"ANTHROPIC_API_KEY", "LOOMCYCLE_OPERATOR_TOKEN_PEPPER", "LOOMCYCLE_LISTEN_ADDR"} {
		if _, ok := by[bad]; ok {
			t.Errorf("knob %q must be skipped (secret or already-covered), but was surfaced", bad)
		}
	}
}

// TestRunTrueNASQuestions_EmitsValidYAML: the command emits one env_options dict
// question that parses as YAML and is grouped under Advanced configuration.
func TestRunTrueNASQuestions_EmitsValidYAML(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := RunTrueNASQuestions(nil, &out, &errb); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errb.String())
	}
	var got []map[string]any
	if err := yaml.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid YAML: %v", err)
	}
	if len(got) != 1 || got[0]["variable"] != "env_options" || got[0]["group"] != "Advanced configuration" {
		t.Fatalf("expected one env_options question grouped under Advanced configuration, got %+v", got)
	}
	// Stray args are a usage error.
	if rc := RunTrueNASQuestions([]string{"x"}, &out, &errb); rc != 2 {
		t.Errorf("with args rc=%d, want 2", rc)
	}
}

// TestTrueNASCatalog_EnvOptionsMatchGenerator: the env_options question checked
// into the TrueNAS catalog form is exactly what `truenas-questions` emits from
// the embedded env catalogue. The form is a spliced copy, so without this a knob
// added to the catalogue never reaches the install wizard until someone
// remembers to regenerate it.
func TestTrueNASCatalog_EnvOptionsMatchGenerator(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "truenas", "catalog", "questions.yaml"))
	if err != nil {
		t.Fatalf("read catalog form: %v", err)
	}
	var form struct {
		Questions []map[string]any `yaml:"questions"`
	}
	if err := yaml.Unmarshal(raw, &form); err != nil {
		t.Fatalf("catalog form is not valid YAML: %v", err)
	}
	var have map[string]any
	for _, q := range form.Questions {
		if q["variable"] == "env_options" {
			have = q
		}
	}
	if have == nil {
		t.Fatal("catalog form has no env_options question")
	}

	var out, errb bytes.Buffer
	if rc := RunTrueNASQuestions(nil, &out, &errb); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errb.String())
	}
	var gen []map[string]any
	if err := yaml.Unmarshal(out.Bytes(), &gen); err != nil || len(gen) != 1 {
		t.Fatalf("generator output: %v (%d questions)", err, len(gen))
	}
	if !reflect.DeepEqual(have, gen[0]) {
		t.Errorf("deploy/truenas/catalog/questions.yaml env_options is stale: regenerate it with "+
			"`loomcycle truenas-questions` (only in the form: %v; only in the catalogue: %v)",
			attrDiff(have, gen[0]), attrDiff(gen[0], have))
	}
}

// attrDiff lists the env_options attr names in a that b lacks (a mismatch with
// none on either side means a help text changed).
func attrDiff(a, b map[string]any) []string {
	names := func(q map[string]any) map[string]bool {
		m := map[string]bool{}
		schema, _ := q["schema"].(map[string]any)
		attrs, _ := schema["attrs"].([]any)
		for _, x := range attrs {
			if v, ok := x.(map[string]any); ok {
				m[fmt.Sprint(v["variable"])] = true
			}
		}
		return m
	}
	in, other := names(a), names(b)
	var out []string
	for n := range in {
		if !other[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func attrKeys(m map[string]tnVar) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
