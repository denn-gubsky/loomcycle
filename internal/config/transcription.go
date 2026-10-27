package config

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// transcription.go — the top-level `transcription:` block and the per-agent
// `transcription:` block: OCR for scans and images, speech-to-text for audio
// and video.
//
// This is the substrate only. The block is typed and validated here so a bad
// value fails `loomcycle validate`, but nothing in the runtime calls an engine
// yet; engines arrive in later phases. Absent, both engines are off.
//
// Engines, providers, models and helper commands are operator configuration.
// What a model may choose later (an OCR mode, a language, a vocabulary hint)
// is sanitised through NormalizeTranscriptionLanguage and
// SanitizeTranscriptionVocabulary, which live here so load-time validation and
// the runtime use one list and one rule.

// TranscriptionConfig is the top-level `transcription:` block. A nil OCR or STT
// is that engine off; so is `engine: off`.
type TranscriptionConfig struct {
	OCR *OCRConfig `yaml:"ocr,omitempty"`
	STT *STTConfig `yaml:"stt,omitempty"`
}

// The engines the block accepts. command and http (both), and gemini-audio
// (STT), are designed but not built: they are refused by name so an operator
// who writes one learns it is coming rather than that it is a typo.
const (
	OCREngineVision      = "vision"
	OCREngineOff         = "off"
	STTEngineOpenAIAudio = "openai-audio"
	STTEngineOff         = "off"
)

var (
	ocrEnginesNotYet = []string{"command", "http"}
	sttEnginesNotYet = []string{"gemini-audio", "command", "http"}
)

// OCRConfig is `transcription.ocr`. Zero numbers mean "the default"; defaults
// are applied by TranscriptionConfig.Resolved, the one place they live.
type OCRConfig struct {
	// Engine is required when the block is written: vision | off.
	Engine string `yaml:"engine"`
	// Tier resolves the vision model through the tier policy, exactly as the
	// document describe-images pass does. Default middle. Ignored when Model is
	// set.
	Tier string `yaml:"tier"`
	// Model optionally pins a models: alias (kind: chat, or untagged). Whether
	// the model can read images is a property of the provider driver and is only
	// known when the engine is built, so it is not checked at load.
	Model string `yaml:"model"`
	// Language is the hint passed to the engine; codes joined by "+" (uk+ru).
	Language           string `yaml:"language"`
	MaxPagesPerCall    int    `yaml:"max_pages_per_call"`
	MaxPagesPerRun     int    `yaml:"max_pages_per_run"`
	OCRMinCharsPerPage int    `yaml:"ocr_min_chars_per_page"`
	MaxTokensPerPage   int    `yaml:"max_tokens_per_page"`
	// Rasterize is the helper that turns PDF pages into images for an
	// image-only engine. Stored and validated here; run by the helper runner.
	Rasterize *HelperCommand `yaml:"rasterize,omitempty"`
	// PromptTemplate replaces the fixed OCR instruction. Operator-only: nothing a
	// model sends ever reaches it.
	PromptTemplate string `yaml:"prompt_template"`
}

// STTConfig is `transcription.stt`.
type STTConfig struct {
	// Engine is required when the block is written: openai-audio | off.
	Engine string `yaml:"engine"`
	// Provider serves POST /v1/audio/transcriptions: a provider on the openai
	// driver (OpenAI, a self-hosted whisper server, Groq…). Required for
	// openai-audio.
	Provider string `yaml:"provider"`
	// Model is a models: alias tagged kind: transcriber. Required for
	// openai-audio.
	Model string `yaml:"model"`
	// Language is ONE code: speech engines take a single language.
	Language string `yaml:"language"`
	// Vocabulary is a prompt hint (names, terms), folded to one line.
	Vocabulary          string `yaml:"vocabulary"`
	Diarize             string `yaml:"diarize"`
	MaxMinutesPerCall   int    `yaml:"max_minutes_per_call"`
	MaxMinutesPerRun    int    `yaml:"max_minutes_per_run"`
	ChunkMinutes        int    `yaml:"chunk_minutes"`
	ChunkOverlapSeconds int    `yaml:"chunk_overlap_seconds"`
	// TimeoutSeconds bounds one engine request and one media conversion. A
	// whisper server on a CPU host can take several minutes for a ten-minute
	// chunk and sends nothing until it is done, so this is long by default.
	TimeoutSeconds int `yaml:"timeout_s"`
	// Media is the converter (ffmpeg or equivalent) that turns a recording
	// into a mono WAV the runtime can cut: `{in}` is the recording, `{out}`
	// the WAV it must write.
	Media *HelperCommand `yaml:"media,omitempty"`
}

