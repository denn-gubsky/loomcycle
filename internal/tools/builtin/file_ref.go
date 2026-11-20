package builtin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// The reference type of the File tool (RFC GW P2 §3.2): a place File reads
// or writes, in the run's inbox, in a volume bound to the agent, or in a
// workspace pinned to the run. A place is resolved once per call with its
// grant checked for the use the call makes of it (copyLocs), and closed when
// the call ends. Each implementation streams: neither a volume's nor a
// workspace's file is ever held whole in memory.
//
// The ops that need a file on the core's disk (extract, transcribe, unpack,
// render, upload) still reach the inbox and volumes through openRef and
// refuse a workspace reference (notOnWorkspace) until the PRs that move
// their conversions onto the workspace's worker.

// locEntry is one entry at or under a place.
type locEntry struct {
	// Path is slash-separated: relative to the folder walked (Walk), or the
	// place's own path (Stat).
	Path string
	// Type is a workspace.Entry* kind; "" is nothing there (Stat).
	Type string
	Size int64
}

// locWritten is a file a place wrote.
type locWritten struct {
	Ref       string
	Size      int64
	SHA256    string
	MediaType string
	// inboxPath is a capture's path relative to the inbox root.
	inboxPath string
}

// locRemoved is what a Remove took away.
type locRemoved struct {
	Files int
	Bytes int64
}

// fileLoc is a place File reads or writes. Every method answers either a
// value or the tool result that explains why not (ok false).
type fileLoc interface {
	// Ref is the place's reference as an answer prints it.
	Ref() string
	// Stat describes the entry at the place; nothing there is a Type of "".
	Stat(ctx context.Context) (locEntry, tools.Result, bool)
	// Walk lists every entry under the folder the place names except the
	// folders themselves (regular files, links, special files), at paths
	// relative to it. Past limit regular files it stops and says so.
	Walk(ctx context.Context, limit int) ([]locEntry, bool, tools.Result, bool)
	// Open streams the regular file at rel under the place ("." is the
	// place itself).
	Open(ctx context.Context, rel string) (io.ReadCloser, tools.Result, bool)
	// Create writes exactly size bytes of r as the file at the place,
	// atomically: nobody sees part of it, a failed write leaves nothing,
	// and an existing file is replaced only with overwrite.
	Create(ctx context.Context, size int64, r io.Reader, overwrite bool) (locWritten, tools.Result, bool)
	// CreateFolder writes files (regular files at paths relative to src)
	// as a folder at the place, all or nothing. With overwrite an existing
	// folder there is merged into, file by file.
	CreateFolder(ctx context.Context, src fileLoc, files []locEntry, overwrite bool) (tools.Result, bool)
	// Remove removes the file or folder at the place.
	Remove(ctx context.Context) (locRemoved, tools.Result, bool)
	Close()
}

// notOnWorkspace refuses a workspace reference to an op that does not take
// one yet.
func notOnWorkspace(r dataRef) tools.Result {
	return errValidation(fmt.Sprintf("%s is a workspace reference, which this op does not take", r.raw),
		"Copy the file into the inbox with File op=copy and use the inbox reference.")
}

// parseWorkspaceRef parses the part after "workspace:": <name>:<path>. The
// path is relative to the workspace root; "" is the root. An absolute path
// and `..` are refused here as the worker refuses them, so a hook never
// judges a path the worker would read differently.
func parseWorkspaceRef(r dataRef, rest string) (dataRef, error) {
	name, p, ok := strings.Cut(rest, ":")
	if !ok {
		return r, errors.New("a workspace reference is workspace:<name>:<path>")
	}
	if strings.Contains(name, "@") {
		return r, errors.New("a reference into another agent's workspace (name@agent) is not supported")
	}
	if !lifecycle.ValidName(name) {
		return r, fmt.Errorf("workspace name %q: 1 to 64 letters, digits, '.', '-' or '_', starting with a letter or digit", name)
	}
	if strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) || strings.Contains(p, "\\") {
		return r, fmt.Errorf("workspace path %q: give a path relative to the workspace", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return r, fmt.Errorf("workspace path %q: `..` is not allowed", p)
		}
	}
	r.kind, r.workspace, r.path = refWorkspace, name, p
	return r, nil
}

// wsClean is a workspace path cleaned: "." for the root.
func wsClean(p string) string {
	c := path.Clean("/" + p)[1:]
	if c == "" {
		return "."
	}
	return c
}

func wsRef(name, p string) string { return "workspace:" + name + ":" + p }

// ---- the inbox and volumes ----

