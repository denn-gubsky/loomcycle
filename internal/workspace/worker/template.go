package worker

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// Workspace templates on the worker (RFC GW §5.5, P6). The core keeps each
// template as one archive; this worker packs a checkpoint into one, and
// unpacks one it is sent into a cache of its own, from which every
// workspace opened from that template is copied.

// templatesDir is the worker's cache of unpacked templates:
// <root>/.loomcycle-templates/<digest>/. Beside the workspaces, never in
// one, as the checkpoints are; the dot keeps held() from taking it for a
// workspace.
const templatesDir = ".loomcycle-templates"

// templateTmp, inside templatesDir, is where an archive is received and
// unpacked before a rename puts it in the cache; start-up clears it.
const templateTmp = ".tmp"

// maxCachedTemplates bounds the cache (D-P6-30): past it the least recently
// used template is removed. The core sends it again when it is next opened.
const maxCachedTemplates = 8

// maxTemplateEntries bounds the entries of an archive unpacked.
const maxTemplateEntries = 1_000_000

// templateRootToken stands in a template's Python environments for the
// folder they were made in (RFC GW §4.3): written at pack, replaced at open
// by the new workspace's folder (host engine) or the session's fixed mount.
const templateRootToken = "/.loomcycle-template-root"

// errTemplateEntry is an archive entry that would land outside its folder.
var errTemplateEntry = errors.New("a template entry names a path outside the template")

func (w *worker) templatesRoot() string { return filepath.Join(w.opt.Root, templatesDir) }

// templateCache is the cached tree of a template; digest is a
// ValidTemplateDigest, so the path stays in the cache.
func (w *worker) templateCache(digest string) string {
	return filepath.Join(w.templatesRoot(), digest)
}

func (w *worker) templateTmpPath(kind string) (string, error) {
	dir := filepath.Join(w.templatesRoot(), templateTmp)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.MkdirTemp(dir, kind+"-")
	if err != nil {
		return "", err
	}
	// The caller makes the folder (or a file) itself.
	return f, os.Remove(f)
}

// cleanTemplates removes what a worker killed part way left in the cache:
// its work folder and anything that is not a template's tree. Run at start.
func (w *worker) cleanTemplates() {
	root := w.templatesRoot()
	if err := removeDir(filepath.Join(root, templateTmp)); err != nil {
		w.logger().Warn("cannot clear the templates' work folder", "err", err)
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || !workspace.ValidTemplateDigest(e.Name()) {
			_ = removeDir(filepath.Join(root, e.Name()))
		}
	}
}

// sessionRoot is the folder a workspace's environment names in its session:
// the workspace's own folder on the host engine, the fixed mount in a
// container.
func (w *worker) sessionRoot(id string) (string, error) {
	if !w.hostEngine() {
		return engine.WorkspaceMount, nil
	}
	real, err := filepath.EvalSymlinks(w.opt.Root)
	if err != nil {
		return "", err
	}
	return filepath.Join(real, id), nil
}

// templatePackOp sends a checkpoint of workspace id as a template archive,
// the op's output. The checkpoint is copied under the workspace's write lock
// (no restore or deletion of it under the copy) and the copy packed after
// the lock is let go; its Python environments name templateRootToken
// instead of this workspace's folder.
func (w *worker) templatePackOp(ctx context.Context, id, dir, real string, raw []byte, stream workspacepb.WorkspaceService_OpClient) answer {
	var in workspace.TemplatePackArgs
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	if !validCheckpointID(in.Checkpoint) {
		return refuse(workspace.ClassCheckpointNotFound, "no checkpoint %q in this workspace", in.Checkpoint)
	}
	tmp, err := w.tmpPath("template")
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer func() { _ = removeDir(tmp) }()
	if a := w.copyCheckpoint(ctx, id, real, in.Checkpoint, tmp); a.class != "" {
		return a
	}
	from := []string{engine.WorkspaceMount}
	if w.hostEngine() {
		from = []string{real, dir}
	}
	rewriteVenvs(tmp, from, templateRootToken)
	cw := bufio.NewWriterSize(chunkSender{stream}, workspace.ChunkSize)
	if err := packTemplate(ctx, tmp, cw); err != nil {
		return refuse(workspace.ClassFailed, "packing the template: %v", err)
	}
	if err := cw.Flush(); err != nil {
		return refuse(workspace.ClassFailed, "sending the template: %v", err)
	}
	return answer{}
}

