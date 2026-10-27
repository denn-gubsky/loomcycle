package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"
)

// WAVFixture is a PCM WAV of the given rate and channels holding seconds of
// a byte pattern (16-bit samples).
func wavFixture(rate, channels int, seconds float64) []byte {
	block := 2 * channels
	data := make([]byte, int(seconds*float64(rate))*block)
	for i := range data {
		data[i] = byte(i * 7)
	}
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(4+8+16+8+len(data)))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint16(channels))
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate))
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate*block))
	_ = binary.Write(&b, binary.LittleEndian, uint16(block))
	_ = binary.Write(&b, binary.LittleEndian, uint16(16))
	// A LIST chunk before the data, as many writers put one.
	b.WriteString("LIST")
	_ = binary.Write(&b, binary.LittleEndian, uint32(3))
	b.WriteString("abc\x00")
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

func box(typ string, body ...[]byte) []byte {
	var b bytes.Buffer
	n := 8
	for _, x := range body {
		n += len(x)
	}
	_ = binary.Write(&b, binary.BigEndian, uint32(n))
	b.WriteString(typ)
	for _, x := range body {
		b.Write(x)
	}
	return b.Bytes()
}

func mvhd0(scale, length uint32) []byte {
	b := make([]byte, 100)
	binary.BigEndian.PutUint32(b[12:], scale)
	binary.BigEndian.PutUint32(b[16:], length)
	return box("mvhd", b)
}

func hdlr(kind string) []byte {
	b := make([]byte, 24)
	copy(b[8:], kind)
	return box("hdlr", b)
}

func mp4Fixture(brand string, tracks ...string) []byte {
	var traks [][]byte
	traks = append(traks, mvhd0(1000, 65500))
	for _, k := range tracks {
		traks = append(traks, box("trak", box("mdia", hdlr(k))))
	}
	return append(box("ftyp", []byte(brand), []byte{0, 0, 0, 0}, []byte("isom")), append(box("moov", traks...), box("mdat", make([]byte, 64))...)...)
}

func oggPage(granule int64, packet []byte) []byte {
	h := make([]byte, 27)
	copy(h, "OggS")
	binary.LittleEndian.PutUint64(h[6:], uint64(granule))
	h[26] = 1
	return append(append(h, byte(len(packet))), packet...)
}

func opusFixture(seconds float64) []byte {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8], head[9] = 1, 1
	binary.LittleEndian.PutUint16(head[10:], 312)
	binary.LittleEndian.PutUint32(head[12:], 16000)
	out := oggPage(0, head)
	out = append(out, make([]byte, 5000)...)
	return append(out, oggPage(int64(seconds*48000)+312, []byte("audio"))...)
}

func mp3Fixture(xingFrames int) []byte {
	id3 := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 10}
	id3 = append(id3, make([]byte, 10)...)
	frame := make([]byte, 417)
	copy(frame, []byte{0xFF, 0xFB, 0x90, 0x64}) // MPEG-1 layer III, 128 kbit/s, 44.1 kHz, stereo
	if xingFrames > 0 {
		copy(frame[4+32:], "Xing")
		binary.BigEndian.PutUint32(frame[4+32+4:], 1)
		binary.BigEndian.PutUint32(frame[4+32+8:], uint32(xingFrames))
	}
	out := append(id3, frame...)
	// 16 000 bytes of further frames at 128 kbit/s: one second.
	return append(out, make([]byte, 16000-len(frame))...)
}

func flacFixture(rate int, samples int64) []byte {
	b := []byte("fLaC")
	b = append(b, 0x80, 0, 0, 34)
	si := make([]byte, 34)
	si[10] = byte(rate >> 12)
	si[11] = byte(rate >> 4)
	si[12] = byte(rate<<4) | 0x02 // 2 channels - 1 = 1, bits per sample high bit 0
	si[13] = 0xF0 | byte(samples>>32&0x0F)
	binary.BigEndian.PutUint32(si[14:], uint32(samples))
	return append(b, si...)
}

