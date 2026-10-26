package builtin

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// The per-agent `transcription:` block round-trips through AgentDef
// create/fork and the lookup read path, merges per field on a fork, is
// content-identifying, and may only lower the operator's caps.

func TestAgentDef_TranscriptionBlockRoundTripsAndIsHashed(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	got := storedRouting(t, tool, ctx, `{"op":"create","name":"reader","overlay":{"tier":"middle","system_prompt":"s","transcription":{"ocr":"auto","max_pages_per_run":40}}}`)
	want := &config.AgentTranscription{OCR: "auto", MaxPagesPerRun: 40}
	if !reflect.DeepEqual(got.Transcription, want) {
		t.Fatalf("stored transcription = %+v, want %+v", got.Transcription, want)
	}

	// A fork that changes only the block is a new version, not a dedup.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"reader","overlay":{"transcription":{"max_pages_per_run":20}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if deduped, _ := decodeResult(t, res.Text)["deduped"].(bool); deduped {
		t.Fatalf("a fork that changes only transcription deduped: %s", res.Text)
	}

	// Per field: a fork naming one field keeps the parent's others.
	got = storedRouting(t, tool, ctx, `{"op":"fork","name":"reader","overlay":{"transcription":{"stt":"off"}}}`)
	want = &config.AgentTranscription{OCR: "auto", STT: "off", MaxPagesPerRun: 40}
	if !reflect.DeepEqual(got.Transcription, want) {
		t.Errorf("forked transcription = %+v, want %+v", got.Transcription, want)
	}
}

func TestAgentDef_TranscriptionBadValueIsRefused(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"bad","overlay":{"tier":"middle","system_prompt":"s","transcription":{"ocr":"sometimes"}}}`))
	if !res.IsError || !strings.Contains(res.Text, "transcription.ocr") {
		t.Errorf("got %s, want a refusal naming transcription.ocr", res.Text)
	}
}

// TestAgentOverlay_TranscriptionCapsOnlyLower — an authored cap above the
// operator's is refused at create and fork, and nothing is stored.
func TestAgentOverlay_TranscriptionCapsOnlyLower(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	tool.Cfg.Transcription = config.TranscriptionConfig{
		OCR: &config.OCRConfig{Engine: config.OCREngineVision, MaxPagesPerRun: 100},
	}.Resolved()

	for _, call := range []string{
		`{"op":"create","name":"greedy","overlay":{"tier":"middle","system_prompt":"s","transcription":{"ocr":"always","max_pages_per_run":101}}}`,
		`{"op":"fork","name":"researcher","overlay":{"transcription":{"max_pages_per_run":500}}}`,
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(call))
		if !res.IsError || !strings.Contains(res.Text, "transcription.max_pages_per_run") || !strings.Contains(res.Text, "only lower") {
			t.Errorf("%s: got %s, want a refusal naming the cap", call, res.Text)
		}
	}
	if _, err := tool.Store.AgentDefGetActive(ctx, "", "greedy"); err == nil {
		t.Error("a refused create still stored a row")
	}
	got := storedRouting(t, tool, ctx, `{"op":"create","name":"modest","overlay":{"tier":"middle","system_prompt":"s","transcription":{"max_pages_per_run":100}}}`)
	if got.Transcription == nil || got.Transcription.MaxPagesPerRun != 100 {
		t.Errorf("a cap equal to the operator's = %+v, want stored", got.Transcription)
	}
}

// TestSignFromMergedDef_NoTranscriptionIsByteStable — an agent without the
// block, or with an empty one, hashes as it did before the field existed.
func TestSignFromMergedDef_NoTranscriptionIsByteStable(t *testing.T) {
	def := staticToMergedDef(config.AgentDef{Tier: "middle", SystemPrompt: "s", Tools: []string{"Read"}})
	before := signFromMergedDef("a", def)
	def.Transcription = &config.AgentTranscription{}
	if got := signFromMergedDef("a", def); got != before {
		t.Errorf("empty transcription hashed %s, want %s", got, before)
	}
	def.Transcription = &config.AgentTranscription{OCR: "never"}
	if signFromMergedDef("a", def) == before {
		t.Error("a set transcription field did not change the hash")
	}
}

// TestStaticToMergedDef_CarriesTranscription — a static agent bootstrapped into
// the substrate keeps its block, so a fork of it inherits it.
func TestStaticToMergedDef_CarriesTranscription(t *testing.T) {
	in := &config.AgentTranscription{OCR: "always", MaxMinutesPerRun: 5}
	got := staticToMergedDef(config.AgentDef{Transcription: in}).Transcription
	if !reflect.DeepEqual(got, in) {
		t.Errorf("staticToMergedDef transcription = %+v, want %+v", got, in)
	}
	if got == in {
		t.Error("staticToMergedDef aliased the operator's block")
	}
}
