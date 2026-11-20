package builtin

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// File op=copy (RFC GW §5.1, P2 §3.4): a file or a folder from the inbox, a
// volume or a workspace into another of them, the bytes streamed and never
// held whole. Every grant both ends need is checked before a byte moves; a
// folder is listed first and refused whole when it passes a bound, and
// written all or nothing. keep stays as it is: copy is its general form.

// copyBounds are the folder-copy bounds: the operator's workspace.copy
// settings, or their defaults where workspaces are off.
func (f *File) copyBounds() (int, int64) {
	if f.Workspaces != nil {
		return f.Workspaces.CopyLimits()
	}
	return lifecycle.DefaultCopyMaxFiles, lifecycle.DefaultCopyMaxBytes
}

func (f *File) copy(ctx context.Context, in fileInput) (tools.Result, error) {
	from, err := fileRef(in.From)
	if err != nil {
		return errValidation("copy: from: "+err.Error(), "Pass from, e.g. workspace:build:out/report.pdf or volume:inbox:documents/a.pdf."), nil
	}
	if strings.TrimSpace(in.To) == "" {
		return errValidation("copy: to is required", "Pass to, e.g. workspace:build:in/ or volume:docs:reports/."), nil
	}
	to, err := fileRef(in.To)
	if err != nil {
		return errValidation("copy: to: "+err.Error(), "Pass to, e.g. workspace:build:in/ or volume:docs:reports/."), nil
	}
	// Both ends are resolved, and their grants checked, before anything is
	// read: a call that cannot write moves nothing.
	src, res, ok := f.copySource(ctx, from)
	if !ok {
		return prefixed("copy", res), nil
	}
	defer src.Close()
	dst, res, ok := f.copyTarget(ctx, to, copyBase(from), copyOrigin(from))
	if !ok {
		return prefixed("copy", res), nil
	}
	defer dst.Close()
	if src.Ref() == dst.Ref() {
		return errValidation("copy: from and to are the same place", "Name another target."), nil
	}
	e, res, ok := src.Stat(ctx)
	if !ok {
		return prefixed("copy", res), nil
	}
	switch e.Type {
	case "":
		return errNotFound(fmt.Sprintf("copy: nothing at %s", src.Ref()), "List the folder with op=ls."), nil
	case workspace.EntryFile:
		return f.copyFile(ctx, src, dst, e, in.Overwrite)
	case workspace.EntryDir:
		return f.copyFolder(ctx, src, dst, in.Overwrite)
	case workspace.EntryLink:
		return errValidation(fmt.Sprintf("copy: %s is a link, which a copy does not follow", src.Ref()), "Copy what it points at."), nil
	}
	return errValidation(fmt.Sprintf("copy: %s is not a regular file or a folder", src.Ref()), ""), nil
}

// copyBase is the name a copy keeps inside a folder target: the source's
// last segment (a workspace's root is named after the workspace).
func copyBase(r dataRef) string {
	b := path.Base(path.Clean("/" + filepath.ToSlash(r.path)))
	if b == "/" || b == "." {
		if r.kind == refWorkspace {
			return r.workspace
		}
		if r.volume != "" {
			return r.volume
		}
		return "copy"
	}
	return b
}

// copyOrigin is what a file copied into the inbox records as its origin.
func copyOrigin(r dataRef) string {
	if r.kind == refWorkspace {
		return "copy:workspace:" + r.workspace
	}
	if r.volume == "" {
		return "copy:volume"
	}
	return "copy:volume:" + r.volume
}

// copySource resolves where a copy reads from, with its read grant: the
// workspace grant and a Workspace hook for a workspace, Read and its hooks
// for a volume, nothing for the run's own inbox (as for every File op).
func (f *File) copySource(ctx context.Context, r dataRef) (fileLoc, tools.Result, bool) {
	if r.kind == refWorkspace {
		l, res, ok := f.workspaceLoc(ctx, r, false, FileToolName)
		if !ok {
			return nil, res, false
		}
		if res, ok := l.hook(ctx, wsHookRead); !ok {
			return nil, res, false
		}
		return l, tools.Result{}, true
	}
	vfs, rel, res, ok := openRef(ctx, r, false)
	if !ok {
		return nil, res, false
	}
	return &volumeLoc{r: r, vfs: vfs, rel: rel, inbox: r.volume == inbox.Name}, tools.Result{}, true
}

// copyTarget resolves where a copy writes. A target ending in "/", or an
// existing folder, takes the source's name inside it. The grant is checked
// first and the hook last, on the final place: a hook allowing "out/" must
// not be what decides a write to "out/report.pdf".
func (f *File) copyTarget(ctx context.Context, r dataRef, base, origin string) (fileLoc, tools.Result, bool) {
	switch {
	case r.kind == refWorkspace:
		l, res, ok := f.workspaceLoc(ctx, r, true, FileToolName)
		if !ok {
			return nil, res, false
		}
		e, res, ok := l.Stat(ctx)
		if !ok {
			return nil, res, false
		}
		if strings.HasSuffix(r.path, "/") || e.Type == workspace.EntryDir {
			l = l.child(base)
		}
		if res, ok := l.hook(ctx, wsHookWrite); !ok {
			return nil, res, false
		}
		return l, tools.Result{}, true
	case r.volume == inbox.Name:
		return copyInboxTarget(ctx, r, base, origin)
	}
	return copyVolumeTarget(ctx, r, base)
}

