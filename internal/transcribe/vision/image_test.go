package vision

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
)

// pngHeader is a PNG that is only a signature and an IHDR claiming w×h at the
// given bit depth and colour type: what DecodeConfig reads, with no pixels.
func pngHeader(w, h uint32, depth, colorType byte) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = depth, colorType
	_ = binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

// countingDecoder wraps image.Decode, counting calls and the most that ran at
// once; hold, when set, keeps each call in the decoder until it is closed.
type countingDecoder struct {
	calls, running, peak atomic.Int32
	hold                 chan struct{}
}

func (d *countingDecoder) decode(r io.Reader) (image.Image, string, error) {
	d.calls.Add(1)
	n := d.running.Add(1)
	defer d.running.Add(-1)
	for {
		p := d.peak.Load()
		if n <= p || d.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if d.hold != nil {
		<-d.hold
	}
	return image.Decode(r)
}

func countingEngine(t *testing.T, d *countingDecoder) (*Engine, transcribe.Plan) {
	t.Helper()
	m := &fakeModel{caps: imageOnlyModel, reply: func(providers.Request) string { return "text" }}
	e := engineWith(t, m, config.OCRConfig{})
	e.decode = d.decode
	plan, err := e.Plan(context.Background(), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	return e, plan
}

// A header claiming a 16-bit image whose decoded pixels would pass the byte
// budget is refused from its header alone: the decoder is never reached, so
// nothing the size of the claimed image is allocated.
func TestVision_HugeSixteenBitHeaderRefusedBeforeDecoding(t *testing.T) {
	d := &countingDecoder{}
	e, plan := countingEngine(t, d)
	// 8000×8000 RGBA at 16 bits per channel: 64 MP (under the old pixel
	// bound), 488 MiB decoded.
	crafted := pngHeader(8000, 8000, 16, 6)
	_, err := e.OCR(context.Background(), plan, imageUnit(t, crafted, "image/png"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10})
	var tooLarge *ImageTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v, want an ImageTooLargeError", err)
	}
	if tooLarge.Limit != MaxDecodeBytes || tooLarge.Width != 8000 || tooLarge.BytesPerPixel != 8 {
		t.Errorf("refusal = %+v", tooLarge)
	}
	if n := d.calls.Load(); n != 0 {
		t.Fatalf("the decoder was reached %d times", n)
	}
	// A 12 MP 16-bit photo is refused too, with advice to drop the depth.
	_, err = e.OCR(context.Background(), plan, imageUnit(t, pngHeader(4000, 3000, 16, 2), "image/png"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10})
	if !errors.As(err, &tooLarge) || tooLarge.BytesPerPixel != 8 || !bytes.Contains([]byte(err.Error()), []byte("8 bits")) {
		t.Fatalf("12 MP 16-bit: %v", err)
	}
	if d.calls.Load() != 0 {
		t.Fatal("the decoder was reached")
	}
}

// An image the model takes as it is never goes through the decoder.
func TestVision_InCapImagePassesThroughUndecoded(t *testing.T) {
	d := &countingDecoder{}
	e, plan := countingEngine(t, d)
	if _, err := e.OCR(context.Background(), plan, imageUnit(t, pngOf(t, 800, 600), "image/png"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10}); err != nil {
		t.Fatal(err)
	}
	if n := d.calls.Load(); n != 0 {
		t.Fatalf("decoded %d times", n)
	}
	// One that needs scaling and fits the byte budget is decoded once.
	if _, err := e.OCR(context.Background(), plan, imageUnit(t, pngOf(t, 1800, 1200), "image/png"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10}); err != nil {
		t.Fatal(err)
	}
	if n := d.calls.Load(); n != 1 {
		t.Fatalf("decoded %d times, want 1", n)
	}
}

// Concurrent calls that each need a decode take turns, so their peaks never
// add up.
func TestVision_ConcurrentDecodesSerialise(t *testing.T) {
	d := &countingDecoder{hold: make(chan struct{})}
	e, plan := countingEngine(t, d)
	big := pngOf(t, MaxImageEdge+200, 100)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.OCR(context.Background(), plan, imageUnit(t, big, "image/png"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10})
			errs <- err
		}()
	}
	// Let every call reach the semaphore, then release decodes one by one.
	deadline := time.After(10 * time.Second)
	for released := 0; released < 4; {
		select {
		case d.hold <- struct{}{}:
			released++
		case <-deadline:
			t.Fatal("decodes did not finish")
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if p := d.peak.Load(); p != 1 {
		t.Fatalf("%d decodes ran at once, want 1", p)
	}
}

// A call waiting its turn gives up when its context ends.
func TestVision_DecodeWaitHonoursContext(t *testing.T) {
	d := &countingDecoder{hold: make(chan struct{})}
	t.Cleanup(func() { close(d.hold) })
	e, plan := countingEngine(t, d)
	big := pngOf(t, MaxImageEdge+200, 100)
	go func() {
		_, _ = e.OCR(context.Background(), plan, imageUnit(t, big, "image/png"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10})
	}()
	for d.running.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := e.OCR(ctx, plan, imageUnit(t, big, "image/png"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10})
		done <- err
	}()
	// Bounded: without the context check the second call would sit in the
	// decoder behind the first until the test's cleanup.
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the context's deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call waiting for the decoder did not give up when its context ended")
	}
}

