package media

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

// TestProbe_ID3SizePastTheEndDoesNotPanic — an ID3 tag whose declared size
// runs past the end of the file is a damaged or hostile header, not a reason
// to allocate a negative buffer.
func TestProbe_ID3SizePastTheEndDoesNotPanic(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("ID3\x03\x00\x00\x7f\x7f\x7f\x7f\xff\xfb\x90\x64"),
		append([]byte("ID3\x03\x00\x10\x00\x00\x00\x02"), make([]byte, 4)...), // footer flag pushes past the end
	} {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("Probe(%q) panicked: %v", data, p)
				}
			}()
			_, _ = Probe(bytes.NewReader(data), int64(len(data)))
		}()
	}
}

// TestProbe_WAVLengthFromItsSamplesNotItsByteRateField — the byte rate a WAV
// header declares is not what a decoder plays it by (sample rate × block
// align is), so a header claiming a huge byte rate must not shrink the
// recording's length under the run's caps: it is refused, as is a block
// align that does not match the channels and sample width.
func TestProbe_WAVLengthFromItsSamplesNotItsByteRateField(t *testing.T) {
	honest := wavFixture(16000, 1, 60)
	lie := append([]byte(nil), honest...)
	binary.LittleEndian.PutUint32(lie[28:], 1_000_000_000) // fmt byte rate
	info, err := Probe(bytes.NewReader(lie), int64(len(lie)))
	if err == nil && info.Duration < time.Minute {
		t.Fatalf("a lying byte rate shortened a 60 s recording to %v", info.Duration)
	}

	misaligned := append([]byte(nil), honest...)
	binary.LittleEndian.PutUint16(misaligned[32:], 64)               // block align 64 for 16-bit mono
	binary.LittleEndian.PutUint32(misaligned[28:], uint32(16000*64)) // and a byte rate to match it
	info, err = Probe(bytes.NewReader(misaligned), int64(len(misaligned)))
	if err == nil && info.Duration < time.Minute {
		t.Fatalf("a block align above the sample width shortened a 60 s recording to %v", info.Duration)
	}
}

// TestBound_AHeaderCanOnlyRaiseTheLength — a compressed container's length
// is the producer's claim. The length the caps judge it by is at least its
// size at the lowest bit rate taken to be plausible, so an MP4 claiming one
// second for a megabyte of audio is counted as at least 1000 s; a longer
// honest claim stands, and a cut-able WAV is its measured length.
func TestBound_AHeaderCanOnlyRaiseTheLength(t *testing.T) {
	m4a := append(mp4Fixture("M4A ", "soun"), make([]byte, 1_000_000)...)
	info := probe(t, m4a)
	if got, floor := info.Bound(int64(len(m4a))), time.Duration(len(m4a))*8*time.Second/MinBitsPerSecond; got < floor || got < info.Duration {
		t.Errorf("bound = %v, want at least %v and the header's %v", got, floor, info.Duration)
	}
	long := Info{Format: "mp4", Duration: 5 * time.Hour}
	if got := long.Bound(1000); got != 5*time.Hour {
		t.Errorf("an honest longer claim: %v", got)
	}
	wav := wavFixture(16000, 1, 2)
	if w := probe(t, wav); w.Bound(int64(len(wav))) != 2*time.Second {
		t.Errorf("a WAV is measured: %v", w.Bound(int64(len(wav))))
	}
}

// FuzzProbe — no input makes Probe, Bound or Cut panic or read out of range.
func FuzzProbe(f *testing.F) {
	f.Add(wavFixture(16000, 1, 0.1))
	f.Add(flacFixture(44100, 44100))
	f.Add(mp4Fixture("M4A ", "soun"))
	f.Add(mp4Fixture("isom", "vide"))
	f.Add(opusFixture(1))
	f.Add(mp3Fixture(10))
	f.Add(mp3Fixture(0))
	f.Add([]byte("ID3\x03\x00\x00\x7f\x7f\x7f\x7f"))
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		info, err := Probe(r, int64(len(data)))
		if err != nil {
			return
		}
		if info.Duration < 0 || info.Bound(int64(len(data))) < 0 {
			t.Fatalf("negative length: %+v", info)
		}
		if info.WAV != nil {
			if piece, n, err := Cut(r, info.WAV, 0, time.Hour); err == nil {
				buf := make([]byte, min(n, 1<<16))
				_, _ = piece.ReadAt(buf, 0)
			}
		}
	})
}
