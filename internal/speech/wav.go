package speech

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidWAV matches every refusal of an input that is not usable PCM WAV.
var ErrInvalidWAV = errors.New("speech: not a usable PCM WAV")

// ErrFormatMismatch matches a join of pieces whose sample rate, channel count
// or sample size differ.
var ErrFormatMismatch = errors.New("speech: audio pieces differ in format")

// ErrTooLarge matches a join whose output would pass its byte bound.
var ErrTooLarge = errors.New("speech: joined audio too large")

// MaxGap is the longest silence JoinWAV inserts between two pieces. A pause
// is a fraction of a second; the bound keeps the frame arithmetic far from
// overflow whatever the sample rate.
const MaxGap = time.Minute

const (
	wavHeaderSize = 44
	// streamedSize is the size field a streaming writer leaves when it does
	// not know the length yet: Ollama's and Phonon's servers write it in both
	// the RIFF and the data chunk (measured).
	streamedSize = 0xFFFFFFFF
	// riffMaxBody is the most data a canonical WAV's 32-bit RIFF size can
	// describe (RIFF size = 36 + data size).
	riffMaxBody = 0xFFFFFFFF - 36

	formatPCM        = 1
	formatExtensible = 0xFFFE
	// pcmSubformat is KSDATAFORMAT_SUBTYPE_PCM, the GUID a
	// WAVE_FORMAT_EXTENSIBLE fmt chunk carries for integer PCM.
	pcmSubformat = "\x01\x00\x00\x00\x00\x00\x10\x00\x80\x00\x00\xaa\x00\x38\x9b\x71"

	maxChannels   = 8
	minSampleRate = 1000
	maxSampleRate = 384000
)

// Format is the shape of PCM audio: what every joined piece must share.
type Format struct {
	SampleRate    int // frames per second
	Channels      int
	BitsPerSample int // 8, 16, 24 or 32
}

// BlockAlign is the size of one frame in bytes.
func (f Format) BlockAlign() int { return f.Channels * f.BitsPerSample / 8 }

// duration is how long frames frames last, without overflow for any frame
// count a byte slice can hold.
func (f Format) duration(frames int64) time.Duration {
	rate := int64(f.SampleRate)
	return time.Duration(frames/rate)*time.Second + time.Duration(frames%rate)*time.Second/time.Duration(rate)
}

// silenceFrames is gap in whole frames, rounded to the nearest frame. gap is
// at most MaxGap, so the product cannot overflow.
func (f Format) silenceFrames(gap time.Duration) int64 {
	return (gap.Nanoseconds()*int64(f.SampleRate) + int64(time.Second)/2) / int64(time.Second)
}

// ParseWAV reads a RIFF/WAVE file's format and returns its PCM samples (a
// sub-slice of wav, never a copy).
//
// The size fields are never trusted to allocate or to read: a streaming
// writer leaves 0xFFFFFFFF in them (Ollama, Phonon), so the RIFF size is
// ignored and a data chunk whose size is 0xFFFFFFFF, or larger than what
// follows its header, runs to the end of the input. A declared data size
// that fits is honoured, so a trailing chunk after the samples (speaches
// writes true sizes) is not read as audio. The samples are cut down to whole
// frames. Chunks before the data (LIST, fact, …) are skipped.
//
// Accepted: format 1 (PCM) and WAVE_FORMAT_EXTENSIBLE with the PCM subformat;
// 1–8 channels; 1 000–384 000 Hz; 8, 16, 24 or 32 bits; a block align that
// matches. The fmt chunk must come before the data chunk.
func ParseWAV(wav []byte) (Format, []byte, error) {
	if len(wav) < 12 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return Format{}, nil, fmt.Errorf("%w: no RIFF/WAVE header", ErrInvalidWAV)
	}
	var f Format
	haveFmt := false
	for off := 12; ; {
		if len(wav)-off < 8 {
			if !haveFmt {
				return Format{}, nil, fmt.Errorf("%w: no fmt chunk", ErrInvalidWAV)
			}
			return Format{}, nil, fmt.Errorf("%w: no data chunk", ErrInvalidWAV)
		}
		id := string(wav[off : off+4])
		size := uint64(binary.LittleEndian.Uint32(wav[off+4 : off+8]))
		body := off + 8
		remaining := uint64(len(wav) - body)
		switch id {
		case "fmt ":
			if size < 16 || size > remaining {
				return Format{}, nil, fmt.Errorf("%w: fmt chunk of %d bytes", ErrInvalidWAV, size)
			}
			var err error
			if f, err = parseFmt(wav[body : body+int(size)]); err != nil {
				return Format{}, nil, err
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return Format{}, nil, fmt.Errorf("%w: data chunk before the fmt chunk", ErrInvalidWAV)
			}
			n := remaining
			if size != streamedSize && size <= remaining {
				n = size
			}
			n -= n % uint64(f.BlockAlign())
			return f, wav[body : body+int(n)], nil
		default:
			if size > remaining {
				return Format{}, nil, fmt.Errorf("%w: %q chunk runs past the end", ErrInvalidWAV, id)
			}
		}
		// Chunks are word-aligned: an odd size is followed by a pad byte.
		next := uint64(body) + size + size&1
		off = int(min(next, uint64(len(wav))))
	}
}

