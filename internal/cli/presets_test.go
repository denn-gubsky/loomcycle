package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunPresets_List: bare `presets` lists every embedded unit with its kind +
// a usage hint, and exits 0.
func TestRunPresets_List(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := RunPresets(nil, &out, &errb); rc != 0 {
		t.Fatalf("RunPresets() rc=%d, stderr=%s", rc, errb.String())
	}
	s := out.String()
	for _, want := range []string{"NAME", "base", "document-agent", "LOOMCYCLE_PRESETS="} {
		if !strings.Contains(s, want) {
			t.Errorf("presets list missing %q\n--- output ---\n%s", want, s)
		}
	}
}

// TestRunPresets_Show: `presets show base` prints the preset YAML; an unknown
// name fails (exit 2) listing the available units.
func TestRunPresets_Show(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := RunPresets([]string{"show", "base"}, &out, &errb); rc != 0 {
		t.Fatalf("presets show base rc=%d, stderr=%s", rc, errb.String())
	}
	if !strings.Contains(out.String(), "provider_priority") {
		t.Errorf("presets show base should print the YAML body, got:\n%s", out.String())
	}

	out.Reset()
	errb.Reset()
	if rc := RunPresets([]string{"show", "nope"}, &out, &errb); rc == 0 {
		t.Errorf("presets show <unknown> should fail")
	}
	if !strings.Contains(errb.String(), "available:") {
		t.Errorf("unknown-name error should list available units, got: %s", errb.String())
	}
}

// TestRunPresets_BadVerb: an unknown subcommand is a usage error (exit 2).
func TestRunPresets_BadVerb(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := RunPresets([]string{"frobnicate"}, &out, &errb); rc != 2 {
		t.Errorf("unknown verb rc=%d, want 2", rc)
	}
}

// TestRunEnvTemplate: prints the embedded env catalogue; rejects stray args.
func TestRunEnvTemplate(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := RunEnvTemplate(nil, &out, &errb); rc != 0 {
		t.Fatalf("env-template rc=%d, stderr=%s", rc, errb.String())
	}
	if !strings.Contains(out.String(), "LOOMCYCLE_") {
		t.Errorf("env-template output should contain LOOMCYCLE_ vars")
	}

	out.Reset()
	errb.Reset()
	if rc := RunEnvTemplate([]string{"extra"}, &out, &errb); rc != 2 {
		t.Errorf("env-template with args rc=%d, want 2", rc)
	}
}