func probe(t *testing.T, data []byte) Info {
	t.Helper()
	info, err := Probe(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	return info
}

// TestProbe_ReadsDurationFromEachContainer — each container's header gives
// its length and the type it is sent as.
func TestProbe_ReadsDurationFromEachContainer(t *testing.T) {
	for _, c := range []struct {
		name      string
		data      []byte
		format    string
		mediaType string
		dur       time.Duration
		estimated bool
		video     bool
	}{
		{"wav", wavFixture(16000, 1, 2.5), "wav", "audio/wav", 2500 * time.Millisecond, false, false},
		{"flac", flacFixture(44100, 441000*3), "flac", "audio/flac", 30 * time.Second, false, false},
		{"m4a", mp4Fixture("M4A ", "soun"), "mp4", "audio/mp4", 65500 * time.Millisecond, false, false},
		{"mp4 video", mp4Fixture("isom", "vide", "soun"), "mp4", "video/mp4", 65500 * time.Millisecond, false, true},
		{"opus", opusFixture(30), "ogg", "audio/ogg", 30 * time.Second, false, false},
		{"mp3 xing", mp3Fixture(1000), "mp3", "audio/mpeg", 26122448979, false, false},
		{"mp3 cbr", mp3Fixture(0), "mp3", "audio/mpeg", time.Second, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			info := probe(t, c.data)
			if info.Format != c.format || info.MediaType != c.mediaType || info.Estimated != c.estimated || info.Video != c.video {
				t.Errorf("info = %+v", info)
			}
			if d := info.Duration - c.dur; d < -time.Millisecond || d > time.Millisecond {
				t.Errorf("duration = %v, want %v", info.Duration, c.dur)
			}
		})
	}
}

// TestProbe_RefusesOrSaysUnknown — an unrecognised file is ErrUnknown, a
// WebM is recognised without a length, and a damaged MP4 box is an error
// rather than a read past the file.
func TestProbe_RefusesOrSaysUnknown(t *testing.T) {
	if _, err := Probe(bytes.NewReader([]byte("hello world, not audio")), 22); !errors.Is(err, ErrUnknown) {
		t.Errorf("text: err = %v", err)
	}
	webm := []byte{0x1A, 0x45, 0xDF, 0xA3, 0, 0, 0, 0}
	if info := probe(t, webm); info.Format != "webm" || info.Duration != 0 {
		t.Errorf("webm = %+v", info)
	}
	bad := mp4Fixture("M4A ", "soun")
	binary.BigEndian.PutUint32(bad[len(box("ftyp", []byte("M4A "), []byte{0, 0, 0, 0}, []byte("isom"))):], 1<<30)
	if _, err := Probe(bytes.NewReader(bad), int64(len(bad))); err == nil {
		t.Error("a moov box larger than the file was accepted")
	}
	// A box chain that never ends is bounded.
	var chain []byte
	chain = append(chain, box("ftyp", []byte("isom"), make([]byte, 4))...)
	for range maxBoxes + 10 {
		chain = append(chain, box("free")...)
	}
	if _, err := Probe(bytes.NewReader(chain), int64(len(chain))); err == nil {
		t.Error("an unbounded box walk was accepted")
	}
}

// TestCut_WAVPieceIsAWAVOfTheStretch — a cut is a WAV whose header reads back
// with its own length and whose samples are exactly the stretch's.
func TestCut_WAVPieceIsAWAVOfTheStretch(t *testing.T) {
	src := wavFixture(16000, 1, 5)
	info := probe(t, src)
	piece, n, err := Cut(bytes.NewReader(src), info.WAV, time.Second, 2500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(io.NewSectionReader(piece, 0, n))
	if err != nil || int64(len(b)) != n {
		t.Fatalf("read %d of %d: %v", len(b), n, err)
	}
	got := probe(t, b)
	if got.Duration != 1500*time.Millisecond || got.WAV.SampleRate != 16000 {
		t.Errorf("piece = %+v", got)
	}
	want := src[info.WAV.DataOffset+32000 : info.WAV.DataOffset+32000+48000]
	if !bytes.Equal(b[got.WAV.DataOffset:], want) {
		t.Error("the piece's samples are not the stretch's")
	}
	// Past the end clamps; an empty stretch is an error.
	if _, n, err := Cut(bytes.NewReader(src), info.WAV, 4*time.Second, time.Hour); err != nil || n != HeaderBytes(info.WAV)-1+32000 {
		t.Errorf("clamped cut: %d %v", n, err)
	}
	if _, _, err := Cut(bytes.NewReader(src), info.WAV, 6*time.Second, 7*time.Second); err == nil {
		t.Error("a stretch past the end was cut")
	}
}