func parseFmt(c []byte) (Format, error) {
	tag := binary.LittleEndian.Uint16(c[0:2])
	f := Format{
		Channels:      int(binary.LittleEndian.Uint16(c[2:4])),
		SampleRate:    int(binary.LittleEndian.Uint32(c[4:8])),
		BitsPerSample: int(binary.LittleEndian.Uint16(c[14:16])),
	}
	blockAlign := int(binary.LittleEndian.Uint16(c[12:14]))
	switch tag {
	case formatPCM:
	case formatExtensible:
		if len(c) < 40 || string(c[24:40]) != pcmSubformat {
			return Format{}, fmt.Errorf("%w: extensible format is not integer PCM", ErrInvalidWAV)
		}
	default:
		return Format{}, fmt.Errorf("%w: format tag %#x is not PCM", ErrInvalidWAV, tag)
	}
	switch {
	case f.Channels < 1 || f.Channels > maxChannels:
		return Format{}, fmt.Errorf("%w: %d channels", ErrInvalidWAV, f.Channels)
	case f.SampleRate < minSampleRate || f.SampleRate > maxSampleRate:
		return Format{}, fmt.Errorf("%w: sample rate %d Hz", ErrInvalidWAV, f.SampleRate)
	case f.BitsPerSample != 8 && f.BitsPerSample != 16 && f.BitsPerSample != 24 && f.BitsPerSample != 32:
		return Format{}, fmt.Errorf("%w: %d bits per sample", ErrInvalidWAV, f.BitsPerSample)
	case blockAlign != f.BlockAlign():
		return Format{}, fmt.Errorf("%w: block align %d, want %d", ErrInvalidWAV, blockAlign, f.BlockAlign())
	}
	return f, nil
}

// Duration is how long a WAV's audio lasts, read by ParseWAV's rules.
func Duration(wav []byte) (time.Duration, error) {
	f, data, err := ParseWAV(wav)
	if err != nil {
		return 0, err
	}
	return f.duration(int64(len(data) / f.BlockAlign())), nil
}

