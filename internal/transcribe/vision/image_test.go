package vision

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
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