// HelperCommand is an operator-declared host command template. `{in}` is the
// input file; the runner substitutes it and the other placeholders and runs the
// command without a shell.
type HelperCommand struct {
	Command string `yaml:"command"`
}

// Defaults. The OCR page caps match the Anthropic per-request ceiling (100) and
// a run budget of three such calls; the minute caps bound one meeting and one
// working session; ten-minute chunks with a two-second overlap keep each
// engine request well under the 25 MB OpenAI audio cap at 16 kHz mono.
const (
	TranscriptionDefaultOCRTier          = "middle"
	TranscriptionDefaultOCRLanguage      = "uk+ru"
	TranscriptionDefaultMaxPagesPerCall  = 100
	TranscriptionDefaultMaxPagesPerRun   = 300
	TranscriptionDefaultMinCharsPerPage  = 40
	TranscriptionDefaultMaxTokensPerPage = 2000

	TranscriptionDefaultSTTLanguage       = "uk"
	TranscriptionDefaultDiarize           = "auto"
	TranscriptionDefaultMaxMinutesPerCall = 120
	TranscriptionDefaultMaxMinutesPerRun  = 300
	TranscriptionDefaultChunkMinutes      = 10
	TranscriptionDefaultChunkOverlapSecs  = 2
	TranscriptionDefaultSTTTimeoutSecs    = 1800
)

// OCREnabled reports whether an OCR engine is configured and not off.
func (t TranscriptionConfig) OCREnabled() bool {
	return t.OCR != nil && t.OCR.Engine != "" && t.OCR.Engine != OCREngineOff
}

// STTEnabled reports whether an STT engine is configured and not off.
func (t TranscriptionConfig) STTEnabled() bool {
	return t.STT != nil && t.STT.Engine != "" && t.STT.Engine != STTEngineOff
}

// Resolved returns a copy with every unset field at its default. It is the one
// place defaults are applied: load stores its result, and a caller holding a
// block built in code gets the same values by calling it. Idempotent.
//
// A per-call default never exceeds the per-run cap the operator wrote (and the
// chunk default never exceeds the per-call cap), so writing only a low per-run
// cap is not refused for a per-call value nobody wrote.
func (t TranscriptionConfig) Resolved() TranscriptionConfig {
	out := TranscriptionConfig{}
	if t.OCR != nil {
		o := *t.OCR
		if o.Rasterize != nil {
			r := *o.Rasterize
			o.Rasterize = &r
		}
		o.Tier = orDefault(o.Tier, TranscriptionDefaultOCRTier)
		o.Language = orDefault(o.Language, TranscriptionDefaultOCRLanguage)
		o.MaxPagesPerRun = orDefaultInt(o.MaxPagesPerRun, TranscriptionDefaultMaxPagesPerRun)
		o.MaxPagesPerCall = orDefaultInt(o.MaxPagesPerCall, min(TranscriptionDefaultMaxPagesPerCall, o.MaxPagesPerRun))
		o.OCRMinCharsPerPage = orDefaultInt(o.OCRMinCharsPerPage, TranscriptionDefaultMinCharsPerPage)
		o.MaxTokensPerPage = orDefaultInt(o.MaxTokensPerPage, TranscriptionDefaultMaxTokensPerPage)
		out.OCR = &o
	}
	if t.STT != nil {
		s := *t.STT
		if s.Media != nil {
			m := *s.Media
			s.Media = &m
		}
		s.Language = orDefault(s.Language, TranscriptionDefaultSTTLanguage)
		s.Diarize = orDefault(s.Diarize, TranscriptionDefaultDiarize)
		s.MaxMinutesPerRun = orDefaultInt(s.MaxMinutesPerRun, TranscriptionDefaultMaxMinutesPerRun)
		s.MaxMinutesPerCall = orDefaultInt(s.MaxMinutesPerCall, min(TranscriptionDefaultMaxMinutesPerCall, s.MaxMinutesPerRun))
		s.ChunkMinutes = orDefaultInt(s.ChunkMinutes, min(TranscriptionDefaultChunkMinutes, s.MaxMinutesPerCall))
		s.ChunkOverlapSeconds = orDefaultInt(s.ChunkOverlapSeconds, TranscriptionDefaultChunkOverlapSecs)
		s.TimeoutSeconds = orDefaultInt(s.TimeoutSeconds, TranscriptionDefaultSTTTimeoutSecs)
		out.STT = &s
	}
	return out
}

