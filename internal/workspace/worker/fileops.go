package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// The raw-byte file ops that take and answer small JSON (RFC GW P2). get and
// put, which carry a file's bytes, are in stream.go. Every op opens the
// workspace through fsops and walks a path one folder at a time, so `..`, an
// absolute path and a link anywhere on the path are refused (opPath,
// openDir), and nothing outside the workspace is reached.

func jsonAnswer(v any) answer {
	out, err := json.Marshal(v)
	if err != nil {
		return refuse(workspace.ClassFailed, "encoding the answer: %v", err)
	}
	return answer{output: out}
}

// fileOp runs one of the JSON file ops in the workspace id whose real path
// is real.
func (w *worker) fileOp(ctx context.Context, id, real, op string, raw []byte) answer {
	v, err := fsops.OpenVolumeFS(real, "")
	if err != nil {
		return refuse(workspace.ClassFailed, "opening the workspace: %v", err)
	}
	defer v.Close()
	switch op {
	case workspace.OpFStat:
		var in workspace.FStatArgs
		if a := decodeArgs(raw, &in); a.class != "" {
			return a
		}
		return fstatOp(v, in)
	case workspace.OpFList:
		var in workspace.FListArgs
		if a := decodeArgs(raw, &in); a.class != "" {
			return a
		}
		return flsOp(v, in)
	case workspace.OpMkdir:
		var in workspace.MkdirArgs
		if a := decodeArgs(raw, &in); a.class != "" {
			return a
		}
		return w.mkdirOp(ctx, id, v, in)
	case workspace.OpFRemove:
		var in workspace.FRemoveArgs
		if a := decodeArgs(raw, &in); a.class != "" {
			return a
		}
		return w.fremoveOp(ctx, id, v, in)
	case workspace.OpStage:
		return w.stageOp(ctx, id, v)
	case workspace.OpCommit:
		var in workspace.CommitArgs
		if a := decodeArgs(raw, &in); a.class != "" {
			return a
		}
		return w.commitOp(ctx, id, v, in)
	case workspace.OpDiscard:
		var in workspace.DiscardArgs
		if a := decodeArgs(raw, &in); a.class != "" {
			return a
		}
		return w.discardOp(ctx, id, v, in)
	case workspace.OpCopyLocal:
		var in workspace.CopyLocalArgs
		if a := decodeArgs(raw, &in); a.class != "" {
			return a
		}
		return w.copyLocalOp(ctx, id, v, in)
	}
	return refuse(workspace.ClassUnsupportedOp, "this worker does not run %q", op)
}

func fstatOp(v *fsops.VolumeFS, in workspace.FStatArgs) answer {
	rel, bad := opPath(in.Path, true)
	if bad != nil {
		return *bad
	}
	parent, err := openDir(v, filepath.Dir(rel), false)
	if err != nil {
		return classify(err, "fstat %q", in.Path)
	}
	defer parent.Close()
	base := filepath.Base(rel)
	fi, err := lstatIn(parent, base)
	if err != nil {
		return classify(err, "fstat %q", in.Path)
	}
	if fi == nil {
		return refuse(workspace.ClassPathNotFound, "%q is not in the workspace", in.Path)
	}
	out := fileInfo(rel, fi)
	if in.SHA256 && fi.Mode().IsRegular() {
		f, err := v.OpenIn(parent, base)
		if err != nil {
			return classify(err, "fstat %q", in.Path)
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return refuse(workspace.ClassFailed, "hashing %q: %v", in.Path, err)
		}
		out.SHA256 = hex.EncodeToString(h.Sum(nil))
	}
	return jsonAnswer(out)
}

func flsOp(v *fsops.VolumeFS, in workspace.FListArgs) answer {
	rel, bad := opPath(in.Path, true)
	if bad != nil {
		return *bad
	}
	limit := in.Limit
	if limit <= 0 {
		limit = workspace.DefaultListLimit
	}
	limit = min(limit, workspace.MaxListLimit)
	dir, err := openDir(v, rel, false)
	if err != nil {
		return classify(err, "fls %q", in.Path)
	}
	defer dir.Close()
	out := workspace.FListAnswer{Entries: []workspace.FileInfo{}}
	walkTree(v, dir, "", func(_ *os.Root, p string, fi os.FileInfo) bool {
		full := filepath.Join(rel, p)
		if full == workDir {
			return true // the worker's own folder is never listed
		}
		if len(out.Entries) >= limit {
			out.Truncated = true
			return false
		}
		out.Entries = append(out.Entries, fileInfo(full, fi))
		return true
	}, func(p string) bool { return in.Recursive && filepath.Join(rel, p) != workDir })
	return jsonAnswer(out)
}

