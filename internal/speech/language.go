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
//   - Latin letters outnumber Cyrillic ones: defaultLang when it is itself a
//     Latin-script language; else the configured Latin-script language when
//     there is exactly one; else "en" when configured.
//   - Otherwise (no letters, a tie, or nothing above configured): defaultLang.
//
// The majority test keeps an English text that names "Київ" with the English
// voice. Text with Ukrainian letters is given a Russian voice only when no
// Ukrainian one is configured — the closer of what is there. Among several
// Latin-script languages the script cannot tell which one a text is in, so
// only a sole one, or English, is guessed; a German text on a deployment
// whose default is Ukrainian and that also has English goes to English, and
// the caller names the language to get German. configured holds the
// operator's language codes (compared case-insensitively); a model can still
// name the language itself.
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
	has := func(lang string) bool {
		for _, c := range configured {
			if strings.EqualFold(strings.TrimSpace(c), lang) {
				return true
			}
		}
		return false
	}
	if latin > cyrillic {
		return latinLanguage(configured, defaultLang, has)
	}
	if cyrillic == 0 || cyrillic == latin {
		return defaultLang
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

// latinScript holds the codes of the speech language list (ISO 639-1) written
// in the Latin alphabet. Serbian is left out: it is written in both scripts.
var latinScript = map[string]bool{
	"en": true, "pl": true, "de": true, "fr": true, "es": true, "it": true,
	"pt": true, "nl": true, "cs": true, "sk": true, "hr": true, "sl": true,
	"ro": true, "hu": true, "lt": true, "lv": true, "et": true, "fi": true,
	"sv": true, "no": true, "da": true, "tr": true, "az": true, "uz": true,
	"vi": true, "id": true,
}

// latinLanguage is Detect's choice for a text mostly in Latin letters.
func latinLanguage(configured []string, defaultLang string, has func(string) bool) string {
	if latinScript[strings.ToLower(strings.TrimSpace(defaultLang))] {
		return defaultLang
	}
	sole := ""
	for _, c := range configured {
		code := strings.ToLower(strings.TrimSpace(c))
		if !latinScript[code] || code == sole {
			continue
		}
		if sole != "" {
			sole = ""
			break
		}
		sole = code
	}
	switch {
	case sole != "":
		return sole
	case has("en"):
		return "en"
	}
	return defaultLang
}
