package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// speech.go — the top-level `speech:` block: text-to-speech, the reverse of
// transcription's speech-to-text. The per-agent block is in agent_speech.go.
//
// This is the substrate only (RFC FK P1). The block is typed and validated
// here so a bad value fails `loomcycle validate`; nothing in the runtime calls
// an engine yet. Absent, speech is off.
//
// Engines, providers, models and voices are operator configuration. What a
// model may choose later is a language and a voice, and only from
// speech.voices: VoiceFor and AllowedVoices are the accessors the runtime reads
// that list through, so the closed list config validates is the one the tool
// enforces.

// SpeechConfig is the `speech:` block. Zero numbers mean "the default";
// defaults are applied by Resolved, the one place they live.
type SpeechConfig struct {
	// Engine is required when the block is written: openai-speech | off.
	Engine string `yaml:"engine"`
	// DefaultLanguage is the language spoken when the caller names none and the
	// text gives no hint. One code from the transcription language list.
	// Required for openai-speech, and it must have an entry in Voices.
	DefaultLanguage string `yaml:"default_language"`
	// Voices maps a language code to the model alias and voice that speak it.
	// It is the closed list a model may pick from: no self-hosted model speaks
	// every language well, so the operator maps each one.
	Voices map[string]SpeechVoice `yaml:"voices,omitempty"`
	// Format is the default output: ogg (Opus) | mp3 | wav. Default ogg when
	// Encode is set, else wav (the runtime asks every engine for WAV, so only
	// ogg and mp3 need the encoder).
	Format string `yaml:"format"`
	// Encode is the converter (ffmpeg or equivalent) that turns the joined WAV
	// into ogg or mp3: `{in}` is the WAV, `{out}` the file it must write.
	Encode           *HelperCommand `yaml:"encode,omitempty"`
	MaxCharsPerCall  int            `yaml:"max_chars_per_call"`
	MaxCharsPerRun   int            `yaml:"max_chars_per_run"`
	MaxSecondsPerRun int            `yaml:"max_seconds_per_run"`
	// TimeoutSeconds bounds one engine request and one encode. A CPU speech
	// host can take about a minute for a full piece (RFC FK §13 P0).
	TimeoutSeconds int `yaml:"timeout_s"`
}

// SpeechVoice is one `speech.voices.<language>` entry.
type SpeechVoice struct {
	// Model is a models: alias tagged kind: synthesizer, on a provider served
	// by the openai driver.
	Model string `yaml:"model"`
	// Voice is the engine's own voice id, used when the caller names none.
	Voice string `yaml:"voice"`
	// Alternates are the other voices a caller may pick for this language.
	Alternates []string `yaml:"alternates,omitempty"`
}

// The engines the block accepts.
const (
	SpeechEngineOpenAISpeech = "openai-speech"
	SpeechEngineOff          = "off"
)

// The output formats.
const (
	SpeechFormatOgg = "ogg"
	SpeechFormatMP3 = "mp3"
	SpeechFormatWAV = "wav"
)

// Defaults and limits. 4 000 characters per call stays under the 4 096 that
// Ollama and OpenAI accept per request; the hard ceiling is that 4 096.
const (
	SpeechDefaultMaxCharsPerCall  = 4000
	SpeechMaxCharsPerCallCeiling  = 4096
	SpeechDefaultMaxCharsPerRun   = 100000
	SpeechDefaultMaxSecondsPerRun = 3600
	SpeechDefaultTimeoutSecs      = 600
)

// Enabled reports whether a speech engine is configured and not off.
func (s *SpeechConfig) Enabled() bool {
	return s != nil && s.Engine != "" && s.Engine != SpeechEngineOff
}

// VoiceFor returns the model alias, default voice and alternates configured
// for lang (a language code, compared trimmed and lower-cased). ok is false
// when speech is absent or the language has no entry. The alternates slice is
// a copy.
func (s *SpeechConfig) VoiceFor(lang string) (model, voice string, alternates []string, ok bool) {
	if s == nil {
		return "", "", nil, false
	}
	v, ok := s.Voices[strings.ToLower(strings.TrimSpace(lang))]
	if !ok {
		return "", "", nil, false
	}
	return v.Model, v.Voice, append([]string(nil), v.Alternates...), true
}

// AllowedVoices returns every voice a caller may pick for lang, the default
// first; nil when the language has no entry.
func (s *SpeechConfig) AllowedVoices(lang string) []string {
	_, voice, alts, ok := s.VoiceFor(lang)
	if !ok {
		return nil
	}
	return append([]string{voice}, alts...)
}