func (w *worker) mkdirOp(ctx context.Context, id string, v *fsops.VolumeFS, in workspace.MkdirArgs) answer {
	rel, bad := opPath(in.Path, false)
	if bad != nil {
		return *bad
	}
	target, bad := stagePath(in.Stage, rel)
	if bad != nil {
		return *bad
	}
	u, release, err := w.writeLock(ctx, id, v)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer release()
	if bad := u.admit(0); bad != nil {
		return *bad
	}
	if in.Stage != "" {
		if a := stageExists(v, in.Stage); a != nil {
			return *a
		}
	}
	parent, err := openDir(v, filepath.Dir(target), true)
	if err != nil {
		return classify(err, "mkdir %q", in.Path)
	}
	defer parent.Close()
	base := filepath.Base(target)
	fi, err := lstatIn(parent, base)
	switch {
	case err != nil:
		return classify(err, "mkdir %q", in.Path)
	case fi == nil:
		if err := parent.Mkdir(base, 0o755); err != nil {
			return classify(err, "mkdir %q", in.Path)
		}
	case !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0:
		return refuse(workspace.ClassExists, "%q is already there and is not a folder", in.Path)
	}
	return answer{}
}

func stageExists(v *fsops.VolumeFS, stage string) *answer {
	if !validStage(stage) {
		return refusal(workspace.ClassInvalid, "malformed stage id")
	}
	d, err := openDir(v, filepath.Join(filepath.FromSlash(stageDir), stage), false)
	if err != nil {
		return refusal(workspace.ClassPathNotFound, "no stage %s in the workspace; make one with stage", stage)
	}
	d.Close()
	return nil
}

func (w *worker) fremoveOp(ctx context.Context, id string, v *fsops.VolumeFS, in workspace.FRemoveArgs) answer {
	rel, bad := opPath(in.Path, false)
	if bad != nil {
		if in.Path == "" || in.Path == "." || in.Path == "/" {
			return refuse(workspace.ClassInvalid, "the workspace's root is not removed by fremove; release the workspace instead")
		}
		return *bad
	}
	u, release, err := w.writeLock(ctx, id, v)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer release()
	files, bytes, a := removeTree(ctx, v, rel)
	if a != nil {
		return *a
	}
	u.used = max(u.used-bytes, 0)
	w.settleLocked(ctx, u, v)
	return jsonAnswer(workspace.FRemoveAnswer{Files: files, Bytes: bytes, Usage: u.snapshot()})
}

// removeTree removes rel (a file, a link or a folder with what it holds) and
// says how many files and bytes went. A link is removed as a link.
func removeTree(ctx context.Context, v *fsops.VolumeFS, rel string) (files int, bytes int64, a *answer) {
	parent, err := openDir(v, filepath.Dir(rel), false)
	if err != nil {
		x := classify(err, "remove %q", filepath.ToSlash(rel))
		return 0, 0, &x
	}
	defer parent.Close()
	base := filepath.Base(rel)
	fi, err := lstatIn(parent, base)
	if err != nil {
		x := classify(err, "remove %q", filepath.ToSlash(rel))
		return 0, 0, &x
	}
	if fi == nil {
		return 0, 0, refusal(workspace.ClassPathNotFound, "%q is not in the workspace", filepath.ToSlash(rel))
	}
	if fi.IsDir() {
		dir, err := v.Step(parent, base)
		if err != nil {
			x := classify(err, "remove %q", filepath.ToSlash(rel))
			return 0, 0, &x
		}
		walkTree(v, dir, "", func(_ *os.Root, _ string, fi os.FileInfo) bool {
			if fi.Mode().IsRegular() {
				files++
				bytes += fi.Size()
			}
			return ctx.Err() == nil
		}, nil)
		dir.Close()
	} else if fi.Mode().IsRegular() {
		files, bytes = 1, fi.Size()
	}
	// os.Root's RemoveAll removes a link as a link and never leaves the
	// folder it was opened at.
	if err := parent.RemoveAll(base); err != nil {
		x := refuse(workspace.ClassFailed, "remove %q: %v", filepath.ToSlash(rel), err)
		return 0, 0, &x
	}
	return files, bytes, nil
}

