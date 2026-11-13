package convert

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/helper"
)

const (
	// DefaultSlotWait is the longest a job waits for an extraction slot when
	// InProcess.SlotWait is zero. A slot frees within an extraction's own
	// time bound of being taken, so a wait this long means a queue, not one
	// slow file.
	DefaultSlotWait = 60 * time.Second
	// MaxRenderedPage caps the image a rasteriser may hand back for one page.
	MaxRenderedPage = 64 << 20
)

// Helper is one operator command a job runs: the command line template, the
// LOOMCYCLE_HELPER_WRAPPER stamped onto it, and its time bound.
type Helper struct {
	// Key names the helper in operator configuration (for example
	// transcription.stt.media); every helper error names it.
	Key     string
	Command string
	Wrapper string
	Timeout time.Duration
}

// InProcess runs every job in this process. The zero value extracts and cuts
// pages with no slot bound, and refuses the helper jobs as not configured.
//
// A caller builds one from its own configuration when it needs one: it is a
// handful of fields, and the helpers' settings are read live.
type InProcess struct {
	// Registry picks the extractor; nil is extract.Default().
	Registry *extract.Registry
	// Extractions bounds the extractions and page cuts the process runs at
	// once; nil does not bound them.
	Extractions *extract.Limiter
	// SlotWait is the longest a job waits for an extraction slot; zero is
	// DefaultSlotWait.
	SlotWait time.Duration
	// ExtractTimeout bounds one extraction, counted from when it holds its
	// slot (time queued behind other files is not this file's); zero is no
	// bound beyond the caller's context.
	ExtractTimeout time.Duration

	// Rasterize renders a PDF page (RenderPage), Media converts a recording
	// (ConvertMedia) and Encode encodes speech (EncodeAudio). A nil helper
	// makes its job return ErrNotConfigured.
	Rasterize, Media, Encode *Helper
	// MaxEncodedBytes caps what the encoder may hand back; zero is no cap.
	MaxEncodedBytes int64
	// Scratch names the operator directory a helper job makes its scratch
	// folder in; the helper is fenced to it. Asked only when a helper runs.
	Scratch func(ctx context.Context) (string, error)

	// Images re-encodes images (ReencodeImage); nil makes ReencodeImage
	// return ErrNotConfigured. The caller passes vision.NewReencoder(): this
	// package stays below the transcription engines that import it.
	Images ImageReencoder
}

// ImageReencoder decodes an image and encodes it again, scaled to fit the
// vision models' edge (vision.Reencoder).
type ImageReencoder interface {
	Reencode(ctx context.Context, data []byte) ([]byte, string, error)
}

var _ Client = (*InProcess)(nil)

func (p *InProcess) registry() *extract.Registry {
	if p.Registry != nil {
		return p.Registry
	}
	return extract.Default()
}

// slot waits for an extraction slot, at most SlotWait.
func (p *InProcess) slot(ctx context.Context) (release func(), err error) {
	if p.Extractions == nil {
		return func() {}, nil
	}
	wait := p.SlotWait
	if wait <= 0 {
		wait = DefaultSlotWait
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return p.Extractions.Acquire(wctx)
}

// Extract runs the extractor for mediaType under an extraction slot and the
// extraction time bound.
func (p *InProcess) Extract(ctx context.Context, mediaType string, r io.ReaderAt, size int64, sink extract.Sink, opts extract.Options) (extract.Report, error) {
	ex, ok := p.registry().For(mediaType, opts.Name)
	if !ok {
		return extract.Report{}, extract.ErrUnsupported
	}
	release, err := p.slot(ctx)
	if err != nil {
		return extract.Report{}, err
	}
	defer release()
	if p.ExtractTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.ExtractTimeout)
		defer cancel()
	}
	return ex.Extract(ctx, r, size, sink, opts)
}

// SelectPages cuts pages out of a PDF under an extraction slot: it reads the
// whole PDF again, which is extraction work of the same budget.
func (p *InProcess) SelectPages(ctx context.Context, r io.ReaderAt, size int64, pages []int) ([]byte, error) {
	release, err := p.slot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return extract.SelectPages(ctx, r, size, pages)
}