// copyInboxTarget is a place in the run's inbox. The inbox is the run's own,
// as for download: no grant is asked. Nothing is resolved on its disk here,
// since a capture makes the inbox and its folders; the capture writes
// through the inbox's own root.
func copyInboxTarget(ctx context.Context, r dataRef, base, origin string) (fileLoc, tools.Result, bool) {
	if !inbox.Available(ctx) {
		return nil, errBusiness("this run has no inbox to keep files in", "Copy into a read-write volume of yours."), false
	}
	p := filepath.ToSlash(r.path)
	if strings.HasPrefix(p, "/") || slices.Contains(strings.Split(p, "/"), "..") {
		return nil, errValidation(fmt.Sprintf("%s: give a path inside the inbox", r.raw), ""), false
	}
	l := &volumeLoc{r: r, rel: filepath.FromSlash(path.Clean(p)), inbox: true, origin: origin}
	if !inboxMissing(ctx, r) {
		root, fence, err := effectiveRoot(ctx, inbox.Name, false)
		if err != nil {
			return nil, errPermission(err.Error(), ""), false
		}
		vfs, err := fsops.OpenVolumeFS(root, fence)
		if err != nil {
			return nil, errPermission(err.Error(), ""), false
		}
		l.vfs = vfs
	}
	e, res, ok := l.Stat(ctx)
	if !ok {
		l.Close()
		return nil, res, false
	}
	if strings.HasSuffix(p, "/") || p == "." || e.Type == workspace.EntryDir {
		l.rel = filepath.Join(l.rel, base)
	}
	return l, tools.Result{}, true
}

// copyVolumeTarget is a place in one of the agent's read-write volumes, as
// keep resolves one: Write held, the volume read-write, the target's
// folder inside the volume and outside its fence, and Write's hooks on the
// final path.
func copyVolumeTarget(ctx context.Context, r dataRef, base string) (fileLoc, tools.Result, bool) {
	if !slices.Contains(tools.PolicyOf(ctx).Tools, "Write") {
		return nil, errPermission(fmt.Sprintf("%s needs the Write tool, which this run does not have", r.raw),
			"Copy into the inbox or a workspace, or ask an operator to grant Write."), false
	}
	root, fence, err := effectiveRoot(ctx, r.volume, true)
	if err != nil {
		return nil, errPermission(err.Error(), "Name a read-write volume you are bound to."), false
	}
	toPath := r.path
	if strings.HasSuffix(toPath, "/") {
		toPath += base
	}
	target, err := fsops.ResolveParentInsideRoot(root, toPath)
	if err == nil {
		err = fsops.OutsideFence(fence, target)
	}
	if err != nil {
		return nil, errPermission(err.Error(), "Create the folder first, or name one that exists."), false
	}
	vfs, err := fsops.OpenVolumeFS(root, fence)
	if err != nil {
		return nil, errPermission(err.Error(), ""), false
	}
	rel, err := vfs.Rel(target)
	if err != nil {
		vfs.Close()
		return nil, errPermission(err.Error(), ""), false
	}
	l := &volumeLoc{r: r, vfs: vfs, rel: rel}
	e, res, ok := l.Stat(ctx)
	if !ok {
		l.Close()
		return nil, res, false
	}
	// A "/" target already took the source's name: what is there now is the
	// place itself (a folder a merge goes into), not one to step inside.
	if e.Type == workspace.EntryDir && !strings.HasSuffix(r.path, "/") {
		l.rel = filepath.Join(l.rel, base)
	}
	final := dataRef{raw: l.Ref(), kind: refVolume, volume: r.volume, path: filepath.ToSlash(l.rel)}
	if res, ok := checkRefGrant(ctx, final, true, FileToolName, "Copy into the inbox"); !ok {
		l.Close()
		return nil, res, false
	}
	return l, tools.Result{}, true
}

func (f *File) copyFile(ctx context.Context, src, dst fileLoc, e locEntry, overwrite bool) (tools.Result, error) {
	if maxFile := inbox.MaxFileBytes(ctx); e.Size > maxFile {
		return errValidation(fmt.Sprintf("copy: %s is %d bytes, over the per-file limit of %d (LOOMCYCLE_FILE_MAX_BYTES); nothing was copied", src.Ref(), e.Size, maxFile), ""), nil
	}
	if res, done := f.copyOnWorker(ctx, src, dst, overwrite); done {
		return res, nil
	}
	rc, res, ok := src.Open(ctx, ".")
	if !ok {
		return prefixed("copy", res), nil
	}
	defer rc.Close()
	w, res, ok := dst.Create(ctx, e.Size, rc, overwrite)
	if !ok {
		return prefixed("copy", res), nil
	}
	out := map[string]any{"from": src.Ref(), "ref": w.Ref, "size": w.Size, "sha256": w.SHA256}
	if w.MediaType != "" {
		out["media_type"] = w.MediaType
	}
	return okJSON(out)
}