// copyCheckpoint copies checkpoint ck of workspace id to tmp, under the
// workspace's write lock.
func (w *worker) copyCheckpoint(ctx context.Context, id, real, ck, tmp string) answer {
	v, err := fsops.OpenVolumeFS(real, "")
	if err != nil {
		return refuse(workspace.ClassFailed, "opening the workspace: %v", err)
	}
	defer v.Close()
	_, release, err := w.writeLock(ctx, id, v)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer release()
	info, ok := w.readCheckpoint(id, ck)
	if !ok {
		return refuse(workspace.ClassCheckpointNotFound, "no checkpoint %q in this workspace (checkpoints lists them)", ck)
	}
	if _, err := w.copyTree(ctx, filepath.Join(w.checkpointsOf(id), info.ID), tmp, workspace.MaxDiskQuotaBytes); err != nil {
		return refuse(workspace.ClassFailed, "copying the checkpoint: %v", err)
	}
	return answer{}
}

// chunkSender writes the op's output to its stream, each write a chunk in a
// buffer of its own.
type chunkSender struct {
	stream workspacepb.WorkspaceService_OpClient
}

func (c chunkSender) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += workspace.ChunkSize {
		end := min(off+workspace.ChunkSize, len(p))
		buf := append([]byte(nil), p[off:end]...)
		if err := c.stream.Send(&workspacepb.OpFromWorker{Msg: &workspacepb.OpFromWorker_Output{Output: &workspacepb.Chunk{Data: buf}}}); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

// packTemplate writes the tree at src to out as a gzip-compressed tar:
// folders, regular files and links (as links), each by its path relative to
// src with forward slashes; anything else is left out. Owners are not kept.
func packTemplate(ctx context.Context, src string, out io.Writer) error {
	v, err := fsops.OpenVolumeFS(src, "")
	if err != nil {
		return err
	}
	defer v.Close()
	zw := gzip.NewWriter(out)
	tw := tar.NewWriter(zw)
	var werr error
	walkTree(v, v.Root(), "", func(parent *os.Root, rel string, fi os.FileInfo) bool {
		if werr = ctx.Err(); werr != nil {
			return false
		}
		m := fi.Mode()
		link := ""
		switch {
		case m.IsDir(), m.IsRegular():
		case m&os.ModeSymlink != 0:
			if link, werr = parent.Readlink(filepath.Base(rel)); werr != nil {
				return false
			}
		default:
			return true
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			werr = err
			return false
		}
		h.Name = filepath.ToSlash(rel)
		if m.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if werr = tw.WriteHeader(h); werr != nil {
			return false
		}
		if m.IsRegular() {
			f, err := v.OpenIn(parent, filepath.Base(rel))
			if err != nil {
				werr = err
				return false
			}
			_, werr = io.CopyN(tw, f, h.Size)
			f.Close()
			if werr != nil {
				return false
			}
		}
		return true
	}, nil)
	if werr != nil {
		return werr
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// unpackTemplate unpacks a template archive into dst, which must not exist
// (D-P6-33). Every entry is made through an os.Root on dst, so neither a
// name that climbs out nor a link out of the folder lets a write land
// outside it; a name that is absolute or not local is refused outright.
// Folders, regular files (permission bits only: no setuid) and links (made
// as links, never followed) are taken; anything else is named in skipped.
// Past maxBytes of files and links it stops with errOverBudget. On any
// error nothing is left at dst.
func unpackTemplate(r io.Reader, dst string, maxBytes int64) (res copyResult, err error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return copyResult{}, fmt.Errorf("reading the archive: %w", err)
	}
	defer zr.Close()
	if err := os.Mkdir(dst, 0o700); err != nil {
		return copyResult{}, err
	}
	defer func() {
		if err != nil {
			_ = removeDir(dst)
			res = copyResult{}
		}
	}()
	root, err := os.OpenRoot(dst)
	if err != nil {
		return copyResult{}, err
	}
	defer root.Close()
	tr := tar.NewReader(zr)
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if err != nil {
			return res, fmt.Errorf("reading the archive: %w", err)
		}
		if n >= maxTemplateEntries {
			return res, fmt.Errorf("the archive holds more than %d entries", maxTemplateEntries)
		}
		name := path.Clean(strings.TrimSuffix(h.Name, "/"))
		if name == "." {
			continue
		}
		rel := filepath.FromSlash(name)
		if path.IsAbs(name) || !filepath.IsLocal(rel) {
			return res, fmt.Errorf("%w: %q", errTemplateEntry, h.Name)
		}
		perm := os.FileMode(h.Mode) & 0o777
		if d := filepath.Dir(rel); d != "." {
			if err := root.MkdirAll(d, 0o700); err != nil {
				return res, err
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(rel, perm|0o700); err != nil {
				return res, err
			}
			if err := root.Chmod(rel, perm|0o700); err != nil {
				return res, err
			}
		case tar.TypeReg:
			if res.bytes+h.Size > maxBytes {
				return res, errOverBudget
			}
			f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return res, err
			}
			written, err := io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return res, err
			}
			if err := root.Chmod(rel, perm); err != nil {
				return res, err
			}
			_ = root.Chtimes(rel, h.ModTime, h.ModTime)
			res.bytes += written
			res.files++
		case tar.TypeSymlink:
			if res.bytes+int64(len(h.Linkname)) > maxBytes {
				return res, errOverBudget
			}
			if err := root.Symlink(h.Linkname, rel); err != nil {
				return res, err
			}
			res.bytes += int64(len(h.Linkname))
		default:
			addSkipped(&res, name)
		}
	}
}

