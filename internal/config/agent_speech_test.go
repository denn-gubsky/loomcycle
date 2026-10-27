package config

import (
	"reflect"
	"strings"
	"testing"
)

const agentSpeechOperator = `
speech:
  engine: openai-speech
  default_language: en
  voices: { en: { model: phonon, voice: Hadley } }
  max_chars_per_run: 20000
agents:
`

// TestAgentSpeech_LoadsFromYAML — the block is read as written, and an
// explicit `enabled: false` is kept apart from an absent one.
func TestAgentSpeech_LoadsFromYAML(t *testing.T) {
	cfg, err := loadSpeech(t, agentSpeechOperator+`
  quiet:
    speech: { enabled: false }
  talker:
    speech: { enabled: true, max_chars_per_run: 5000 }
  plain: {}
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got, want := cfg.Agents["quiet"].Speech, (&AgentSpeech{Enabled: boolPtr(false)}); !reflect.DeepEqual(got, want) {
		t.Errorf("quiet speech = %+v, want enabled=false", got)
	}
	if got, want := cfg.Agents["talker"].Speech, (&AgentSpeech{Enabled: boolPtr(true), MaxCharsPerRun: 5000}); !reflect.DeepEqual(got, want) {
		t.Errorf("talker speech = %+v, want %+v", got, want)
	}
	if got := cfg.Agents["plain"].Speech; got != nil {
		t.Errorf("an agent without the block has speech %+v, want nil", got)
	}
}

// TestAgentSpeech_CapsOnlyLower — the lower-only rule is a REFUSAL at load: an
// agent asking for more than the operator allows names both values. Equal is
// allowed.
func TestAgentSpeech_CapsOnlyLower(t *testing.T) {
	_, err := loadSpeech(t, agentSpeechOperator+"  greedy:\n    speech: { max_chars_per_run: 20001 }\n")
	const want = "speech.max_chars_per_run (20001) is above the operator's speech.max_chars_per_run (20000)"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), `agent "greedy"`) {
		t.Errorf("err = %v, want it to contain %q", err, want)
	}
	if _, err := loadSpeech(t, agentSpeechOperator+"  even:\n    speech: { max_chars_per_run: 20000 }\n"); err != nil {
		t.Errorf("a cap equal to the operator's must load: %v", err)
	}
}

// TestAgentSpeech_CapsJudgedAgainstTheDefault — with no cap written by the
// operator, the agent is judged against the default per-run cap.
func TestAgentSpeech_CapsJudgedAgainstTheDefault(t *testing.T) {
	const op = "speech: { engine: openai-speech, default_language: en, voices: { en: { model: phonon, voice: Hadley } } }\nagents:\n"
	_, err := loadSpeech(t, op+"  greedy:\n    speech: { max_chars_per_run: 100001 }\n")
	if err == nil || !strings.Contains(err.Error(), "(100000)") {
		t.Errorf("err = %v, want a refusal against the default cap 100000", err)
	}
}

// TestAgentSpeech_CapsInertWhenEngineOff — with no engine there is no cap to
// exceed, so the agent's number is accepted (it binds once the operator
// configures one, through the runtime's minimum).
func TestAgentSpeech_CapsInertWhenEngineOff(t *testing.T) {
	if _, err := loadSpeech(t, "agents:\n  early:\n    speech: { max_chars_per_run: 9000000 }\n"); err != nil {
		t.Errorf("an agent cap with no operator block must load: %v", err)
	}
	if _, err := loadSpeech(t, "speech: { engine: off, max_chars_per_run: 10 }\nagents:\n  early:\n    speech: { max_chars_per_run: 9000000 }\n"); err != nil {
		t.Errorf("an agent cap with engine: off must load: %v", err)
	}
}

func TestAgentSpeech_RefusesBadValues(t *testing.T) {
	for _, c := range []struct{ name, block, want string }{
		{"negative cap", "{ max_chars_per_run: -1 }", "speech.max_chars_per_run must be >= 0"},
		{"unknown key", "{ enabled: true, voice: Hadley }", `speech: unknown key "voice"`},
		{"enabled not a bool", "{ enabled: sometimes }", "`sometimes` into bool"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadSpeech(t, "agents:\n  a:\n    speech: "+c.block+"\n")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

// TestCheckAgentSpeech_JudgesAgainstTheLoadedOperator — the same check the
// AgentDef overlay path calls, on a loaded config.
func TestCheckAgentSpeech_JudgesAgainstTheLoadedOperator(t *testing.T) {
	cfg, err := loadSpeech(t, agentSpeechOperator)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.CheckAgentSpeech(nil); err != nil {
		t.Errorf("nil block: %v", err)
	}
	if err := cfg.CheckAgentSpeech(&AgentSpeech{Enabled: boolPtr(false)}); err != nil {
		t.Errorf("switching speech off: %v", err)
	}
	if err := cfg.CheckAgentSpeech(&AgentSpeech{MaxCharsPerRun: 20000}); err != nil {
		t.Errorf("equal cap: %v", err)
	}
	if err := cfg.CheckAgentSpeech(&AgentSpeech{MaxCharsPerRun: 20001}); err == nil {
		t.Error("a cap above the operator's was accepted")
	}
	if err := cfg.CheckAgentSpeech(&AgentSpeech{MaxCharsPerRun: -5}); err == nil {
		t.Error("a negative cap was accepted")
	}
}

// TestMergeAgentSpeech_PerField — a set field in the overlay wins (an explicit
// false included), an unset one keeps the base, and neither input is aliased.
func TestMergeAgentSpeech_PerField(t *testing.T) {
	base := &AgentSpeech{Enabled: boolPtr(true), MaxCharsPerRun: 10}
	over := &AgentSpeech{Enabled: boolPtr(false)}
	got := MergeAgentSpeech(base, over)
	want := &AgentSpeech{Enabled: boolPtr(false), MaxCharsPerRun: 10}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merge = %+v, want %+v", got, want)
	}
	*got.Enabled = true
	if *base.Enabled != true || *over.Enabled != false {
		t.Error("merge aliased an input's Enabled pointer")
	}
	got = MergeAgentSpeech(base, &AgentSpeech{MaxCharsPerRun: 5})
	if !reflect.DeepEqual(got, &AgentSpeech{Enabled: boolPtr(true), MaxCharsPerRun: 5}) {
		t.Errorf("merge cap only = %+v, want the base's enabled kept", got)
	}
	if MergeAgentSpeech(nil, &AgentSpeech{}) != nil {
		t.Error("merging two empty blocks must give nil (hash byte-stability)")
	}
}

// TestAgentSpeech_CloneIsDeep — a clone shares no pointer with its source.
func TestAgentSpeech_CloneIsDeep(t *testing.T) {
	in := &AgentSpeech{Enabled: boolPtr(true), MaxCharsPerRun: 3}
	out := in.Clone()
	if !reflect.DeepEqual(in, out) || out.Enabled == in.Enabled {
		t.Errorf("clone = %+v, want an equal value with its own Enabled pointer", out)
	}
	if (*AgentSpeech)(nil).Clone() != nil {
		t.Error("Clone of nil must be nil")
	}
	if !(&AgentSpeech{}).IsZero() || (&AgentSpeech{Enabled: boolPtr(false)}).IsZero() {
		t.Error("IsZero must treat only the empty block as zero; enabled=false is set")
	}
}

// TestMergeAgentDef_CarriesSpeech — a yaml override on an MD-discovered agent
// merges the block per field like transcription.
func TestMergeAgentDef_CarriesSpeech(t *testing.T) {
	out := mergeAgentDef(
		AgentDef{Speech: &AgentSpeech{Enabled: boolPtr(true), MaxCharsPerRun: 9}},
		AgentDef{Speech: &AgentSpeech{Enabled: boolPtr(false)}},
	)
	want := &AgentSpeech{Enabled: boolPtr(false), MaxCharsPerRun: 9}
	if !reflect.DeepEqual(out.Speech, want) {
		t.Errorf("merged = %+v, want %+v", out.Speech, want)
	}
}