// Resolved returns a copy with every unset field at its default; nil for a nil
// block. It is the one place defaults are applied: load stores its result.
// Idempotent. The per-call default never exceeds the per-run cap the operator
// wrote, so writing only a low per-run cap is not refused for a per-call value
// nobody wrote.
func (s *SpeechConfig) Resolved() *SpeechConfig {
	if s == nil {
		return nil
	}
	out := *s
	if s.Encode != nil {
		e := *s.Encode
		out.Encode = &e
	}
	if s.Voices != nil {
		out.Voices = make(map[string]SpeechVoice, len(s.Voices))
		for lang, v := range s.Voices {
			v.Alternates = append([]string(nil), v.Alternates...)
			out.Voices[lang] = v
		}
	}
	if out.Format == "" {
		out.Format = SpeechFormatWAV
		if out.Encode != nil {
			out.Format = SpeechFormatOgg
		}
	}
	out.MaxCharsPerRun = orDefaultInt(out.MaxCharsPerRun, SpeechDefaultMaxCharsPerRun)
	out.MaxCharsPerCall = orDefaultInt(out.MaxCharsPerCall, min(SpeechDefaultMaxCharsPerCall, out.MaxCharsPerRun))
	out.MaxSecondsPerRun = orDefaultInt(out.MaxSecondsPerRun, SpeechDefaultMaxSecondsPerRun)
	out.TimeoutSeconds = orDefaultInt(out.TimeoutSeconds, SpeechDefaultTimeoutSecs)
	return &out
}

// resolveSpeech validates the block, then stores the resolved form on c. Runs
// after models: and providers: are validated (voices name both) and before the
// agents loop (whose lower-only check reads the resolved cap).
func resolveSpeech(c *Config) error {
	if c.Speech == nil {
		return nil
	}
	if err := c.validateSpeech(c.Speech); err != nil {
		return err
	}
	c.Speech = c.Speech.Resolved()
	// Cross-field bounds are judged on the resolved values so a default is held
	// to the same rule as a written value.
	s := c.Speech
	if s.MaxCharsPerCall > s.MaxCharsPerRun {
		return fmt.Errorf("speech: max_chars_per_call (%d) is above max_chars_per_run (%d)", s.MaxCharsPerCall, s.MaxCharsPerRun)
	}
	if (s.Format == SpeechFormatOgg || s.Format == SpeechFormatMP3) && s.Encode == nil {
		return fmt.Errorf("speech.format: %s needs speech.encode (a converter such as ffmpeg -i {in} -c:a libopus {out}); engines are asked for wav, and without an encoder only wav can be written", s.Format)
	}
	for lang, v := range s.Voices {
		if ref := c.Models[v.Model]; ref.License == ModelLicenseNoncommercial {
			c.Warnings = append(c.Warnings, fmt.Sprintf("speech.voices.%s uses models.%s, which is license: noncommercial: its weights may not be used commercially", lang, v.Model))
		}
	}
	return nil
}

func (c *Config) validateSpeech(s *SpeechConfig) error {
	const place = "speech"
	if err := checkEngine(place, s.Engine, []string{SpeechEngineOpenAISpeech, SpeechEngineOff}, nil); err != nil {
		return err
	}
	if s.DefaultLanguage != "" {
		lang, err := NormalizeTranscriptionLanguage(s.DefaultLanguage, false)
		if err != nil {
			return fmt.Errorf("%s.default_language: %w", place, err)
		}
		s.DefaultLanguage = lang
	}
	voices, err := c.validateSpeechVoices(s.Voices)
	if err != nil {
		return err
	}
	s.Voices = voices
	if s.Engine == SpeechEngineOpenAISpeech {
		if len(s.Voices) == 0 {
			return fmt.Errorf("%s.voices: engine %s needs at least one language (e.g. en: {model: <a kind: %s alias>, voice: <the engine's voice id>})", place, s.Engine, ModelKindSynthesizer)
		}
		if s.DefaultLanguage == "" {
			return fmt.Errorf("%s.default_language is required for engine %s (one of the languages in %s.voices: %s)", place, s.Engine, place, strings.Join(sortedKeys(s.Voices), ", "))
		}
		if _, ok := s.Voices[s.DefaultLanguage]; !ok {
			return fmt.Errorf("%s.default_language: %q has no entry in %s.voices (configured: %s)", place, s.DefaultLanguage, place, strings.Join(sortedKeys(s.Voices), ", "))
		}
	}
	switch s.Format {
	case "", SpeechFormatOgg, SpeechFormatMP3, SpeechFormatWAV:
	default:
		return fmt.Errorf("%s.format: %q is not one of ogg, mp3, wav", place, s.Format)
	}
	if s.Encode != nil {
		if err := s.Encode.validate(place + ".encode"); err != nil {
			return err
		}
		// The runtime hands the encoder the joined WAV and reads back the file
		// it writes, so the command must write to the file it is handed.
		if !strings.Contains(s.Encode.Command, "{out}") {
			return fmt.Errorf("%s.encode.command must reference {out}, the file the encoder writes (e.g. ffmpeg -i {in} -c:a libopus -b:a 32k {out})", place)
		}
	}
	for _, f := range []struct {
		name string
		v    int
	}{
		{"max_chars_per_call", s.MaxCharsPerCall},
		{"max_chars_per_run", s.MaxCharsPerRun},
		{"max_seconds_per_run", s.MaxSecondsPerRun},
		{"timeout_s", s.TimeoutSeconds},
	} {
		if f.v < 0 {
			return fmt.Errorf("%s.%s must be >= 0 (0 = the default), got %d", place, f.name, f.v)
		}
	}
	if s.MaxCharsPerCall > SpeechMaxCharsPerCallCeiling {
		return fmt.Errorf("%s.max_chars_per_call (%d) is above %d, the most a speech endpoint accepts in one request", place, s.MaxCharsPerCall, SpeechMaxCharsPerCallCeiling)
	}
	return nil
}

