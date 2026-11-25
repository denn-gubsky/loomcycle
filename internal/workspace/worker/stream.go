package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// streamOp reports whether op carries bytes on the stream itself (get and
// the conversions as their output, put as its input, a tunnel both ways at
// once) and so is served by serveStream, which never holds them whole:
// serveOne would read all of the input first and collect all of the output.
func streamOp(op string) bool {
	return op == workspace.OpGet || op == workspace.OpPut || op == workspace.OpTunnel || isConvertOp(op)
}

// serveStream runs a get or a put on its stream. A put reads its input
// frame by frame into a temporary file; a get sends its output frame by
// frame from the file. Either way the answer then ends the op as serveOne's
// does.
func (w *worker) serveStream(ctx, sctx context.Context, cancel context.CancelFunc, stream workspacepb.WorkspaceService_OpClient, start *workspacepb.OpStart) error {
	id := start.GetWorkspaceId()
	dir, real, a := w.workspaceDir(id)
	if a.class == "" && start.GetOp() == workspace.OpTunnel {
		t, done := w.tunnelOp(sctx, cancel, id, real, start, stream)
		return finish(ctx, stream, hidePaths(t, false, real, dir, w.opt.Root), done)
	}
	if a.class == "" && start.GetOp() == workspace.OpPut {
		a = w.putOp(sctx, id, real, start, stream)
	}
	// A put refused before its input ended leaves the rest of it on the
	// stream: this drains it, and sees the core end the stream.
	done := watchEnd(sctx, cancel, stream)
	if a.class == "" && start.GetOp() == workspace.OpGet {
		a = getOp(sctx, real, start.GetArgs(), stream)
	}
	if a.class == "" && isConvertOp(start.GetOp()) {
		a = w.convertOp(sctx, id, real, start, stream)
	}
	return finish(ctx, stream, hidePaths(a, false, real, dir), done)
}

// workspaceDir is workspace id's directory and its real path, or a refusal.
func (w *worker) workspaceDir(id string) (dir, real string, a answer) {
	if !workspace.ValidID(id) {
		return "", "", refuse(workspace.ClassInvalid, "malformed workspace id")
	}
	// The id holds no separator, so dir is a direct child of the root.
	dir = filepath.Join(w.opt.Root, id)
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() {
		return "", "", refuse(workspace.ClassNotFound, "the workspace is not on this worker")
	}
	real, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", refuse(workspace.ClassFailed, "reading the workspace: %v", err)
	}
	return dir, real, answer{}
}