// orDefault / orDefaultInt fill an unset value. A negative int is left as
// written so validation can refuse it rather than have it silently replaced.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func orDefaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// resolveTranscription validates the block, then stores the resolved form on
// c. Runs after models: and providers: are validated (it names both) and
// before the agents loop (whose lower-only check reads the resolved caps).
func resolveTranscription(c *Config) error {
	t := c.Transcription
	if t.OCR != nil {
		if err := c.validateOCR(t.OCR); err != nil {
			return err
		}
	}
	if t.STT != nil {
		if err := c.validateSTT(t.STT); err != nil {
			return err
		}
	}
	c.Transcription = t.Resolved()
	// Cross-field bounds are judged on the resolved values so a default is
	// held to the same rule as a written value.
	if o := c.Transcription.OCR; o != nil && o.MaxPagesPerCall > o.MaxPagesPerRun {
		return fmt.Errorf("transcription.ocr: max_pages_per_call (%d) is above max_pages_per_run (%d)", o.MaxPagesPerCall, o.MaxPagesPerRun)
	}
	if s := c.Transcription.STT; s != nil {
		if s.MaxMinutesPerCall > s.MaxMinutesPerRun {
			return fmt.Errorf("transcription.stt: max_minutes_per_call (%d) is above max_minutes_per_run (%d)", s.MaxMinutesPerCall, s.MaxMinutesPerRun)
		}
		if s.ChunkMinutes > s.MaxMinutesPerCall {
			return fmt.Errorf("transcription.stt: chunk_minutes (%d) is above max_minutes_per_call (%d)", s.ChunkMinutes, s.MaxMinutesPerCall)
		}
		if s.ChunkOverlapSeconds >= s.ChunkMinutes*60 {
			return fmt.Errorf("transcription.stt: chunk_overlap_seconds (%d) must be shorter than chunk_minutes (%d min)", s.ChunkOverlapSeconds, s.ChunkMinutes)
		}
	}
	return nil
}

