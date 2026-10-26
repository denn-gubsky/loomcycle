package config

import (
	"reflect"
	"strings"
	"testing"
)

const agentTranscriptionOperator = `
transcription:
  ocr: { engine: vision, max_pages_per_run: 100 }
  stt: { engine: openai-audio, provider: whisper-local, model: whisper, max_minutes_per_run: 60 }
agents:
`

// TestAgentTranscription_LoadsFromYAML — the block is read as written.
func TestAgentTranscription_LoadsFromYAML(t *testing.T) {
	cfg, err := loadTranscription(t, agentTranscriptionOperator+`
  reader:
    transcription: { ocr: auto, stt: off, max_pages_per_run: 40, max_minutes_per_run: 30 }
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := &AgentTranscription{OCR: "auto", STT: "off", MaxPagesPerRun: 40, MaxMinutesPerRun: 30}
	if got := cfg.Agents["reader"].Transcription; !reflect.DeepEqual(got, want) {
		t.Errorf("transcription = %+v, want %+v", got, want)
	}
}

// TestAgentTranscription_CapsOnlyLower — the lower-only rule is a REFUSAL at
// load: an agent asking for more than the operator allows names both values.
// Equal is allowed (it lowers nothing, and raises nothing).
func TestAgentTranscription_CapsOnlyLower(t *testing.T) {
	for _, c := range []struct{ name, block, want string }{
		{"pages above", "{ ocr: auto, max_pages_per_run: 101 }", "transcription.max_pages_per_run (101) is above the operator's transcription.ocr.max_pages_per_run (100)"},
		{"minutes above", "{ max_minutes_per_run: 61 }", "transcription.max_minutes_per_run (61) is above the operator's transcription.stt.max_minutes_per_run (60)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadTranscription(t, agentTranscriptionOperator+"  greedy:\n    transcription: "+c.block+"\n")
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), `agent "greedy"`) {
				t.Errorf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
	if _, err := loadTranscription(t, agentTranscriptionOperator+"  even:\n    transcription: { max_pages_per_run: 100, max_minutes_per_run: 60 }\n"); err != nil {
		t.Errorf("caps equal to the operator's must load: %v", err)
	}
}

// TestAgentTranscription_CapsInertWhenEngineOff — with no engine there is no
// cap to exceed, so the agent's number is accepted (it binds once the operator
// configures one, through the same min).
func TestAgentTranscription_CapsInertWhenEngineOff(t *testing.T) {
	if _, err := loadTranscription(t, "agents:\n  early:\n    transcription: { ocr: always, max_pages_per_run: 5000 }\n"); err != nil {
		t.Errorf("an agent cap with no operator engine must load: %v", err)
	}
}

func TestAgentTranscription_RefusesBadValues(t *testing.T) {
	for _, c := range []struct{ name, block, want string }{
		{"ocr mode", "{ ocr: sometimes }", `transcription.ocr: "sometimes" is not one of auto, always, never`},
		{"stt mode", "{ stt: maybe }", `transcription.stt: "maybe" is not one of on, off`},
		{"negative pages", "{ max_pages_per_run: -1 }", "transcription.max_pages_per_run must be >= 0"},
		{"negative minutes", "{ max_minutes_per_run: -1 }", "transcription.max_minutes_per_run must be >= 0"},
		{"unknown key", "{ ocr: auto, language: uk }", `transcription: unknown key "language"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadTranscription(t, "agents:\n  a:\n    transcription: "+c.block+"\n")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

// TestCheckAgentTranscription_JudgesAgainstTheLoadedOperator — the same check
// the AgentDef overlay path calls, on a loaded config.
func TestCheckAgentTranscription_JudgesAgainstTheLoadedOperator(t *testing.T) {
	cfg, err := loadTranscription(t, agentTranscriptionOperator)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.CheckAgentTranscription(nil); err != nil {
		t.Errorf("nil block: %v", err)
	}
	if err := cfg.CheckAgentTranscription(&AgentTranscription{MaxPagesPerRun: 100}); err != nil {
		t.Errorf("equal cap: %v", err)
	}
	if err := cfg.CheckAgentTranscription(&AgentTranscription{MaxPagesPerRun: 101}); err == nil {
		t.Error("a cap above the operator's was accepted")
	}
}

// TestMergeAgentTranscription_PerField — a set field in the overlay wins, an
// unset one keeps the base, and neither input is aliased.
func TestMergeAgentTranscription_PerField(t *testing.T) {
	base := &AgentTranscription{OCR: "auto", MaxPagesPerRun: 10}
	over := &AgentTranscription{STT: "off", MaxPagesPerRun: 5}
	got := MergeAgentTranscription(base, over)
	want := &AgentTranscription{OCR: "auto", STT: "off", MaxPagesPerRun: 5}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merge = %+v, want %+v", got, want)
	}
	got.OCR = "never"
	if base.OCR != "auto" {
		t.Error("merge aliased its base")
	}
	if MergeAgentTranscription(nil, &AgentTranscription{}) != nil {
		t.Error("merging two empty blocks must give nil (hash byte-stability)")
	}
}

// TestMergeAgentDef_CarriesTranscription — a yaml override on an MD-discovered
// agent merges the block per field like compaction.
func TestMergeAgentDef_CarriesTranscription(t *testing.T) {
	out := mergeAgentDef(
		AgentDef{Transcription: &AgentTranscription{OCR: "auto", MaxMinutesPerRun: 9}},
		AgentDef{Transcription: &AgentTranscription{OCR: "always"}},
	)
	want := &AgentTranscription{OCR: "always", MaxMinutesPerRun: 9}
	if !reflect.DeepEqual(out.Transcription, want) {
		t.Errorf("merged = %+v, want %+v", out.Transcription, want)
	}
}