// volumeLoc is a file or folder in a volume, or in the run's inbox (inbox
// true). vfs is nil for an inbox no capture has made yet: nothing is there,
// and a capture makes it.
type volumeLoc struct {
	r      dataRef
	vfs    *fsops.VolumeFS
	rel    string // relative to the volume's root, OS separators
	inbox  bool
	origin string // what a capture into the inbox records as the file's origin
}

func (l *volumeLoc) Ref() string {
	if l.inbox {
		return inbox.Ref(filepath.ToSlash(l.rel))
	}
	return refPrefix(l.r) + filepath.ToSlash(l.rel)
}

func (l *volumeLoc) Close() {
	if l.vfs != nil {
		l.vfs.Close()
	}
}

func entryType(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return workspace.EntryLink
	case m.IsDir():
		return workspace.EntryDir
	case m.IsRegular():
		return workspace.EntryFile
	}
	return workspace.EntryOther
}

func (l *volumeLoc) Stat(_ context.Context) (locEntry, tools.Result, bool) {
	e := locEntry{Path: filepath.ToSlash(l.rel)}
	if l.vfs == nil {
		return e, tools.Result{}, true
	}
	dir, err := l.vfs.OpenDir(filepath.Dir(l.rel))
	if errors.Is(err, fs.ErrNotExist) {
		return e, tools.Result{}, true
	}
	if err != nil {
		return e, errPermission(err.Error(), ""), false
	}
	defer dir.Close()
	fi, err := dir.Lstat(filepath.Base(l.rel))
	if errors.Is(err, fs.ErrNotExist) {
		return e, tools.Result{}, true
	}
	if err != nil {
		return e, errPermission(err.Error(), ""), false
	}
	e.Type = entryType(fi.Mode())
	if e.Type == workspace.EntryFile {
		e.Size = fi.Size()
	}
	return e, tools.Result{}, true
}

// skipLocal leaves out what a walk of a local folder must not copy: the
// inbox's half-written captures (hidden, as ls hides them) and the
// temporaries File's own writes leave in a volume while they run.
func skipLocal(p string, inboxLoc bool) bool {
	if inboxLoc {
		return hiddenPath(p)
	}
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if strings.HasPrefix(seg, ".loomcycle-") {
			return true
		}
	}
	return false
}

func (l *volumeLoc) Walk(_ context.Context, limit int) ([]locEntry, bool, tools.Result, bool) {
	var out []locEntry
	files, truncated := 0, false
	if l.vfs == nil {
		return out, false, tools.Result{}, true
	}
	l.vfs.Walk(l.rel, func(dir *os.Root, p string, _ fs.DirEntry) {
		if truncated || skipLocal(p, l.inbox) {
			return
		}
		fi, err := dir.Lstat(filepath.Base(p))
		if err != nil {
			return
		}
		e := locEntry{Path: filepath.ToSlash(p), Type: entryType(fi.Mode())}
		if e.Type == workspace.EntryFile {
			if files >= limit {
				truncated = true
				return
			}
			files++
			e.Size = fi.Size()
		}
		out = append(out, e)
	})
	return out, truncated, tools.Result{}, true
}

func (l *volumeLoc) Open(_ context.Context, rel string) (io.ReadCloser, tools.Result, bool) {
	shown := l.Ref()
	if rel != "." {
		shown += "/" + rel
	}
	if l.vfs == nil {
		return nil, errNotFound(fmt.Sprintf("no file at %s", shown), ""), false
	}
	fh, err := l.vfs.Open(filepath.Join(l.rel, filepath.FromSlash(rel)))
	if errors.Is(err, fsops.ErrNotRegular) {
		return nil, errValidation(fmt.Sprintf("%s is not a regular file", shown), ""), false
	}
	if err != nil {
		return nil, errNotFound(fmt.Sprintf("no file at %s", shown), "List the folder with op=ls."), false
	}
	return fh, tools.Result{}, true
}

