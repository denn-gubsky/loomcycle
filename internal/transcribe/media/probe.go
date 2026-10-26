// Package media reads what speech-to-text needs from a recording without
// decoding it: the container, the media type to send it as, and how long it
// is (from header fields: MP4/M4A mvhd, WAV, FLAC STREAMINFO, Ogg granule
// positions, an MP3's Xing/VBRI frame count or its bit rate), so the run's
// minute caps are checked and a dry run is answered before any engine call.
// It also cuts a PCM WAV into pieces with a header of their own, which is how
// a long recording is split once the media helper has made it a WAV.
//
// Everything a recording says is untrusted. Every read is bounded by the
// file's size, box and block walks are capped, and a duration is a number the
// caller still checks against its caps; nothing here allocates by a size the
// file declares.
package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"time"
)

// Info is what a header says about a recording.
type Info struct {
	// Format is the container: wav, flac, mp4, ogg, mp3, webm; "" when none
	// was recognised.
	Format string
	// MediaType is the type to send the file as: audio/wav, audio/flac,
	// audio/mp4 (an M4A or an audio-only MP4), video/mp4, audio/ogg,
	// audio/mpeg, video/webm; "" when unrecognised.
	MediaType string
	// Duration is the recording's length; 0 when the header does not say.
	Duration time.Duration
	// Estimated marks a duration computed from the bit rate of an MP3 with
	// no frame count: close for a constant-rate file, a guess otherwise.
	Estimated bool
	// Video is true for an MP4 with a video track.
	Video bool
	// WAV is the layout of a WAV file, which Cut needs.
	WAV *WAVLayout
}

// MinBitsPerSecond is the lowest bit rate a compressed recording is taken to
// carry (MP3's lowest, 8 kbit/s). A compressed container's length is its
// producer's claim; counting at least its size at this rate means a header
// that understates its length can only be raised.
const MinBitsPerSecond = 8000

// Bound is the length the run's caps judge a recording of size bytes by. A
// sample-framed WAV is measured from its samples, which is exactly what the
// runtime sends. Any other container's header is a claim: the length is at
// least size at MinBitsPerSecond, so it can over-count a recording of a
// higher bit rate (the speech engine's own report reconciles the count after
// the call) but never under-count one at a plausible rate.
func (i Info) Bound(size int64) time.Duration {
	if i.WAV != nil {
		return i.Duration
	}
	return max(i.Duration, dur(float64(max(size, 0))*8/MinBitsPerSecond))
}

// ErrUnknown is returned for a file whose container is not recognised.
var ErrUnknown = errors.New("media: not a recognised audio or video container")

