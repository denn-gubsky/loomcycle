package inbox_test

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox/inboxtest"
)

const rtfHead = `{\rtf1\ansi\ansicpg1251\deff0 Hello.\par}`

// An RTF document is typed by its header (RFC GV §4.9): after a byte order
// mark too, whatever generic type the producer declared, and text/rtf
// folds into application/rtf.
func TestSniff_RTFIsTypedByItsHeader(t *testing.T) {
	for name, head := range map[string][]byte{
		"plain": []byte(rtfHead),
		"bom":   []byte("\xEF\xBB\xBF" + rtfHead),
	} {
		for _, declared := range []string{"", "application/octet-stream", "text/plain", "text/rtf", "application/x-rtf", "application/rtf"} {
			if mt := inbox.Sniff(declared, head, "file"); mt != "application/rtf" {
				t.Errorf("%s, declared %q: %q", name, declared, mt)
			}
		}
	}
	if mt := inbox.Sniff("", []byte("x"+rtfHead), "file"); mt == "application/rtf" {
		t.Error("text with an RTF header past its start was typed as RTF")
	}
	if mt := inbox.Sniff("text/rtf", []byte("not rtf"), "file"); mt != "application/rtf" {
		t.Errorf("declared text/rtf: %q, want it folded", mt)
	}
	if mt := inbox.Sniff("", []byte("plain words"), "notes.rtf"); mt != "application/rtf" {
		t.Errorf("by the .rtf extension: %q", mt)
	}
	if mt := inbox.Sniff("text/html", []byte(rtfHead), "page.html"); mt != "text/html" {
		t.Errorf("a specific declared type other than Word's was overridden: %q", mt)
	}
}

// RTF is often saved under a .doc name, and declared a Word document: its
// header wins over the name and the type, so the RTF reader reads it
// rather than nothing.
func TestRTF_RTFNamedDocIsSniffedAsRTF(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	for _, declared := range []string{"", "application/msword", "application/octet-stream"} {
		res := inbox.Capture(r.Ctx, inbox.Item{Name: "report.doc", MediaType: declared, Origin: "o"}, []byte(rtfHead))
		if res.Record.MediaType != "application/rtf" || !strings.HasPrefix(res.Record.Path, "documents/") {
			t.Errorf("declared %q: %q at %q", declared, res.Record.MediaType, res.Record.Path)
		}
		if e, ok := extract.Default().For(res.Record.MediaType, "report.doc"); !ok || e != (extract.RTF{}) {
			t.Errorf("declared %q: extractor %T, %t", declared, e, ok)
		}
	}
	// With no name it is given the .rtf extension.
	res := inbox.Capture(r.Ctx, inbox.Item{Fallback: "letter", Origin: "o"}, []byte(rtfHead))
	if !strings.HasSuffix(res.Record.Path, ".rtf") {
		t.Errorf("unnamed RTF kept as %q", res.Record.Path)
	}
	// A real Word document (OLE2) named .doc stays one.
	ole := []byte("\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1" + strings.Repeat("\x00", 504))
	if mt := inbox.Sniff("", ole, "report.doc"); mt != "application/msword" {
		t.Errorf("an OLE2 .doc: %q", mt)
	}
}
