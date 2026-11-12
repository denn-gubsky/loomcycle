package config

import (
	"strings"
	"testing"
)

func TestAgentTelemetry_LoadsNeverFromYAMLAndRefusesAnythingElse(t *testing.T) {
	cfg, err := loadSpeech(t, "agents:\n  secretive:\n    telemetry: { content_capture: never }\n  plain: {}\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Agents["secretive"].Telemetry.NeverCapturesContent() {
		t.Errorf("secretive telemetry = %+v, want content_capture never", cfg.Agents["secretive"].Telemetry)
	}
	if got := cfg.Agents["plain"].Telemetry; got != nil || got.NeverCapturesContent() {
		t.Errorf("an agent without the block has telemetry %+v, want nil", got)
	}
	for _, c := range []struct{ block, want string }{
		{"{ content_capture: always }", `telemetry.content_capture must be "never" or unset`},
		{"{ content_capture: on }", `telemetry.content_capture must be "never" or unset`},
		{"{ content_capture: never, sampler: 1 }", `telemetry: unknown key "sampler"`},
	} {
		if _, err := loadSpeech(t, "agents:\n  a:\n    telemetry: "+c.block+"\n"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", c.block, err, c.want)
		}
	}
}

func TestMergeAgentTelemetry_AnOverlayCanOptOutButNeverBackIn(t *testing.T) {
	never := &AgentTelemetry{ContentCapture: AgentContentCaptureNever}
	if got := MergeAgentTelemetry(nil, nil); got != nil {
		t.Errorf("merge of nothing = %+v, want nil", got)
	}
	if got := MergeAgentTelemetry(nil, never); !got.NeverCapturesContent() || got == never {
		t.Errorf("opt-out overlay = %+v (aliased %v)", got, got == never)
	}
	if got := MergeAgentTelemetry(never, &AgentTelemetry{}); !got.NeverCapturesContent() {
		t.Errorf("an empty overlay removed the opt-out: %+v", got)
	}
}
