package config

import (
	"reflect"
	"strings"
	"testing"
)

// transcriptionBase declares what the transcription block may point at: an
// OpenAI-protocol provider serving a whisper model, and one alias of each kind.
const transcriptionBase = `
defaults: { provider: anthropic, model: x }
providers:
  whisper-local: { driver: openai, base_url: "http://whisper:8000/v1" }
models:
  whisper: { provider: whisper-local, model: large-v3, kind: transcriber }
  whisper-free: { model: large-v3, kind: transcriber }
  whisper-pattern: { provider: whisper-local, model_pattern: "large-*", kind: transcriber }
  whisper-elsewhere: { provider: openai, model: whisper-1, kind: transcriber }
  talker: { provider: anthropic, model: claude-x, kind: chat }
  plain: { provider: anthropic, model: claude-y }
  embed: { provider: openai, model: text-embedding-3-small, kind: embedder }
`

func loadTranscription(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	return LoadLayers(Layer{Name: "op", Data: []byte(transcriptionBase + extra)})
}

// TestTranscriptionConfig_AbsentBlockIsOff — a config without the block loads
// with both engines off, which is the state every existing deployment is in.
func TestTranscriptionConfig_AbsentBlockIsOff(t *testing.T) {
	cfg, err := loadTranscription(t, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Transcription.OCREnabled() || cfg.Transcription.STTEnabled() {
		t.Errorf("transcription = %+v, want both engines off", cfg.Transcription)
	}
}

// TestTranscriptionConfig_DefaultsAppliedAtLoad — every unset field comes back
// at its documented default, and the values written stay as written.
func TestTranscriptionConfig_DefaultsAppliedAtLoad(t *testing.T) {
	cfg, err := loadTranscription(t, `
transcription:
  ocr: { engine: vision }
  stt: { engine: openai-audio, provider: whisper-local, model: whisper }
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	wantOCR := OCRConfig{
		Engine: "vision", Tier: "middle", Language: "uk+ru",
		MaxPagesPerCall: 100, MaxPagesPerRun: 300, OCRMinCharsPerPage: 40, MaxTokensPerPage: 2000,
	}
	if cfg.Transcription.OCR == nil || !reflect.DeepEqual(*cfg.Transcription.OCR, wantOCR) {
		t.Errorf("ocr = %+v, want %+v", cfg.Transcription.OCR, wantOCR)
	}
	wantSTT := STTConfig{
		Engine: "openai-audio", Provider: "whisper-local", Model: "whisper", Language: "uk", Diarize: "auto",
		MaxMinutesPerCall: 120, MaxMinutesPerRun: 300, ChunkMinutes: 10, ChunkOverlapSeconds: 2, TimeoutSeconds: 1800,
	}
	if cfg.Transcription.STT == nil || !reflect.DeepEqual(*cfg.Transcription.STT, wantSTT) {
		t.Errorf("stt = %+v, want %+v", cfg.Transcription.STT, wantSTT)
	}
	if !cfg.Transcription.OCREnabled() || !cfg.Transcription.STTEnabled() {
		t.Error("both engines are configured, want both enabled")
	}
}

// TestTranscriptionConfig_ResolvedIsIdempotent — the one place defaults are
// applied can be called again on its own output without changing it, so a
// caller that resolves a loaded (already resolved) block gets the same block.
func TestTranscriptionConfig_ResolvedIsIdempotent(t *testing.T) {
	in := TranscriptionConfig{OCR: &OCRConfig{Engine: "vision"}, STT: &STTConfig{Engine: "off"}}
	once := in.Resolved()
	twice := once.Resolved()
	if !reflect.DeepEqual(once, twice) {
		t.Errorf("Resolved twice = %+v / %+v, want equal", *twice.OCR, *twice.STT)
	}
	if in.OCR.Tier != "" {
		t.Error("Resolved wrote into its input; it must return a copy")
	}
}

// TestTranscriptionConfig_LowerPerRunCapLowersDefaultPerCall — an operator who
// writes only a per-run cap below the per-call default is not refused for a
// value they never wrote: the per-call default follows the per-run cap down.
func TestTranscriptionConfig_LowerPerRunCapLowersDefaultPerCall(t *testing.T) {
	cfg, err := loadTranscription(t, `
transcription:
  ocr: { engine: vision, max_pages_per_run: 50 }
  stt: { engine: off, max_minutes_per_run: 60, max_minutes_per_call: 5 }
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Transcription.OCR.MaxPagesPerCall; got != 50 {
		t.Errorf("max_pages_per_call = %d, want 50 (the per-run cap)", got)
	}
	if got := cfg.Transcription.STT.ChunkMinutes; got != 5 {
		t.Errorf("chunk_minutes = %d, want 5 (the per-call cap)", got)
	}
}

// TestTranscriptionConfig_RefusesBadValues — each field's refusal, naming the
// place, so `loomcycle validate` points at the line to fix.
func TestTranscriptionConfig_RefusesBadValues(t *testing.T) {
	for _, c := range []struct {
		name, block string
		want        []string
	}{
		// ocr
		{"ocr engine missing", "ocr: { tier: middle }", []string{"transcription.ocr.engine is required", "vision", "off"}},
		{"ocr engine unknown", "ocr: { engine: tesseract }", []string{"transcription.ocr.engine", `"tesseract"`}},
		{"ocr command engine", "ocr: { engine: command }", []string{"transcription.ocr.engine", `"command"`, "not available yet"}},
		{"ocr http engine", "ocr: { engine: http }", []string{"transcription.ocr.engine", `"http"`, "not available yet"}},
		{"ocr command block", "ocr: { engine: vision, command: { template: x } }", []string{"transcription.ocr.command", "not available yet"}},
		{"ocr http block", "ocr: { engine: vision, http: { url: x } }", []string{"transcription.ocr.http", "not available yet"}},
		{"ocr unknown key", "ocr: { engine: vision, max_page_per_run: 3 }", []string{"transcription.ocr", `unknown key "max_page_per_run"`}},
		{"ocr bad tier", "ocr: { engine: vision, tier: huge }", []string{"transcription.ocr.tier", `"huge"`}},
		{"ocr model not an alias", "ocr: { engine: vision, model: gpt-x }", []string{"transcription.ocr.model", `"gpt-x" is not a models: alias`}},
		{"ocr model a transcriber", "ocr: { engine: vision, model: whisper }", []string{"transcription.ocr.model", "kind: transcriber", "kind: chat is needed"}},
		{"ocr model an embedder", "ocr: { engine: vision, model: embed }", []string{"transcription.ocr.model", "kind: embedder"}},
		{"ocr bad language", "ocr: { engine: vision, language: klingon }", []string{"transcription.ocr.language", `"klingon"`}},
		{"ocr negative per call", "ocr: { engine: vision, max_pages_per_call: -1 }", []string{"transcription.ocr.max_pages_per_call", ">= 0"}},
		{"ocr negative per run", "ocr: { engine: vision, max_pages_per_run: -1 }", []string{"transcription.ocr.max_pages_per_run"}},
		{"ocr negative min chars", "ocr: { engine: vision, ocr_min_chars_per_page: -1 }", []string{"transcription.ocr.ocr_min_chars_per_page"}},
		{"ocr negative tokens", "ocr: { engine: vision, max_tokens_per_page: -1 }", []string{"transcription.ocr.max_tokens_per_page"}},
		{"ocr per call above per run", "ocr: { engine: vision, max_pages_per_call: 20, max_pages_per_run: 10 }", []string{"max_pages_per_call (20)", "max_pages_per_run (10)"}},
		{"ocr rasterize empty", "ocr: { engine: vision, rasterize: { command: \"  \" } }", []string{"transcription.ocr.rasterize.command", "required"}},
		{"ocr rasterize without in", "ocr: { engine: vision, rasterize: { command: \"pdftoppm x\" } }", []string{"transcription.ocr.rasterize.command", "{in}"}},
		{"ocr rasterize unknown key", "ocr: { engine: vision, rasterize: { cmd: x } }", []string{"transcription.ocr.rasterize", `unknown key "cmd"`}},
		{"ocr blank prompt template", "ocr: { engine: vision, prompt_template: \"   \" }", []string{"transcription.ocr.prompt_template", "blank"}},
		// stt
		{"stt engine missing", "stt: { provider: whisper-local }", []string{"transcription.stt.engine is required", "openai-audio", "off"}},
		{"stt engine unknown", "stt: { engine: deepgram }", []string{"transcription.stt.engine", `"deepgram"`}},
		{"stt gemini engine", "stt: { engine: gemini-audio }", []string{"transcription.stt.engine", `"gemini-audio"`, "not available yet"}},
		{"stt command engine", "stt: { engine: command }", []string{"transcription.stt.engine", `"command"`, "not available yet"}},
		{"stt http engine", "stt: { engine: http }", []string{"transcription.stt.engine", `"http"`, "not available yet"}},
		{"stt command block", "stt: { engine: off, command: { template: x } }", []string{"transcription.stt.command", "not available yet"}},
		{"stt unknown key", "stt: { engine: off, chunk_minute: 3 }", []string{"transcription.stt", `unknown key "chunk_minute"`}},
		{"stt provider missing", "stt: { engine: openai-audio, model: whisper }", []string{"transcription.stt.provider is required"}},
		{"stt provider unknown", "stt: { engine: openai-audio, provider: nowhere, model: whisper }", []string{"transcription.stt.provider", `"nowhere"`}},
		{"stt provider not openai protocol", "stt: { engine: openai-audio, provider: anthropic, model: whisper-free }", []string{"transcription.stt.provider", `"anthropic"`, "openai"}},
		{"stt model missing", "stt: { engine: openai-audio, provider: whisper-local }", []string{"transcription.stt.model is required"}},
		{"stt model not an alias", "stt: { engine: openai-audio, provider: whisper-local, model: large-v3 }", []string{"transcription.stt.model", `"large-v3" is not a models: alias`}},
		{"stt model a chat alias", "stt: { engine: openai-audio, provider: whisper-local, model: talker }", []string{"transcription.stt.model", "kind: chat", "kind: transcriber is needed"}},
		{"stt model an untagged alias", "stt: { engine: openai-audio, provider: whisper-local, model: plain }", []string{"transcription.stt.model", "kind: transcriber"}},
		{"stt model on another provider", "stt: { engine: openai-audio, provider: whisper-local, model: whisper-elsewhere }", []string{"transcription.stt.model", `"openai"`, `"whisper-local"`}},
		{"stt bad language", "stt: { engine: off, language: xx }", []string{"transcription.stt.language", `"xx"`}},
		{"stt combined language", "stt: { engine: off, language: uk+ru }", []string{"transcription.stt.language", "one language"}},
		{"stt vocabulary too long", "stt: { engine: off, vocabulary: \"" + strings.Repeat("a", MaxTranscriptionVocabularyRunes+1) + "\" }", []string{"transcription.stt.vocabulary", "longer than"}},
		{"stt bad diarize", "stt: { engine: off, diarize: maybe }", []string{"transcription.stt.diarize", `"maybe"`}},
		{"stt negative per call", "stt: { engine: off, max_minutes_per_call: -1 }", []string{"transcription.stt.max_minutes_per_call"}},
		{"stt negative per run", "stt: { engine: off, max_minutes_per_run: -1 }", []string{"transcription.stt.max_minutes_per_run"}},
		{"stt negative chunk", "stt: { engine: off, chunk_minutes: -1 }", []string{"transcription.stt.chunk_minutes"}},
		{"stt negative overlap", "stt: { engine: off, chunk_overlap_seconds: -1 }", []string{"transcription.stt.chunk_overlap_seconds"}},
		{"stt per call above per run", "stt: { engine: off, max_minutes_per_call: 90, max_minutes_per_run: 60 }", []string{"max_minutes_per_call (90)", "max_minutes_per_run (60)"}},
		{"stt chunk above per call", "stt: { engine: off, chunk_minutes: 30, max_minutes_per_call: 20 }", []string{"chunk_minutes (30)", "max_minutes_per_call (20)"}},
		{"stt overlap not under chunk", "stt: { engine: off, chunk_minutes: 1, chunk_overlap_seconds: 60 }", []string{"chunk_overlap_seconds (60)", "chunk_minutes"}},
		{"stt media without in", "stt: { engine: off, media: { command: \"ffmpeg -f wav {out}\" } }", []string{"transcription.stt.media.command", "{in}"}},
		{"stt media without out", "stt: { engine: off, media: { command: \"ffmpeg -i {in} -f wav out.wav\" } }", []string{"transcription.stt.media.command", "{out}"}},
		{"stt negative timeout", "stt: { engine: off, timeout_s: -1 }", []string{"transcription.stt.timeout_s"}},
		{"stt model a pattern alias", "stt: { engine: openai-audio, provider: whisper-local, model: whisper-pattern }", []string{"transcription.stt.model", "must name its model"}},
		// block
		{"unknown sub-block", "tts: { engine: off }", []string{"transcription", `unknown key "tts"`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadTranscription(t, "transcription:\n  "+c.block+"\n")
			if err == nil {
				t.Fatal("the config loaded; want a refusal")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %q, want it to contain %q", err, w)
				}
			}
		})
	}
}

// TestTranscriptionConfig_AcceptsGoodValues — the values the refusals above sit
// next to load, normalised where the field has a canonical form.
func TestTranscriptionConfig_AcceptsGoodValues(t *testing.T) {
	cfg, err := loadTranscription(t, `
transcription:
  ocr:
    engine: vision
    tier: high
    model: talker
    language: " UK+En "
    max_pages_per_call: 10
    max_pages_per_run: 20
    ocr_min_chars_per_page: 5
    max_tokens_per_page: 500
    rasterize: { command: "pdftoppm -r 150 -png {in} {out_prefix}" }
    prompt_template: "Transcribe."
  stt:
    engine: openai-audio
    provider: whisper-local
    model: whisper-free
    language: EN
    vocabulary: "Kyiv\nOblenergo"
    diarize: off
    max_minutes_per_call: 30
    max_minutes_per_run: 60
    chunk_minutes: 5
    chunk_overlap_seconds: 3
    media: { command: "ffmpeg -i {in} -ac 1 -ar 16000 -f wav {out}" }
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Transcription.OCR.Language; got != "uk+en" {
		t.Errorf("ocr.language = %q, want uk+en", got)
	}
	if got := cfg.Transcription.STT.Language; got != "en" {
		t.Errorf("stt.language = %q, want en", got)
	}
	if got := cfg.Transcription.STT.Vocabulary; got != "Kyiv Oblenergo" {
		t.Errorf("stt.vocabulary = %q, want the newline folded to a space", got)
	}
	if got := cfg.Transcription.OCR.Rasterize.Command; !strings.Contains(got, "{in}") {
		t.Errorf("rasterize.command = %q, want it stored as written", got)
	}
	if !cfg.Transcription.OCREnabled() || !cfg.Transcription.STTEnabled() {
		t.Error("both engines configured, want both enabled")
	}

	// An engine set to off is configured and disabled.
	cfg, err = loadTranscription(t, "transcription:\n  ocr: { engine: off }\n  stt: { engine: off }\n")
	if err != nil {
		t.Fatalf("load off: %v", err)
	}
	if cfg.Transcription.OCREnabled() || cfg.Transcription.STTEnabled() {
		t.Error("engine: off must leave the engine disabled")
	}
}

// TestModelKind_TranscriberRefusedWhereChatIsNeeded — a transcriber alias is
// a check on where the alias may be written, through the same paths that
// already refuse a decision or embedder alias.
func TestModelKind_TranscriberRefusedWhereChatIsNeeded(t *testing.T) {
	for _, c := range []struct{ name, extra, want string }{
		{"agent model", "agents:\n  a: { model: whisper }\n", `agent "a": model: models.whisper is kind: transcriber`},
		{"tier candidate", "tiers:\n  middle: [{ model: whisper }]\n", "models.whisper is kind: transcriber"},
		{"embedder", "memory:\n  embedder: { model: whisper }\n", "memory.embedder.model: models.whisper is kind: transcriber"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadTranscription(t, c.extra)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
	if err := checkModelKind(map[string]ModelRef{"w": {Kind: ModelKindTranscriber}}, "w", ModelKindDecision); err == nil {
		t.Error("a transcriber alias passed a decision check")
	}
}

// TestModelKind_TranscriberIsAKnownKind — the alias loads; an unknown kind is
// still refused and the refusal lists the new kind among the valid ones.
func TestModelKind_TranscriberIsAKnownKind(t *testing.T) {
	if _, err := loadTranscription(t, ""); err != nil {
		t.Fatalf("a kind: transcriber alias must load: %v", err)
	}
	_, err := loadTranscription(t, "  odd: { provider: anthropic, model: m, kind: speech }\n")
	if err == nil || !strings.Contains(err.Error(), "transcriber") {
		t.Errorf("err = %v, want the refusal to list transcriber", err)
	}
}

// --- sanitisers that config owns (the transcribe package wraps them) ---

func TestNormalizeTranscriptionLanguage(t *testing.T) {
	for _, c := range []struct {
		in    string
		combo bool
		want  string
		ok    bool
	}{
		{"uk", false, "uk", true},
		{" RU ", false, "ru", true},
		{"uk+ru", true, "uk+ru", true},
		{"uk+ru", false, "", false},
		{"uk+uk", true, "", false},
		{"uk+ru+en+de", true, "", false},
		{"uk+", true, "", false},
		{"", false, "", false},
		{"xx", false, "", false},
		{"uk;rm -rf", true, "", false},
		{"uk\nru", true, "", false},
	} {
		got, err := NormalizeTranscriptionLanguage(c.in, c.combo)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("NormalizeTranscriptionLanguage(%q, %v) = (%q, %v), want (%q, ok=%v)", c.in, c.combo, got, err, c.want, c.ok)
		}
	}
}

func TestSanitizeTranscriptionVocabulary(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"Kyiv, Oblenergo", "Kyiv, Oblenergo", true},
		{"a\nb\r\nc\td", "a b c d", true},
		{"  spaced   out  ", "spaced out", true},
		{"bell\x07char", "bellchar", true},
		{"rtl\u202eoverride\u200bzw", "rtloverridezw", true},
		{"\x00\x1b[31m", "[31m", true},
		{"", "", true},
		{strings.Repeat("я", MaxTranscriptionVocabularyRunes), strings.Repeat("я", MaxTranscriptionVocabularyRunes), true},
		{strings.Repeat("я", MaxTranscriptionVocabularyRunes+1), "", false},
		{"bad\xffutf8", "", false},
	} {
		got, err := SanitizeTranscriptionVocabulary(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("SanitizeTranscriptionVocabulary(%q) = (%q, %v), want (%q, ok=%v)", c.in, got, err, c.want, c.ok)
		}
	}
}