// putOp writes the op's input to Path. The bytes go to a temporary file in
// the work folder as they arrive, are synced, and the file is renamed into
// place only when exactly Size bytes came and InputEnd closed them; on any
// other ending the temporary file is removed and nothing is left. The quota
// is checked for Size before a byte is read, under the write lock held to
// the end.
//
// An unknown size (PutSizeUnknown) is admitted for MaxBytes instead: the
// write lock held to the end is what reserves it, since no other write of
// the workspace's files runs meanwhile, and the count then takes the bytes
// that came, which releases the rest. Once admitted the worker says so with
// an empty output chunk, so the core reads nothing of an input it would
// refuse (a download body stays unread past its headers).
func (w *worker) putOp(ctx context.Context, id, real string, start *workspacepb.OpStart, stream workspacepb.WorkspaceService_OpClient) answer {
	if !start.GetHasInput() {
		return refuse(workspace.ClassInvalid, "put takes the file's bytes as the op's input")
	}
	var in workspace.PutArgs
	if a := decodeArgs(start.GetArgs(), &in); a.class != "" {
		return a
	}
	need, a := putNeed(in)
	if a != nil {
		return *a
	}
	rel, bad := opPath(in.Path, false)
	if bad != nil {
		return *bad
	}
	target, bad := stagePath(in.Stage, rel)
	if bad != nil {
		return *bad
	}
	v, err := fsops.OpenVolumeFS(real, "")
	if err != nil {
		return refuse(workspace.ClassFailed, "opening the workspace: %v", err)
	}
	defer v.Close()
	u, release, err := w.writeLock(ctx, id, v)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer release()
	if bad := u.admit(need); bad != nil {
		return *bad
	}
	if in.Stage != "" {
		if a := stageExists(v, in.Stage); a != nil {
			return *a
		}
	}
	if a := checkTarget(v, target, in.Path, in.Overwrite); a != nil {
		return *a
	}
	td, err := openDir(v, filepath.FromSlash(tmpDir), true)
	if err != nil {
		return classify(err, "making the temporary file")
	}
	defer td.Close()
	f, name, err := fsops.CreateTemp(td, "put-")
	if err != nil {
		return refuse(workspace.ClassFailed, "making the temporary file: %v", err)
	}
	placed := false
	defer func() {
		if !placed {
			_ = f.Close()
			_ = td.Remove(name)
		}
	}()
	if in.Size == workspace.PutSizeUnknown {
		if err := stream.Send(&workspacepb.OpFromWorker{Msg: &workspacepb.OpFromWorker_Output{Output: &workspacepb.Chunk{}}}); err != nil {
			return refuse(workspace.ClassFailed, "%v", err)
		}
	}
	n, sum, a := receive(stream, f, in.Size, in.MaxBytes)
	if a != nil {
		return *a
	}
	if err := f.Chmod(0o644); err != nil {
		return refuse(workspace.ClassFailed, "writing %q: %v", in.Path, err)
	}
	if err := f.Sync(); err != nil {
		return refuse(workspace.ClassFailed, "writing %q: %v", in.Path, err)
	}
	if err := f.Close(); err != nil {
		return refuse(workspace.ClassFailed, "writing %q: %v", in.Path, err)
	}
	freed, x := placeFile(v, filepath.Join(filepath.FromSlash(tmpDir), name), target, in.Overwrite)
	if x != nil {
		return *x
	}
	placed = true
	u.used = max(u.used+n-freed, 0)
	w.settleLocked(ctx, u, v)
	return jsonAnswer(workspace.PutAnswer{Size: n, SHA256: sum, Usage: u.snapshot()})
}

// putNeed is what a put asks of the quota: its Size, or its MaxBytes when
// its size is unknown. A malformed pair is refused.
func putNeed(in workspace.PutArgs) (int64, *answer) {
	if in.Size == workspace.PutSizeUnknown {
		if in.MaxBytes <= 0 || in.MaxBytes > workspace.MaxDiskQuotaBytes {
			return 0, refusal(workspace.ClassInvalid, "a put of unknown size takes max_bytes from 1 to %d", int64(workspace.MaxDiskQuotaBytes))
		}
		return in.MaxBytes, nil
	}
	if in.Size < 0 || in.Size > workspace.MaxDiskQuotaBytes {
		return 0, refusal(workspace.ClassInvalid, "size must be from 0 to %d, or %d with max_bytes", int64(workspace.MaxDiskQuotaBytes), workspace.PutSizeUnknown)
	}
	if in.MaxBytes != 0 {
		return 0, refusal(workspace.ClassInvalid, "max_bytes goes only with a size of %d (unknown)", workspace.PutSizeUnknown)
	}
	return in.Size, nil
}

// checkTarget refuses a put onto a folder, or onto a file without
// overwrite, before any byte is read. placeFile checks again at the rename.
func checkTarget(v *fsops.VolumeFS, target, shown string, overwrite bool) *answer {
	parent, err := openDir(v, filepath.Dir(target), false)
	var nf errPathNotFound
	if errors.As(err, &nf) {
		return nil // made at the rename
	}
	if err != nil {
		x := classify(err, "put %q", shown)
		return &x
	}
	defer parent.Close()
	fi, err := lstatIn(parent, filepath.Base(target))
	switch {
	case err != nil:
		x := classify(err, "put %q", shown)
		return &x
	case fi == nil:
		return nil
	case fi.IsDir():
		return refusal(workspace.ClassExists, "%q is a folder", shown)
	case !overwrite:
		return refusal(workspace.ClassExists, "%q is already there; ask to overwrite it", shown)
	}
	return nil
}

