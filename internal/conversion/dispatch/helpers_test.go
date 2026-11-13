package dispatch_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/conversion/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/conversion/worker"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/vision"
)

// helperWorker runs a worker with opt's helpers and image re-encoder.
func helperWorker(t *testing.T, target, id string, opt worker.Options) {
	t.Helper()
	opt.Target, opt.Token, opt.ID = target, token, id
	if opt.Scratch == "" {
		opt.Scratch = t.TempDir()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = worker.Run(ctx, opt)
	}()
	t.Cleanup(func() { cancel(); <-done })
}

func copyHelper(key string) *convert.Helper {
	return &convert.Helper{Key: key, Command: "cp {in} {out}"}
}

// wideImage is a PNG wider than the vision models' edge, so re-encoding it
// scales it rather than passing it through.
func wideImage(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2400, 40))
	for x := range 2400 {
		img.Set(x, x%40, color.RGBA{G: 180, A: 255})
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The helper and image jobs on a worker give what the same jobs give in this
// process: the rendered page, the encoded audio, the converted recording and
// the re-encoded image, byte for byte.
func TestPool_HelperJobsMatchInProcess(t *testing.T) {
	scratch := t.TempDir()
	d, target := pool(t, dispatch.Options{Scratch: scratch})
	helperWorker(t, target, "w1", worker.Options{
		Rasterize: copyHelper("transcription.ocr.rasterize"), Media: copyHelper("transcription.stt.media"),
		Encode: copyHelper("speech.encode"), Images: vision.NewReencoder(),
	})
	waitIdle(t, d, 1)
	here := &convert.InProcess{
		Rasterize: copyHelper("transcription.ocr.rasterize"), Media: copyHelper("transcription.stt.media"),
		Encode: copyHelper("speech.encode"), Images: vision.NewReencoder(),
		Scratch: func(context.Context) (string, error) { return t.TempDir(), nil },
	}
	pool := d.Client()
	ctx := context.Background()
	page := samplePDF(t)

	want, err := here.RenderPage(ctx, page, "en")
	if err != nil {
		t.Fatal(err)
	}
	got, err := pool.RenderPage(ctx, page, "en")
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("RenderPage: pool %d bytes (%v), in process %d bytes", len(got), err, len(want))
	}

	wav := bytes.Repeat([]byte("RIFF-not-really-a-wav-"), 5000)
	want, err = here.EncodeAudio(ctx, wav, "mp3")
	if err != nil {
		t.Fatal(err)
	}
	got, err = pool.EncodeAudio(ctx, wav, "mp3")
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("EncodeAudio: pool %d bytes (%v), in process %d bytes", len(got), err, len(want))
	}

	// Larger than one chunk, so the output arrives in several.
	rec := bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7}, 400000)
	sp, err := pool.ConvertMedia(ctx, bytes.NewReader(rec), int64(len(rec)), "audio/mpeg", "en")
	if err != nil {
		t.Fatal(err)
	}
	if sp.Size() != int64(len(rec)) {
		t.Errorf("ConvertMedia: spool is %d bytes, want %d", sp.Size(), len(rec))
	}
	back, err := io.ReadAll(io.NewSectionReader(sp, 0, sp.Size()))
	if err != nil || !bytes.Equal(back, rec) {
		t.Errorf("ConvertMedia: the spool differs from what the converter wrote (%v)", err)
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(scratch); len(left) != 0 {
		t.Errorf("a closed spool left %d files in the core's scratch", len(left))
	}

	pic := wideImage(t)
	wantImg, wantType, err := here.ReencodeImage(ctx, pic)
	if err != nil {
		t.Fatal(err)
	}
	gotImg, gotType, err := pool.ReencodeImage(ctx, pic)
	if err != nil || gotType != wantType || !bytes.Equal(gotImg, wantImg) {
		t.Errorf("ReencodeImage: pool %s %d bytes (%v), in process %s %d bytes", gotType, len(gotImg), err, wantType, len(wantImg))
	}
}

// A helper job no attached worker can run is refused at once as not
// configured, naming the helper, rather than waiting for a slot or calling
// the pool down: the workers are up and lack the program.
func TestPool_AHelperNoWorkerHasIsNotConfigured(t *testing.T) {
	d, target := pool(t, dispatch.Options{QueueWait: time.Minute})
	helperWorker(t, target, "w1", worker.Options{
		// Configured, but the program is not on this host.
		Media: &convert.Helper{Key: "transcription.stt.media", Command: "loomcycle-no-such-program-x {in} {out}"},
	})
	waitIdle(t, d, 1)
	start := time.Now()
	_, err := d.Client().RenderPage(context.Background(), []byte("%PDF-1.4"), "en")
	if !errors.Is(err, convert.ErrNotConfigured) || !strings.Contains(err.Error(), "render") {
		t.Errorf("RenderPage with no rasteriser: %v, want ErrNotConfigured naming render", err)
	}
	_, err = d.Client().ConvertMedia(context.Background(), bytes.NewReader([]byte("x")), 1, "audio/mpeg", "")
	if !errors.Is(err, convert.ErrNotConfigured) {
		t.Errorf("ConvertMedia with a converter that is not installed: %v, want ErrNotConfigured", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("the refusals waited %v; they must not queue", time.Since(start))
	}
}

// A helper that fails on a worker reads as the helper's own failure, not as
// an extraction's.
func TestPool_AHelperFailureNamesTheHelper(t *testing.T) {
	d, target := pool(t, dispatch.Options{})
	helperWorker(t, target, "w1", worker.Options{Encode: &convert.Helper{Key: "speech.encode", Command: "false {in} {out}"}})
	waitIdle(t, d, 1)
	_, err := d.Client().EncodeAudio(context.Background(), []byte("RIFF"), "mp3")
	if err == nil || !strings.Contains(err.Error(), "speech.encode") || strings.HasPrefix(err.Error(), "extract:") {
		t.Errorf("a failing encoder: %v, want an error naming speech.encode", err)
	}
	if errors.Is(err, convert.ErrQuarantined) {
		t.Errorf("a helper's exit status quarantined the file: %v", err)
	}
}

// An encode format that is not a plain name is refused by the worker: it
// names the encoder's output file.
func TestPool_AnEncodeFormatMustBeAName(t *testing.T) {
	d, target := pool(t, dispatch.Options{})
	scratch := t.TempDir()
	helperWorker(t, target, "w1", worker.Options{Encode: copyHelper("speech.encode"), Scratch: scratch})
	waitIdle(t, d, 1)
	_, err := d.Client().EncodeAudio(context.Background(), []byte("RIFF"), "../../escape")
	if err == nil || !strings.Contains(err.Error(), "not an audio format name") {
		t.Errorf("format ../../escape: %v, want a refusal", err)
	}
	if _, serr := os.Stat(filepath.Join(filepath.Dir(scratch), "escape")); serr == nil {
		t.Error("the encoder wrote outside its scratch folder")
	}
}