func (w *worker) stageOp(ctx context.Context, id string, v *fsops.VolumeFS) answer {
	_, release, err := w.writeLock(ctx, id, v)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer release()
	stage := randomName()
	d, err := openDir(v, filepath.Join(filepath.FromSlash(stageDir), stage), true)
	if err != nil {
		return classify(err, "making a stage")
	}
	d.Close()
	return jsonAnswer(workspace.StageAnswer{Stage: stage})
}

func (w *worker) discardOp(ctx context.Context, id string, v *fsops.VolumeFS, in workspace.DiscardArgs) answer {
	if !validStage(in.Stage) {
		return refuse(workspace.ClassInvalid, "malformed stage id")
	}
	u, release, err := w.writeLock(ctx, id, v)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer release()
	_, bytes, a := removeTree(ctx, v, filepath.Join(filepath.FromSlash(stageDir), in.Stage))
	if a != nil && a.class != workspace.ClassPathNotFound {
		return *a
	}
	u.used = max(u.used-bytes, 0)
	w.settleLocked(ctx, u, v)
	return answer{}
}

func (w *worker) commitOp(ctx context.Context, id string, v *fsops.VolumeFS, in workspace.CommitArgs) answer {
	rel, bad := opPath(in.Path, false)
	if bad != nil {
		return *bad
	}
	u, release, err := w.writeLock(ctx, id, v)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer release()
	// A stage's bytes were counted as they were put: the stage is in the
	// workspace, so they were on its disk from the moment they arrived, and
	// the commit, a rename, adds none. Its check is only the full mark.
	if bad := u.admit(0); bad != nil {
		return *bad
	}
	if a := stageExists(v, in.Stage); a != nil {
		return *a
	}
	stage := filepath.Join(filepath.FromSlash(stageDir), in.Stage)
	freed, a := placeTree(ctx, v, stage, rel, in.Overwrite)
	if a != nil {
		return *a
	}
	u.used = max(u.used-freed, 0)
	w.settleLocked(ctx, u, v)
	return jsonAnswer(u.snapshot())
}

// placeTree moves the folder src (in the work folder) to rel. A rel not
// there yet takes it in one rename, so the folder appears whole or not at
// all. A folder already there is refused unless overwrite, which merges the
// files of src into it one by one, each replacing what it lands on; freed
// is the bytes the replaced files held. src is removed either way.
func placeTree(ctx context.Context, v *fsops.VolumeFS, src, rel string, overwrite bool) (freed int64, a *answer) {
	defer func() { _ = v.Root().RemoveAll(src) }()
	parent, err := openDir(v, filepath.Dir(rel), true)
	if err != nil {
		x := classify(err, "placing %q", filepath.ToSlash(rel))
		return 0, &x
	}
	defer parent.Close()
	fi, err := lstatIn(parent, filepath.Base(rel))
	if err != nil {
		x := classify(err, "placing %q", filepath.ToSlash(rel))
		return 0, &x
	}
	if fi == nil {
		// The parent was checked link-free one folder at a time; the rename
		// goes through the workspace's os.Root, so even a folder swapped
		// for a link since cannot carry it out of the workspace.
		if err := v.Root().Rename(src, rel); err != nil {
			x := refuse(workspace.ClassFailed, "placing %q: %v", filepath.ToSlash(rel), err)
			return 0, &x
		}
		return 0, nil
	}
	if !overwrite {
		return 0, refusal(workspace.ClassExists, "%q is already there; ask to overwrite it", filepath.ToSlash(rel))
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return 0, refusal(workspace.ClassExists, "%q is already there and is not a folder", filepath.ToSlash(rel))
	}
	sd, err := openDir(v, src, false)
	if err != nil {
		x := classify(err, "reading the stage")
		return 0, &x
	}
	defer sd.Close()
	var files []string
	walkTree(v, sd, "", func(_ *os.Root, p string, fi os.FileInfo) bool {
		if !fi.IsDir() {
			files = append(files, p)
		}
		return ctx.Err() == nil
	}, nil)
	for _, p := range files {
		n, x := placeFile(v, filepath.Join(src, p), filepath.Join(rel, p), true)
		if x != nil {
			return freed, x
		}
		freed += n
	}
	return freed, nil
}