// Create writes the file as keep does: a temporary file beside the target,
// renamed into place. Into the inbox the file is captured instead, so it is
// recorded and counted against the inbox's budget like any kept file.
func (l *volumeLoc) Create(ctx context.Context, size int64, r io.Reader, overwrite bool) (locWritten, tools.Result, bool) {
	if l.inbox {
		return l.capture(ctx, filepath.ToSlash(l.rel), size, r)
	}
	dir, err := l.vfs.OpenDir(filepath.Dir(l.rel))
	if err != nil {
		return locWritten{}, errNotFound("the folder for "+l.Ref()+" does not exist", "Create it first, or name one that exists."), false
	}
	defer dir.Close()
	name := filepath.Base(l.rel)
	if fi, err := dir.Lstat(name); err == nil && (!fi.Mode().IsRegular() || !overwrite) {
		return locWritten{}, errBusiness(fmt.Sprintf("%s already exists", l.Ref()), "Pass overwrite: true to replace it, or choose another name."), false
	}
	tmp, tmpName, err := fsops.CreateTemp(dir, ".loomcycle-copy-")
	if err != nil {
		return locWritten{}, errTransient(err.Error(), "Try again."), false
	}
	defer dir.Remove(tmpName)
	sum := sha256.New()
	head := &headWriter{max: 512}
	n, err := io.Copy(io.MultiWriter(tmp, sum, head), io.LimitReader(r, size+1))
	closeErr := tmp.Close()
	if err != nil {
		return locWritten{}, copyReadError(err), false
	}
	if closeErr != nil {
		return locWritten{}, errTransient(closeErr.Error(), "Try again."), false
	}
	if n != size {
		return locWritten{}, errTransient(fmt.Sprintf("the source changed while it was copied (%d bytes, expected %d)", n, size), "Try again."), false
	}
	if err := ctx.Err(); err != nil {
		return locWritten{}, errTransient("the call was cancelled", ""), false
	}
	if err := dir.Rename(tmpName, name); err != nil {
		return locWritten{}, errTransient(err.Error(), "Try again."), false
	}
	return locWritten{Ref: l.Ref(), Size: n, SHA256: hex.EncodeToString(sum.Sum(nil)), MediaType: inbox.Sniff("", head.b, name)}, tools.Result{}, true
}

// capture keeps one file in the inbox at rel (a folder and a name, both
// sanitised by the capture). A name already taken gets a numbered variant,
// as every capture does: the inbox never replaces a file.
func (l *volumeLoc) capture(ctx context.Context, rel string, size int64, r io.Reader) (locWritten, tools.Result, bool) {
	it := inbox.Item{Name: path.Base(rel), Origin: l.origin, Fallback: "copy"}
	if d := path.Dir(rel); d != "." {
		it.Dir = d
	}
	// One byte past size is read, so a source that grew since it was
	// listed is told apart from one that ends on size.
	kept := inbox.CaptureReader(ctx, it, io.LimitReader(r, size+1), size)
	if !kept.Kept {
		return locWritten{}, errBusiness(strings.Trim(kept.Stub, "[]"), ""), false
	}
	rec := kept.Record
	if rec.Size != size {
		forgetCaptured(ctx, []string{rec.Path})
		return locWritten{}, errTransient(fmt.Sprintf("the source changed while it was copied (%d bytes, expected %d)", rec.Size, size), "Try again."), false
	}
	return locWritten{Ref: rec.Ref, Size: rec.Size, SHA256: rec.SHA256, MediaType: rec.MediaType, inboxPath: rec.Path}, tools.Result{}, true
}

// forgetCaptured removes files a failed folder copy captured into the inbox
// and drops their records, which gives their bytes back to the budget.
func forgetCaptured(ctx context.Context, rels []string) {
	if len(rels) == 0 {
		return
	}
	root, fence, err := effectiveRoot(ctx, inbox.Name, false)
	if err != nil {
		return
	}
	vfs, err := fsops.OpenVolumeFS(root, fence)
	if err != nil {
		return
	}
	defer vfs.Close()
	for _, rel := range rels {
		_ = vfs.Root().Remove(filepath.FromSlash(rel))
		inbox.Forget(ctx, rel)
	}
}

