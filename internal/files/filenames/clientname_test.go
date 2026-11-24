package filenames

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A person's file keeps its own name, in its own script, NFC-normalised;
// what the name may not hold becomes "-".
func TestSanitizeClientName_KeepsUnicodeNames(t *testing.T) {
	breve := string(rune(0x0306))
	for _, c := range []struct{ in, want string }{
		{"Паспорт.docx", "Паспорт.docx"},
		{"Паспорт рішення (v2).docx", "Паспорт рішення (v2).docx"},
		// и + combining breve is й, composed.
		{"Мии" + breve + ".pdf", "Мий.pdf"},
		{"Звіт: №3*?.pdf", "Звіт- -3-.pdf"},
		{"  report.pdf ", "report.pdf"},
		{"日本語の資料.pdf", "日本語の資料.pdf"},
	} {
		got, err := SanitizeClientName(c.in)
		if err != nil || got != c.want {
			t.Errorf("%q: got %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

// A name that names a path, could hide what it says, or is a hidden file is
// refused, not repaired: the bidi overrides make "exe.pdf" read as
// "fdp.exe", so stripping them would still keep a name the person did not
// see.
func TestSanitizeClientName_RefusesPathsHiddenFilesAndFormatCharacters(t *testing.T) {
	for _, in := range []string{
		"", " ", ".", "..", "../etc/passwd", `..\x.pdf`, "a/b.pdf", `a\b.pdf`, ".hidden.pdf",
		"invoice" + string(rune(0x202E)) + "fdp.exe",
		"a" + string(rune(0x2066)) + "b.pdf",
		"a" + string(rune(0x200B)) + "b.pdf", // zero width space is Cf
		"a\x07b.pdf", "a\nb.pdf", "a\x00b.pdf",
		"***", "\xff\xfe.pdf",
	} {
		if got, err := SanitizeClientName(in); err == nil {
			t.Errorf("%q: kept as %q, want a refusal", in, got)
		}
	}
}

// The name is cut to 120 bytes on a rune boundary, keeping its extension.
func TestSanitizeClientName_CapsBytesOnARuneBoundaryKeepingTheExtension(t *testing.T) {
	got, err := SanitizeClientName(strings.Repeat("я", 100) + ".docx") // 205 bytes
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > ClientNameMaxBytes || !utf8.ValidString(got) || !strings.HasSuffix(got, ".docx") || !strings.HasPrefix(got, "яя") {
		t.Fatalf("got %q (%d bytes)", got, len(got))
	}
}