// JoinWAV joins WAV pieces into one canonical PCM WAV (a 44-byte header with
// true sizes) with gaps[i] of silence after pieces[i]. gaps is empty (no
// pauses) or one shorter than pieces; each gap is 0 to MaxGap and becomes
// whole frames of silence. Every piece must parse (ParseWAV), carry some
// audio — an engine answer with none is a failed call, not a sentence to
// skip — and share the first piece's format, or the error names the piece
// and the difference (ErrFormatMismatch).
//
// The output is at most maxBytes, checked before anything is allocated; the
// allocation is the exact output size, computed from the samples actually
// present and the gaps, never from a header field. The output is written as
// format 1 (PCM) even when a piece was WAVE_FORMAT_EXTENSIBLE.
func JoinWAV(pieces [][]byte, gaps []time.Duration, maxBytes int64) ([]byte, Format, error) {
	if len(pieces) == 0 {
		return nil, Format{}, errors.New("speech: no audio pieces to join")
	}
	if len(gaps) != 0 && len(gaps) != len(pieces)-1 {
		return nil, Format{}, fmt.Errorf("speech: %d gaps for %d pieces, want none or %d", len(gaps), len(pieces), len(pieces)-1)
	}
	for i, g := range gaps {
		if g < 0 || g > MaxGap {
			return nil, Format{}, fmt.Errorf("speech: gap %d of %v is outside 0..%v", i, g, MaxGap)
		}
	}
	var f Format
	datas := make([][]byte, len(pieces))
	for i, p := range pieces {
		pf, data, err := ParseWAV(p)
		if err != nil {
			return nil, Format{}, fmt.Errorf("piece %d: %w", i, err)
		}
		if len(data) == 0 {
			return nil, Format{}, fmt.Errorf("piece %d: %w: no audio", i, ErrInvalidWAV)
		}
		if i == 0 {
			f = pf
		} else if err := sameFormat(f, pf, i); err != nil {
			return nil, Format{}, err
		}
		datas[i] = data
	}

	align := int64(f.BlockAlign())
	var total int64
	for _, d := range datas {
		total += int64(len(d))
	}
	silence := make([]int64, len(gaps))
	for i, g := range gaps {
		silence[i] = f.silenceFrames(g) * align
		total += silence[i]
	}
	if total > riffMaxBody || wavHeaderSize+total > maxBytes {
		return nil, Format{}, fmt.Errorf("%w: %d bytes of audio, limit %d", ErrTooLarge, wavHeaderSize+total, maxBytes)
	}

	out := make([]byte, wavHeaderSize, wavHeaderSize+total)
	putHeader(out, f, uint32(total))
	for i, d := range datas {
		out = append(out, d...)
		if i < len(silence) && silence[i] > 0 {
			start := len(out)
			// The capacity make reserved is zeroed: extending into it is
			// silence for signed samples. 8-bit PCM is unsigned, centred on
			// 0x80.
			out = out[:start+int(silence[i])]
			if f.BitsPerSample == 8 {
				for j := start; j < len(out); j++ {
					out[j] = 0x80
				}
			}
		}
	}
	return out, f, nil
}

func sameFormat(want, got Format, i int) error {
	switch {
	case got.SampleRate != want.SampleRate:
		return fmt.Errorf("%w: piece %d is %d Hz, piece 0 is %d Hz", ErrFormatMismatch, i, got.SampleRate, want.SampleRate)
	case got.Channels != want.Channels:
		return fmt.Errorf("%w: piece %d has %d channels, piece 0 has %d", ErrFormatMismatch, i, got.Channels, want.Channels)
	case got.BitsPerSample != want.BitsPerSample:
		return fmt.Errorf("%w: piece %d is %d-bit, piece 0 is %d-bit", ErrFormatMismatch, i, got.BitsPerSample, want.BitsPerSample)
	}
	return nil
}

// putHeader writes a canonical 44-byte PCM WAV header for dataSize bytes of
// samples into h.
func putHeader(h []byte, f Format, dataSize uint32) {
	le := binary.LittleEndian
	copy(h[0:4], "RIFF")
	le.PutUint32(h[4:8], 36+dataSize)
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	le.PutUint32(h[16:20], 16)
	le.PutUint16(h[20:22], formatPCM)
	le.PutUint16(h[22:24], uint16(f.Channels))
	le.PutUint32(h[24:28], uint32(f.SampleRate))
	le.PutUint32(h[28:32], uint32(f.SampleRate*f.BlockAlign()))
	le.PutUint16(h[32:34], uint16(f.BlockAlign()))
	le.PutUint16(h[34:36], uint16(f.BitsPerSample))
	copy(h[36:40], "data")
	le.PutUint32(h[40:44], dataSize)
}
