package transcribe

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestPieces_CutWithOverlapBack — 25 minutes in ten-minute pieces with a
// two-second overlap: three pieces, the second and third reaching back two
// seconds, the last ending at the recording's end.
func TestPieces_CutWithOverlapBack(t *testing.T) {
	m, s := time.Minute, time.Second
	got := Pieces(25*m, 10*m, 2*s)
	want := []Piece{
		{Start: 0, From: 0, To: 10 * m},
		{Start: 10 * m, From: 10*m - 2*s, To: 20 * m},
		{Start: 20 * m, From: 20*m - 2*s, To: 25 * m},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pieces = %+v", got)
	}
	if one := Pieces(4*m, 10*m, 2*s); len(one) != 1 || one[0].To != 4*m || one[0].From != 0 {
		t.Errorf("a short recording = %+v, want one piece", one)
	}
	if Pieces(0, 10*m, 0) != nil {
		t.Error("nothing to cut must give no pieces")
	}
}

// TestTranscribe_ChunksOverlapDeduped — the overlap's speech, heard by two
// pieces, is kept once (from the earlier piece); a segment straddling the cut
// with most of it after the cut is kept from the later one, which heard it
// whole, and the earlier piece's cut-off copy of it goes; every time is on the
// recording's clock, in order.
func TestTranscribe_ChunksOverlapDeduped(t *testing.T) {
	s := time.Second
	pieces := Pieces(20*s, 10*s, 2*s) // [0,10) and [8,20) carrying from 8
	results := []Segments{
		{{Start: 0, End: 5 * s, Text: "one"}, {Start: 5 * s, End: 9 * s, Text: "two"}, {Start: 9 * s, End: 10 * s, Text: "thr"}},
		// Timed from 8 s: "two"'s end again (8–9), "three" whole (9–12), then "four".
		{{Start: 0, End: 1 * s, Text: "two (end)"}, {Start: 1 * s, End: 4 * s, Text: "three"}, {Start: 4 * s, End: 12 * s, Text: "four"}},
	}
	got := Merge(pieces, results)
	var texts []string
	for _, x := range got {
		texts = append(texts, x.Text)
	}
	want := []string{"one", "two", "three", "four"}
	if !reflect.DeepEqual(texts, want) {
		t.Fatalf("texts = %q, want %q", texts, want)
	}
	if got[2].Start != 9*s || got[3].Start != 12*s || got[3].End != 20*s {
		t.Errorf("times not on the recording's clock: %+v", got)
	}
	// A piece that was not transcribed contributes nothing.
	if n := len(Merge(pieces, results[:1])); n != 3 {
		t.Errorf("first piece only: %d segments", n)
	}
}

// TestTranscribe_UntimedPiecesOverlapDedupedByText — an engine with no
// timestamps returns each piece as one untimed segment, so the overlap is
// found by text: the words the previous piece ended on and this one starts
// with are said once, even when the cut split a word at either end; without
// such a run both texts are kept whole.
func TestTranscribe_UntimedPiecesOverlapDedupedByText(t *testing.T) {
	s := time.Second
	pieces := Pieces(90*s, 60*s, 2*s) // [0,60) and [58,90) carrying from 58
	untimed := func(text string, end time.Duration) Segments {
		return Segments{{Start: 0, End: end, Text: text, Untimed: true}}
	}
	for _, c := range []struct {
		name, first, second, want string
	}{
		{"the overlap repeated", "We met on Friday to review the plan.", "review the plan. Then we agreed on dates.", "We met on Friday to review the plan. | Then we agreed on dates."},
		{"case and punctuation differ", "and we signed the contract", "We signed the contract, finally.", "and we signed the contract | finally."},
		{"the cut split the previous piece's last word", "the budget for next quart", "for next quarter is approved", "the budget for next | quarter is approved"},
		{"the cut split this piece's first word", "the budget for next quarter is", "xt quarter is approved", "the budget for next quarter is | approved"},
		{"no shared run", "The first part ends here.", "A second part starts.", "The first part ends here. | A second part starts."},
		{"one shared word is not enough", "it was the", "the end", "it was the | the end"},
		{"the second piece only repeats", "one two three four", "three four", "one two three four"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Merge(pieces, []Segments{untimed(c.first, 60*s), untimed(c.second, 32*s)})
			var texts []string
			for _, x := range got {
				texts = append(texts, x.Text)
			}
			if strings.Join(texts, " | ") != c.want {
				t.Errorf("got %q, want %q", strings.Join(texts, " | "), c.want)
			}
			if len(got) == 2 && (got[1].Start != 60*s || got[1].End != 90*s) {
				t.Errorf("second segment at %v–%v, want 60s–90s", got[1].Start, got[1].End)
			}
		})
	}
}