func (c *Config) validateOCR(o *OCRConfig) error {
	const place = "transcription.ocr"
	if err := checkEngine(place, o.Engine, []string{OCREngineVision, OCREngineOff}, ocrEnginesNotYet); err != nil {
		return err
	}
	if o.Tier != "" && !validTierNames[o.Tier] {
		return fmt.Errorf("%s.tier: %q is not one of low, middle, high", place, o.Tier)
	}
	if o.Model != "" {
		if _, ok := c.Models[o.Model]; !ok {
			return fmt.Errorf("%s.model: %q is not a models: alias (the OCR model has no provider of its own, so it must be one)", place, o.Model)
		}
		if err := c.requireModelKind(place+".model", o.Model, ModelKindChat); err != nil {
			return err
		}
	}
	if o.Language != "" {
		lang, err := NormalizeTranscriptionLanguage(o.Language, true)
		if err != nil {
			return fmt.Errorf("%s.language: %w", place, err)
		}
		o.Language = lang
	}
	for _, f := range []struct {
		name string
		v    int
	}{
		{"max_pages_per_call", o.MaxPagesPerCall},
		{"max_pages_per_run", o.MaxPagesPerRun},
		{"ocr_min_chars_per_page", o.OCRMinCharsPerPage},
		{"max_tokens_per_page", o.MaxTokensPerPage},
	} {
		if f.v < 0 {
			return fmt.Errorf("%s.%s must be >= 0 (0 = the default), got %d", place, f.name, f.v)
		}
	}
	if o.Rasterize != nil {
		if err := o.Rasterize.validate(place + ".rasterize"); err != nil {
			return err
		}
	}
	if o.PromptTemplate != "" && strings.TrimSpace(o.PromptTemplate) == "" {
		return fmt.Errorf("%s.prompt_template is blank: remove it to use the built-in instruction", place)
	}
	return nil
}

func (c *Config) validateSTT(s *STTConfig) error {
	const place = "transcription.stt"
	if err := checkEngine(place, s.Engine, []string{STTEngineOpenAIAudio, STTEngineOff}, sttEnginesNotYet); err != nil {
		return err
	}
	if s.Engine == STTEngineOpenAIAudio {
		if s.Provider == "" {
			return fmt.Errorf("%s.provider is required for engine %s (a provider on the openai driver whose base_url serves /v1/audio/transcriptions)", place, s.Engine)
		}
		if s.Model == "" {
			return fmt.Errorf("%s.model is required for engine %s (a models: alias with kind: %s)", place, s.Engine, ModelKindTranscriber)
		}
	}
	if s.Provider != "" {
		if !c.knownProviderIDs()[s.Provider] {
			return fmt.Errorf("%s.provider: %q is not a known provider", place, s.Provider)
		}
		// Only the openai driver speaks the audio transcription endpoint. A
		// built-in id with no providers: entry is its own driver.
		driver := s.Provider
		if pc, ok := c.Providers[s.Provider]; ok {
			driver = pc.Driver
		}
		if driver != "openai" {
			return fmt.Errorf("%s.provider: %q uses the %q driver; the audio transcription endpoint is served by providers on the openai driver", place, s.Provider, driver)
		}
	}
	if s.Model != "" {
		ref, ok := c.Models[s.Model]
		if !ok {
			return fmt.Errorf("%s.model: %q is not a models: alias (it must be one tagged kind: %s)", place, s.Model, ModelKindTranscriber)
		}
		// Stricter than requireModelKind: there are no untagged transcriber
		// aliases from before the kind existed, so the tag is required.
		if ref.Kind != ModelKindTranscriber {
			kind := ref.Kind
			if kind == "" {
				kind = "untagged (chat)"
			}
			return fmt.Errorf("%s.model: models.%s is kind: %s, and a model of kind: %s is needed here", place, s.Model, kind, ModelKindTranscriber)
		}
		if ref.Model == "" {
			return fmt.Errorf("%s.model: models.%s must name its model with model:; a transcriber is not chosen from a provider's catalog", place, s.Model)
		}
		if ref.Provider != "" && s.Provider != "" && ref.Provider != s.Provider {
			return fmt.Errorf("%s.model: models.%s is served by provider %q, not by %s.provider %q", place, s.Model, ref.Provider, place, s.Provider)
		}
	}
	if s.Language != "" {
		lang, err := NormalizeTranscriptionLanguage(s.Language, false)
		if err != nil {
			return fmt.Errorf("%s.language: %w", place, err)
		}
		s.Language = lang
	}
	if s.Vocabulary != "" {
		v, err := SanitizeTranscriptionVocabulary(s.Vocabulary)
		if err != nil {
			return fmt.Errorf("%s.vocabulary: %w", place, err)
		}
		s.Vocabulary = v
	}
	switch s.Diarize {
	case "", "auto", "on", "off":
	default:
		return fmt.Errorf("%s.diarize: %q is not one of auto, on, off", place, s.Diarize)
	}
	for _, f := range []struct {
		name string
		v    int
	}{
		{"max_minutes_per_call", s.MaxMinutesPerCall},
		{"max_minutes_per_run", s.MaxMinutesPerRun},
		{"chunk_minutes", s.ChunkMinutes},
		{"chunk_overlap_seconds", s.ChunkOverlapSeconds},
		{"timeout_s", s.TimeoutSeconds},
	} {
		if f.v < 0 {
			return fmt.Errorf("%s.%s must be >= 0 (0 = the default), got %d", place, f.name, f.v)
		}
	}
	if s.Media != nil {
		if err := s.Media.validate(place + ".media"); err != nil {
			return err
		}
		// The runtime cuts the converted WAV itself, so the converter must
		// write to the file it is handed.
		if !strings.Contains(s.Media.Command, "{out}") {
			return fmt.Errorf("%s.media.command must reference {out}, the WAV file the converter writes (e.g. ffmpeg -i {in} -ac 1 -ar 16000 -f wav {out})", place)
		}
	}
	return nil
}

