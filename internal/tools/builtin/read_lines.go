package builtin

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// defaultReadScanBytes is how far a line-range read looks into a file to
// count its lines when Read.MaxScanBytes is unset.
const defaultReadScanBytes = 64 << 20

// readBinarySniffBytes is the prefix checked for a NUL byte, the same test
// Grep uses to skip a binary file.
const readBinarySniffBytes = 8 * 1024

// lineWindow is one line-range request: lines offset..offset+limit-1, under a
// byte limit for the returned text and a scan limit for counting the file.
type lineWindow struct {
	offset   int   // first line, from 1
	limit    int   // 0 = to the end of the file
	numbers  bool  // prefix each line with its number
	maxBytes int64 // limit on the returned lines
	maxScan  int64 // limit on bytes read to count lines; 0 = default
}

// readLines returns the requested lines of r, whole lines only, followed by
// one status line that says which lines were returned, how many the file has,
// and where to continue when the byte limit cut the result short.
//
// The file is streamed, never held whole: the lines before the window are
// counted and dropped, the window is kept up to maxBytes, and the rest is
// counted so the status line can give the total. The scan limit stops only
// that last count: it never stops the read before the window is reached.
func readLines(ctx context.Context, r io.Reader, w lineWindow) tools.Result {
	maxScan := w.maxScan
	if maxScan <= 0 {
		maxScan = defaultReadScanBytes
	}
	br := bufio.NewReaderSize(r, 64*1024)
	if head, _ := br.Peek(readBinarySniffBytes); bytes.IndexByte(head, 0) >= 0 {
		return errBusiness("Read: this looks like a binary file; a line range reads text only",
			"Call Read without offset, limit and line_numbers to get the file's bytes.")
	}

	var (
		out      bytes.Buffer
		lineNo   int   // lines seen so far
		first    int   // first line returned
		last     int   // last line returned
		next     int   // offset to continue from when cut; 0 = not cut
		scanned  int64 // bytes read
		scanCut  bool  // stopped counting at maxScan
		longNote string
	)
	wanted := func(n int) bool {
		return next == 0 && n >= w.offset && (w.limit == 0 || n < w.offset+w.limit)
	}

	for {
		if err := ctx.Err(); err != nil {
			return tools.Result{Text: err.Error(), IsError: true}
		}
		// One line, possibly in several pieces when it is longer than the
		// reader's buffer. Only a line inside the window is kept, and only up
		// to the byte limit; its full length is still counted.
		var (
			kept    []byte
			lineLen int64
			keep    = wanted(lineNo + 1)
			eof     bool
		)
		for {
			piece, err := br.ReadSlice('\n')
			lineLen += int64(len(piece))
			scanned += int64(len(piece))
			if keep && int64(len(kept)) <= w.maxBytes {
				kept = append(kept, piece...)
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					return tools.Result{Text: err.Error(), IsError: true}
				}
				eof = true
			}
			break
		}
		if lineLen == 0 && eof {
			break // nothing after the last newline
		}
		lineNo++

		if keep {
			prefix := ""
			if w.numbers {
				prefix = fmt.Sprintf("%6d\t", lineNo)
			}
			need := int64(len(prefix)) + lineLen
			switch {
			case int64(out.Len())+need <= w.maxBytes:
				out.WriteString(prefix)
				out.Write(kept)
				if first == 0 {
					first = lineNo
				}
				last = lineNo
			case first == 0:
				// The first wanted line alone is over the limit. Returning
				// nothing would leave the caller no way past it, so return
				// what fits, cut between characters, and say so.
				room := w.maxBytes - int64(len(prefix))
				if room < 0 {
					room = 0
				}
				cut := cutOnRuneBoundary(kept, room)
				out.WriteString(prefix)
				out.Write(cut)
				longNote = fmt.Sprintf("[line %d cut at %d bytes of %d]\n", lineNo, len(cut), lineLen)
				first, last, next = lineNo, lineNo, lineNo+1
			default:
				next = lineNo
			}
		}

		if eof {
			break
		}
		windowDone := next != 0 || (w.limit != 0 && lineNo >= w.offset+w.limit-1)
		if windowDone && scanned >= maxScan {
			scanCut = true
			break
		}
	}

	total := fmt.Sprintf("%d", lineNo)
	if scanCut {
		total = fmt.Sprintf("more than %d", lineNo)
	}
	if first == 0 {
		return tools.Result{Text: fmt.Sprintf("[no lines: the file has %s lines]\n", total)}
	}
	if b := out.Bytes(); len(b) > 0 && b[len(b)-1] != '\n' {
		out.WriteByte('\n') // the status line always starts on its own line
	}
	out.WriteString(longNote)
	status := fmt.Sprintf("[lines %d-%d of %s", first, last, total)
	if next != 0 {
		status += fmt.Sprintf("; cut at %s, continue with offset %d", byteLimitLabel(w.maxBytes), next)
	}
	out.WriteString(status + "]\n")
	return tools.Result{Text: out.String()}
}

// cutOnRuneBoundary returns the longest prefix of b that is at most n bytes
// and does not end inside a UTF-8 character.
func cutOnRuneBoundary(b []byte, n int64) []byte {
	if int64(len(b)) <= n {
		return b
	}
	i := int(n)
	for i > 0 && !utf8.RuneStart(b[i]) {
		i--
	}
	return b[:i]
}

// byteLimitLabel renders a byte limit the way the tool's description names it.
func byteLimitLabel(n int64) string {
	if n > 0 && n%1024 == 0 {
		return fmt.Sprintf("%d KiB", n/1024)
	}
	return fmt.Sprintf("%d bytes", n)
}