// templatePutOp receives a template archive (the op's input, exactly Size
// bytes), checks it against its digest, unpacks it and puts the tree in the
// cache. A template already cached is kept as it is.
func (w *worker) templatePutOp(ctx context.Context, start *workspacepb.OpStart, stream workspacepb.WorkspaceService_OpClient) answer {
	if !start.GetHasInput() {
		return refuse(workspace.ClassInvalid, "template_put takes the archive as the op's input")
	}
	var in workspace.TemplatePutArgs
	if a := decodeArgs(start.GetArgs(), &in); a.class != "" {
		return a
	}
	switch {
	case !workspace.ValidTemplateDigest(in.Digest):
		return refuse(workspace.ClassInvalid, "digest must be a SHA-256 in lowercase hex")
	case in.Size <= 0 || in.Size > workspace.MaxDiskQuotaBytes:
		return refuse(workspace.ClassInvalid, "size must be from 1 to %d", int64(workspace.MaxDiskQuotaBytes))
	case in.MaxBytes <= 0 || in.MaxBytes > workspace.MaxDiskQuotaBytes:
		return refuse(workspace.ClassInvalid, "max_bytes must be from 1 to %d", int64(workspace.MaxDiskQuotaBytes))
	}
	archive, err := w.templateTmpPath("archive")
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	// #nosec G304 -- the path is a fresh name the worker made in its own work folder
	f, err := os.OpenFile(archive, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(archive)
	}()
	_, sum, a := receive(stream, f, in.Size, 0)
	if a != nil {
		return *a
	}
	if sum != in.Digest {
		return refuse(workspace.ClassInvalid, "the archive does not match its digest")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	tree, err := w.templateTmpPath("tree")
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	if _, err := unpackTemplate(f, tree, in.MaxBytes); err != nil {
		if errors.Is(err, errOverBudget) {
			return refuse(workspace.ClassOverQuota, "the template's files pass %d bytes", in.MaxBytes)
		}
		return refuse(workspace.ClassInvalid, "unpacking the template: %v", err)
	}
	w.tplMu.Lock()
	defer w.tplMu.Unlock()
	cache := w.templateCache(in.Digest)
	if _, err := os.Lstat(cache); err == nil {
		_ = removeDir(tree)
		return answer{}
	}
	if err := os.Rename(tree, cache); err != nil {
		_ = removeDir(tree)
		return refuse(workspace.ClassFailed, "caching the template: %v", err)
	}
	w.evictTemplatesLocked(in.Digest)
	return answer{}
}

// evictTemplatesLocked removes the least recently used templates past
// maxCachedTemplates, never keep. tplMu is held.
func (w *worker) evictTemplatesLocked(keep string) {
	ents, err := os.ReadDir(w.templatesRoot())
	if err != nil {
		return
	}
	type cached struct {
		digest string
		used   time.Time
	}
	var all []cached
	for _, e := range ents {
		if !e.IsDir() || !workspace.ValidTemplateDigest(e.Name()) || e.Name() == keep {
			continue
		}
		if fi, err := e.Info(); err == nil {
			all = append(all, cached{e.Name(), fi.ModTime()})
		}
	}
	slices.SortFunc(all, func(a, b cached) int { return a.used.Compare(b.used) })
	for len(all) >= maxCachedTemplates {
		if err := removeDir(w.templateCache(all[0].digest)); err != nil {
			w.logger().Warn("cannot remove a cached template", "err", err)
		}
		all = all[1:]
	}
}

// templateOpenOp makes workspace id, which must not exist, as a copy of a
// cached template, answered as a fork is. The copy must fit the new
// workspace's quota. A template not in the cache is ClassTemplateMissing.
func (w *worker) templateOpenOp(ctx context.Context, id, dir string, raw []byte) answer {
	var in workspace.TemplateOpenArgs
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	if !workspace.ValidTemplateDigest(in.Digest) {
		return refuse(workspace.ClassInvalid, "digest must be a SHA-256 in lowercase hex")
	}
	if in.DiskQuotaBytes < 0 || in.DiskQuotaBytes > workspace.MaxDiskQuotaBytes {
		return refuse(workspace.ClassInvalid, "disk_quota_bytes must be from 1 to %d", int64(workspace.MaxDiskQuotaBytes))
	}
	if st, err := os.Lstat(dir); err == nil {
		if !st.IsDir() {
			return refuse(workspace.ClassFailed, "the workspace's name is taken by something that is not a directory")
		}
		// A retried open that finished: the rename into place is its last
		// step.
		return w.forkAnswer(ctx, id, dir, workspace.ForkAnswer{Mechanism: w.mechanism()})
	}
	if a := w.atCapacity(); a.class != "" {
		return a
	}
	quota := in.DiskQuotaBytes
	if quota == 0 {
		quota = workspace.DefaultDiskQuotaBytes
	}
	tmp, err := w.tmpPath("template-open")
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	res, a := w.copyTemplate(ctx, in.Digest, tmp, quota)
	if a.class != "" {
		return a
	}
	out := workspace.ForkAnswer{Bytes: res.bytes, Files: res.files, Mechanism: w.mechanism(), Skipped: res.skipped}
	to, err := w.sessionRoot(id)
	if err != nil {
		_ = removeDir(tmp)
		return refuse(workspace.ClassFailed, "%v", err)
	}
	out.Rewritten = rewriteVenvs(tmp, []string{templateRootToken}, to)
	w.mu.Lock()
	defer w.mu.Unlock()
	if a := w.atCapacity(); a.class != "" {
		_ = removeDir(tmp)
		return a
	}
	if err := w.setLimit(id, quota); err != nil {
		_ = removeDir(tmp)
		return refuse(workspace.ClassFailed, "recording the workspace's quota: %v", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = removeDir(tmp)
		_ = os.Remove(w.quotaFile(id))
		return refuse(workspace.ClassFailed, "placing the workspace: %v", err)
	}
	return w.forkAnswer(ctx, id, dir, out)
}

// copyTemplate copies the cached template to tmp, holding the cache's lock
// so the tree is not evicted under the copy, and marks it used.
func (w *worker) copyTemplate(ctx context.Context, digest, tmp string, quota int64) (copyResult, answer) {
	w.tplMu.Lock()
	defer w.tplMu.Unlock()
	cache := w.templateCache(digest)
	if st, err := os.Lstat(cache); err != nil || !st.IsDir() {
		return copyResult{}, refuse(workspace.ClassTemplateMissing, "this worker does not hold the template")
	}
	now := time.Now()
	_ = os.Chtimes(cache, now, now)
	res, err := w.copyTree(ctx, cache, tmp, quota)
	if errors.Is(err, errOverBudget) {
		return copyResult{}, refuse(workspace.ClassOverQuota, "the template holds more than a new workspace's quota of %d bytes; nothing was made", quota)
	}
	if err != nil {
		return copyResult{}, refuse(workspace.ClassFailed, "copying the template: %v", err)
	}
	return res, answer{}
}
