package builtin

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/files/extract/pdftest"
)

// A run whose inbox is not bound — it has kept no file yet, so the inbox is
// not made, or the deployment has no dynamic_root volume — can still render
// a PDF into one of its volumes: the typesetter works in File.ScratchDir.
// It used to look for the inbox to work in and refused with "not bound to
// volume inbox", although nothing was written there; the RFC GW P2 data
// pass hit it rendering into a volume. A stand-in typesetter copies a ready
// PDF to {out}, so the test needs no Typst.
func TestFileRender_PDFIntoAVolumeNeedsNoInbox(t *testing.T) {
	f, ctx, d, docs := fileDocRun(t)
	fixture := filepath.Join(t.TempDir(), "typeset.pdf")
	pdf := pdftest.Doc{Pages: []pdftest.Page{{Lines: []pdftest.Line{{Text: "Typeset", X: 72, Y: 770, Size: 12}}}}}.Bytes()
	if err := os.WriteFile(fixture, pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	f.PDFCommand = "cp " + fixture + " {out}"
	f.ScratchDir = t.TempDir()
	docID, _ := renderFixtureDoc(t, d, ctx)

	out, res := fileOp(t, f, ctx, map[string]any{"op": "render", "document": docID, "format": "pdf", "to": "volume:docs:minutes.pdf"})
	if res.IsError {
		t.Fatalf("render into a volume without an inbox: %s", res.Text)
	}
	if asStr(out["ref"]) != "volume:docs:minutes.pdf" {
		t.Fatalf("result = %v", out)
	}
	got, err := os.ReadFile(filepath.Join(docs, "minutes.pdf"))
	if err != nil || !bytes.Equal(got, pdf) {
		t.Fatalf("the volume does not hold the typeset PDF: %v", err)
	}
	left, err := os.ReadDir(f.ScratchDir)
	if err != nil || len(left) != 0 {
		t.Fatalf("the typesetter's scratch was not removed: %v %v", left, err)
	}
}
