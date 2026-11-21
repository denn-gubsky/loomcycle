package worker

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/files/filenames"
	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// The conversion ops (RFC GW D34). This process opens the source, checks it
// is still the file the core judged, types it from its first bytes and
// writes what the conversion child sends back; the child alone parses it.

// helperPrograms names what a missing helper label stands for, so a
// refusal tells the operator what to install where the worker runs.
var helperPrograms = map[string]string{
	wire.LabelRender:     "pdftoppm (transcription.ocr.rasterize)",
	wire.LabelMedia:      "ffmpeg (transcription.stt.media)",
	wire.LabelEncode:     "ffmpeg (speech.encode)",
	wire.LabelWritePDF:   "typst",
	wire.LabelDraw:       "merman-cli",
	wire.LabelDjVu:       "DjVuLibre's djvutxt",
	wire.LabelDjVuRender: "DjVuLibre's ddjvu",
}

// isConvertOp reports whether op is a conversion op.
func isConvertOp(op string) bool {
	return op == workspace.OpConvertExtract || op == workspace.OpConvertRenderHTML || op == workspace.OpConvertUnpack
}

// convertOp runs one conversion op on workspace id (real is its real path).
func (w *worker) convertOp(ctx context.Context, id, real string, start *workspacepb.OpStart, stream workspacepb.WorkspaceService_OpClient) answer {
	if w.opt.Convert == nil {
		return refuse(workspace.ClassHelperMissing, "this worker converts no file: its conversion child could not be started")
	}
	out := &chunkWriter{stream: stream}
	switch start.GetOp() {
	case workspace.OpConvertExtract:
		var in workspace.ConvertExtractArgs
		if a := decodeArgs(start.GetArgs(), &in); a.class != "" {
			return a
		}
		return w.extractOp(ctx, real, in, out)
	case workspace.OpConvertRenderHTML:
		var in workspace.ConvertRenderHTMLArgs
		if a := decodeArgs(start.GetArgs(), &in); a.class != "" {
			return a
		}
		return w.renderHTMLOp(ctx, real, in, out)
	case workspace.OpConvertUnpack:
		var in workspace.ConvertUnpackArgs
		if a := decodeArgs(start.GetArgs(), &in); a.class != "" {
			return a
		}
		return w.unpackOp(ctx, real, in, out)
	}
	return refuse(workspace.ClassUnsupportedOp, "this worker does not run %q", start.GetOp())
}

// chunkWriter sends what is written as the op's output, in chunks of at
// most ChunkSize, each a buffer of its own.
type chunkWriter struct {
	stream workspacepb.WorkspaceService_OpClient
	n      int64
}

func (c *chunkWriter) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += workspace.ChunkSize {
		chunk := append([]byte(nil), p[off:min(off+workspace.ChunkSize, len(p))]...)
		if err := c.stream.Send(&workspacepb.OpFromWorker{Msg: &workspacepb.OpFromWorker_Output{Output: &workspacepb.Chunk{Data: chunk}}}); err != nil {
			return off, err
		}
	}
	c.n += int64(len(p))
	return len(p), nil
}

// convertSource is an opened source file.
type convertSource struct {
	f    *os.File
	size int64
	name string
	mt   string
}

