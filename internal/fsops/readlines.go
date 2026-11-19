package fsops

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// DefaultReadScanBytes is how far a line-range read looks into a file to
// count its lines when LineWindow.MaxScan is unset.
const DefaultReadScanBytes = 64 << 20

// DefaultReadSeekBytes is how far a line-range read goes to reach its first
// line when LineWindow.MaxSeek is unset. Without it an offset past the end of
// a multi-gigabyte file read the whole file on every call.
const DefaultReadSeekBytes = 1 << 30

// readBinarySniffBytes is the prefix checked for a NUL byte, the same test
// Grep uses to skip a binary file.
const readBinarySniffBytes = 8 * 1024

// ErrBinaryLineRange is a line-range read of a file that looks binary (a NUL
// byte in its first 8 KiB). A line range reads text only.
var ErrBinaryLineRange = errors.New("this looks like a binary file; a line range reads text only")

// LineWindow is one line-range request: lines Offset..Offset+Limit-1, under a
// byte limit for the returned text and a scan limit for counting the file.
// The caller validates Offset >= 1 and Limit >= 0.
type LineWindow struct {
	Offset   int   // first line, from 1
	Limit    int   // 0 = to the end of the file
	Numbers  bool  // prefix each line with its number
	MaxBytes int64 // limit on the returned lines
	MaxScan  int64 // limit on bytes read to count lines; 0 = DefaultReadScanBytes
	MaxSeek  int64 // limit on bytes read to reach Offset; 0 = DefaultReadSeekBytes
}

// ReadLines returns the requested lines of r, whole lines only, followed by
// one status line that says which lines were returned, how many the file has,
// and where to continue when the byte limit cut the result short.
//
// Bounds: the file is streamed, never held whole. The lines before the window
// are counted and dropped (at most MaxSeek bytes), the window is kept up to
// MaxBytes, and the rest is counted (at most MaxScan bytes) so the status line
// can give the total. Both limits, and ctx, are checked inside a line as well
// as between lines, so one long line with no newline cannot get past them.
//
// Errors: ErrBinaryLineRange, ctx's error, or r's.
func ReadLines(ctx context.Context, r io.Reader, w LineWindow) (string, error) {
	maxScan := w.MaxScan
	if maxScan <= 0 {
		maxScan = DefaultReadScanBytes
	}
	maxSeek := w.MaxSeek
	if maxSeek <= 0 {
		maxSeek = DefaultReadSeekBytes
	}
	br := bufio.NewReaderSize(r, 64*1024)
	if head, _ := br.Peek(readBinarySniffBytes); bytes.IndexByte(head, 0) >= 0 {
		return "", ErrBinaryLineRange
	}

	var (
		out      bytes.Buffer
		lineNo   int   // lines seen so far
		first    int   // first line returned
		last     int   // last line returned
		next     int   // offset to continue from when cut; 0 = not cut
		scanned  int64 // bytes read
		scanCut  bool  // stopped counting at maxScan
		seekCut  bool  // stopped short of the window at maxSeek
		longNote string
	)
	// The window is compared as a distance from offset, never as
	// offset+limit: a limit near the largest int wraps that sum negative.
	wanted := func(n int) bool {
		return next == 0 && n >= w.Offset && (w.Limit == 0 || n-w.Offset < w.Limit)
	}

	for {
		// One line, possibly in several pieces when it is longer than the
		// reader's buffer. Only a line inside the window is kept, and only up
		// to the byte limit; its full length is still counted.
		var (
			kept    []byte
			lineLen int64
			keep    = wanted(lineNo + 1)
			before  = lineNo+1 < w.Offset // still on the way to the window
			eof     bool
			stopped bool
		)
		// A line that is not kept is either before the window or after it
		// (the window is done): the first is bounded by maxSeek, the second
		// by maxScan.
		stopAt := maxScan
		if before {
			stopAt = maxSeek
		}
		for {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if !keep && scanned >= stopAt {
				stopped = true
				break
			}
			piece, err := br.ReadSlice('\n')
			lineLen += int64(len(piece))
			scanned += int64(len(piece))
			if keep && int64(len(kept)) <= w.MaxBytes {
				kept = append(kept, piece...)
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					return "", err
				}
				eof = true
			}
			break
		}
		if stopped {
			// The line in hand is not counted: it was not read to its end.
			if before {
				seekCut = true
			} else {
				scanCut = true
			}
			break
		}
		if lineLen == 0 && eof {
			break // nothing after the last newline
		}
		lineNo++

		if keep {
			prefix := ""
			if w.Numbers {
				prefix = fmt.Sprintf("%6d\t", lineNo)
			}
			need := int64(len(prefix)) + lineLen
			switch {
			case int64(out.Len())+need <= w.MaxBytes:
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
				room := w.MaxBytes - int64(len(prefix))
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
	}

	total := fmt.Sprintf("%d", lineNo)
	if scanCut || seekCut {
		total = fmt.Sprintf("more than %d", lineNo)
	}
	if seekCut {
		return fmt.Sprintf("[no lines: line %d is past the first %d bytes of the file, which is as far as Read goes to find a line; the file has %s lines]\n", w.Offset, maxSeek, total), nil
	}
	if first == 0 {
		return fmt.Sprintf("[no lines: the file has %s lines]\n", total), nil
	}
	if b := out.Bytes(); len(b) > 0 && b[len(b)-1] != '\n' {
		out.WriteByte('\n') // the status line always starts on its own line
	}
	out.WriteString(longNote)
	status := fmt.Sprintf("[lines %d-%d of %s", first, last, total)
	if next != 0 {
		status += fmt.Sprintf("; cut at %s, continue with offset %d", byteLimitLabel(w.MaxBytes), next)
	}
	out.WriteString(status + "]\n")
	return out.String(), nil
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
