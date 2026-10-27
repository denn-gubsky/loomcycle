package speech

import (
	"strings"
	"unicode"
)

// Detect picks the language to speak text in when the caller named none, by
// a script check over the text's letters:
//
//   - Cyrillic letters outnumber Latin ones: "uk" if the text has a
//     Ukrainian-only letter (і ї є ґ, either case) and "uk" is configured;
//     otherwise "ru" if configured, else "uk" if configured.
//   - Otherwise, or when neither is configured: defaultLang.
//
// The majority test keeps an English text that names "Київ" with the English
// voice. Text with Ukrainian letters is given a Russian voice only when no
// Ukrainian one is configured — the closer of what is there. A Latin-script
// language is never guessed: the text goes to defaultLang. configured holds
// the operator's language codes (compared case-insensitively); a model can
// still name the language itself.
func Detect(text string, configured []string, defaultLang string) string {
	var latin, cyrillic int
	ukrainian := false
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Cyrillic, r) && unicode.IsLetter(r):
			cyrillic++
			switch r {
			case 'і', 'ї', 'є', 'ґ', 'І', 'Ї', 'Є', 'Ґ':
				ukrainian = true
			}
		case unicode.Is(unicode.Latin, r) && unicode.IsLetter(r):
			latin++
		}
	}
	if cyrillic == 0 || cyrillic <= latin {
		return defaultLang
	}
	has := func(lang string) bool {
		for _, c := range configured {
			if strings.EqualFold(strings.TrimSpace(c), lang) {
				return true
			}
		}
		return false
	}
	switch {
	case ukrainian && has("uk"):
		return "uk"
	case has("ru"):
		return "ru"
	case has("uk"):
		return "uk"
	}
	return defaultLang
}
