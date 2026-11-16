package extract

import (
	"bytes"
	"io"
	"regexp"
)

// pdfEncryptWindow is how much of each end of a file pdfDeclaresEncryption
// reads. The trailer a reader starts from sits just before the final
// startxref, and a linearized file's first-page trailer near the start; each
// is a few hundred bytes.
const pdfEncryptWindow = 64 << 10

var (
	// A key /Encrypt (not /EncryptMetadata) whose value is a reference or
	// an inline dictionary, and a key /Root whose value is a reference.
	pdfEncryptKey = regexp.MustCompile(`/Encrypt[\x00\t\n\f\r ]*(?:\d+[\x00\t\n\f\r ]+\d+[\x00\t\n\f\r ]+R|<<)`)
	pdfRootKey    = regexp.MustCompile(`/Root[\x00\t\n\f\r ]*\d+[\x00\t\n\f\r ]+\d+[\x00\t\n\f\r ]+R`)
	pdfXRefType   = regexp.MustCompile(`/Type[\x00\t\n\f\r ]*/XRef[^A-Za-z0-9]`)
	pdfTrailerKw  = regexp.MustCompile(`trailer[\x00\t\n\f\r ]*$`)
)

// pdfDeclaresEncryption reports whether a trailer, or a cross-reference
// stream's dictionary, in the first or last pdfEncryptWindow bytes names
// both /Encrypt and /Root: the file is encrypted. It reads only those two
// windows, so a file pdfcpu would decrypt and read whole before saying it is
// encrypted (an owner-password-only file opens with an empty password) is
// refused for the cost of 128 KiB.
//
// A dictionary only counts in one of those two places, and only with both
// keys holding references, so text that merely mentions /Encrypt does not
// trip it; a file whose uncompressed content draws a whole trailer as text,
// within either window, would be refused as encrypted. A miss costs nothing:
// openPDFContext still refuses every file pdfcpu finds encrypted.
func pdfDeclaresEncryption(r io.ReaderAt, size int64) bool {
	if size <= 0 {
		return false
	}
	if size <= 2*pdfEncryptWindow {
		return pdfWindowDeclaresEncryption(r, 0, size)
	}
	return pdfWindowDeclaresEncryption(r, 0, pdfEncryptWindow) ||
		pdfWindowDeclaresEncryption(r, size-pdfEncryptWindow, pdfEncryptWindow)
}

func pdfWindowDeclaresEncryption(r io.ReaderAt, off, n int64) bool {
	buf := make([]byte, n)
	got, err := r.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return false
	}
	buf = buf[:got]
	if !bytes.Contains(buf, []byte("/Encrypt")) {
		return false
	}
	// Each dictionary's span, innermost ones first, from a scan of << and
	// >>. A hex string is <...> and never holds <<, so the pairs balance on
	// well-formed input; a dictionary cut by the window's edge is skipped.
	var open []int
	for i := 0; i+1 < len(buf); i++ {
		switch {
		case buf[i] == '<' && buf[i+1] == '<':
			open = append(open, i)
			i++
		case buf[i] == '>' && buf[i+1] == '>':
			if len(open) == 0 {
				i++
				continue
			}
			start := open[len(open)-1]
			open = open[:len(open)-1]
			i++
			// Only an outermost dictionary can be a trailer or an XRef
			// stream's; a nested one is a value inside one.
			if len(open) == 0 && pdfIsEncryptingTrailer(buf[:start], buf[start:i+1]) {
				return true
			}
		}
	}
	return false
}

// pdfIsEncryptingTrailer reports whether dict, preceded by before, is a
// trailer (after the trailer keyword) or an XRef stream's dictionary that
// names /Encrypt and /Root.
func pdfIsEncryptingTrailer(before, dict []byte) bool {
	if !pdfEncryptKey.Match(dict) || !pdfRootKey.Match(dict) {
		return false
	}
	tail := before
	if len(tail) > 32 {
		tail = tail[len(tail)-32:]
	}
	return pdfTrailerKw.Match(tail) || pdfXRefType.Match(dict)
}
