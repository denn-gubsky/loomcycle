package transcribe

import "github.com/denn-gubsky/loomcycle/internal/config"

// The model may choose a language hint and a vocabulary hint; both reach an
// engine prompt or a helper command line, so both are sanitised here before
// anything else sees them. The rules live in config, so load-time validation
// of the operator's values and the runtime's check of a model's are one rule.

// SanitizeLanguage returns a language hint normalised (lower case, trimmed)
// when every code is on the closed list; up to three codes joined by "+" (an
// OCR hint for a mixed-script page). Anything else is an error.
func SanitizeLanguage(code string) (string, error) {
	return config.NormalizeTranscriptionLanguage(code, true)
}

// SanitizeSTTLanguage is SanitizeLanguage for a speech engine, which takes
// exactly one language.
func SanitizeSTTLanguage(code string) (string, error) {
	return config.NormalizeTranscriptionLanguage(code, false)
}

// SanitizeVocabulary folds a vocabulary hint to one line (newlines and tabs to
// spaces, control and invisible formatting characters dropped) and refuses one
// longer than config.MaxTranscriptionVocabularyRunes.
func SanitizeVocabulary(v string) (string, error) {
	return config.SanitizeTranscriptionVocabulary(v)
}