// openSource opens the source a conversion reads in the workspace at real:
// a regular file reached through no link, of at most in.MaxBytes, that
// still hashes to in.SHA256, typed from its first bytes and its name. The
// caller closes it.
func openSource(real string, in workspace.ConvertSource) (*convertSource, func(), *answer) {
	if len(in.SHA256) != sha256.Size*2 {
		return nil, nil, refusal(workspace.ClassInvalid, "a conversion names its source's sha256")
	}
	rel, bad := opPath(in.Path, false)
	if bad != nil {
		return nil, nil, bad
	}
	v, err := fsops.OpenVolumeFS(real, "")
	if err != nil {
		return nil, nil, refusal(workspace.ClassFailed, "opening the workspace: %v", err)
	}
	parent, err := openDir(v, filepath.Dir(rel), false)
	if err != nil {
		v.Close()
		x := classify(err, "convert %q", in.Path)
		return nil, nil, &x
	}
	base := filepath.Base(rel)
	closeAll := func() { parent.Close(); v.Close() }
	fi, err := lstatIn(parent, base)
	var a *answer
	switch {
	case err != nil:
		x := classify(err, "convert %q", in.Path)
		a = &x
	case fi == nil:
		a = refusal(workspace.ClassPathNotFound, "%q is not in the workspace", in.Path)
	case fi.Mode()&os.ModeSymlink != 0:
		a = refusal(workspace.ClassInvalid, "%q %v", in.Path, errLink)
	case fi.IsDir():
		a = refusal(workspace.ClassInvalid, "%q is a folder", in.Path)
	case !fi.Mode().IsRegular():
		a = refusal(workspace.ClassNotRegular, "%q %v", in.Path, fsops.ErrNotRegular)
	}
	if a != nil {
		closeAll()
		return nil, nil, a
	}
	f, err := v.OpenIn(parent, base)
	if err != nil {
		closeAll()
		x := classify(err, "convert %q", in.Path)
		return nil, nil, &x
	}
	done := func() { f.Close(); closeAll() }
	st, err := f.Stat()
	if err != nil {
		done()
		return nil, nil, refusal(workspace.ClassFailed, "convert %q: %v", in.Path, err)
	}
	if in.MaxBytes > 0 && st.Size() > in.MaxBytes {
		done()
		return nil, nil, refusal(workspace.ClassInvalid, "%q is %d bytes, over the %d a converted file may be", in.Path, st.Size(), in.MaxBytes)
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, st.Size())); err != nil {
		done()
		return nil, nil, refusal(workspace.ClassFailed, "reading %q: %v", in.Path, err)
	}
	if hex.EncodeToString(h.Sum(nil)) != strings.ToLower(in.SHA256) {
		done()
		return nil, nil, refusal(workspace.ClassSourceChanged, "%q changed since it was checked; try again", in.Path)
	}
	head := make([]byte, 512)
	n, _ := f.ReadAt(head, 0)
	name := filepath.Base(rel)
	return &convertSource{f: f, size: st.Size(), name: name, mt: filenames.Sniff("", head[:n], name)}, done, nil
}

// needLabels refuses a conversion whose labels this worker's child lacks,
// naming the helper.
func (w *worker) needLabels(need []string) *answer {
	for _, l := range need {
		if slices.Contains(w.opt.Labels, l) {
			continue
		}
		if p, ok := helperPrograms[l]; ok {
			return refusal(workspace.ClassHelperMissing, "this workspace's worker cannot run %s (helper %q): install it where the worker runs (the loomcycle-worker image carries it)", p, l)
		}
		return refusal(workspace.ClassHelperMissing, "this workspace's worker does not convert %q; update it to this server's version", l)
	}
	return nil
}

// convertError is a conversion's failure as an answer.
func convertError(err error, what string) answer {
	switch {
	case errors.Is(err, convert.ErrQuarantined):
		return refuse(workspace.ClassConversionCrashed, "%s crashed the conversion child", what)
	case errors.Is(err, convert.ErrNoHelper), errors.Is(err, convert.ErrNotConfigured):
		return refuse(workspace.ClassHelperMissing, "%s: %v", what, err)
	case errors.Is(err, convert.ErrBusy), errors.Is(err, convert.ErrNoWorkers):
		return refuse(workspace.ClassConversionBusy, "the conversion child is busy or restarting; try again")
	case errors.Is(err, extract.ErrUnsupported):
		return refuse(workspace.ClassConversionUnsupported, "nothing here reads %s", what)
	case errors.Is(err, context.DeadlineExceeded):
		return refuse(workspace.ClassConversionFailed, "%s took too long to convert", what)
	case extract.IsRefused(err):
		return refuse(workspace.ClassConversionFailed, "%s was refused: %s", what, strings.TrimPrefix(err.Error(), "extract: refused: "))
	}
	return refuse(workspace.ClassConversionFailed, "could not read %s: %s", what, strings.TrimPrefix(err.Error(), "extract: "))
}