// placeFile renames the file src (in the work folder) to rel, making
// missing parents; with overwrite it replaces a file there, and freed is
// that file's size. A folder at rel is never replaced.
func placeFile(v *fsops.VolumeFS, src, rel string, overwrite bool) (freed int64, a *answer) {
	parent, err := openDir(v, filepath.Dir(rel), true)
	if err != nil {
		x := classify(err, "placing %q", filepath.ToSlash(rel))
		return 0, &x
	}
	defer parent.Close()
	fi, err := lstatIn(parent, filepath.Base(rel))
	if err != nil {
		x := classify(err, "placing %q", filepath.ToSlash(rel))
		return 0, &x
	}
	if fi != nil {
		switch {
		case fi.IsDir():
			return 0, refusal(workspace.ClassExists, "%q is a folder", filepath.ToSlash(rel))
		case !overwrite:
			return 0, refusal(workspace.ClassExists, "%q is already there; ask to overwrite it", filepath.ToSlash(rel))
		case fi.Mode().IsRegular():
			freed = fi.Size()
		}
	}
	if err := v.Root().Rename(src, rel); err != nil {
		x := refuse(workspace.ClassFailed, "placing %q: %v", filepath.ToSlash(rel), err)
		return 0, &x
	}
	return freed, nil
}

// copyLocalOp copies FromPath of workspace FromWorkspace, on this worker,
// into this workspace (id). The source is listed first, and a copy over the
// copy bounds or the target's quota is refused before anything is written.
// The copy is built in a stage of the target and placed as commit places
// one, so a folder appears whole; on any failure the stage is removed.
func (w *worker) copyLocalOp(ctx context.Context, id string, v *fsops.VolumeFS, in workspace.CopyLocalArgs) answer {
	if !workspace.ValidID(in.FromWorkspace) {
		return refuse(workspace.ClassInvalid, "malformed source workspace id")
	}
	if in.MaxFiles <= 0 || in.MaxBytes <= 0 {
		return refuse(workspace.ClassInvalid, "max_files and max_bytes must be positive")
	}
	srcRel, bad := opPath(in.FromPath, true)
	if bad != nil {
		return *bad
	}
	rel, bad := opPath(in.Path, false)
	if bad != nil {
		return *bad
	}
	srcDir := filepath.Join(w.opt.Root, in.FromWorkspace)
	if st, err := os.Lstat(srcDir); err != nil || !st.IsDir() {
		return refuse(workspace.ClassNotFound, "the source workspace is not on this worker")
	}
	sv, err := fsops.OpenVolumeFS(srcDir, "")
	if err != nil {
		return refuse(workspace.ClassFailed, "opening the source workspace: %v", err)
	}
	defer sv.Close()
	plan, a := listCopy(ctx, sv, srcRel, in.MaxFiles, in.MaxBytes)
	if a != nil {
		return *a
	}
	u, release, err := w.writeLock(ctx, id, v)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer release()
	if bad := u.admit(plan.bytes); bad != nil {
		return *bad
	}
	stage := filepath.Join(filepath.FromSlash(stageDir), randomName())
	if a := copyFiles(ctx, sv, v, srcRel, stage, plan); a != nil {
		_ = v.Root().RemoveAll(stage)
		return *a
	}
	var freed int64
	if plan.dir {
		freed, a = placeTree(ctx, v, stage, rel, in.Overwrite)
	} else {
		freed, a = placeFile(v, filepath.Join(stage, plan.files[0].rel), rel, in.Overwrite)
		_ = v.Root().RemoveAll(stage)
	}
	if a != nil {
		return *a
	}
	u.used = max(u.used+plan.bytes-freed, 0)
	w.settleLocked(ctx, u, v)
	return jsonAnswer(workspace.CopyAnswer{Files: len(plan.files), Bytes: plan.bytes, Skipped: plan.skipped, Usage: u.snapshot()})
}

type copyFile struct {
	rel  string
	size int64
}

type copyPlan struct {
	dir     bool
	files   []copyFile
	dirs    []string // folders to make, empty ones included
	bytes   int64
	skipped []string
}