// checkEngine refuses a missing engine, a second-line engine (by name, as not
// available yet), and anything else unknown.
func checkEngine(place, engine string, valid, notYet []string) error {
	switch {
	case engine == "":
		return fmt.Errorf("%s.engine is required (one of %s)", place, strings.Join(valid, ", "))
	case contains(valid, engine):
		return nil
	case contains(notYet, engine):
		return fmt.Errorf("%s.engine: %q is not available yet (available: %s)", place, engine, strings.Join(valid, ", "))
	default:
		return fmt.Errorf("%s.engine: %q is not one of %s", place, engine, strings.Join(valid, ", "))
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func (h *HelperCommand) validate(place string) error {
	if strings.TrimSpace(h.Command) == "" {
		return fmt.Errorf("%s.command is required when %s is set", place, place)
	}
	if !strings.Contains(h.Command, "{in}") {
		return fmt.Errorf("%s.command must reference {in}, the input file", place)
	}
	return nil
}

// --- strict decoding: the rest of the config decodes leniently, so a typo in
// a cap (`max_page_per_run`) would load as "the default" without a word. These
// blocks check their own keys, the way decision: does. ---

func (t *TranscriptionConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "transcription", nil, reflect.TypeOf(*t)); err != nil {
		return err
	}
	type plain TranscriptionConfig
	return n.Decode((*plain)(t))
}

func (o *OCRConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "transcription.ocr", ocrEnginesNotYet, reflect.TypeOf(*o)); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "rasterize"), "transcription.ocr.rasterize", nil, reflect.TypeOf(HelperCommand{})); err != nil {
		return err
	}
	type plain OCRConfig
	return n.Decode((*plain)(o))
}

func (s *STTConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "transcription.stt", sttEnginesNotYet, reflect.TypeOf(*s)); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "media"), "transcription.stt.media", nil, reflect.TypeOf(HelperCommand{})); err != nil {
		return err
	}
	type plain STTConfig
	return n.Decode((*plain)(s))
}

// strictKeys refuses any key of mapping node n that struct type t does not
// declare in its yaml tags. A key named in notYet is a designed-but-unbuilt
// sub-block and is refused as such. A nil or non-mapping node passes (the
// decode that follows reports a wrong shape).
func strictKeys(n *yaml.Node, place string, notYet []string, t reflect.Type) error {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	known := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		known[name] = true
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		if known[key] {
			continue
		}
		if contains(notYet, key) {
			return fmt.Errorf("%s.%s: the %s engine is not available yet", place, key, key)
		}
		names := make([]string, 0, len(known))
		for k := range known {
			names = append(names, k)
		}
		sort.Strings(names)
		return fmt.Errorf("%s: unknown key %q (the block takes: %s)", place, key, strings.Join(names, ", "))
	}
	return nil
}

