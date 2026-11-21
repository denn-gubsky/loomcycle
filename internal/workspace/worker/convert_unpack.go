package worker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/filenames"
	"github.com/denn-gubsky/loomcycle/internal/files/unpack"
	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// convert_unpack (RFC GW D34): an archive in a workspace is walked in the
// conversion child, which sends what it keeps as an unpack tar. This
// process never parses the archive: streamed back, the tar passes through
// to the core as it is; into a workspace, it is read with an
// unpack.Replayer (which holds every path and size to what the walk may
// make) and each file is written into a stage under the target's quota.
// The stage is not committed here: the core judges every file's final path
// first, then commits it, or discards it.

// unpackOp runs one convert_unpack on the workspace at real.
func (w *worker) unpackOp(ctx context.Context, real string, in workspace.ConvertUnpackArgs, out *chunkWriter) answer {
	if in.PerFile <= 0 || in.MaxListed <= 0 || in.MaxWritten <= 0 || in.MaxTotal <= 0 {
		return refuse(workspace.ClassInvalid, "an unpack names its bounds: per_file, max_listed, max_written and max_total")
	}
	src, done, a := openSource(real, in.ConvertSource)
	if a != nil {
		return *a
	}
	defer done()
	if a := w.needLabels([]string{wire.LabelUnpack}); a != nil {
		return *a
	}
	d := unpack.DefaultLimits
	opt := unpack.Options{Limits: unpack.Limits{Listed: in.MaxListed, Written: in.MaxWritten, Total: in.MaxTotal, Ratio: d.Ratio, RatioFree: d.RatioFree},
		PerFile: in.PerFile, Budget: in.Budget, DryRun: in.DryRun, All: in.All}
	if in.To.Workspace == "" {
		if err := w.opt.Convert.Unpack(ctx, src.f, src.size, opt, out); err != nil {
			return unpackError(err, in.Path)
		}
		return answer{}
	}
	return w.unpackInto(ctx, in, src, opt)
}

// unpackError is an unpack's failure as an answer: an archive refused whole
// keeps its reason.
func unpackError(err error, shown string) answer {
	var r *unpack.Refusal
	if errors.As(err, &r) {
		return refuse(workspace.ClassUnpackRefused, "%s", r.Reason)
	}
	return convertError(err, fmt.Sprintf("%q", shown))
}

// unpackInto writes the archive's files into a stage of the target
// workspace and hands the stage to the core. A walk that stopped at a bound
// leaves nothing: the target gets the whole archive or none of it.
func (w *worker) unpackInto(ctx context.Context, in workspace.ConvertUnpackArgs, src *convertSource, opt unpack.Options) answer {
	t, a := w.openTarget(in.To)
	if a != nil {
		return *a
	}
	defer t.close()

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	produced := make(chan error, 1)
	safego.Go(cctx, "workspace.worker.unpack", func() {
		// Failure until the child says otherwise: a recovered panic must
		// still end the reader and report.
		err := errors.New("the unpack stopped")
		defer func() { pw.CloseWithError(err); produced <- err }()
		err = w.opt.Convert.Unpack(cctx, src.f, src.size, opt, pw)
	})

	taken := map[string]bool{}
	var writeErr *answer
	rp := &unpack.Replayer{PerFile: in.PerFile, MaxWritten: in.MaxWritten,
		Planned: func(segs []string) string { return path.Join(segs...) },
		Write: func(e unpack.Entry, head []byte, body io.Reader) (unpack.Written, string, error) {
			rel := unusedPath(taken, path.Join(e.Segs...))
			if a := w.stageStream(cctx, t, rel, e.Size, io.MultiReader(bytes.NewReader(head), body)); a != nil {
				writeErr = a
				return unpack.Written{}, "", errors.New(a.detail)
			}
			return unpack.Written{Ref: rel, Name: e.Display, Size: e.Size, MediaType: filenames.Sniff("", head, path.Base(rel))}, "", nil
		}}
	// stopErr is the tar holding what the walk never makes (Replayer);
	// tarErr the stream ending early, with the child's own error when it
	// failed (the pipe carries it).
	var stopErr, tarErr error
	tr := tar.NewReader(pr)
	for {
		h, err := tr.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				tarErr = err
			}
			break
		}
		if err := rp.Entry(h, tr); err != nil {
			stopErr = err
			break
		}
	}
	cancel()
	_ = pr.CloseWithError(errors.New("the unpack's reader stopped"))
	childErr := <-produced
	switch {
	case writeErr != nil:
		// Over the quota, or a path the stage cannot hold: nothing stays.
		return *writeErr
	case childErr != nil && (stopErr == nil || errors.Is(childErr, convert.ErrQuarantined)):
		// The child's own failure: a refusal, a crash (quarantined by the
		// core), a time out. Once this side stopped reading, the child's
		// error is only that, unless it crashed meanwhile.
		return unpackError(childErr, in.Path)
	case stopErr != nil || tarErr != nil:
		return refuse(workspace.ClassConversionFailed, "the conversion child's unpack of %q could not be read: %v", in.Path, errors.Join(stopErr, tarErr))
	}
	kind, res, err := rp.Result()
	if err != nil {
		return refuse(workspace.ClassConversionFailed, "the conversion child's unpack of %q could not be read: %v", in.Path, err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return refuse(workspace.ClassFailed, "encoding the unpack's result: %v", err)
	}
	ans := workspace.ConvertUnpackAnswer{Format: kind, Result: raw}
	if res.Stopped == "" && !res.DryRun {
		// The core commits it once it has judged every path in it; until
		// then it is a stage like any other, removed at release.
		t.committed = true
		ans.Stage = t.stage
	}
	return jsonAnswer(ans)
}

// unusedPath is rel, or rel with -2, -3, ... before its extension when an
// earlier file of the archive took it: an archive may name one path twice.
func unusedPath(taken map[string]bool, rel string) string {
	out := rel
	dir, name := path.Split(rel)
	ext := filenames.FileExt(name)
	for i := 2; taken[out]; i++ {
		out = dir + strings.TrimSuffix(name, ext) + "-" + strconv.Itoa(i) + ext
	}
	taken[out] = true
	return out
}

// stageStream writes size bytes of r as rel in the target's stage, counted
// against the target's quota under its write lock, as a put is: a
// temporary file, synced, renamed into the stage. stageWrite is the same
// for bytes in hand.
func (w *worker) stageStream(ctx context.Context, t *convTarget, rel string, size int64, r io.Reader) *answer {
	clean, bad := opPath(rel, false)
	if bad != nil {
		return bad
	}
	u, release, err := w.writeLock(ctx, t.id, t.v)
	if err != nil {
		return refusal(workspace.ClassFailed, "%v", err)
	}
	defer release()
	if bad := u.admit(size); bad != nil {
		return bad
	}
	td, err := openDir(t.v, filepath.FromSlash(tmpDir), true)
	if err != nil {
		x := classify(err, "making the temporary file")
		return &x
	}
	defer td.Close()
	f, name, err := fsops.CreateTemp(td, "unpack-")
	if err != nil {
		return refusal(workspace.ClassFailed, "making the temporary file: %v", err)
	}
	n, err := io.Copy(f, io.LimitReader(r, size+1))
	if err == nil && n != size {
		err = fmt.Errorf("%d bytes, not the %d announced", n, size)
	}
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
	u.used += size
	w.settleLocked(ctx, u, t.v)
	return nil
}
