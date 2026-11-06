package transcribe

import (
	"strings"
	"time"
	"unicode"
)

// Piece is one speech request's stretch of a recording. A long recording is
// cut into pieces of a fixed length, each but the first reaching back by an
// overlap so a word spoken across a cut is heard whole by one of them.
type Piece struct {
	// Start is where the piece's own part begins: speech before it belongs to
	// the previous piece.
	Start time.Duration
	// From and To are the audio the request carries; From is Start less the
	// overlap (0 for the first piece).
	From, To time.Duration
}

// Length is the audio the request carries.
func (p Piece) Length() time.Duration { return p.To - p.From }

// Pieces cuts total into pieces of length, each but the first starting
// overlap early. One piece when total fits in length. Assumes length >
// overlap >= 0 and total > 0 (config validation and the caller hold both).
func Pieces(total, length, overlap time.Duration) []Piece {
	if total <= 0 || length <= 0 {
		return nil
	}
	overlap = min(max(overlap, 0), length/2)
	var out []Piece
	for start := time.Duration(0); start < total; start += length {
		out = append(out, Piece{Start: start, From: max(start-overlap, 0), To: min(start+length, total)})
	}
	return out
}

// Merge puts the pieces' segments on the recording's clock and drops the
// overlap's duplicates. Each piece's segments are timed from its From.
//
//   - A segment of a later piece whose middle falls before the piece's Start
//     was heard in the overlap, and the previous piece already has it.
//   - One that straddles the cut with its middle after it is kept, as the
//     previous piece heard only its beginning; and the previous piece's
//     segments from that start on (the cut-off part it heard) are dropped,
//     so the transcript keeps the whole utterance once and stays in time
//     order.
//   - A piece whose engine returned no timestamps (one Untimed segment) has
//     its overlap dropped by text: see mergeUntimed.
//
// results[i] belongs to pieces[i]; a missing or nil entry (a piece not
// transcribed) contributes nothing.
func Merge(pieces []Piece, results []Segments) Segments {
	var out Segments
	for i, p := range pieces {
		if i >= len(results) {
			break
		}
		if i > 0 && len(results[i]) == 1 && results[i][0].Untimed {
			out = mergeUntimed(out, p, results[i][0])
			continue
		}
		var kept Segments
		for _, s := range results[i] {
			s.Start += p.From
			s.End += p.From
			if i > 0 && s.Start+(s.End-s.Start)/2 < p.Start {
				continue
			}
			kept = append(kept, s)
		}
		if i > 0 && len(kept) > 0 && kept[0].Start < p.Start {
			n := len(out)
			for n > 0 && out[n-1].Start >= kept[0].Start {
				n--
			}
			out = out[:n]
		}
		out = append(out, kept...)
	}
	return out
}

// maxSeamWords bounds the overlap searched for by text: a few seconds of
// speech is a dozen words, and the default overlap is 2 s.
const maxSeamWords = 60

// mergeUntimed appends an untimed piece's text after out. The piece heard the
// overlap again, so its text starts with what the previous piece ended on.
// The longest run of words (two or more, compared without case and
// punctuation) that ends the previous text and starts this one is said once.
// The cut may fall inside a word, so the previous text's last word and this
// text's first word may be fragments: each may be skipped in the match, and
// a skipped fragment of the previous text is removed with the repeat. Without
// such a run, both texts are kept whole: a repeated word is better than a
// lost one.
func mergeUntimed(out Segments, p Piece, s Segment) Segments {
	s.Start, s.End = p.Start, p.From+s.End
	if s.End < s.Start {
		s.End = s.Start
	}
	if n := len(out); n > 0 && out[n-1].Untimed {
		prev, next := strings.Fields(out[n-1].Text), strings.Fields(s.Text)
		if dropPrev, skipNext, k := seam(prev, next); k > 0 {
			out[n-1].Text = strings.Join(prev[:len(prev)-dropPrev], " ")
			s.Text = strings.Join(next[skipNext+k:], " ")
			if out[n-1].Text == "" {
				out = out[:n-1]
			}
		}
	}
	if s.Text == "" {
		return out
	}
	return append(out, s)
}

// seam finds the overlap: the longest k >= 2 such that prev's last k words
// (before dropPrev fragment words) equal next's k words after skipNext
// fragment words.
func seam(prev, next []string) (dropPrev, skipNext, k int) {
	for n := min(maxSeamWords, len(prev), len(next)); n >= 2; n-- {
		for _, dp := range []int{0, 1} {
			for _, sn := range []int{0, 1} {
				if dp+n > len(prev) || sn+n > len(next) {
					continue
				}
				if sameWords(prev[len(prev)-dp-n:len(prev)-dp], next[sn:sn+n]) {
					return dp, sn, n
				}
			}
		}
	}
	return 0, 0, 0
}

func sameWords(a, b []string) bool {
	for i := range a {
		if wordKey(a[i]) != wordKey(b[i]) || wordKey(a[i]) == "" {
			return false
		}
	}
	return true
}

// wordKey is a word lower-cased with everything but letters and digits removed.
func wordKey(w string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, w)
}
