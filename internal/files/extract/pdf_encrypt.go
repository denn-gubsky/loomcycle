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
	// >>. A hex string is <...> and never holds <<, but a stream's bytes and
	// a literal string's text can hold either pair (an encrypted stream's
	// ciphertext does, at random), so both are skipped, and so is the data
	// of a stream the window begins inside. A dictionary cut by the window's
	// edge is skipped too.
	var open []int
	for i := pdfScanStart(buf); i+1 < len(buf); i++ {
		switch {
		case pdfStreamStarts(buf, i):
			end := bytes.Index(buf[i:], []byte("endstream"))
			if end < 0 {
				return false
			}
			open = nil
			i += end + len("endstream") - 1
		case buf[i] == '(':
			i = pdfSkipLiteralString(buf, i)
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

// pdfScanStart returns where the dictionary scan of a window begins: past the
// first endstream when no stream starts before it, because then the window
// began inside a stream and everything up to there is its data.
func pdfScanStart(buf []byte) int {
	end := bytes.Index(buf, []byte("endstream"))
	if end < 0 {
		return 0
	}
	for i := 0; i < end; {
		j := bytes.Index(buf[i:end], []byte("stream"))
		if j < 0 {
			break
		}
		if pdfStreamStarts(buf, i+j) {
			return 0
		}
		i += j + 1
	}
	return end + len("endstream")
}

// pdfStreamStarts reports whether the stream keyword starts at i: right after
// a dictionary's >> (whitespace between) and followed by an end of line, so
// the word inside a string or a name does not count.
func pdfStreamStarts(buf []byte, i int) bool {
	rest := buf[i:]
	if !bytes.HasPrefix(rest, []byte("stream")) {
		return false
	}
	rest = rest[len("stream"):]
	if !bytes.HasPrefix(rest, []byte("\n")) && !bytes.HasPrefix(rest, []byte("\r\n")) {
		return false
	}
	before := bytes.TrimRight(buf[:i], "\x00\t\n\f\r ")
	return bytes.HasSuffix(before, []byte(">>"))
}

// pdfSkipLiteralString returns the index of the ) that closes the literal
// string opening at i, honouring nested parentheses and backslash escapes, or
// the last index when the window ends inside it.
func pdfSkipLiteralString(buf []byte, i int) int {
	depth := 0
	for ; i < len(buf); i++ {
		switch buf[i] {
		case '\\':
			i++
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return len(buf) - 1
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