// listCopy lists what a copy of srcRel would write, refusing it when it
// passes maxFiles or maxBytes.
func listCopy(ctx context.Context, sv *fsops.VolumeFS, srcRel string, maxFiles int, maxBytes int64) (copyPlan, *answer) {
	var plan copyPlan
	parent, err := openDir(sv, filepath.Dir(srcRel), false)
	if err != nil {
		x := classify(err, "copy from %q", filepath.ToSlash(srcRel))
		return plan, &x
	}
	defer parent.Close()
	base := filepath.Base(srcRel)
	fi, err := lstatIn(parent, base)
	switch {
	case err != nil:
		x := classify(err, "copy from %q", filepath.ToSlash(srcRel))
		return plan, &x
	case fi == nil:
		return plan, refusal(workspace.ClassPathNotFound, "%q is not in the source workspace", filepath.ToSlash(srcRel))
	case fi.Mode()&os.ModeSymlink != 0:
		return plan, refusal(workspace.ClassInvalid, "%q %v", filepath.ToSlash(srcRel), errLink)
	case fi.Mode().IsRegular():
		if fi.Size() > maxBytes {
			return plan, refusal(workspace.ClassCopyLimit, "the file is %d bytes, over the %d bytes a copy may move (workspace.copy.max_bytes)", fi.Size(), maxBytes)
		}
		plan.files = []copyFile{{rel: base, size: fi.Size()}}
		plan.bytes = fi.Size()
		return plan, nil
	case !fi.IsDir():
		return plan, refusal(workspace.ClassInvalid, "%q is neither a file nor a folder", filepath.ToSlash(srcRel))
	}
	plan.dir = true
	dir, err := sv.Step(parent, base)
	if err != nil {
		x := classify(err, "copy from %q", filepath.ToSlash(srcRel))
		return plan, &x
	}
	defer dir.Close()
	var over *answer
	walkTree(sv, dir, "", func(_ *os.Root, p string, fi os.FileInfo) bool {
		if ctx.Err() != nil {
			return false
		}
		if srcRel == "." && p == workDir {
			return true
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			if len(plan.skipped) < workspace.MaxSkippedNamed {
				plan.skipped = append(plan.skipped, filepath.ToSlash(p))
			}
		case fi.IsDir():
			plan.dirs = append(plan.dirs, p)
		case fi.Mode().IsRegular():
			plan.files = append(plan.files, copyFile{rel: p, size: fi.Size()})
			plan.bytes += fi.Size()
			if len(plan.files) > maxFiles {
				over = refusal(workspace.ClassCopyLimit, "the folder holds more than %d files, the most a copy may move (workspace.copy.max_files)", maxFiles)
				return false
			}
			if plan.bytes > maxBytes {
				over = refusal(workspace.ClassCopyLimit, "the folder holds more than %d bytes, the most a copy may move (workspace.copy.max_bytes)", maxBytes)
				return false
			}
		}
		return true
	}, func(p string) bool { return !(srcRel == "." && p == workDir) })
	if over != nil {
		return plan, over
	}
	if err := ctx.Err(); err != nil {
		return plan, refusal(workspace.ClassFailed, "%v", err)
	}
	return plan, nil
}

// copyFiles copies the plan's files (and folders) from srcRel in sv into
// the folder stage of v, each exactly the size it was listed at.
func copyFiles(ctx context.Context, sv, v *fsops.VolumeFS, srcRel, stage string, plan copyPlan) *answer {
	sd, err := openDir(v, stage, true)
	if err != nil {
		x := classify(err, "making the copy's stage")
		return &x
	}
	sd.Close()
	for _, d := range plan.dirs {
		dd, err := openDir(v, filepath.Join(stage, d), true)
		if err != nil {
			x := classify(err, "copying %q", filepath.ToSlash(d))
			return &x
		}
		dd.Close()
	}
	for _, f := range plan.files {
		if err := ctx.Err(); err != nil {
			return refusal(workspace.ClassFailed, "%v", err)
		}
		src := filepath.Join(srcRel, f.rel)
		if !plan.dir {
			src = srcRel
		}
		if a := copyOne(sv, v, src, filepath.Join(stage, f.rel), f.size); a != nil {
			return a
		}
	}
	return nil
}

func copyOne(sv, v *fsops.VolumeFS, src, dst string, size int64) *answer {
	sp, err := openDir(sv, filepath.Dir(src), false)
	if err != nil {
		x := classify(err, "copying %q", filepath.ToSlash(src))
		return &x
	}
	defer sp.Close()
	in, err := sv.OpenIn(sp, filepath.Base(src))
	if err != nil {
		x := classify(err, "copying %q", filepath.ToSlash(src))
		return &x
	}
	defer in.Close()
	dp, err := openDir(v, filepath.Dir(dst), true)
	if err != nil {
		x := classify(err, "copying %q", filepath.ToSlash(src))
		return &x
	}
	defer dp.Close()
	out, err := dp.OpenFile(filepath.Base(dst), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return refusal(workspace.ClassFailed, "copying %q: %v", filepath.ToSlash(src), err)
	}
	_, err = io.CopyN(out, in, size)
	if errors.Is(err, io.EOF) {
		err = errors.New("the file shrank while it was copied")
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return refusal(workspace.ClassFailed, "copying %q: %v", filepath.ToSlash(src), err)
	}
	return nil
}
