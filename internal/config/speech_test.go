package config

import (
	"reflect"
	"strings"
	"testing"
)

// speechBase declares what the speech block may point at: OpenAI-protocol
// speech servers, synthesizer aliases on them, and aliases of the other kinds.
const speechBase = `
defaults: { provider: anthropic, model: x }
providers:
  speech-local: { driver: openai, base_url: "http://speaches:8000/v1" }
  phonon-local: { driver: openai, base_url: "http://ptts:8880/v1" }
models:
  tts-uk: { provider: speech-local, model: speaches-ai/piper-uk_UA-ukrainian_tts-medium, kind: synthesizer }
  tts-en: { provider: speech-local, model: speaches-ai/Kokoro-82M-v1.0-ONNX, kind: synthesizer }
  phonon: { provider: phonon-local, model: phonon, kind: synthesizer }
  phonon-nc: { provider: phonon-local, model: "phonon:40m-multilingual", kind: synthesizer, license: noncommercial }
  tts-openai: { provider: openai, model: tts-1, kind: synthesizer }
  tts-free: { model: tts-1, kind: synthesizer }
  tts-pattern: { provider: speech-local, model_pattern: "kokoro-*", kind: synthesizer }
  tts-anthropic: { provider: anthropic, model: claude-x, kind: synthesizer }
  whisper: { provider: speech-local, model: large-v3, kind: transcriber }
  talker: { provider: anthropic, model: claude-x, kind: chat }
  plain: { provider: anthropic, model: claude-y }
  embed: { provider: openai, model: text-embedding-3-small, kind: embedder }
`

func loadSpeech(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	return LoadLayers(Layer{Name: "op", Data: []byte(speechBase + extra)})
}

