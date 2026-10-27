package transcribe

import "time"

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
//
// results[i] belongs to pieces[i]; a missing or nil entry (a piece not
// transcribed) contributes nothing.
func Merge(pieces []Piece, results []Segments) Segments {
	var out Segments
	for i, p := range pieces {
		if i >= len(results) {
			break
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