// pngHeaderInterlaced is pngHeader with the IHDR interlace byte set (Adam7).
func pngHeaderInterlaced(w, h uint32, depth, colorType byte) []byte {
	b := pngHeader(w, h, depth, colorType)
	// signature 8 + length 4 + "IHDR" 4 + 12 bytes into the data.
	b[28] = 1
	chunk := b[12:29]
	binary.BigEndian.PutUint32(b[29:], crc32.ChecksumIEEE(chunk))
	return b
}

// jpegHeader is a JPEG that is only SOI and a start-of-frame segment of the
// given marker (0xC0 baseline, 0xC2 progressive) and component count, with
// an Adobe APP14 marker before it when adobe is set, then the start of a
// scan: what DecodeConfig reads, with no pixel data after it.
func jpegHeader(marker byte, w, h uint16, comps int, adobe bool) []byte {
	var b bytes.Buffer
	b.Write([]byte{0xFF, 0xD8})
	if adobe {
		// APP14 "Adobe": version, flags0, flags1, transform (2 = YCCK).
		seg := []byte("Adobe\x00\x64\x00\x00\x00\x00\x02")
		b.Write([]byte{0xFF, 0xEE})
		_ = binary.Write(&b, binary.BigEndian, uint16(len(seg)+2))
		b.Write(seg)
	}
	sof := []byte{8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), byte(comps)}
	for i := 1; i <= comps; i++ {
		sof = append(sof, byte(i), 0x11, 0)
	}
	b.Write([]byte{0xFF, marker})
	_ = binary.Write(&b, binary.BigEndian, uint16(len(sof)+2))
	b.Write(sof)
	// A start-of-scan marker: where a config-only read stops.
	b.Write([]byte{0xFF, 0xDA, 0x00, 0x08})
	return b.Bytes()
}

// The decode estimate is per format, from the header: a progressive JPEG
// holds its coefficients beside the output, a 4-component JPEG converts into
// a second buffer, an interlaced PNG keeps per-pass buffers. Each of these at
// a size whose colour model alone would pass is refused before decoding; the
// same size in the plain form is not.
func TestVision_DecodeEstimateIsPerFormat(t *testing.T) {
	const side = 3000 // 9 MP: 36 MiB at 4 bytes a pixel, under the budget
	for _, c := range []struct {
		name    string
		data    []byte
		refused bool
	}{
		{"progressive JPEG", jpegHeader(0xC2, side, side, 3, false), true},
		{"baseline JPEG", jpegHeader(0xC0, side, side, 3, false), false},
		{"4-component JPEG", jpegHeader(0xC0, side, side, 4, true), true},
		{"interlaced PNG", pngHeaderInterlaced(side, side, 8, 6), true},
		{"non-interlaced PNG", pngHeader(side, side, 8, 6), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := &countingDecoder{}
			e, plan := countingEngine(t, d)
			mt := "image/png"
			if c.data[0] == 0xFF {
				mt = "image/jpeg"
			}
			_, err := e.OCR(context.Background(), plan, imageUnit(t, c.data, mt), transcribe.OCROptions{Prompt: "x", MaxTokens: 10})
			var tooLarge *ImageTooLargeError
			if c.refused {
				if !errors.As(err, &tooLarge) || d.calls.Load() != 0 {
					t.Fatalf("err = %v, decoded %d times; want a refusal before decoding", err, d.calls.Load())
				}
				return
			}
			// Accepted by the budget: the decoder is reached (and fails, as
			// the file is only a header), and the error is not the budget's.
			if errors.As(err, &tooLarge) || d.calls.Load() != 1 {
				t.Fatalf("err = %v, decoded %d times; want it decoded", err, d.calls.Load())
			}
		})
	}
}

