package transcribe

import (
	"errors"
	"strings"
	"testing"
)

// The agent's mode is both the default and the ceiling: a call may ask for
// less, never more, and asking for nothing runs the agent's own.
func TestModeFor_CallMayLowerNeverRaiseTheAgentsMode(t *testing.T) {
	for _, c := range []struct {
		ceiling   OCRMode
		requested string
		want      OCRMode
		refused   bool
	}{
		{OCRNever, "", OCRNever, false},
		{OCRAuto, "", OCRAuto, false},
		{OCRAlways, "", OCRAlways, false},
		{OCRAlways, "auto", OCRAuto, false},
		{OCRAlways, "never", OCRNever, false},
		{OCRAuto, "never", OCRNever, false},
		{OCRAuto, "always", "", true},
		{OCRNever, "auto", "", true},
		{OCRNever, "always", "", true},
		{OCRAuto, "sometimes", "", true},
		{"", "", OCRNever, false},
	} {
		got, err := ModeFor(c.ceiling, c.requested)
		if c.refused {
			if err == nil {
				t.Errorf("ModeFor(%q, %q) = %q, want a refusal", c.ceiling, c.requested, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ModeFor(%q, %q) = %q, %v; want %q", c.ceiling, c.requested, got, err, c.want)
		}
	}
}

// A reservation that one budget took and the next refused is given back, so
// a refused call spends nothing anywhere.
func TestBudget_ReleasePagesGivesBackAReservation(t *testing.T) {
	b := NewBudget(Caps{MaxPagesPerRun: 10})
	if err := b.ReservePages(8); err != nil {
		t.Fatal(err)
	}
	b.ReleasePages(5)
	if b.Pages() != 3 {
		t.Fatalf("pages = %d after release, want 3", b.Pages())
	}
	if err := b.ReservePages(7); err != nil {
		t.Fatalf("the released pages were not available again: %v", err)
	}
	b.ReleasePages(100)
	if b.Pages() != 0 {
		t.Fatalf("pages = %d after releasing more than held, want 0", b.Pages())
	}
	if err := b.ReservePages(11); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("release raised the cap: %v", err)
	}
}

// The instruction is the operator's template or the built-in one, with the
// sanitised language hint in it; nothing else varies.
func TestOCRPrompt_TemplateAndLanguage(t *testing.T) {
	p := OCRPrompt("", "uk+ru")
	for _, want := range []string{"verbatim", "[illegible]", "Do not translate", "uk, ru"} {
		if !strings.Contains(p, want) {
			t.Errorf("built-in prompt lacks %q:\n%s", want, p)
		}
	}
	if got := OCRPrompt("Transcribe. Language: {language}.", "uk"); got != "Transcribe. Language: uk." {
		t.Errorf("operator template = %q", got)
	}
}