// CreateFolder writes the folder under a hidden staging folder beside the
// target and renames it into place in one step, so the folder appears
// whole or not at all. Into the inbox each file is captured, and on a
// failure the ones already captured are removed again.
func (l *volumeLoc) CreateFolder(ctx context.Context, src fileLoc, files []locEntry, overwrite bool) (tools.Result, bool) {
	if l.inbox {
		return l.captureFolder(ctx, src, files)
	}
	parent, err := l.vfs.OpenDir(filepath.Dir(l.rel))
	if err != nil {
		return errNotFound("the folder for "+l.Ref()+" does not exist", "Create it first, or name one that exists."), false
	}
	defer parent.Close()
	name := filepath.Base(l.rel)
	fi, statErr := parent.Lstat(name)
	exists := statErr == nil
	if exists && (!fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 || !overwrite) {
		return errBusiness(fmt.Sprintf("%s already exists", l.Ref()), "Pass overwrite: true to merge into a folder there, or choose another name."), false
	}
	var rb [8]byte
	_, _ = rand.Read(rb[:])
	stage := ".loomcycle-copy-" + hex.EncodeToString(rb[:])
	if err := parent.Mkdir(stage, 0o700); err != nil {
		return errTransient(err.Error(), "Try again."), false
	}
	defer func() { _ = parent.RemoveAll(stage) }()
	sd, err := l.vfs.Step(parent, stage)
	if err != nil {
		return errTransient(err.Error(), "Try again."), false
	}
	defer sd.Close()
	for _, f := range files {
		if res, ok := writeStaged(ctx, sd, src, f); !ok {
			return res, false
		}
	}
	if err := ctx.Err(); err != nil {
		return errTransient("the call was cancelled", ""), false
	}
	if !exists {
		if err := parent.Rename(stage, name); err != nil {
			return errTransient(err.Error(), "Try again."), false
		}
		return tools.Result{}, true
	}
	// A merge: each staged file is renamed onto its place in the existing
	// folder, whose subfolders are entered with the volume's checks, so a
	// link planted there is refused rather than written through.
	for _, f := range files {
		segs := strings.Split(f.Path, "/")
		target, err := l.vfs.Step(parent, name)
		if err != nil {
			return errPermission(err.Error(), ""), false
		}
		for _, s := range segs[:len(segs)-1] {
			if err := target.Mkdir(s, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				target.Close()
				return errTransient(err.Error(), "Try again."), false
			}
			next, err := l.vfs.Step(target, s)
			target.Close()
			if err != nil {
				return errPermission(err.Error(), ""), false
			}
			target = next
		}
		if fi, err := target.Lstat(segs[len(segs)-1]); err == nil && !fi.Mode().IsRegular() {
			target.Close()
			return errBusiness(fmt.Sprintf("%s/%s is not a file, so it cannot be replaced", l.Ref(), f.Path), ""), false
		}
		target.Close()
		if err := parent.Rename(filepath.Join(stage, filepath.FromSlash(f.Path)), filepath.Join(name, filepath.FromSlash(f.Path))); err != nil {
			return errTransient(err.Error(), "Try again."), false
		}
	}
	return tools.Result{}, true
}

// writeStaged writes one file of a folder copy into the private staging
// folder sd, at f.Path.
func writeStaged(ctx context.Context, sd *os.Root, src fileLoc, f locEntry) (tools.Result, bool) {
	rel := filepath.FromSlash(f.Path)
	if d := filepath.Dir(rel); d != "." {
		if err := sd.MkdirAll(d, 0o700); err != nil {
			return errTransient(err.Error(), "Try again."), false
		}
	}
	rc, res, ok := src.Open(ctx, f.Path)
	if !ok {
		return res, false
	}
	defer rc.Close()
	out, err := sd.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errTransient(err.Error(), "Try again."), false
	}
	n, err := io.Copy(out, io.LimitReader(rc, f.Size+1))
	closeErr := out.Close()
	if err != nil {
		return copyReadError(err), false
	}
	if closeErr != nil {
		return errTransient(closeErr.Error(), "Try again."), false
	}
	if n != f.Size {
		return errTransient(fmt.Sprintf("%s changed while it was copied", f.Path), "Try again."), false
	}
	return tools.Result{}, true
}

func (l *volumeLoc) captureFolder(ctx context.Context, src fileLoc, files []locEntry) (tools.Result, bool) {
	var kept []string
	for _, f := range files {
		rc, res, ok := src.Open(ctx, f.Path)
		if !ok {
			forgetCaptured(ctx, kept)
			return res, false
		}
		w, res, ok := l.capture(ctx, path.Join(filepath.ToSlash(l.rel), f.Path), f.Size, rc)
		rc.Close()
		if !ok {
			forgetCaptured(ctx, kept)
			return res, false
		}
		kept = append(kept, w.inboxPath)
	}
	return tools.Result{}, true
}

// Remove is not offered on a volume or the inbox here: op=rm and op=clear
// keep their own inbox-only rules.
func (l *volumeLoc) Remove(context.Context) (locRemoved, tools.Result, bool) {
	return locRemoved{}, errValidation("only a workspace's files are removed through this path", ""), false
}

// copyReadError explains a failure reading the source of a copy: a
// workspace's refusal keeps its class, anything else is worth a retry.
func copyReadError(err error) tools.Result {
	if res, ok := workspaceFileError(err); ok {
		return res
	}
	return errTransient("reading the source failed: "+err.Error(), "Try again.")
}