// RenderPage renders a one-page PDF to PNG with the rasterize helper, in a
// scratch folder removed afterwards; only the paths made here reach its
// command line.
func (p *InProcess) RenderPage(ctx context.Context, onePage []byte, language string) ([]byte, error) {
	if p.Rasterize == nil {
		return nil, ErrNotConfigured
	}
	s, err := p.openScratch(ctx, ".ocr-")
	if err != nil {
		return nil, err
	}
	defer s.remove()
	if err := writeNew(s.dir, "page.pdf", onePage); err != nil {
		return nil, err
	}
	prefix := filepath.Join(s.path, "page")
	if err := s.run(ctx, p.Rasterize, map[string]string{"in": filepath.Join(s.path, "page.pdf"), "out_prefix": prefix, "out": prefix + ".png", "lang": language}); err != nil {
		return nil, err
	}
	return readRenderedPage(s.dir)
}

// ConvertMedia has the media helper write the recording as a WAV in a
// scratch folder, and returns that WAV open; closing it removes the folder.
// The helper gets a copy of the recording, never the original's path.
func (p *InProcess) ConvertMedia(ctx context.Context, r io.ReaderAt, size int64, mediaType, language string) (Spool, error) {
	if p.Media == nil {
		return nil, ErrNotConfigured
	}
	s, err := p.openScratch(ctx, ".stt-")
	if err != nil {
		return nil, err
	}
	inName := "input" + recordingExt(mediaType)
	if err := copyNew(s.dir, inName, io.NewSectionReader(r, 0, size)); err != nil {
		s.remove()
		return nil, fmt.Errorf("copying the recording for the converter: %w", err)
	}
	if err := s.run(ctx, p.Media, map[string]string{"in": filepath.Join(s.path, inName), "out": filepath.Join(s.path, "audio.wav"), "lang": language}); err != nil {
		s.remove()
		return nil, err
	}
	fi, err := s.dir.Lstat("audio.wav")
	if err != nil || !fi.Mode().IsRegular() {
		s.remove()
		return nil, fmt.Errorf("%s: the converter wrote no WAV at {out}", p.Media.Key)
	}
	fh, err := s.dir.Open("audio.wav")
	if err != nil {
		s.remove()
		return nil, err
	}
	return &fileSpool{File: fh, size: fi.Size(), scratch: s}, nil
}

// EncodeAudio has the encode helper turn a WAV into format, in a scratch
// folder removed afterwards.
func (p *InProcess) EncodeAudio(ctx context.Context, wav []byte, format string) ([]byte, error) {
	if p.Encode == nil {
		return nil, ErrNotConfigured
	}
	s, err := p.openScratch(ctx, ".tts-")
	if err != nil {
		return nil, err
	}
	defer s.remove()
	if err := s.dir.WriteFile("speech.wav", wav, 0o600); err != nil {
		return nil, fmt.Errorf("writing the audio for the encoder: %w", err)
	}
	outName := "speech." + format
	if err := s.run(ctx, p.Encode, map[string]string{"in": filepath.Join(s.path, "speech.wav"), "out": filepath.Join(s.path, outName)}); err != nil {
		return nil, err
	}
	fi, err := s.dir.Lstat(outName)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return nil, fmt.Errorf("%s: the encoder wrote no file at {out}", p.Encode.Key)
	}
	limit := p.MaxEncodedBytes
	if limit > 0 && fi.Size() > limit {
		return nil, fmt.Errorf("%s: the encoded audio is %d bytes, over the %d limit", p.Encode.Key, fi.Size(), limit)
	}
	fh, err := s.dir.Open(outName)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	if limit > 0 {
		return io.ReadAll(io.LimitReader(fh, limit))
	}
	return io.ReadAll(fh)
}

// ReencodeImage decodes and encodes the image again with Images.
func (p *InProcess) ReencodeImage(ctx context.Context, data []byte) ([]byte, string, error) {
	if p.Images == nil {
		return nil, "", ErrNotConfigured
	}
	return p.Images.Reencode(ctx, data)
}

// ---- scratch ----