// mdSink gathers an extraction's Markdown within the caller's bounds, as
// File op=extract's own sink does: max_chars characters, MaxMarkdown bytes.
type mdSink struct {
	md       strings.Builder
	maxChars int
	maxBytes int
	chars    int
	sampled  bool
	cut      bool
	starts   map[int]int
	image    func(extract.Image) (string, error)
}

func (s *mdSink) StartPage(n int) {
	if s.starts == nil {
		s.starts = map[int]int{}
	}
	s.starts[n] = s.md.Len()
}

func (s *mdSink) Text(md string) error {
	if s.maxChars > 0 {
		if s.sampled {
			return extract.ErrSinkFull
		}
		if n := utf8.RuneCountInString(md); s.chars+n > s.maxChars {
			md = cutRunes(md, s.maxChars-s.chars)
			s.sampled = true
		}
	}
	if s.md.Len()+len(md) > s.maxBytes {
		s.cut = true
		return extract.ErrSinkFull
	}
	s.md.WriteString(md)
	s.chars += utf8.RuneCountInString(md)
	if s.sampled {
		return extract.ErrSinkFull
	}
	return nil
}

func (s *mdSink) Image(img extract.Image) (string, error) {
	if s.image == nil {
		return "", nil
	}
	return s.image(img)
}