// receive copies the op's input into f, frame by frame, and returns how
// many bytes came and their sha256. More than size bytes, or InputEnd
// before size, is a mismatch; with size PutSizeUnknown, more than maxBytes
// is.
func receive(stream workspacepb.WorkspaceService_OpClient, f *os.File, size, maxBytes int64) (int64, string, *answer) {
	bound, what := size, "declared"
	if size == workspace.PutSizeUnknown {
		bound, what = maxBytes, "maximum"
	}
	h := sha256.New()
	var n int64
	for {
		fr, err := stream.Recv()
		if err != nil {
			return 0, "", refusal(workspace.ClassFailed, "the input ended before InputEnd: %v", err)
		}
		switch {
		case fr.GetInput() != nil:
			data := fr.GetInput().GetData()
			if n+int64(len(data)) > bound {
				return 0, "", refusal(workspace.ClassSizeMismatch, "the input carries more than its %s %d bytes", what, bound)
			}
			if _, err := f.Write(data); err != nil {
				return 0, "", refusal(workspace.ClassFailed, "writing: %v", err)
			}
			h.Write(data)
			n += int64(len(data))
		case fr.GetInputEnd() != nil:
			if size != workspace.PutSizeUnknown && n != size {
				return 0, "", refusal(workspace.ClassSizeMismatch, "the input carried %d bytes, not its declared %d", n, size)
			}
			return n, hex.EncodeToString(h.Sum(nil)), nil
		default:
			return 0, "", refusal(workspace.ClassInvalid, "an unexpected frame in the op's input")
		}
	}
}

// getOp sends Length bytes of Path from Offset (to the end when Length is
// zero) as the op's output, one chunk at a time, each in a buffer of its
// own. A file that ends before the bytes it had at the open fails the op.
func getOp(ctx context.Context, real string, raw []byte, stream workspacepb.WorkspaceService_OpClient) answer {
	var in workspace.GetArgs
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	if in.Offset < 0 || in.Length < 0 {
		return refuse(workspace.ClassInvalid, "offset and length may not be negative")
	}
	rel, bad := opPath(in.Path, false)
	if bad != nil {
		return *bad
	}
	v, err := fsops.OpenVolumeFS(real, "")
	if err != nil {
		return refuse(workspace.ClassFailed, "opening the workspace: %v", err)
	}
	defer v.Close()
	parent, err := openDir(v, filepath.Dir(rel), false)
	if err != nil {
		return classify(err, "get %q", in.Path)
	}
	defer parent.Close()
	base := filepath.Base(rel)
	fi, err := lstatIn(parent, base)
	switch {
	case err != nil:
		return classify(err, "get %q", in.Path)
	case fi == nil:
		return refuse(workspace.ClassPathNotFound, "%q is not in the workspace", in.Path)
	case fi.Mode()&os.ModeSymlink != 0:
		return refuse(workspace.ClassInvalid, "%q %v", in.Path, errLink)
	case fi.IsDir():
		return refuse(workspace.ClassInvalid, "%q is a folder", in.Path)
	case !fi.Mode().IsRegular():
		return refuse(workspace.ClassNotRegular, "%q %v", in.Path, fsops.ErrNotRegular)
	}
	f, err := v.OpenIn(parent, base)
	if err != nil {
		return classify(err, "get %q", in.Path)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return refuse(workspace.ClassFailed, "get %q: %v", in.Path, err)
	}
	if in.Offset > st.Size() {
		return refuse(workspace.ClassInvalid, "offset %d is past the end of the file (%d bytes)", in.Offset, st.Size())
	}
	left := st.Size() - in.Offset
	if in.Length > 0 && in.Length < left {
		left = in.Length
	}
	if _, err := f.Seek(in.Offset, io.SeekStart); err != nil {
		return refuse(workspace.ClassFailed, "get %q: %v", in.Path, err)
	}
	for left > 0 {
		if err := ctx.Err(); err != nil {
			return refuse(workspace.ClassFailed, "%v", err)
		}
		buf := make([]byte, min(left, workspace.ChunkSize))
		if _, err := io.ReadFull(f, buf); err != nil {
			return refuse(workspace.ClassFailed, "get %q: the file changed while it was read: %v", in.Path, err)
		}
		if err := stream.Send(&workspacepb.OpFromWorker{Msg: &workspacepb.OpFromWorker_Output{Output: &workspacepb.Chunk{Data: buf}}}); err != nil {
			return refuse(workspace.ClassFailed, "%v", err)
		}
		left -= int64(len(buf))
	}
	return answer{}
}
