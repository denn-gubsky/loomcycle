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

// `presets show` of a preset that includes others names the whole stack, in
// layer order, before the preset's own YAML; a unit that includes nothing
// prints its YAML alone, as before.
func TestRunPresets_ShowNamesTheExpandedStack(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := RunPresets([]string{"show", "personal"}, &out, &errb); rc != 0 {
		t.Fatalf("presets show personal rc=%d, stderr=%s", rc, errb.String())
	}
	first := strings.SplitN(out.String(), "\n", 2)[0]
	if !strings.HasPrefix(first, "#") || !strings.Contains(first, "base, local, chat, document-agent, personal") {
		t.Errorf("first line = %q, want a comment naming the stack in order", first)
	}
	if !strings.Contains(out.String(), "include: [base, local, chat, document-agent]") {
		t.Errorf("presets show personal should still print the preset's own YAML, got:\n%s", out.String())
	}

	out.Reset()
	if rc := RunPresets([]string{"show", "base"}, &out, &errb); rc != 0 {
		t.Fatalf("presets show base rc=%d", rc)
	}
	if strings.Contains(out.String(), "expands to") {
		t.Errorf("base includes nothing; its output should carry no stack line")
	}

	out.Reset()
	if rc := RunPresets(nil, &out, &errb); rc != 0 {
		t.Fatalf("presets rc=%d", rc)
	}
	if !strings.Contains(out.String(), "base,local,chat,document-agent") {
		t.Errorf("the list should show personal's includes, got:\n%s", out.String())
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