// TestSpeechConfig_AbsentBlockIsOff — a config without the block loads with
// speech off, which is the state every existing deployment is in.
func TestSpeechConfig_AbsentBlockIsOff(t *testing.T) {
	cfg, err := loadSpeech(t, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Speech != nil || cfg.Speech.Enabled() {
		t.Errorf("speech = %+v, want nil and off", cfg.Speech)
	}
	if _, _, _, ok := cfg.Speech.VoiceFor("en"); ok {
		t.Error("VoiceFor on an absent block reported a voice")
	}
	if got := cfg.Speech.AllowedVoices("en"); got != nil {
		t.Errorf("AllowedVoices on an absent block = %v, want nil", got)
	}
}

// TestSpeechConfig_DefaultsAppliedAtLoad — every unset field comes back at its
// documented default, and the values written stay as written.
func TestSpeechConfig_DefaultsAppliedAtLoad(t *testing.T) {
	cfg, err := loadSpeech(t, `
speech:
  engine: openai-speech
  default_language: uk
  voices:
    uk: { model: tts-uk, voice: ukrainian_tts }
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := SpeechConfig{
		Engine: "openai-speech", DefaultLanguage: "uk",
		Voices: map[string]SpeechVoice{"uk": {Model: "tts-uk", Voice: "ukrainian_tts"}},
		Format: "wav", MaxCharsPerCall: 4000, MaxCharsPerRun: 100000, MaxSecondsPerRun: 3600, TimeoutSeconds: 600,
	}
	if cfg.Speech == nil || !reflect.DeepEqual(*cfg.Speech, want) {
		t.Errorf("speech = %+v, want %+v", cfg.Speech, want)
	}
	if !cfg.Speech.Enabled() {
		t.Error("an openai-speech engine is configured, want enabled")
	}
}

// TestSpeechConfig_DefaultFormatIsOggWithEncoder — the output defaults to Opus
// in .ogg when an encoder is configured (what Telegram voice notes need), else
// to wav, which needs no encoder.
func TestSpeechConfig_DefaultFormatIsOggWithEncoder(t *testing.T) {
	cfg, err := loadSpeech(t, `
speech:
  engine: openai-speech
  default_language: en
  voices: { en: { model: phonon, voice: Hadley } }
  encode: { command: "ffmpeg -i {in} -c:a libopus -b:a 32k {out}" }
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Speech.Format; got != SpeechFormatOgg {
		t.Errorf("format = %q, want ogg when encode is set", got)
	}
}

// TestSpeechConfig_ResolvedIsIdempotentAndCopies — the one place defaults are
// applied can be called again on its own output without changing it, and it
// never writes into, or shares maps and slices with, its input.
func TestSpeechConfig_ResolvedIsIdempotentAndCopies(t *testing.T) {
	in := &SpeechConfig{
		Engine: "off",
		Voices: map[string]SpeechVoice{"en": {Model: "phonon", Voice: "Hadley", Alternates: []string{"Zoey"}}},
		Encode: &HelperCommand{Command: "ffmpeg -i {in} {out}"},
	}
	once := in.Resolved()
	twice := once.Resolved()
	if !reflect.DeepEqual(once, twice) {
		t.Errorf("Resolved twice = %+v / %+v, want equal", once, twice)
	}
	if in.Format != "" || in.MaxCharsPerRun != 0 {
		t.Error("Resolved wrote into its input; it must return a copy")
	}
	once.Voices["en"].Alternates[0] = "changed"
	once.Encode.Command = "changed"
	if in.Voices["en"].Alternates[0] != "Zoey" || in.Encode.Command != "ffmpeg -i {in} {out}" {
		t.Error("Resolved shares the voices or encode with its input")
	}
	var nilBlock *SpeechConfig
	if nilBlock.Resolved() != nil {
		t.Error("Resolved of an absent block must stay nil (off)")
	}
}

// TestSpeechConfig_LowerPerRunCapLowersDefaultPerCall — an operator who writes
// only a per-run cap below the per-call default is not refused for a value
// they never wrote.
func TestSpeechConfig_LowerPerRunCapLowersDefaultPerCall(t *testing.T) {
	cfg, err := loadSpeech(t, "speech: { engine: off, max_chars_per_run: 1000 }\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Speech.MaxCharsPerCall; got != 1000 {
		t.Errorf("max_chars_per_call = %d, want 1000 (the per-run cap)", got)
	}
}

// TestSpeechConfig_RefusesBadValues — each field's refusal, naming the place,
// so `loomcycle validate` points at the line to fix.
func TestSpeechConfig_RefusesBadValues(t *testing.T) {
	const ok = "engine: openai-speech, default_language: en, voices: { en: { model: phonon, voice: Hadley } }"
	for _, c := range []struct {
		name, block string
		want        []string
	}{
		{"engine missing", "{ default_language: en }", []string{"speech.engine is required", "openai-speech", "off"}},
		{"engine unknown", "{ engine: gemini-tts }", []string{"speech.engine", `"gemini-tts"`, "not one of"}},
		{"unknown key", "{ engine: off, max_char_per_run: 3 }", []string{"speech", `unknown key "max_char_per_run"`}},
		{"no voices", "{ engine: openai-speech, default_language: en }", []string{"speech.voices", "at least one language"}},
		{"default language missing", "{ engine: openai-speech, voices: { en: { model: phonon, voice: Hadley } } }", []string{"speech.default_language is required", "en"}},
		{"default language without a voice", "{ engine: openai-speech, default_language: uk, voices: { en: { model: phonon, voice: Hadley } } }", []string{"speech.default_language", `"uk" has no entry in speech.voices`}},
		{"default language bad code", "{ engine: off, default_language: klingon }", []string{"speech.default_language", `"klingon"`}},
		{"default language combined", "{ engine: off, default_language: uk+en }", []string{"speech.default_language", "one language"}},
		{"voice key not a language", "{ engine: off, voices: { xx: { model: phonon, voice: Hadley } } }", []string{"speech.voices.xx", `"xx"`}},
		{"voice key twice", "{ engine: off, voices: { en: { model: phonon, voice: Hadley }, EN: { model: phonon, voice: Zoey } } }", []string{"speech.voices.en", `"en" is configured twice`}},
		{"voice unknown key", "{ engine: off, voices: { en: { model: phonon, voice: Hadley, speed: 2 } } }", []string{"speech.voices.en", `unknown key "speed"`}},
		{"voice model missing", "{ engine: off, voices: { en: { voice: Hadley } } }", []string{"speech.voices.en.model is required", "kind: synthesizer"}},
		{"voice model not an alias", "{ engine: off, voices: { en: { model: tts-1, voice: alloy } } }", []string{"speech.voices.en.model", `"tts-1" is not a models: alias`}},
		{"voice model a chat alias", "{ engine: off, voices: { en: { model: talker, voice: x } } }", []string{"speech.voices.en.model", "kind: chat", "kind: synthesizer is needed"}},
		{"voice model untagged", "{ engine: off, voices: { en: { model: plain, voice: x } } }", []string{"speech.voices.en.model", "untagged (chat)", "kind: synthesizer"}},
		{"voice model a transcriber", "{ engine: off, voices: { en: { model: whisper, voice: x } } }", []string{"speech.voices.en.model", "kind: transcriber", "kind: synthesizer is needed"}},
		{"voice model an embedder", "{ engine: off, voices: { en: { model: embed, voice: x } } }", []string{"speech.voices.en.model", "kind: embedder"}},
		{"voice model a pattern", "{ engine: off, voices: { en: { model: tts-pattern, voice: x } } }", []string{"speech.voices.en.model", "must name its model"}},
		{"voice model no provider", "{ engine: off, voices: { en: { model: tts-free, voice: alloy } } }", []string{"speech.voices.en.model", "must name its provider"}},
		{"voice model not openai driver", "{ engine: off, voices: { en: { model: tts-anthropic, voice: x } } }", []string{"speech.voices.en.model", `"anthropic"`, "openai driver"}},
		{"voice missing", "{ engine: off, voices: { en: { model: phonon } } }", []string{"speech.voices.en.voice is required"}},
		{"voice blank", "{ engine: off, voices: { en: { model: phonon, voice: \"  \" } } }", []string{"speech.voices.en.voice is required"}},
		{"voice padded", "{ engine: off, voices: { en: { model: phonon, voice: \" Hadley\" } } }", []string{"speech.voices.en.voice", "spaces"}},
		{"voice control char", "{ engine: off, voices: { en: { model: phonon, voice: \"Had\\u202eley\" } } }", []string{"speech.voices.en.voice", "control or formatting"}},
		{"alternate duplicates voice", "{ engine: off, voices: { en: { model: phonon, voice: Hadley, alternates: [Zoey, Hadley] } } }", []string{"speech.voices.en.alternates[1]", `"Hadley" is listed twice`}},
		{"alternate twice", "{ engine: off, voices: { en: { model: phonon, voice: Hadley, alternates: [Zoey, Zoey] } } }", []string{"speech.voices.en.alternates[1]", `"Zoey" is listed twice`}},
		{"alternate empty", "{ engine: off, voices: { en: { model: phonon, voice: Hadley, alternates: [\"\"] } } }", []string{"speech.voices.en.alternates[0] is required"}},
		{"format unknown", "{ engine: off, format: opus }", []string{"speech.format", `"opus"`, "ogg, mp3, wav"}},
		{"ogg without encode", "{ " + ok + ", format: ogg }", []string{"speech.format", "ogg", "speech.encode"}},
		{"mp3 without encode", "{ " + ok + ", format: mp3 }", []string{"speech.format", "mp3", "speech.encode"}},
		{"encode empty", "{ engine: off, encode: { command: \"  \" } }", []string{"speech.encode.command", "required"}},
		{"encode without in", "{ engine: off, encode: { command: \"ffmpeg {out}\" } }", []string{"speech.encode.command", "{in}"}},
		{"encode without out", "{ engine: off, encode: { command: \"ffmpeg -i {in} out.ogg\" } }", []string{"speech.encode.command", "{out}"}},
		{"encode unknown key", "{ engine: off, encode: { cmd: x } }", []string{"speech.encode", `unknown key "cmd"`}},
		{"per call above ceiling", "{ engine: off, max_chars_per_call: 4097 }", []string{"speech.max_chars_per_call (4097)", "4096"}},
		{"per call above per run", "{ engine: off, max_chars_per_call: 3000, max_chars_per_run: 2000 }", []string{"max_chars_per_call (3000)", "max_chars_per_run (2000)"}},
		{"negative per call", "{ engine: off, max_chars_per_call: -1 }", []string{"speech.max_chars_per_call", ">= 0"}},
		{"negative per run", "{ engine: off, max_chars_per_run: -1 }", []string{"speech.max_chars_per_run", ">= 0"}},
		{"negative seconds", "{ engine: off, max_seconds_per_run: -1 }", []string{"speech.max_seconds_per_run", ">= 0"}},
		{"negative timeout", "{ engine: off, timeout_s: -1 }", []string{"speech.timeout_s", ">= 0"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadSpeech(t, "speech: "+c.block+"\n")
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

// TestSpeechConfig_AcceptsGoodValues — the values the refusals above sit next
// to load, normalised where the field has a canonical form, and the accessors
// read the closed voice list back.
func TestSpeechConfig_AcceptsGoodValues(t *testing.T) {
	cfg, err := loadSpeech(t, `
speech:
  engine: openai-speech
  default_language: " UK "
  voices:
    UK: { model: tts-uk, voice: ukrainian_tts }
    en: { model: phonon, voice: Hadley, alternates: [Zoey, Easton, Freya] }
    fr: { model: tts-openai, voice: alloy }
  format: mp3
  encode: { command: "ffmpeg -i {in} -b:a 64k {out}" }
  max_chars_per_call: 4096
  max_chars_per_run: 50000
  max_seconds_per_run: 600
  timeout_s: 120
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := cfg.Speech
	if s.DefaultLanguage != "uk" {
		t.Errorf("default_language = %q, want uk", s.DefaultLanguage)
	}
	if s.Format != "mp3" || s.MaxCharsPerCall != 4096 || s.MaxCharsPerRun != 50000 || s.MaxSecondsPerRun != 600 || s.TimeoutSeconds != 120 {
		t.Errorf("speech = %+v, want the written values", s)
	}
	model, voice, alts, ok := s.VoiceFor("uk")
	if !ok || model != "tts-uk" || voice != "ukrainian_tts" || len(alts) != 0 {
		t.Errorf("VoiceFor(uk) = (%q, %q, %v, %v), want the upper-case key normalised", model, voice, alts, ok)
	}
	model, voice, alts, ok = s.VoiceFor(" EN ")
	if !ok || model != "phonon" || voice != "Hadley" || !reflect.DeepEqual(alts, []string{"Zoey", "Easton", "Freya"}) {
		t.Errorf("VoiceFor(EN) = (%q, %q, %v, %v)", model, voice, alts, ok)
	}
	alts[0] = "changed"
	if got := s.AllowedVoices("en"); !reflect.DeepEqual(got, []string{"Hadley", "Zoey", "Easton", "Freya"}) {
		t.Errorf("AllowedVoices(en) = %v, want the default first, then the alternates, unchanged by a caller", got)
	}
	if _, _, _, ok := s.VoiceFor("de"); ok {
		t.Error("VoiceFor(de) found a voice for an unconfigured language")
	}
	if s.AllowedVoices("de") != nil {
		t.Error("AllowedVoices(de) must be nil for an unconfigured language")
	}

	// An engine set to off is configured and disabled, with no voice needed.
	cfg, err = loadSpeech(t, "speech: { engine: off }\n")
	if err != nil {
		t.Fatalf("load off: %v", err)
	}
	if cfg.Speech == nil || cfg.Speech.Enabled() {
		t.Error("engine: off must load and leave speech disabled")
	}
}

// TestSpeechConfig_NoncommercialVoiceWarns — a voice on a noncommercial alias
// loads (nothing enforces the licence) but leaves a warning naming it.
func TestSpeechConfig_NoncommercialVoiceWarns(t *testing.T) {
	cfg, err := loadSpeech(t, `
speech:
  engine: openai-speech
  default_language: fr
  voices: { fr: { model: phonon-nc, voice: Juliette } }
`)
	if err != nil {
		t.Fatalf("a noncommercial voice must load: %v", err)
	}
	found := false
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "speech.voices.fr") && strings.Contains(w, "models.phonon-nc") && strings.Contains(w, "noncommercial") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %q, want one naming speech.voices.fr and models.phonon-nc", cfg.Warnings)
	}
	if got := cfg.Models["phonon-nc"].License; got != ModelLicenseNoncommercial {
		t.Errorf("models.phonon-nc.license = %q, want noncommercial", got)
	}
}

// TestModelRef_LicenseValues — the licence tag accepts the documented values
// and refuses anything else, naming the alias.
func TestModelRef_LicenseValues(t *testing.T) {
	if _, err := loadSpeech(t, "  com: { provider: speech-local, model: m, kind: synthesizer, license: commercial }\n"); err != nil {
		t.Errorf("license: commercial must load: %v", err)
	}
	_, err := loadSpeech(t, "  odd: { provider: speech-local, model: m, kind: synthesizer, license: cc-by-nc }\n")
	if err == nil || !strings.Contains(err.Error(), "models.odd") || !strings.Contains(err.Error(), `"cc-by-nc"`) {
		t.Errorf("err = %v, want a refusal naming models.odd and the value", err)
	}
}

// TestModelKind_SynthesizerRefusedWhereAnotherKindIsNeeded — a synthesizer
// alias is refused everywhere a chat, decision, embedder or transcriber model
// is written, through the paths that already check the kind.
func TestModelKind_SynthesizerRefusedWhereAnotherKindIsNeeded(t *testing.T) {
	for _, c := range []struct{ name, extra, want string }{
		{"agent model", "agents:\n  a: { model: phonon }\n", `agent "a": model: models.phonon is kind: synthesizer`},
		{"tier candidate", "tiers:\n  middle: [{ model: phonon }]\n", "models.phonon is kind: synthesizer"},
		{"embedder", "memory:\n  embedder: { model: phonon }\n", "memory.embedder.model: models.phonon is kind: synthesizer"},
		{"ocr model", "transcription:\n  ocr: { engine: vision, model: phonon }\n", "transcription.ocr.model: models.phonon is kind: synthesizer"},
		{"stt model", "transcription:\n  stt: { engine: openai-audio, provider: speech-local, model: phonon }\n", "transcription.stt.model: models.phonon is kind: synthesizer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadSpeech(t, c.extra)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
	if err := checkModelKind(map[string]ModelRef{"s": {Kind: ModelKindSynthesizer}}, "s", ModelKindDecision); err == nil {
		t.Error("a synthesizer alias passed a decision check")
	}
}

// TestModelKind_SynthesizerIsAKnownKind — the alias loads; an unknown kind is
// still refused and the refusal lists the new kind among the valid ones.
func TestModelKind_SynthesizerIsAKnownKind(t *testing.T) {
	if _, err := loadSpeech(t, ""); err != nil {
		t.Fatalf("a kind: synthesizer alias must load: %v", err)
	}
	_, err := loadSpeech(t, "  odd: { provider: anthropic, model: m, kind: tts }\n")
	if err == nil || !strings.Contains(err.Error(), "synthesizer") {
		t.Errorf("err = %v, want the refusal to list synthesizer", err)
	}
}