// validateSpeechVoices checks every voices entry and returns the map keyed by
// normalised language code.
func (c *Config) validateSpeechVoices(in map[string]SpeechVoice) (map[string]SpeechVoice, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make(map[string]SpeechVoice, len(in))
	// Sorted so the first refusal is the same on every load.
	for _, key := range sortedKeys(in) {
		v := in[key]
		place := "speech.voices." + key
		lang, err := NormalizeTranscriptionLanguage(key, false)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", place, err)
		}
		if _, dup := out[lang]; dup {
			return nil, fmt.Errorf("%s: language %q is configured twice", place, lang)
		}
		if err := c.checkSynthesizerModel(place+".model", v.Model); err != nil {
			return nil, err
		}
		if err := checkVoiceID(place+".voice", v.Voice); err != nil {
			return nil, err
		}
		seen := map[string]bool{v.Voice: true}
		for i, alt := range v.Alternates {
			if err := checkVoiceID(fmt.Sprintf("%s.alternates[%d]", place, i), alt); err != nil {
				return nil, err
			}
			if seen[alt] {
				return nil, fmt.Errorf("%s.alternates[%d]: %q is listed twice (as the voice or another alternate)", place, i, alt)
			}
			seen[alt] = true
		}
		out[lang] = v
	}
	return out, nil
}

// checkSynthesizerModel requires a models: alias tagged kind: synthesizer that
// names its own provider and model, on the openai driver.
func (c *Config) checkSynthesizerModel(place, model string) error {
	if model == "" {
		return fmt.Errorf("%s is required (a models: alias with kind: %s)", place, ModelKindSynthesizer)
	}
	ref, ok := c.Models[model]
	if !ok {
		return fmt.Errorf("%s: %q is not a models: alias (it must be one tagged kind: %s)", place, model, ModelKindSynthesizer)
	}
	// Stricter than requireModelKind: there are no untagged synthesizer aliases
	// from before the kind existed, so the tag is required.
	if ref.Kind != ModelKindSynthesizer {
		kind := ref.Kind
		if kind == "" {
			kind = "untagged (chat)"
		}
		return fmt.Errorf("%s: models.%s is kind: %s, and a model of kind: %s is needed here", place, model, kind, ModelKindSynthesizer)
	}
	if ref.Model == "" {
		return fmt.Errorf("%s: models.%s must name its model with model:; a synthesizer is not chosen from a provider's catalog", place, model)
	}
	// The voice has no provider of its own, so the alias must carry one.
	if ref.Provider == "" {
		return fmt.Errorf("%s: models.%s must name its provider: (a provider on the openai driver whose base_url serves /v1/audio/speech)", place, model)
	}
	// Only the openai driver speaks the speech endpoint. A built-in id with no
	// providers: entry is its own driver.
	driver := ref.Provider
	if pc, ok := c.Providers[ref.Provider]; ok {
		driver = pc.Driver
	}
	if driver != "openai" {
		return fmt.Errorf("%s: models.%s is served by provider %q on the %q driver; the speech endpoint is served by providers on the openai driver", place, model, ref.Provider, driver)
	}
	return nil
}

// checkVoiceID refuses an empty voice id and one with spaces at its ends or
// control or invisible formatting characters: a model picks voices from this
// list by name, so a name must read back exactly as the engine knows it.
func checkVoiceID(place, v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s is required (the engine's voice id)", place)
	}
	if strings.TrimSpace(v) != v {
		return fmt.Errorf("%s: %q has spaces at its start or end", place, v)
	}
	for _, r := range v {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%s: %q contains a control or formatting character", place, v)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// UnmarshalYAML decodes strictly: the rest of the config decodes leniently,
// so a typo in a cap (`max_char_per_run`) would load as the default without a
// word.
func (s *SpeechConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "speech", nil, reflect.TypeOf(*s)); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "encode"), "speech.encode", nil, reflect.TypeOf(HelperCommand{})); err != nil {
		return err
	}
	if vs := valueOf(n, "voices"); vs != nil && vs.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(vs.Content); i += 2 {
			if err := strictKeys(vs.Content[i+1], "speech.voices."+vs.Content[i].Value, nil, reflect.TypeOf(SpeechVoice{})); err != nil {
				return err
			}
		}
	}
	type plain SpeechConfig
	return n.Decode((*plain)(s))
}
