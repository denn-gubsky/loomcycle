package speech

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

var mono24k = Format{SampleRate: 24000, Channels: 1, BitsPerSample: 16}

// wavOpts shapes a test WAV the ways real engines write one.
type wavOpts struct {
	streamed   bool   // 0xFFFFFFFF in the RIFF and data sizes (Ollama, Phonon)
	before     []byte // a raw chunk (with its header) between fmt and data
	after      []byte // a raw chunk after the data (true sizes only)
	extensible bool   // WAVE_FORMAT_EXTENSIBLE with the PCM subformat
}

func chunk(id string, body []byte) []byte {
	out := make([]byte, 8, 8+len(body)+1)
	copy(out, id)
	binary.LittleEndian.PutUint32(out[4:], uint32(len(body)))
	out = append(out, body...)
	if len(body)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func fmtBody(f Format, extensible bool) []byte {
	le := binary.LittleEndian
	n := 16
	if extensible {
		n = 40
	}
	b := make([]byte, n)
	tag := uint16(formatPCM)
	if extensible {
		tag = formatExtensible
	}
	le.PutUint16(b[0:], tag)
	le.PutUint16(b[2:], uint16(f.Channels))
	le.PutUint32(b[4:], uint32(f.SampleRate))
	le.PutUint32(b[8:], uint32(f.SampleRate*f.BlockAlign()))
	le.PutUint16(b[12:], uint16(f.BlockAlign()))
	le.PutUint16(b[14:], uint16(f.BitsPerSample))
	if extensible {
		le.PutUint16(b[16:], 22)
		le.PutUint16(b[18:], uint16(f.BitsPerSample))
		copy(b[24:], pcmSubformat)
	}
	return b
}

func makeWAV(f Format, data []byte, o wavOpts) []byte {
	var body bytes.Buffer
	body.WriteString("WAVE")
	body.Write(chunk("fmt ", fmtBody(f, o.extensible)))
	body.Write(o.before)
	dataChunk := chunk("data", data)
	if o.streamed {
		binary.LittleEndian.PutUint32(dataChunk[4:], streamedSize)
	}
	body.Write(dataChunk)
	body.Write(o.after)
	out := append([]byte("RIFF\x00\x00\x00\x00"), body.Bytes()...)
	size := uint32(body.Len())
	if o.streamed {
		size = streamedSize
	}
	binary.LittleEndian.PutUint32(out[4:], size)
	return out
}

// samples is n frames of a recognisable non-zero pattern.
func samples(f Format, frames int, seed byte) []byte {
	b := make([]byte, frames*f.BlockAlign())
	for i := range b {
		b[i] = seed + byte(i%7) + 1
	}
	return b
}

// TestSpeech_WAVParsesEveryWriterShape — a plain WAV, a streamed one with
// max-value sizes, one with chunks around the data, and an extensible one
// all yield the same samples.
func TestSpeech_WAVParsesEveryWriterShape(t *testing.T) {
	data := samples(mono24k, 2400, 3) // 0.1 s
	list := chunk("LIST", []byte("INFOISFT\x05\x00\x00\x00Lavf\x00\x00"))
	odd := chunk("junk", []byte{1, 2, 3}) // odd size: a pad byte follows
	cases := map[string]wavOpts{
		"plain":                 {},
		"streamed max sizes":    {streamed: true},
		"LIST before data":      {before: list},
		"odd chunk before data": {before: odd},
		"trailing chunk honoured": {
			after: chunk("LIST", []byte("trailing metadata!")),
		},
		"extensible PCM": {extensible: true},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			f, got, err := ParseWAV(makeWAV(mono24k, data, o))
			if err != nil {
				t.Fatal(err)
			}
			if f != mono24k || !bytes.Equal(got, data) {
				t.Errorf("format %+v, %d bytes; want %+v, %d bytes", f, len(got), mono24k, len(data))
			}
			d, err := Duration(makeWAV(mono24k, data, o))
			if err != nil || d != 100*time.Millisecond {
				t.Errorf("Duration = %v, %v; want 100ms", d, err)
			}
		})
	}
}

