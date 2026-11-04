package http

import (
	"context"
	"math"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
)

// TestTranscriptionLedger_PricesSpeechByMinuteAndTokens — a speech request is
// written as one stt row with its seconds, priced by the pair's per-minute
// price, or by its tokens for a token-billed model; an unlisted pair stays
// unpriced.
func TestTranscriptionLedger_PricesSpeechByMinuteAndTokens(t *testing.T) {
	srv, st := newUsageTestServer(t)
	cfg := srv.cfg()
	cfg.Pricing = config.PricingConfig{Models: map[string]config.ModelPrice{
		"whisper-local/large-v3":        {PerMinute: 0.006},
		"openai/gpt-4o-transcribe":      {Input: 2.5, Output: 10},
		"openai/gpt-4o-mini-transcribe": {PerMinute: 0.003, Input: 1},
	}}
	l := transcriptionLedger{s: srv}
	ctx := runCtx("run-stt")
	for _, c := range []struct {
		charge transcribe.Charge
		want   float64
		priced bool
	}{
		{transcribe.Charge{Purpose: transcribe.PurposeSTT, Provider: "whisper-local", Model: "large-v3", AudioSeconds: 90}, 0.009, true},
		{transcribe.Charge{Purpose: transcribe.PurposeSTT, Provider: "openai", Model: "gpt-4o-transcribe", AudioSeconds: 60, InputTokens: 1_000_000, OutputTokens: 100_000}, 3.5, true},
		{transcribe.Charge{Purpose: transcribe.PurposeSTT, Provider: "openai", Model: "gpt-4o-mini-transcribe", AudioSeconds: 120, InputTokens: 1_000_000}, 1.006, true},
		{transcribe.Charge{Purpose: transcribe.PurposeSTT, Provider: "elsewhere", Model: "x", AudioSeconds: 60}, 0, false},
	} {
		p, err := l.Charge(ctx, c.charge)
		if err != nil {
			t.Fatal(err)
		}
		if p.Priced != c.priced || math.Abs(p.Cost-c.want) > 1e-9 {
			t.Errorf("%s/%s: price = %+v, want %v priced=%v", c.charge.Provider, c.charge.Model, p, c.want, c.priced)
		}
	}
	rows, err := st.TokenUsageForRun(context.Background(), "run-stt")
	if err != nil || len(rows) != 4 {
		t.Fatalf("ledger = %d rows, %v", len(rows), err)
	}
	if r := rows[0]; r.Purpose != "stt" || r.AudioSeconds != 90 || math.Abs(r.Cost-0.009) > 1e-9 {
		t.Errorf("row = %+v", r)
	}
	if e := l.EstimateSeconds("whisper-local", "large-v3", 600); !e.Priced || math.Abs(e.Cost-0.06) > 1e-9 {
		t.Errorf("estimate = %+v", e)
	}
}

// TestFileSTT_BuiltFromTheOperatorBlock — no engine is built while
// speech-to-text is off; configured, the engine resolves the alias to the
// operator's provider and concrete model from the live configuration.
func TestFileSTT_BuiltFromTheOperatorBlock(t *testing.T) {
	srv, _ := newUsageTestServer(t)
	if f, err := srv.FileSTT(); f != nil || err != nil {
		t.Fatalf("off: %v %v", f, err)
	}
	cfg := srv.cfg()
	cfg.Models = map[string]config.ModelRef{"whisper": {Provider: "whisper-local", Model: "Systran/faster-whisper-large-v3", Kind: config.ModelKindTranscriber}}
	cfg.Transcription = config.TranscriptionConfig{STT: &config.STTConfig{Engine: config.STTEngineOpenAIAudio, Provider: "whisper-local", Model: "whisper"}}
	f, err := srv.FileSTT()
	if err != nil || f == nil || f.Engine == nil || f.Config.STT.TimeoutSeconds != config.TranscriptionDefaultSTTTimeoutSecs {
		t.Fatalf("built = %+v, %v", f, err)
	}
	p, m, err := srv.ResolveSTTModel(context.Background())
	if err != nil || p != "whisper-local" || m != "Systran/faster-whisper-large-v3" {
		t.Errorf("resolve = %q %q %v", p, m, err)
	}
}

// TestTranscriptionLedger_PricesTTSByCharsOrMinute — a text-to-speech request
// is written as one tts row carrying the characters it sent and the seconds it
// produced, priced by the pair's per_million_chars or per_minute price.
func TestTranscriptionLedger_PricesTTSByCharsOrMinute(t *testing.T) {
	srv, st := newUsageTestServer(t)
	cfg := srv.cfg()
	cfg.Pricing = config.PricingConfig{Models: map[string]config.ModelPrice{
		"openai/tts-1":       {PerMillionChars: 15},
		"cloud-tts/by-time":  {PerMinute: 0.03},
		"speech-local/piper": {},
	}}
	l := transcriptionLedger{s: srv}
	ctx := runCtx("run-tts")
	for _, c := range []struct {
		charge transcribe.Charge
		want   float64
	}{
		{transcribe.Charge{Purpose: transcribe.PurposeTTS, Provider: "openai", Model: "tts-1", InputChars: 200_000, AudioSeconds: 600}, 3},
		{transcribe.Charge{Purpose: transcribe.PurposeTTS, Provider: "cloud-tts", Model: "by-time", InputChars: 1000, AudioSeconds: 120}, 0.06},
		{transcribe.Charge{Purpose: transcribe.PurposeTTS, Provider: "speech-local", Model: "piper", InputChars: 1000, AudioSeconds: 60}, 0},
	} {
		p, err := l.Charge(ctx, c.charge)
		if err != nil {
			t.Fatal(err)
		}
		if !p.Priced || math.Abs(p.Cost-c.want) > 1e-9 {
			t.Errorf("%s/%s: price = %+v, want %v", c.charge.Provider, c.charge.Model, p, c.want)
		}
	}
	rows, err := st.TokenUsageForRun(context.Background(), "run-tts")
	if err != nil || len(rows) != 3 {
		t.Fatalf("ledger = %d rows, %v", len(rows), err)
	}
	if r := rows[0]; r.Purpose != "tts" || r.InputChars != 200_000 || r.AudioSeconds != 600 || math.Abs(r.Cost-3) > 1e-9 {
		t.Errorf("row = %+v", r)
	}
}
