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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Detect(c.text, c.configured, c.def); got != c.want {
				t.Errorf("Detect(%q, %v, %q) = %q, want %q", c.text, c.configured, c.def, got, c.want)
			}
		})
	}
}