// TestSpeech_WAVJoinIgnoresHeaderSizes — a streamed WAV's max-value sizes and
// a data size past the end are not trusted: the audio is what is present,
// cut to whole frames, and the output carries true sizes.
func TestSpeech_WAVJoinIgnoresHeaderSizes(t *testing.T) {
	data := samples(mono24k, 480, 1)
	streamed := makeWAV(mono24k, data, wavOpts{streamed: true})
	// A streamed body cut mid-frame: the half frame is dropped.
	cut := append(append([]byte{}, streamed...), 0x7f)
	// A data size that claims far more than is present.
	lying := makeWAV(mono24k, data, wavOpts{})
	binary.LittleEndian.PutUint32(lying[40:], 1<<30)

	out, f, err := JoinWAV([][]byte{streamed, cut, lying}, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if f != mono24k {
		t.Errorf("format = %+v", f)
	}
	if want := 44 + 3*len(data); len(out) != want {
		t.Fatalf("output %d bytes, want %d", len(out), want)
	}
	if got := binary.LittleEndian.Uint32(out[4:]); got != uint32(len(out)-8) {
		t.Errorf("RIFF size = %d, want %d", got, len(out)-8)
	}
	if got := binary.LittleEndian.Uint32(out[40:]); got != uint32(len(out)-44) {
		t.Errorf("data size = %d, want %d", got, len(out)-44)
	}
	if !bytes.Equal(out[44:], bytes.Repeat(data, 3)) {
		t.Error("joined samples differ from the inputs")
	}
}

// TestSpeech_WAVJoinRefusesMixedSampleRates — pieces that differ in rate,
// channels or sample size are refused, naming the piece and the difference.
func TestSpeech_WAVJoinRefusesMixedSampleRates(t *testing.T) {
	base := makeWAV(mono24k, samples(mono24k, 10, 0), wavOpts{})
	cases := []struct {
		f    Format
		want string
	}{
		{Format{SampleRate: 22050, Channels: 1, BitsPerSample: 16}, "piece 1 is 22050 Hz, piece 0 is 24000 Hz"},
		{Format{SampleRate: 24000, Channels: 2, BitsPerSample: 16}, "piece 1 has 2 channels"},
		{Format{SampleRate: 24000, Channels: 1, BitsPerSample: 24}, "piece 1 is 24-bit, piece 0 is 16-bit"},
	}
	for _, c := range cases {
		other := makeWAV(c.f, samples(c.f, 10, 0), wavOpts{})
		_, _, err := JoinWAV([][]byte{base, other}, nil, 1<<20)
		if !errors.Is(err, ErrFormatMismatch) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err = %v, want ErrFormatMismatch naming %q", c.f, err, c.want)
		}
	}
}

// TestSpeech_WAVJoinSilenceIsExact — a 200 ms gap at 24 kHz is exactly 4 800
// zero frames, placed between the pieces; 8-bit silence is 0x80.
func TestSpeech_WAVJoinSilenceIsExact(t *testing.T) {
	a := samples(mono24k, 100, 1)
	b := samples(mono24k, 50, 2)
	out, _, err := JoinWAV([][]byte{makeWAV(mono24k, a, wavOpts{}), makeWAV(mono24k, b, wavOpts{streamed: true})},
		[]time.Duration{200 * time.Millisecond}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	gap := 4800 * 2
	if len(out) != 44+len(a)+gap+len(b) {
		t.Fatalf("output %d bytes, want %d", len(out), 44+len(a)+gap+len(b))
	}
	body := out[44:]
	if !bytes.Equal(body[:len(a)], a) || !bytes.Equal(body[len(a)+gap:], b) {
		t.Error("pieces not in place around the gap")
	}
	if !bytes.Equal(body[len(a):len(a)+gap], make([]byte, gap)) {
		t.Error("gap is not zero samples")
	}
	// 150 frames at 24 kHz are 6.25 ms, plus the 200 ms gap.
	if d, err := Duration(out); err != nil || d != 206250*time.Microsecond {
		t.Errorf("Duration = %v, %v; want 206.25ms", d, err)
	}

	u8 := Format{SampleRate: 8000, Channels: 1, BitsPerSample: 8}
	out, _, err = JoinWAV([][]byte{makeWAV(u8, samples(u8, 4, 0), wavOpts{}), makeWAV(u8, samples(u8, 4, 0), wavOpts{})},
		[]time.Duration{time.Millisecond}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out[44+4:44+12], bytes.Repeat([]byte{0x80}, 8)) {
		t.Errorf("8-bit silence = %x, want 0x80 × 8", out[44+4:44+12])
	}
}

// TestSpeech_WAVJoinRoundTripDuration — the output parses back, and its
// duration is the inputs' plus the gaps'.
func TestSpeech_WAVJoinRoundTripDuration(t *testing.T) {
	stereo := Format{SampleRate: 44100, Channels: 2, BitsPerSample: 16}
	pieces := [][]byte{
		makeWAV(stereo, samples(stereo, 44100, 1), wavOpts{}),
		makeWAV(stereo, samples(stereo, 22050, 2), wavOpts{streamed: true}),
		makeWAV(stereo, samples(stereo, 4410, 3), wavOpts{extensible: true}),
	}
	gaps := []time.Duration{200 * time.Millisecond, 50 * time.Millisecond}
	var want time.Duration
	for _, p := range pieces {
		d, err := Duration(p)
		if err != nil {
			t.Fatal(err)
		}
		want += d
	}
	for _, g := range gaps {
		want += g
	}
	out, f, err := JoinWAV(pieces, gaps, 1<<24)
	if err != nil {
		t.Fatal(err)
	}
	pf, _, err := ParseWAV(out)
	if err != nil || pf != f || f != stereo {
		t.Fatalf("output parses as %+v, %v; want %+v", pf, err, stereo)
	}
	if got, _ := Duration(out); got != want || want != 1850*time.Millisecond {
		t.Errorf("duration = %v, want %v (1.85s)", got, want)
	}
}

// TestSpeech_WAVJoinBoundsTheOutput — an output over maxBytes is refused
// before anything is allocated, including one inflated only by gaps.
func TestSpeech_WAVJoinBoundsTheOutput(t *testing.T) {
	p := makeWAV(mono24k, samples(mono24k, 100, 1), wavOpts{})
	if _, _, err := JoinWAV([][]byte{p, p}, nil, 44+400); err != nil {
		t.Errorf("exact fit refused: %v", err)
	}
	if _, _, err := JoinWAV([][]byte{p, p}, nil, 44+399); !errors.Is(err, ErrTooLarge) {
		t.Errorf("one byte over = %v, want ErrTooLarge", err)
	}
	if _, _, err := JoinWAV([][]byte{p, p}, []time.Duration{MaxGap}, 1<<20); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a long gap over the bound = %v, want ErrTooLarge", err)
	}
}