// valueOf returns the value node of key in mapping node n, or nil.
func valueOf(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// --- languages and vocabulary ---

// transcriptionLanguages is the closed list of language codes a hint may name
// (ISO 639-1). Closed because the value is interpolated into an engine prompt
// and a helper command line: a list is the only sanitiser that is obviously
// complete. Extend it by adding codes here.
var transcriptionLanguages = map[string]bool{
	"uk": true, "ru": true, "en": true, "be": true, "pl": true, "de": true,
	"fr": true, "es": true, "it": true, "pt": true, "nl": true, "cs": true,
	"sk": true, "bg": true, "sr": true, "hr": true, "sl": true, "ro": true,
	"hu": true, "lt": true, "lv": true, "et": true, "fi": true, "sv": true,
	"no": true, "da": true, "el": true, "tr": true, "ka": true, "hy": true,
	"kk": true, "az": true, "uz": true, "ar": true, "he": true, "fa": true,
	"hi": true, "zh": true, "ja": true, "ko": true, "vi": true, "th": true,
	"id": true,
}

// maxTranscriptionLanguages bounds a combined hint (uk+ru+en).
const maxTranscriptionLanguages = 3

// NormalizeTranscriptionLanguage returns code lower-cased and trimmed when it
// is on the closed list. With combo, up to three distinct codes joined by "+"
// are accepted (an OCR hint for a mixed-script page); without it, exactly one
// (speech engines take a single language).
func NormalizeTranscriptionLanguage(code string, combo bool) (string, error) {
	s := strings.ToLower(strings.TrimSpace(code))
	if s == "" {
		return "", errors.New("language is empty")
	}
	parts := strings.Split(s, "+")
	if !combo && len(parts) > 1 {
		return "", fmt.Errorf("%q names more than one language; this engine takes one language", code)
	}
	if len(parts) > maxTranscriptionLanguages {
		return "", fmt.Errorf("%q names more than %d languages", code, maxTranscriptionLanguages)
	}
	seen := map[string]bool{}
	for _, p := range parts {
		if !transcriptionLanguages[p] {
			return "", fmt.Errorf("%q is not a supported language code (two-letter ISO 639-1, e.g. uk, ru, en; join several with +)", code)
		}
		if seen[p] {
			return "", fmt.Errorf("%q repeats %q", code, p)
		}
		seen[p] = true
	}
	return s, nil
}

// MaxTranscriptionVocabularyRunes caps a vocabulary hint. whisper-1 reads at
// most 224 prompt tokens; 400 characters of Cyrillic or Latin names stays under
// that, and the hint is a list of names, not a paragraph.
const MaxTranscriptionVocabularyRunes = 400

// SanitizeTranscriptionVocabulary folds a vocabulary hint to one line: newlines
// and tabs become spaces, other control and invisible formatting characters
// (escape sequences, bidi overrides, zero-width marks) are dropped, and runs of
// spaces collapse. Invalid UTF-8 and a result longer than the cap are refused,
// not cut: a silently shortened hint would drop exactly the names at its end.
func SanitizeTranscriptionVocabulary(v string) (string, error) {
	if !utf8.ValidString(v) {
		return "", errors.New("vocabulary is not valid UTF-8")
	}
	var b strings.Builder
	space := false
	for _, r := range v {
		switch {
		case unicode.IsSpace(r):
			space = true
			continue
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := b.String()
	if n := utf8.RuneCountInString(out); n > MaxTranscriptionVocabularyRunes {
		return "", fmt.Errorf("vocabulary is longer than %d characters (%d)", MaxTranscriptionVocabularyRunes, n)
	}
	return out, nil
}