// scratch is one job's scratch folder. Every file operation goes through an
// os.Root (the operator root's, then one held on the folder from the moment
// it is made), never through a joined path the kernel walks again: a folder
// swapped for a symlink while the helper runs cannot carry a write, a read or
// the removal out of the root. The absolute paths exist only for the helper's
// command line and working directory, which helper.Run fences itself.
type scratch struct {
	rootPath string
	rootFS   *os.Root
	name     string
	path     string
	dir      *os.Root
}

func (p *InProcess) openScratch(ctx context.Context, prefix string) (*scratch, error) {
	if p.Scratch == nil {
		return nil, errors.New("there is no scratch directory to run the helper in")
	}
	root, err := p.Scratch(ctx)
	if err != nil {
		return nil, err
	}
	// Only the operator's root is resolved; everything below it is derived.
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("scratch root: %w", err)
	}
	rootFS, err := os.OpenRoot(real)
	if err != nil {
		return nil, fmt.Errorf("scratch root: %w", err)
	}
	name, err := scratchName(prefix)
	if err != nil {
		rootFS.Close()
		return nil, err
	}
	// Mkdir refuses a name that exists, so a planted entry is an error, never
	// reused.
	if err := rootFS.Mkdir(name, 0o700); err != nil {
		rootFS.Close()
		return nil, err
	}
	dir, err := rootFS.OpenRoot(name)
	if err != nil {
		_ = rootFS.RemoveAll(name)
		rootFS.Close()
		return nil, err
	}
	return &scratch{rootPath: root, rootFS: rootFS, name: name, path: filepath.Join(root, name), dir: dir}, nil
}

func (s *scratch) run(ctx context.Context, h *Helper, vars map[string]string) error {
	_, err := helper.Run(ctx, helper.Spec{
		Key:      h.Key,
		Template: h.Command,
		Wrapper:  h.Wrapper,
		Vars:     vars,
		Root:     s.rootPath,
		Dir:      s.path,
		Timeout:  h.Timeout,
	})
	return err
}

func (s *scratch) remove() {
	s.dir.Close()
	_ = s.rootFS.RemoveAll(s.name)
	s.rootFS.Close()
}

// fileSpool is a helper's output file, open in its scratch folder.
type fileSpool struct {
	*os.File
	size    int64
	scratch *scratch
}

func (f *fileSpool) Size() int64 { return f.size }

func (f *fileSpool) Close() error {
	err := f.File.Close()
	f.scratch.remove()
	return err
}

// scratchName is a fresh scratch folder name.
func scratchName(prefix string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// writeNew writes data to a file of dir that must not exist yet.
func writeNew(dir *os.Root, name string, data []byte) error {
	fh, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(data); err != nil {
		fh.Close()
		return err
	}
	return fh.Close()
}

// copyNew copies r into a file of dir that must not exist yet.
func copyNew(dir *os.Root, name string, r io.Reader) error {
	fh, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(fh, r); err != nil {
		fh.Close()
		return err
	}
	return fh.Close()
}

// readRenderedPage returns the first regular page*.png the helper wrote in
// the scratch folder ({out_prefix}*.png, or {out}), read through the folder's
// own handle and bounded by MaxRenderedPage.
func readRenderedPage(dir *os.Root) ([]byte, error) {
	dh, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	names, err := dh.Readdirnames(-1)
	dh.Close()
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	for _, n := range names {
		if !strings.HasPrefix(n, "page") || !strings.HasSuffix(n, ".png") {
			continue
		}
		fi, err := dir.Lstat(n)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		fh, err := dir.Open(n)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(fh, MaxRenderedPage+1))
		fh.Close()
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > MaxRenderedPage {
			return nil, fmt.Errorf("the rendered page is over %d bytes", MaxRenderedPage)
		}
		return data, nil
	}
	return nil, errors.New("the rasterize helper wrote no PNG (expected {out_prefix}*.png or {out})")
}

// recordingExt is the extension the converter's input copy gets, so a tool
// that reads the format from the name is not misled.
func recordingExt(mt string) string {
	switch mt {
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4":
		return ".m4a"
	case "video/mp4":
		return ".mp4"
	case "audio/flac":
		return ".flac"
	case "audio/ogg", "application/ogg":
		return ".ogg"
	case "video/webm", "audio/webm":
		return ".webm"
	case "audio/wav", "audio/wave", "audio/x-wav":
		return ".wav"
	}
	return ".bin"
}