// TestSpeech_WAVRefusesOddInputs — malformed inputs fail with ErrInvalidWAV
// (or a plain argument error), never a panic.
func TestSpeech_WAVRefusesOddInputs(t *testing.T) {
	good := makeWAV(mono24k, samples(mono24k, 10, 0), wavOpts{})
	absurd := makeWAV(mono24k, samples(mono24k, 10, 0), wavOpts{})
	binary.LittleEndian.PutUint16(absurd[22:], 65535)      // channels
	binary.LittleEndian.PutUint32(absurd[24:], 0xFFFFFFFF) // sample rate
	float := append([]byte{}, good...)
	binary.LittleEndian.PutUint16(float[20:], 3) // IEEE float
	badAlign := append([]byte{}, good...)
	binary.LittleEndian.PutUint16(badAlign[32:], 3)
	noData := good[:36]
	dataFirst := append([]byte("RIFF\x00\x00\x00\x00WAVE"), chunk("data", []byte{0, 0})...)
	bigFmt := append([]byte{}, good...)
	binary.LittleEndian.PutUint32(bigFmt[16:], 0xFFFFFFF0)
	bigList := append([]byte("RIFF\xff\xff\xff\xffWAVE"), append(chunk("fmt ", fmtBody(mono24k, false)), "LIST\xf0\xff\xff\xff"...)...)
	extNotPCM := makeWAV(mono24k, samples(mono24k, 10, 0), wavOpts{extensible: true})
	extNotPCM[20+24] = 3 // subformat IEEE float
	headerOnly := makeWAV(mono24k, nil, wavOpts{})

	cases := map[string][]byte{
		"empty":              nil,
		"truncated header":   good[:20],
		"not RIFF":           append([]byte("RIFX"), good[4:]...),
		"absurd fmt values":  absurd,
		"float format":       float,
		"block align lies":   badAlign,
		"no data chunk":      noData,
		"data before fmt":    dataFirst,
		"fmt size past end":  bigFmt,
		"chunk past end":     bigList,
		"extensible float":   extNotPCM,
		"no fmt (only RIFF)": []byte("RIFF\x00\x00\x00\x00WAVE"),
	}
	for name, in := range cases {
		if _, _, err := ParseWAV(in); !errors.Is(err, ErrInvalidWAV) {
			t.Errorf("%s: ParseWAV err = %v, want ErrInvalidWAV", name, err)
		}
		if _, _, err := JoinWAV([][]byte{good, in}, nil, 1<<20); !errors.Is(err, ErrInvalidWAV) {
			t.Errorf("%s: JoinWAV err = %v, want ErrInvalidWAV", name, err)
		}
	}
	if _, _, err := JoinWAV([][]byte{headerOnly}, nil, 1<<20); !errors.Is(err, ErrInvalidWAV) || !strings.Contains(err.Error(), "no audio") {
		t.Errorf("header-only piece: err = %v, want a no-audio refusal", err)
	}
	for name, call := range map[string]func() error{
		"no pieces":    func() error { _, _, err := JoinWAV(nil, nil, 1<<20); return err },
		"wrong gaps":   func() error { _, _, err := JoinWAV([][]byte{good, good}, []time.Duration{1, 2}, 1<<20); return err },
		"negative gap": func() error { _, _, err := JoinWAV([][]byte{good, good}, []time.Duration{-1}, 1<<20); return err },
		"gap too long": func() error {
			_, _, err := JoinWAV([][]byte{good, good}, []time.Duration{MaxGap + 1}, 1<<20)
			return err
		},
	} {
		if call() == nil {
			t.Errorf("%s: JoinWAV succeeded, want an error", name)
		}
	}
}
