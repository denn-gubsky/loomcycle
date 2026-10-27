package speech

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

// fuzzMaxBytes bounds a fuzzed join's output, so a header or a gap that
// asks for a huge allocation must be refused rather than honoured.
const fuzzMaxBytes = 4 << 20

// wavSeeds are the shapes the fuzzers start from: what real engines write,
// and the broken variants the parser must refuse safely.
func wavSeeds() [][]byte {
	data := samples(mono24k, 64, 1)
	good := makeWAV(mono24k, data, wavOpts{})
	absurd := append([]byte{}, good...)
	binary.LittleEndian.PutUint16(absurd[22:], 65535)
	binary.LittleEndian.PutUint32(absurd[24:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint16(absurd[32:], 0xFFFF)
	lyingData := append([]byte{}, good...)
	binary.LittleEndian.PutUint32(lyingData[40:], 0x7FFFFFFF)
	stereo8 := Format{SampleRate: 8000, Channels: 2, BitsPerSample: 8}
	return [][]byte{
		good,
		makeWAV(mono24k, data, wavOpts{streamed: true}),
		makeWAV(mono24k, data, wavOpts{before: chunk("LIST", []byte("INFOx")), after: chunk("id3 ", []byte{1, 2})}),
		makeWAV(mono24k, data, wavOpts{extensible: true}),
		makeWAV(stereo8, samples(stereo8, 9, 0), wavOpts{}),
		good[:20],
		good[:44],
		absurd,
		lyingData,
		[]byte("RIFF\xff\xff\xff\xffWAVEfmt \xff\xff\xff\xff"),
	}
}

// FuzzParseWAV — any input either is refused with ErrInvalidWAV or yields
// whole frames lying inside the input, with a consistent duration.
func FuzzParseWAV(f *testing.F) {
	for _, s := range wavSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		fm, data, err := ParseWAV(in)
		if err != nil {
			if !errors.Is(err, ErrInvalidWAV) {
				t.Fatalf("error %v is not ErrInvalidWAV", err)
			}
			return
		}
		if len(data) > len(in) || len(data)%fm.BlockAlign() != 0 {
			t.Fatalf("%d data bytes from %d input bytes, block align %d", len(data), len(in), fm.BlockAlign())
		}
		d, err := Duration(in)
		if err != nil || d != fm.duration(int64(len(data)/fm.BlockAlign())) {
			t.Fatalf("Duration = %v, %v", d, err)
		}
	})
}

// FuzzWAVJoin — joining two fuzzed pieces never panics; when it succeeds the
// output parses back in the same format, its header sizes are true, and its
// length is the inputs' frames plus the gap's, so its duration is the sum of
// the inputs' and the gap's (to within rounding of one frame each).
func FuzzWAVJoin(f *testing.F) {
	seeds := wavSeeds()
	for i, a := range seeds {
		b := seeds[(i+1)%len(seeds)]
		f.Add(a, b, uint16(200))
		f.Add(a, a, uint16(0))
	}
	f.Fuzz(func(t *testing.T, a, b []byte, gapMs uint16) {
		gap := time.Duration(gapMs) * time.Millisecond
		out, fm, err := JoinWAV([][]byte{a, b}, []time.Duration{gap}, fuzzMaxBytes)
		if err != nil {
			if gap <= MaxGap && !errors.Is(err, ErrInvalidWAV) && !errors.Is(err, ErrFormatMismatch) && !errors.Is(err, ErrTooLarge) {
				t.Fatalf("unexpected error kind: %v", err)
			}
			return
		}
		if len(out) > fuzzMaxBytes {
			t.Fatalf("output %d bytes over the %d bound", len(out), fuzzMaxBytes)
		}
		pf, data, err := ParseWAV(out)
		if err != nil || pf != fm {
			t.Fatalf("output does not parse back: %+v, %v", pf, err)
		}
		if len(data) != len(out)-wavHeaderSize ||
			binary.LittleEndian.Uint32(out[4:8]) != uint32(len(out)-8) ||
			binary.LittleEndian.Uint32(out[40:44]) != uint32(len(data)) {
			t.Fatal("output header sizes are not true")
		}
		_, da, _ := ParseWAV(a)
		_, db, _ := ParseWAV(b)
		align := fm.BlockAlign()
		wantFrames := int64(len(da)/align+len(db)/align) + fm.silenceFrames(gap)
		if got := int64(len(data) / align); got != wantFrames {
			t.Fatalf("output has %d frames, want %d", got, wantFrames)
		}
		durA, _ := Duration(a)
		durB, _ := Duration(b)
		got, _ := Duration(out)
		frame := time.Second / time.Duration(fm.SampleRate)
		if diff := got - (durA + durB + gap); diff > 3*frame || diff < -3*frame {
			t.Fatalf("duration %v, want %v ± %v", got, durA+durB+gap, 3*frame)
		}
	})
}