func cutRunes(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

// maxMarkdown bounds what a caller may ask the Markdown to hold.
const maxMarkdown = 64 << 20

// extractOp extracts the source's text and images: into a stage of the
// target workspace committed at To.Path, or as a tar streamed to out.
func (w *worker) extractOp(ctx context.Context, real string, in workspace.ConvertExtractArgs, out *chunkWriter) answer {
	if in.MaxMarkdown <= 0 || in.MaxMarkdown > maxMarkdown || in.MaxChars < 0 || in.MaxRows < 0 {
		return refuse(workspace.ClassInvalid, "max_markdown must be from 1 to %d; max_chars and max_rows may not be negative", maxMarkdown)
	}
	src, done, a := openSource(real, in.ConvertSource)
	if a != nil {
		return *a
	}
	defer done()
	if _, ok := extract.Default().For(src.mt, src.name); !ok && !extract.IsDjVu(src.mt, "") {
		return refuse(workspace.ClassConversionUnsupported, "cannot extract text from %q (%s)", in.Path, src.mt)
	}
	if a := w.needLabels(wire.ExtractLabels(src.mt, src.name, len(in.Pages) > 0, false)); a != nil {
		return *a
	}
	sink := &mdSink{maxChars: in.MaxChars, maxBytes: in.MaxMarkdown}
	slug := filenames.SanitizeName(strings.TrimSuffix(src.name, path.Ext(src.name)))
	if slug == "" {
		slug = "document"
	}
	opts := extract.Options{Name: src.name, Images: in.Images, MaxRows: in.MaxRows, InferHeadings: in.InferHeadings, Pages: in.Pages}
	if in.To.Workspace != "" {
		return w.extractInto(ctx, in, src, sink, slug, opts)
	}
	if in.ImageMark == "" {
		return refuse(workspace.ClassInvalid, "a streamed extraction names its image_mark")
	}
	tw := tar.NewWriter(out)
	images := 0
	sink.image = func(img extract.Image) (string, error) {
		images++
		hdr := &tar.Header{Name: workspace.ConvertTarImage + strconv.Itoa(images), Mode: 0o644, Size: int64(len(img.Data)), Format: tar.FormatPAX,
			PAXRecords: map[string]string{workspace.ConvertPAXName: img.Name, workspace.ConvertPAXType: img.MediaType, workspace.ConvertPAXAlt: img.Alt,
				workspace.ConvertPAXPage: strconv.Itoa(img.Page), workspace.ConvertPAXSlide: strconv.Itoa(img.Slide)}}
		if err := tw.WriteHeader(hdr); err != nil {
			return "", err
		}
		if _, err := tw.Write(img.Data); err != nil {
			return "", err
		}
		return in.ImageMark + strconv.Itoa(images) + workspace.ImageMarkEnd, nil
	}
	rep, err := w.opt.Convert.Extract(ctx, src.mt, src.f, src.size, sink, opts)
	if err != nil && !errors.Is(err, extract.ErrSinkFull) {
		return convertError(err, fmt.Sprintf("%q", in.Path))
	}
	ans, a := extractAnswer(src, sink, rep)
	if a != nil {
		return *a
	}
	if err := writeTarFile(tw, workspace.ConvertTarText, []byte(sink.md.String())); err != nil {
		return refuse(workspace.ClassFailed, "sending the text: %v", err)
	}
	b, _ := json.Marshal(ans)
	if err := writeTarFile(tw, workspace.ConvertTarAnswer, b); err != nil {
		return refuse(workspace.ClassFailed, "sending the answer: %v", err)
	}
	if err := tw.Close(); err != nil {
		return refuse(workspace.ClassFailed, "sending the answer: %v", err)
	}
	return answer{}
}

func extractAnswer(src *convertSource, sink *mdSink, rep extract.Report) (workspace.ConvertExtractAnswer, *answer) {
	raw, err := json.Marshal(rep)
	if err != nil {
		return workspace.ConvertExtractAnswer{}, refusal(workspace.ClassFailed, "encoding the report: %v", err)
	}
	return workspace.ConvertExtractAnswer{MediaType: src.mt, Report: raw, Sampled: sink.sampled, Cut: sink.cut, PageStarts: sink.starts}, nil
}

func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Format: tar.FormatPAX}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// extractInto writes the extraction into a stage of the target workspace,
// <slug>.md and images/<name>, and commits the stage at To.Path. On any
// failure the stage is dropped and nothing is left.
func (w *worker) extractInto(ctx context.Context, in workspace.ConvertExtractArgs, src *convertSource, sink *mdSink, slug string, opts extract.Options) answer {
	t, a := w.openTarget(in.To)
	if a != nil {
		return *a
	}
	defer t.close()
	var written []string
	taken := map[string]bool{}
	var notKept []string
	var writeErr *answer
	sink.image = func(img extract.Image) (string, error) {
		name := uniqueName(taken, filenames.WithExtension(filenames.SanitizeName(img.Name), img.MediaType))
		rel := path.Join("images", name)
		if a := w.stageWrite(ctx, t, rel, img.Data); a != nil {
			if a.class == workspace.ClassOverQuota {
				writeErr = a
				return "", extract.ErrSinkFull
			}
			if len(notKept) < 10 {
				notKept = append(notKept, img.Name+": "+a.detail)
			}
			return "", nil
		}
		written = append(written, path.Join(in.To.Path, rel))
		return rel, nil
	}
	rep, err := w.opt.Convert.Extract(ctx, src.mt, src.f, src.size, sink, opts)
	if writeErr != nil {
		return *writeErr
	}
	if err != nil && !errors.Is(err, extract.ErrSinkFull) {
		return convertError(err, fmt.Sprintf("%q", in.Path))
	}
	ans, a := extractAnswer(src, sink, rep)
	if a != nil {
		return *a
	}
	md := slug + ".md"
	text := sink.md.String()
	if sink.cut || sink.sampled {
		text += "\n\n[the rest of the document was not extracted]"
	}
	if a := w.stageWrite(ctx, t, md, []byte(text)); a != nil {
		return *a
	}
	usage, a := w.commitTarget(ctx, t)
	if a != nil {
		return *a
	}
	ans.Markdown, ans.Images, ans.NotKept, ans.Usage = path.Join(in.To.Path, md), written, notKept, &usage
	return jsonAnswer(ans)
}

// uniqueName is name, or name-2, name-3, ... when an earlier image took it.
func uniqueName(taken map[string]bool, name string) string {
	if name == "" {
		name = "image.bin"
	}
	out := name
	ext := filenames.FileExt(name)
	for i := 2; taken[out]; i++ {
		out = strings.TrimSuffix(name, ext) + "-" + strconv.Itoa(i) + ext
	}
	taken[out] = true
	return out
}

