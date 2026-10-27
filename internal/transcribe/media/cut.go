package media

import (
	"encoding/binary"
	"errors"
	"io"
	"time"
)

// Cut returns the stretch [from, to) of a WAV file as a WAV of its own: a
// header carrying the original fmt chunk, then the samples, read from r on
// demand (nothing is copied). Both ends are rounded down to a whole sample
// frame and clamped to the data.
func Cut(r io.ReaderAt, lay *WAVLayout, from, to time.Duration) (io.ReaderAt, int64, error) {
	if lay == nil || lay.ByteRate <= 0 || lay.BlockAlign <= 0 || len(lay.Fmt) < 16 {
		return nil, 0, errors.New("media: not a WAV layout that can be cut")
	}
	at := func(d time.Duration) int64 {
		b := int64(d.Seconds()*float64(lay.ByteRate)) / int64(lay.BlockAlign) * int64(lay.BlockAlign)
		return min(max(b, 0), lay.DataSize/int64(lay.BlockAlign)*int64(lay.BlockAlign))
	}
	start, end := at(from), at(to)
	if end <= start {
		return nil, 0, errors.New("media: the stretch to cut is empty")
	}
	n := end - start
	fmtLen := len(lay.Fmt)
	pad := fmtLen % 2
	h := make([]byte, 0, 12+8+fmtLen+pad+8)
	h = append(h, "RIFF"...)
	h = binary.LittleEndian.AppendUint32(h, uint32(4+8+fmtLen+pad+8+int(n)))
	h = append(h, "WAVE"...)
	h = append(h, "fmt "...)
	h = binary.LittleEndian.AppendUint32(h, uint32(fmtLen))
	h = append(h, lay.Fmt...)
	if pad == 1 {
		h = append(h, 0)
	}
	h = append(h, "data"...)
	h = binary.LittleEndian.AppendUint32(h, uint32(n))
	return joined{head: h, body: io.NewSectionReader(r, lay.DataOffset+start, n)}, int64(len(h)) + n, nil
}

// HeaderBytes is the most a piece's header adds to its samples, for sizing
// pieces under a request cap.
func HeaderBytes(lay *WAVLayout) int64 { return int64(12 + 8 + len(lay.Fmt) + 1 + 8) }

// joined reads head, then body, as one stream.
type joined struct {
	head []byte
	body *io.SectionReader
}

func (j joined) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("media: negative offset")
	}
	n := 0
	if off < int64(len(j.head)) {
		n = copy(p, j.head[off:])
		if n == len(p) {
			return n, nil
		}
		off = int64(len(j.head))
	}
	m, err := j.body.ReadAt(p[n:], off-int64(len(j.head)))
	return n + m, err
}