// copyOnWorker copies between two workspaces of the run on one worker,
// which does it itself: the bytes never cross to the core. done is false
// when they are on two workers (or either end is not a workspace), and the
// caller streams the copy through the core instead.
func (f *File) copyOnWorker(ctx context.Context, src, dst fileLoc, overwrite bool) (tools.Result, bool) {
	s, ok1 := src.(*workspaceLoc)
	d, ok2 := dst.(*workspaceLoc)
	if !ok1 || !ok2 {
		return tools.Result{}, false
	}
	ans, err := f.Workspaces.CopyLocal(ctx, s.o, lifecycle.CopyLocalRequest{From: s.name, FromPath: s.path, To: d.name, ToPath: d.path, Overwrite: overwrite})
	if errors.Is(err, lifecycle.ErrNotSameWorker) {
		return tools.Result{}, false
	}
	if err != nil {
		return prefixed("copy", workspaceRefError(err, d.name)), true
	}
	out := map[string]any{"from": src.Ref(), "to": dst.Ref(), "files": ans.Files, "bytes": ans.Bytes, "same_worker": true}
	if len(ans.Skipped) > 0 {
		out["skipped"] = ans.Skipped
	}
	res, _ := okJSON(out)
	return res, true
}

// copyFolder lists the source first and refuses the whole copy, before
// anything is written, when it passes a bound: the folder-copy file count
// or byte total, the per-file limit, or the inbox's remaining budget. A
// workspace target's disk quota is the worker's to judge as the files
// arrive in a stage; a refusal there discards the stage, so nothing is
// left either way. Links and special files are skipped and named.
func (f *File) copyFolder(ctx context.Context, src, dst fileLoc, overwrite bool) (tools.Result, error) {
	maxFiles, maxBytes := f.copyBounds()
	entries, truncated, res, ok := src.Walk(ctx, maxFiles)
	if !ok {
		return prefixed("copy", res), nil
	}
	if truncated {
		return errValidation(fmt.Sprintf("copy_limit: %s holds more than %d files (workspace.copy.max_files); nothing was copied", src.Ref(), maxFiles),
			"Copy a smaller folder, or ask the operator to raise the bound."), nil
	}
	maxFile := inbox.MaxFileBytes(ctx)
	var files []locEntry
	var skipped []string
	var total int64
	for _, e := range entries {
		if e.Type != workspace.EntryFile {
			if len(skipped) < workspace.MaxSkippedNamed {
				skipped = append(skipped, e.Path)
			}
			continue
		}
		if e.Size > maxFile {
			return errValidation(fmt.Sprintf("copy_limit: %s/%s is %d bytes, over the per-file limit of %d (LOOMCYCLE_FILE_MAX_BYTES); nothing was copied", src.Ref(), e.Path, e.Size, maxFile), ""), nil
		}
		total += e.Size
		files = append(files, e)
	}
	if total > maxBytes {
		return errValidation(fmt.Sprintf("copy_limit: %s holds %d bytes, over %d (workspace.copy.max_bytes); nothing was copied", src.Ref(), total, maxBytes),
			"Copy a smaller folder, or ask the operator to raise the bound."), nil
	}
	if l, ok := dst.(*volumeLoc); ok && l.inbox {
		if left := inbox.Remaining(ctx); total > left {
			return errBusiness(fmt.Sprintf("copy: %s holds %d bytes, more than the %d the run's inbox has left; nothing was copied", src.Ref(), total, left),
				"Remove files from the inbox with op=rm or op=clear, or copy less."), nil
		}
	}
	// Each file is judged on its own source and target path before anything
	// is written: a hook (or a grant) that refuses one path must not be
	// passed by copying the folder that holds it. The folder itself was
	// judged when the two places were resolved. At most max_files entries.
	for _, e := range files {
		if res, ok := src.Judge(ctx, e.Path, false); !ok {
			return prefixed("copy", res), nil
		}
		if res, ok := dst.Judge(ctx, e.Path, true); !ok {
			return prefixed("copy", res), nil
		}
	}
	// Two workspaces on one worker: the worker copies, after the same
	// listing was judged. It lists the folder again itself, so a file a
	// command adds in between is copied unjudged; the bounds still hold.
	if res, done := f.copyOnWorker(ctx, src, dst, overwrite); done {
		return res, nil
	}
	if res, ok := dst.CreateFolder(ctx, src, files, overwrite); !ok {
		return prefixed("copy", res), nil
	}
	out := map[string]any{"from": src.Ref(), "to": dst.Ref(), "files": len(files), "bytes": total}
	if len(skipped) > 0 {
		out["skipped"] = skipped
		out["note"] = "links and special files are not copied"
	}
	return okJSON(out)
}
