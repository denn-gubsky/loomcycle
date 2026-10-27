package builtin

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

// The per-agent `speech:` block round-trips through AgentDef create/fork and
// the lookup read path, merges per field on a fork, is content-identifying,
// and may only lower the operator's per-run cap.

func speechBool(b bool) *bool { return &b }

func TestAgentDef_SpeechBlockRoundTripsAndIsHashed(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	got := storedRouting(t, tool, ctx, `{"op":"create","name":"talker","overlay":{"tier":"middle","system_prompt":"s","speech":{"enabled":true,"max_chars_per_run":4000}}}`)
	want := &config.AgentSpeech{Enabled: speechBool(true), MaxCharsPerRun: 4000}
	if !reflect.DeepEqual(got.Speech, want) {
		t.Fatalf("stored speech = %+v, want %+v", got.Speech, want)
	}

	// A fork that only switches speech off is a new version, not a dedup, and
	// the explicit false survives the stored round trip (it is not "unset").
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"talker","overlay":{"speech":{"enabled":false}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if deduped, _ := decodeResult(t, res.Text)["deduped"].(bool); deduped {
		t.Fatalf("a fork that changes only speech.enabled deduped: %s", res.Text)
	}
	defID, _ := decodeResult(t, res.Text)["def_id"].(string)
	row, err := tool.Store.AgentDefGet(ctx, defID)
	if err != nil {
		t.Fatalf("AgentDefGet(%s): %v", defID, err)
	}
	if off, ok := lookup.AgentFromDefRow(row); !ok || !reflect.DeepEqual(off.Speech, &config.AgentSpeech{Enabled: speechBool(false), MaxCharsPerRun: 4000}) {
		t.Errorf("forked speech = %+v, want enabled=false with the parent's cap", off.Speech)
	}

	// Per field: a fork naming one field keeps the parent's other. A fork does
	// not become active, so the parent is still the created version.
	got = storedRouting(t, tool, ctx, `{"op":"fork","name":"talker","overlay":{"speech":{"max_chars_per_run":2000}}}`)
	want = &config.AgentSpeech{Enabled: speechBool(true), MaxCharsPerRun: 2000}
	if !reflect.DeepEqual(got.Speech, want) {
		t.Errorf("forked speech = %+v, want %+v", got.Speech, want)
	}
}

func TestAgentDef_SpeechBadValueIsRefused(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"bad","overlay":{"tier":"middle","system_prompt":"s","speech":{"max_chars_per_run":-1}}}`))
	if !res.IsError || !strings.Contains(res.Text, "speech.max_chars_per_run") {
		t.Errorf("got %s, want a refusal naming speech.max_chars_per_run", res.Text)
	}
}

// TestAgentOverlay_SpeechCapsOnlyLower — an authored cap above the operator's
// is refused at create and fork, naming both values, and nothing is stored.
func TestAgentOverlay_SpeechCapsOnlyLower(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	tool.Cfg.Speech = (&config.SpeechConfig{
		Engine: config.SpeechEngineOpenAISpeech, DefaultLanguage: "en",
		Voices:         map[string]config.SpeechVoice{"en": {Model: "phonon", Voice: "Hadley"}},
		MaxCharsPerRun: 10000,
	}).Resolved()

	for _, call := range []string{
		`{"op":"create","name":"greedy","overlay":{"tier":"middle","system_prompt":"s","speech":{"max_chars_per_run":10001}}}`,
		`{"op":"fork","name":"researcher","overlay":{"speech":{"max_chars_per_run":500000}}}`,
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(call))
		if !res.IsError || !strings.Contains(res.Text, "speech.max_chars_per_run") || !strings.Contains(res.Text, "(10000)") || !strings.Contains(res.Text, "only lower") {
			t.Errorf("%s: got %s, want a refusal naming the cap and the operator's 10000", call, res.Text)
		}
	}
	if _, err := tool.Store.AgentDefGetActive(ctx, "", "greedy"); err == nil {
		t.Error("a refused create still stored a row")
	}
	got := storedRouting(t, tool, ctx, `{"op":"create","name":"modest","overlay":{"tier":"middle","system_prompt":"s","speech":{"max_chars_per_run":10000}}}`)
	if got.Speech == nil || got.Speech.MaxCharsPerRun != 10000 {
		t.Errorf("a cap equal to the operator's = %+v, want stored", got.Speech)
	}
}

// TestAgentOverlay_SpeechCapAcceptedWithoutEngine — with no speech engine
// configured there is nothing to exceed, so any cap is stored.
func TestAgentOverlay_SpeechCapAcceptedWithoutEngine(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	got := storedRouting(t, tool, ctx, `{"op":"create","name":"early","overlay":{"tier":"middle","system_prompt":"s","speech":{"max_chars_per_run":9000000}}}`)
	if got.Speech == nil || got.Speech.MaxCharsPerRun != 9000000 {
		t.Errorf("speech = %+v, want the cap stored", got.Speech)
	}
}

// TestSignFromMergedDef_NoSpeechIsByteStable — an agent without the block, or
// with an empty one, hashes as it did before the field existed.
func TestSignFromMergedDef_NoSpeechIsByteStable(t *testing.T) {
	def := staticToMergedDef(config.AgentDef{Tier: "middle", SystemPrompt: "s", Tools: []string{"Read"}})
	before := signFromMergedDef("a", def)
	def.Speech = &config.AgentSpeech{}
	if got := signFromMergedDef("a", def); got != before {
		t.Errorf("empty speech hashed %s, want %s", got, before)
	}
	def.Speech = &config.AgentSpeech{Enabled: speechBool(false)}
	off := signFromMergedDef("a", def)
	if off == before {
		t.Error("speech.enabled=false did not change the hash")
	}
	def.Speech = &config.AgentSpeech{Enabled: speechBool(true)}
	if signFromMergedDef("a", def) == off {
		t.Error("speech.enabled true and false hash the same")
	}
}

// TestStaticToMergedDef_CarriesSpeech — a static agent bootstrapped into the
// substrate keeps its block, so a fork of it inherits it.
func TestStaticToMergedDef_CarriesSpeech(t *testing.T) {
	in := &config.AgentSpeech{Enabled: speechBool(false), MaxCharsPerRun: 5}
	got := staticToMergedDef(config.AgentDef{Speech: in}).Speech
	if !reflect.DeepEqual(got, in) {
		t.Errorf("staticToMergedDef speech = %+v, want %+v", got, in)
	}
	if got == in || got.Enabled == in.Enabled {
		t.Error("staticToMergedDef aliased the operator's block")
	}
}