// Probe reads r's header. A recognised container whose length cannot be read
// is returned with Duration 0 and a nil error; a damaged header of a
// recognised container is an error.
func Probe(r io.ReaderAt, size int64) (Info, error) {
	head := make([]byte, 64)
	n, _ := r.ReadAt(head, 0)
	head = head[:n]
	switch {
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		return probeWAV(r, size)
	case len(head) >= 4 && string(head[:4]) == "fLaC":
		return probeFLAC(r, size, 0)
	case len(head) >= 8 && string(head[4:8]) == "ftyp":
		return probeMP4(r, size)
	case len(head) >= 4 && string(head[:4]) == "OggS":
		return probeOgg(r, size)
	case len(head) >= 4 && bytes.Equal(head[:4], []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return Info{Format: "webm", MediaType: "video/webm"}, nil
	case len(head) >= 10 && string(head[:3]) == "ID3":
		off := id3Size(head)
		if off >= size {
			return Info{}, errors.New("media: the ID3 tag runs past the end of the file")
		}
		var sig [4]byte
		if _, err := r.ReadAt(sig[:], off); err == nil && string(sig[:]) == "fLaC" {
			return probeFLAC(r, size, off)
		}
		return probeMP3(r, size, off)
	case len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0:
		return probeMP3(r, size, 0)
	}
	return Info{}, ErrUnknown
}

// maxRead bounds one header read: the largest is an Ogg file's tail.
const maxRead = 1 << 20

// readAt reads exactly n bytes at off, or fails. n and off come from the
// file, so both are checked before anything is allocated.
func readAt(r io.ReaderAt, off int64, n int) ([]byte, error) {
	if n < 0 || n > maxRead || off < 0 {
		return nil, errors.New("media: a header field points outside the file")
	}
	b := make([]byte, n)
	if _, err := r.ReadAt(b, off); err != nil {
		return nil, err
	}
	return b, nil
}

func dur(seconds float64) time.Duration {
	if math.IsNaN(seconds) || seconds <= 0 {
		return 0
	}
	// A week bounds what a header may claim; a longer one is a lie the caps
	// would refuse anyway, and the multiplication must not overflow.
	const week = 7 * 24 * 3600
	if seconds > week {
		seconds = week
	}
	return time.Duration(seconds * float64(time.Second))
}

// ---- WAV ----

// WAVLayout is where a WAV file's samples are and how they are laid out.
type WAVLayout struct {
	// Fmt is the fmt chunk's body as stored, copied into each piece Cut makes.
	Fmt       []byte
	FormatTag uint16
	Channels  int
	// SampleRate and BlockAlign are what a decoder plays the samples by;
	// ByteRate is their product, never the header's own byte-rate field.
	SampleRate    int
	BitsPerSample int
	ByteRate      int
	BlockAlign    int
	DataOffset    int64
	DataSize      int64
}

// sampleFramed are the WAV encodings whose samples come in fixed frames of
// channels × sample width: integer and float PCM, A-law and µ-law, and the
// extensible header that wraps them. Only these are measured and cut; a
// compressed WAV (ADPCM, GSM, MP3 inside RIFF) has no length the runtime can
// check from its header.
var sampleFramed = map[uint16]bool{1: true, 3: true, 6: true, 7: true, 0xFFFE: true}

// maxWAVChunks bounds the chunk walk of a WAV header.
const maxWAVChunks = 256

func probeWAV(r io.ReaderAt, size int64) (Info, error) {
	info := Info{Format: "wav", MediaType: "audio/wav"}
	lay := &WAVLayout{}
	off := int64(12)
	haveFmt := false
	for i := 0; i < maxWAVChunks && off+8 <= size; i++ {
		h, err := readAt(r, off, 8)
		if err != nil {
			return info, err
		}
		id, n := string(h[:4]), int64(binary.LittleEndian.Uint32(h[4:]))
		body := off + 8
		switch id {
		case "fmt ":
			if n < 16 || n > 1024 || body+n > size {
				return info, errors.New("media: the WAV fmt chunk is damaged")
			}
			f, err := readAt(r, body, int(n))
			if err != nil {
				return info, err
			}
			lay.Fmt = f
			lay.FormatTag = binary.LittleEndian.Uint16(f[0:])
			lay.Channels = int(binary.LittleEndian.Uint16(f[2:]))
			lay.SampleRate = int(binary.LittleEndian.Uint32(f[4:]))
			declaredRate := int64(binary.LittleEndian.Uint32(f[8:]))
			lay.BlockAlign = int(binary.LittleEndian.Uint16(f[12:]))
			lay.BitsPerSample = int(binary.LittleEndian.Uint16(f[14:]))
			if !sampleFramed[lay.FormatTag] {
				// Recognised, but its length is not checkable: the caller
				// measures it another way or refuses it.
				return info, nil
			}
			// The decoder plays sample frames: the length is the data's
			// frames over the sample rate. A header whose fields disagree
			// with that is refused rather than believed either way.
			if lay.Channels < 1 || lay.Channels > 32 || lay.SampleRate < 1 || lay.SampleRate > 768000 ||
				lay.BitsPerSample < 1 || lay.BitsPerSample > 64 {
				return info, errors.New("media: the WAV fmt chunk is out of range")
			}
			if lay.BlockAlign != lay.Channels*((lay.BitsPerSample+7)/8) {
				return info, errors.New("media: the WAV block align does not match its channels and sample width")
			}
			lay.ByteRate = lay.SampleRate * lay.BlockAlign
			if declaredRate != int64(lay.ByteRate) {
				return info, errors.New("media: the WAV byte rate does not match its sample rate and block align")
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return info, errors.New("media: the WAV data chunk comes before its fmt chunk")
			}
			// A streaming writer leaves the size unset (0 or all ones):
			// the samples run to the end of the file.
			if n == 0 || n == 0xFFFFFFFF || body+n > size {
				n = size - body
			}
			n = n / int64(lay.BlockAlign) * int64(lay.BlockAlign) // whole frames only
			lay.DataOffset, lay.DataSize = body, n
			info.WAV = lay
			info.Duration = dur(float64(n) / float64(lay.ByteRate))
			return info, nil
		}
		off = body + n + n%2 // chunks are word-aligned
	}
	return info, errors.New("media: the WAV file has no data chunk")
}

// ---- FLAC ----

func probeFLAC(r io.ReaderAt, size, off int64) (Info, error) {
	info := Info{Format: "flac", MediaType: "audio/flac"}
	b, err := readAt(r, off+4, 4+34)
	if err != nil || b[0]&0x7F != 0 {
		return info, errors.New("media: the FLAC file has no STREAMINFO block")
	}
	si := b[4:]
	rate := int(si[10])<<12 | int(si[11])<<4 | int(si[12])>>4
	total := int64(si[13]&0x0F)<<32 | int64(binary.BigEndian.Uint32(si[14:18]))
	if rate > 0 && total > 0 {
		info.Duration = dur(float64(total) / float64(rate))
	}
	return info, nil
}

// ---- MP4 / M4A ----

// maxBoxes bounds the boxes an MP4 walk visits, all levels together.
const maxBoxes = 4096

type mp4Walk struct {
	r       io.ReaderAt
	visited int
	info    *Info
	scale   uint32
	length  uint64
}

func probeMP4(r io.ReaderAt, size int64) (Info, error) {
	info := Info{Format: "mp4", MediaType: "audio/mp4"}
	w := &mp4Walk{r: r, info: &info}
	brand, err := readAt(r, 8, 4)
	if err != nil {
		return info, err
	}
	if err := w.boxes(0, size, 0); err != nil {
		return info, err
	}
	if w.scale > 0 {
		info.Duration = dur(float64(w.length) / float64(w.scale))
	}
	// An M4A is audio whatever its tracks say; any other MP4 with a video
	// track is a video.
	if info.Video && string(brand) != "M4A " && string(brand) != "M4B " {
		info.MediaType = "video/mp4"
	}
	return info, nil
}

// containers are the boxes whose children the walk reads.
var containers = map[string]bool{"moov": true, "trak": true, "mdia": true}

func (w *mp4Walk) boxes(start, end int64, depth int) error {
	off := start
	for off+8 <= end {
		if w.visited++; w.visited > maxBoxes || depth > 8 {
			return errors.New("media: the MP4 box tree is too large")
		}
		h, err := readAt(w.r, off, 8)
		if err != nil {
			return err
		}
		n, typ, hdr := int64(binary.BigEndian.Uint32(h)), string(h[4:8]), int64(8)
		switch n {
		case 0:
			n = end - off
		case 1:
			x, err := readAt(w.r, off+8, 8)
			if err != nil {
				return err
			}
			big := binary.BigEndian.Uint64(x)
			if big > uint64(end-off) {
				return errors.New("media: an MP4 box runs past its parent")
			}
			n, hdr = int64(big), 16
		}
		if n < hdr || off+n > end {
			return errors.New("media: an MP4 box runs past its parent")
		}
		body, bodyLen := off+hdr, n-hdr
		switch {
		case typ == "mvhd":
			if err := w.mvhd(body, bodyLen); err != nil {
				return err
			}
		case typ == "hdlr" && bodyLen >= 12:
			b, err := readAt(w.r, body+8, 4)
			if err != nil {
				return err
			}
			if string(b) == "vide" {
				w.info.Video = true
			}
		case containers[typ]:
			if err := w.boxes(body, body+bodyLen, depth+1); err != nil {
				return err
			}
		}
		off += n
	}
	return nil
}

func (w *mp4Walk) mvhd(body, n int64) error {
	if n < 20 {
		return errors.New("media: the MP4 mvhd box is damaged")
	}
	b, err := readAt(w.r, body, int(min(n, 32)))
	if err != nil {
		return err
	}
	if b[0] == 1 {
		if len(b) < 32 {
			return errors.New("media: the MP4 mvhd box is damaged")
		}
		w.scale, w.length = binary.BigEndian.Uint32(b[20:]), binary.BigEndian.Uint64(b[24:])
		return nil
	}
	w.scale, w.length = binary.BigEndian.Uint32(b[12:]), uint64(binary.BigEndian.Uint32(b[16:]))
	if w.length == 0xFFFFFFFF { // unknown duration
		w.length = 0
	}
	return nil
}

// ---- Ogg (Opus, Vorbis) ----

// oggTail is how far from the end the last page is looked for: a page is at
// most 65 307 bytes.
const oggTail = 65536 + 512

func probeOgg(r io.ReaderAt, size int64) (Info, error) {
	info := Info{Format: "ogg", MediaType: "audio/ogg"}
	h, err := readAt(r, 0, 27)
	if err != nil {
		return info, err
	}
	// The first packet follows the page header and its segment table.
	pkt, _ := readAt(r, int64(27+int(h[26])), 20)
	if pkt == nil {
		return info, nil
	}
	var rate float64
	var preSkip int64
	switch {
	case bytes.HasPrefix(pkt, []byte("OpusHead")):
		rate = 48000 // an Opus granule counts 48 kHz samples
		preSkip = int64(binary.LittleEndian.Uint16(pkt[10:]))
	case bytes.HasPrefix(pkt, []byte("\x01vorbis")):
		rate = float64(binary.LittleEndian.Uint32(pkt[12:]))
	default:
		return info, nil
	}
	from := max(size-oggTail, 0)
	tail, err := readAt(r, from, int(size-from))
	if err != nil {
		return info, err
	}
	i := bytes.LastIndex(tail, []byte("OggS"))
	for i >= 0 && i+14 > len(tail) {
		i = bytes.LastIndex(tail[:i], []byte("OggS"))
	}
	if i < 0 || rate <= 0 {
		return info, nil
	}
	granule := int64(binary.LittleEndian.Uint64(tail[i+6:]))
	if granule > preSkip {
		info.Duration = dur(float64(granule-preSkip) / rate)
	}
	return info, nil
}

// ---- MP3 ----

// id3Size is the length of an ID3v2 tag at the start of head, footer included.
func id3Size(head []byte) int64 {
	n := int64(head[6]&0x7F)<<21 | int64(head[7]&0x7F)<<14 | int64(head[8]&0x7F)<<7 | int64(head[9]&0x7F)
	n += 10
	if head[5]&0x10 != 0 {
		n += 10
	}
	return n
}

var (
	// mp3Bitrates[v1][layer3] in kbit/s, index 1..14.
	mp3BitratesV1L3 = [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	mp3BitratesV2L3 = [16]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
	mp3Rates        = [4]int{44100, 48000, 32000, 0}
)

// mp3Scan is how far the first frame is looked for after the tags.
const mp3Scan = 64 << 10

func probeMP3(r io.ReaderAt, size, start int64) (Info, error) {
	info := Info{Format: "mp3", MediaType: "audio/mpeg"}
	if start < 0 || start >= size {
		return info, nil
	}
	buf, _ := readAt(r, start, int(min(mp3Scan, size-start)))
	if buf == nil {
		return info, nil
	}
	for i := 0; i+4 <= len(buf); i++ {
		if buf[i] != 0xFF || buf[i+1]&0xE0 != 0xE0 {
			continue
		}
		h := buf[i : i+4]
		version := (h[1] >> 3) & 3 // 3 = MPEG1, 2 = MPEG2, 0 = MPEG2.5
		layer := (h[1] >> 1) & 3   // 1 = layer III
		brIdx, srIdx := h[2]>>4, (h[2]>>2)&3
		if version == 1 || layer != 1 || brIdx == 0 || brIdx == 15 || srIdx == 3 {
			continue
		}
		rate := mp3Rates[srIdx]
		samples := 1152
		bitrate := mp3BitratesV1L3[brIdx]
		if version != 3 {
			rate /= 2
			if version == 0 {
				rate /= 2
			}
			samples = 576
			bitrate = mp3BitratesV2L3[brIdx]
		}
		mono := h[3]>>6 == 3
		side := 32
		switch {
		case version == 3 && mono:
			side = 17
		case version != 3 && !mono:
			side = 17
		case version != 3 && mono:
			side = 9
		}
		if frames := xingFrames(buf[i:], 4+side); frames > 0 {
			info.Duration = dur(float64(frames) * float64(samples) / float64(rate))
			return info, nil
		}
		if bitrate > 0 {
			info.Duration = dur(float64(size-start-int64(i)) * 8 / float64(bitrate*1000))
			info.Estimated = true
		}
		return info, nil
	}
	return info, nil
}

// xingFrames is the frame count of a Xing/Info header at off, or of a VBRI
// header at 36, in the first frame; 0 when there is none.
func xingFrames(frame []byte, off int) int64 {
	if off+12 <= len(frame) {
		if tag := string(frame[off : off+4]); tag == "Xing" || tag == "Info" {
			if binary.BigEndian.Uint32(frame[off+4:])&1 != 0 {
				return int64(binary.BigEndian.Uint32(frame[off+8:]))
			}
		}
	}
	if 36+18 <= len(frame) && string(frame[36:40]) == "VBRI" {
		return int64(binary.BigEndian.Uint32(frame[36+14:]))
	}
	return 0
}
