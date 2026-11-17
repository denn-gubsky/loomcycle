package extract

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/files/extract/pdftest"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// minimalPDF is a one-page PDF whose trailer carries the extra entries given.
// The catalog, pages and page are objects 1 to 3; extra objects follow from 4.
func minimalPDF(trailer string, extra ...string) []byte {
	w := &pdftest.Writer{}
	catalog := w.Reserve()
	pages := w.Reserve()
	page := w.Add("<< /Type /Page /MediaBox [0 0 595 842] /Parent 2 0 R >>")
	for _, o := range extra {
		w.Add(o)
	}
	w.Set(pages, "<< /Type /Pages /Kids ["+strconv.Itoa(page)+" 0 R] /Count 1 >>")
	w.Set(catalog, "<< /Type /Catalog /Pages "+strconv.Itoa(pages)+" 0 R >>")
	return w.Bytes(catalog, trailer)
}

// A PDF that declares encryption is refused from its trailer, before pdfcpu
// reads its objects: an encrypted file that opens with an empty password is
// otherwise decrypted and read whole first (380 MiB for a 22 MB
// specification), which is what kills a small conversion worker. The
// /Encrypt here names an encryption dictionary pdfcpu cannot use, so only a
// refusal made before parsing says "encrypted".
func TestExtractPDF_EncryptedRefusedBeforeItIsParsed(t *testing.T) {
	cases := map[string][]byte{
		"trailer": minimalPDF("/Encrypt 4 0 R", "<< /Filter /Standard /V 99 /R 99 >>"),
		"xref stream": []byte("%PDF-1.7\n" +
			"1 0 obj\n<< /Type /XRef /Root 2 0 R /Encrypt 3 0 R /Size 4 /W [1 2 1] /Length 0 >>\nstream\n\nendstream\nendobj\n" +
			"startxref\n9\n%%EOF\n"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, err := extractPDF(t, data, Options{Name: "x.pdf"})
			if !IsRefused(err) || !strings.Contains(err.Error(), "encrypted") {
				t.Fatalf("err = %v, want an encryption refusal", err)
			}
			if s.md.Len() != 0 {
				t.Errorf("wrote %q", s.md.String())
			}
			if _, err := SelectPages(context.Background(), bytes.NewReader(data), int64(len(data)), []int{1}); !IsRefused(err) {
				t.Errorf("SelectPages err = %v, want an encryption refusal", err)
			}
		})
	}
}

// The files pdfcpu itself encrypts are caught by the cheap check, in both
// the user-password and the owner-password-only form.
func TestPDFDeclaresEncryption_RealEncryptedFiles(t *testing.T) {
	plain, _ := boardPDF(t)
	if pdfDeclaresEncryption(bytes.NewReader(plain), int64(len(plain))) {
		t.Fatal("the plain fixture is reported encrypted")
	}
	for _, pw := range [][2]string{{"secret", "owner"}, {"", "owner"}} {
		conf := model.NewAESConfiguration(pw[0], pw[1], 256)
		conf.Offline = true
		var enc bytes.Buffer
		if err := api.Encrypt(context.Background(), bytes.NewReader(plain), &enc, conf); err != nil {
			t.Fatalf("encrypt the fixture: %v", err)
		}
		if !pdfDeclaresEncryption(bytes.NewReader(enc.Bytes()), int64(enc.Len())) {
			t.Errorf("user password %q: an encrypted file is not caught", pw[0])
		}
	}
}

// A linearized file declares encryption in its first-page trailer, at the
// start; the trailer at its end may carry only /Size.
func TestPDFDeclaresEncryption_FirstPageTrailer(t *testing.T) {
	data := []byte("%PDF-1.7\ntrailer\n<< /Size 3 /Root 1 0 R /Encrypt 2 0 R >>\n" +
		strings.Repeat("% filler line\n", 20000) +
		"trailer\n<< /Size 3 >>\nstartxref\n0\n%%EOF\n")
	if len(data) <= 2*pdfEncryptWindow {
		t.Fatalf("the fixture (%d bytes) must be longer than both windows", len(data))
	}
	if !pdfDeclaresEncryption(bytes.NewReader(data), int64(len(data))) {
		t.Error("an /Encrypt in the first-page trailer is not caught")
	}
}

// Stream data and literal strings may hold << and >> (an encrypted stream's
// ciphertext does, at random); they must not unbalance the dictionary scan
// and hide the trailer after them. Each case failed before streams and
// strings were skipped.
func TestPDFDeclaresEncryption_StrayBracketsDoNotHideTheTrailer(t *testing.T) {
	trailer := "trailer\n<< /Size 4 /Root 1 0 R /Encrypt 3 0 R >>\nstartxref\n0\n%%EOF\n"
	cases := map[string]string{
		"an unclosed << in a stream":                      "%PDF-1.7\n2 0 obj\n<< /Length 6 >>\nstream\nab<<cd\nendstream\nendobj\n" + trailer,
		"a stray >> in a stream":                          "%PDF-1.7\n2 0 obj\n<< /Length 6 >>\nstream\n<<x>>>>\nendstream\nendobj\n" + trailer,
		"an unclosed << in a string":                      "%PDF-1.7\n2 0 obj\n<< /Title (a << b) >>\nendobj\n" + trailer,
		"the window starts in a stream, after a stray <<": "\x9c<<\x01\nendstream\nendobj\n" + trailer,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if !pdfDeclaresEncryption(bytes.NewReader([]byte(data)), int64(len(data))) {
				t.Fatal("the encrypting trailer is not caught")
			}
		})
	}
}

// Only a trailer or a cross-reference stream's dictionary that names both
// /Encrypt and /Root counts. A plain PDF that carries lookalikes elsewhere
// still extracts.
func TestPDFDeclaresEncryption_NotFooledByLookalikes(t *testing.T) {
	cases := map[string][]byte{
		"an ordinary object": minimalPDF("", "<< /Root 1 0 R /Encrypt 2 0 R >>"),
		"a longer key":       minimalPDF("/EncryptX 4 0 R", "<< /Note 1 >>"),
		"EncryptMetadata":    minimalPDF("/EncryptMetadata false"),
		"a value, not a ref": minimalPDF("/Encrypt /None"),
		"no Root beside it":  []byte("%PDF-1.7\ntrailer\n<< /Size 3 /Encrypt 2 0 R >>\nstartxref\n0\n%%EOF\n"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if pdfDeclaresEncryption(bytes.NewReader(data), int64(len(data))) {
				t.Fatal("reported encrypted")
			}
		})
	}
	// The ordinary-object case is a whole, readable PDF: it extracts.
	data := cases["an ordinary object"]
	if _, _, err := extractPDF(t, data, Options{Name: "x.pdf"}); err != nil {
		t.Errorf("a plain PDF with a lookalike object: %v", err)
	}
}
