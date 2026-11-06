package speech

import "testing"

// TestSpeech_LanguageFromScript — the script check picks uk / ru / the
// default as documented on Detect.
func TestSpeech_LanguageFromScript(t *testing.T) {
	both := []string{"uk", "ru", "en"}
	ukEN := []string{"uk", "en"}
	ruEN := []string{"ru", "en"}
	cases := []struct {
		name, text string
		configured []string
		def, want  string
	}{
		{"ukrainian letter", "Привіт, як справи?", both, "en", "uk"},
		{"ukrainian capital letter", "ЄВРОПА ПОРУЧ", both, "en", "uk"},
		{"ґ alone marks ukrainian", "ґрунт", both, "en", "uk"},
		{"other cyrillic is ru when configured", "Привет, как дела?", both, "en", "ru"},
		{"other cyrillic falls to uk without ru", "Привет, как дела?", ukEN, "en", "uk"},
		{"ukrainian without uk configured goes to ru", "Привіт", ruEN, "en", "ru"},
		{"cyrillic with neither configured is default", "Привіт", []string{"en"}, "en", "en"},
		{"latin text is default", "Hello there.", both, "en", "en"},
		{"latin majority with a cyrillic name stays default", "The office in Київ opened today.", both, "en", "en"},
		{"cyrillic majority with a latin word", "Звіт про API за жовтень", both, "en", "uk"},
		{"stress marks are not letters", "мо́ва", both, "en", "ru"},
		{"no letters is default", "123 — 456!", both, "uk", "uk"},
		{"empty is default", "", both, "en", "en"},
		{"configured is case-insensitive", "Привіт", []string{" UK "}, "en", "uk"},
		// A Latin-script text on a deployment whose default is not Latin-script.
		{"latin text goes to en over a uk default", "Leaves change colour in autumn.", ukEN, "uk", "en"},
		{"latin majority naming Київ goes to en", "The capital is Київ.", ukEN, "uk", "en"},
		{"cyrillic majority with latin names stays uk", "Ми використовуємо React і TypeScript.", ukEN, "uk", "uk"},
		{"latin text with only uk stays uk", "Leaves change colour in autumn.", []string{"uk"}, "uk", "uk"},
		{"the sole latin-script language is chosen", "Guten Morgen.", []string{"uk", "de"}, "uk", "de"},
		{"several latin-script languages fall to en", "Guten Morgen.", []string{"uk", "de", "en"}, "uk", "en"},
		{"several latin-script languages without en stay default", "Bonjour.", []string{"uk", "de", "fr"}, "uk", "uk"},
		{"a latin-script default is kept", "Hello there.", []string{"de", "en"}, "de", "de"},
		{"a code configured twice counts once", "Hello.", []string{"uk", "EN", "en"}, "uk", "en"},
		{"serbian is not taken for latin", "Dobar dan.", []string{"uk", "sr"}, "uk", "uk"},
		{"a tie stays default", "ab аб", ukEN, "uk", "uk"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Detect(c.text, c.configured, c.def); got != c.want {
				t.Errorf("Detect(%q, %v, %q) = %q, want %q", c.text, c.configured, c.def, got, c.want)
			}
		})
	}
}