// renderHTMLOp reads an HTML file into Markdown for a render and streams
// the Markdown back. Pictures are not kept: a render never fetches.
func (w *worker) renderHTMLOp(ctx context.Context, real string, in workspace.ConvertRenderHTMLArgs, out *chunkWriter) answer {
	if in.MaxMarkdown <= 0 || in.MaxMarkdown > maxMarkdown {
		return refuse(workspace.ClassInvalid, "max_markdown must be from 1 to %d", maxMarkdown)
	}
	src, done, a := openSource(real, in.ConvertSource)
	if a != nil {
		return *a
	}
	defer done()
	if a := w.needLabels([]string{wire.LabelExtract}); a != nil {
		return *a
	}
	sink := &mdSink{maxBytes: in.MaxMarkdown}
	_, err := w.opt.Convert.Extract(ctx, "text/html", src.f, src.size, sink, extract.Options{Name: src.name})
	if err != nil && !errors.Is(err, extract.ErrSinkFull) {
		return convertError(err, fmt.Sprintf("%q", in.Path))
	}
	if _, err := io.WriteString(out, sink.md.String()); err != nil {
		return refuse(workspace.ClassFailed, "sending the Markdown: %v", err)
	}
	return answer{}
}

// convTarget is a stage in the workspace a conversion writes into.
type convTarget struct {
	id, stage string
	path      string
	overwrite bool
	v         *fsops.VolumeFS
	committed bool
	w         *worker
}

// openTarget opens the target workspace and makes a stage in it.
func (w *worker) openTarget(to workspace.ConvertTarget) (*convTarget, *answer) {
	if !workspace.ValidID(to.Workspace) {
		return nil, refusal(workspace.ClassInvalid, "malformed target workspace id")
	}
	rel, bad := opPath(to.Path, false)
	if bad != nil {
		return nil, bad
	}
	_, real, a := w.workspaceDir(to.Workspace)
	if a.class != "" {
		return nil, &a
	}
	v, err := fsops.OpenVolumeFS(real, "")
	if err != nil {
		return nil, refusal(workspace.ClassFailed, "opening the target workspace: %v", err)
	}
	return &convTarget{id: to.Workspace, stage: randomName(), path: filepath.ToSlash(rel), overwrite: to.Overwrite, v: v, w: w}, nil
}

// close drops the stage unless it was committed.
func (t *convTarget) close() {
	if !t.committed {
		_ = t.w.discardOp(context.Background(), t.id, t.v, workspace.DiscardArgs{Stage: t.stage})
	}
	t.v.Close()
}

func (w *worker) commitTarget(ctx context.Context, t *convTarget) (workspace.Usage, *answer) {
	a := w.commitOp(ctx, t.id, t.v, workspace.CommitArgs{Stage: t.stage, Path: t.path, Overwrite: t.overwrite})
	if a.class != "" {
		return workspace.Usage{}, &a
	}
	t.committed = true
	var u workspace.Usage
	_ = json.Unmarshal(a.output, &u)
	return u, nil
}

// stageWrite writes data as rel in the target's stage, counted against the
// target's quota under its write lock: a temporary file, synced, renamed
// into the stage.
func (w *worker) stageWrite(ctx context.Context, t *convTarget, rel string, data []byte) *answer {
	clean, bad := opPath(rel, false)
	if bad != nil {
		return bad
	}
	u, release, err := w.writeLock(ctx, t.id, t.v)
	if err != nil {
		return refusal(workspace.ClassFailed, "%v", err)
	}
	defer release()
	if bad := u.admit(int64(len(data))); bad != nil {
		return bad
	}
	td, err := openDir(t.v, filepath.FromSlash(tmpDir), true)
	if err != nil {
		x := classify(err, "making the temporary file")
		return &x
	}
	defer td.Close()
	f, name, err := fsops.CreateTemp(td, "conv-")
	if err != nil {
		return refusal(workspace.ClassFailed, "making the temporary file: %v", err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = td.Remove(name)
		return refusal(workspace.ClassFailed, "writing %q: %v", rel, err)
	}
	if _, a := placeFile(t.v, filepath.Join(filepath.FromSlash(tmpDir), name), filepath.Join(filepath.FromSlash(stageDir), t.stage, clean), false); a != nil {
		_ = td.Remove(name)
		return a
	}
	u.used += int64(len(data))
	w.settleLocked(ctx, u, t.v)
	return nil
}
