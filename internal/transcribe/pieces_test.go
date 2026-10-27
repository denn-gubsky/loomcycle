package transcribe

import (
	"reflect"
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