// jpegWith is SOI, then the given segments as written, then a start of scan.
func jpegWith(parts ...[]byte) []byte {
	b := []byte{0xFF, 0xD8}
	for _, p := range parts {
		b = append(b, p...)
	}
	return append(b, 0xFF, 0xDA, 0x00, 0x08)
}

// sofSegment is a start-of-frame segment of the given marker.
func sofSegment(marker byte, w, h uint16, comps int) []byte {
	return jpegHeader(marker, w, h, comps, false)[2 : len(jpegHeader(marker, w, h, comps, false))-4]
}

// The cheap JPEG estimate must be PROVED by a strict walk that finds exactly
// one baseline frame before the scan. Anything image/jpeg reads more
// leniently than that walk — junk bytes it skips, an FF 00 it ignores, a
// second frame header — is budgeted at the worst case, never at a guess.
func TestVision_JPEGEstimateFailsClosed(t *testing.T) {
	const side = 2500
	ycc := image.Config{Width: side, Height: side, ColorModel: color.YCbCrModel}
	cmyk := image.Config{Width: side, Height: side, ColorModel: color.CMYKModel}
	for _, c := range []struct {
		name string
		data []byte
		cfg  image.Config
		want int
	}{
		{"junk before a progressive frame", jpegWith([]byte{0x00, 0x13, 0x37}, sofSegment(0xC2, side, side, 3)), ycc, jpegWorstCase},
		{"FF 00 before a baseline frame", jpegWith([]byte{0xFF, 0x00}, sofSegment(0xC0, side, side, 3)), ycc, jpegWorstCase},
		{"two frame headers", jpegWith(sofSegment(0xC0, side, side, 3), sofSegment(0xC2, side, side, 3)), ycc, jpegWorstCase},
		{"no frame header", jpegWith(), ycc, jpegWorstCase},
		{"clean progressive", jpegWith(sofSegment(0xC2, side, side, 3)), ycc, 4 + 4*3},
		{"clean progressive CMYK", jpegWith(sofSegment(0xC2, side, side, 4)), cmyk, 8 + 4*4},
		{"clean baseline CMYK", jpegWith(sofSegment(0xC0, side, side, 4)), cmyk, 8},
		{"clean baseline", jpegWith(sofSegment(0xC0, side, side, 3)), ycc, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, _ := decodeCost(c.data, "jpeg", c.cfg); got != c.want {
				t.Errorf("decodeCost = %d bytes a pixel, want %d", got, c.want)
			}
		})
	}
}

// End to end: a progressive JPEG that image/jpeg reads past junk bytes is
// refused before decoding at a size its cheapest reading would allow.
func TestVision_JunkPrefixedProgressiveJPEGRefusedBeforeDecoding(t *testing.T) {
	const side = 2500 // 6.25 MP: 8 bytes a pixel would pass, the worst case does not
	data := jpegWith([]byte{0x00, 0x13, 0x37}, sofSegment(0xC2, side, side, 3))
	if cfg, format, err := image.DecodeConfig(bytes.NewReader(data)); err != nil || format != "jpeg" || cfg.Width != side {
		t.Fatalf("image/jpeg does not read the fixture: %v %s %+v", err, format, cfg)
	}
	d := &countingDecoder{}
	e, plan := countingEngine(t, d)
	_, err := e.OCR(context.Background(), plan, imageUnit(t, data, "image/jpeg"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10})
	var tooLarge *ImageTooLargeError
	if !errors.As(err, &tooLarge) || d.calls.Load() != 0 {
		t.Fatalf("err = %v, decoded %d times; want a refusal before decoding", err, d.calls.Load())
	}
}
