package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// FileToolName is the File tool's name in an agent's `tools:` list.
const FileToolName = "File"

// fileMaxListed caps one ls answer.
const fileMaxListed = 1000

// File handles the files the runtime keeps for a run (RFC FF): a file a tool
// returned is kept in the run tree's inbox volume and the model is handed a
// reference to it, never its bytes. This tool inspects those files, copies
// one out of the inbox into a durable volume, and removes them.
//
// It moves bytes between volumes itself, so a file never passes through the
// model, and it reaches a volume only through the agent's own bindings and
// the run tree's inbox, with the same resolution and fence as the other file
// tools. Changing the inbox needs it writable to this run, as for any
// ephemeral volume: a sub-agent given the tree's volumes read-only can look,
// not remove.
type File struct{}

func (f *File) Name() string { return FileToolName }

func (f *File) Description() string {
	return "Inspect and manage files the runtime kept for this run, without their bytes passing through you. " +
		"A file a tool returns (an MCP attachment or image, a binary download) is kept in your run's inbox volume and you see a line like " +
		"`[file: volume:inbox:documents/report.pdf · application/pdf · 1.8 MiB · sha256:… · from …]` instead of its bytes. " +
		"Ops: stat (size, type, sha256 and origin of one file), ls (list the inbox or a folder of a volume), " +
		"keep (copy a file out of the inbox into one of your read-write volumes, since the inbox is deleted when the run ends), " +
		"rm (delete one inbox file), clear (empty the inbox, or one folder of it). " +
		"Paths are references: `volume:inbox:documents/a.pdf`, `volume:<name>:/path`, or `volume:path` for your default volume; a bare path means the inbox. " +
		"Do NOT use it to read a file's text — use Read for a text file; and do not ask for a file's base64, which you could neither hold nor write back."
}

func (f *File) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"op":        {"type": "string", "enum": ["stat", "ls", "keep", "rm", "clear"]},
			"path":      {"type": "string", "description": "stat, rm: the file; ls: the folder (default volume:inbox:). A volume reference, or a bare path in the inbox."},
			"from":      {"type": "string", "description": "keep: the inbox file to copy."},
			"to":        {"type": "string", "description": "keep: where to copy it, in one of your read-write volumes (needs the Write tool). A path ending in / (or an existing folder) keeps the file's name. Its folder must exist."},
			"overwrite": {"type": "boolean", "description": "keep: replace an existing file at to. Default false."},
			"recursive": {"type": "boolean", "description": "ls: list subfolders too."},
			"prefix":    {"type": "string", "description": "clear: only this folder of the inbox (e.g. documents, web/example.com). Omit to empty the whole inbox."}
		},
		"required": ["op"]
	}`)
}

type fileInput struct {
	Op        string `json:"op"`
	Path      string `json:"path"`
	From      string `json:"from"`
	To        string `json:"to"`
	Overwrite bool   `json:"overwrite"`
	Recursive bool   `json:"recursive"`
	Prefix    string `json:"prefix"`
}

func (f *File) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var in fileInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errValidation("invalid input: "+err.Error(), "Pass a JSON object with an op."), nil
	}
	switch in.Op {
	case "stat":
		return f.stat(ctx, in)
	case "ls":
		return f.ls(ctx, in)
	case "keep":
		return f.keep(ctx, in)
	case "rm":
		return f.rm(ctx, in)
	case "clear":
		return f.clear(ctx, in)
	case "":
		return errValidation("missing required field: op", "One of stat, ls, keep, rm, clear."), nil
	}
	return errValidation(fmt.Sprintf("unknown op %q", in.Op), "One of stat, ls, keep, rm, clear."), nil
}

// fileRef parses a path argument: a volume reference, or a bare path that
// names a file in the inbox.
func fileRef(s string) (dataRef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return dataRef{}, errors.New("no path given")
	}
	if !strings.HasPrefix(s, "volume:") {
		return dataRef{raw: inbox.Ref(s), kind: refVolume, volume: inbox.Name, path: s}, nil
	}
	r, err := parseDataRef(s)
	if err != nil {
		return dataRef{}, err
	}
	return r, nil
}

// openRef resolves a reference to its volume, checks it lies inside it, and
// opens the volume for the call. rel is the path relative to the volume root.
func openRef(ctx context.Context, r dataRef, write bool) (vfs *volumeFS, rel string, res tools.Result, ok bool) {
	if inboxMissing(ctx, r) {
		return nil, "", errNotFound("nothing has been kept in this run's inbox yet", "A file a tool returns lands there; until one does, the inbox does not exist."), false
	}
	// The inbox is this tool's own: the files in it are the run's tool
	// results. Any other volume is reached as Crypto's from/to reach one: the
	// run must hold Read (or Write) itself, and that tool's hooks judge it.
	if r.volume != inbox.Name {
		if res, ok := checkRefGrant(ctx, r, write, FileToolName, "Work in the inbox only"); !ok {
			return nil, "", res, false
		}
	}
	root, fence, err := effectiveRoot(ctx, r.volume, write)
	if err != nil {
		return nil, "", errPermission(err.Error(), "Name a volume you are bound to; Context op=self lists them."), false
	}
	resolved, err := resolveInsideRoot(root, r.path)
	if err == nil {
		err = outsideFence(fence, resolved)
	}
	if err != nil {
		return nil, "", errPermission(err.Error(), ""), false
	}
	vfs, err = openVolumeFS(root, fence)
	if err != nil {
		return nil, "", errPermission(err.Error(), ""), false
	}
	rel, err = vfs.rel(resolved)
	if err != nil {
		vfs.Close()
		return nil, "", errPermission(err.Error(), ""), false
	}
	return vfs, rel, tools.Result{}, true
}

// inboxMissing reports a reference to the inbox of a run tree that has kept
// nothing yet: the volume is made on the first capture.
func inboxMissing(ctx context.Context, r dataRef) bool {
	return r.volume == inbox.Name && !tools.EphemeralVolumes(ctx).Has(inbox.Name)
}

// ---- stat ----

func (f *File) stat(ctx context.Context, in fileInput) (tools.Result, error) {
	r, err := fileRef(in.Path)
	if err != nil {
		return errValidation("stat: "+err.Error(), "Pass path, e.g. volume:inbox:documents/a.pdf."), nil
	}
	vfs, rel, res, ok := openRef(ctx, r, false)
	if !ok {
		return res, nil
	}
	defer vfs.Close()
	fh, err := vfs.open(rel)
	if err != nil {
		return errNotFound(fmt.Sprintf("stat: no file at %s", r.raw), "List the folder with op=ls."), nil
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errValidation(fmt.Sprintf("stat: %s is not a file", r.raw), "List a folder with op=ls."), nil
	}
	sum := sha256.New()
	head := make([]byte, 512)
	n, _ := io.ReadFull(fh, head)
	sum.Write(head[:n])
	if _, err := io.Copy(sum, fh); err != nil {
		return errTransient(fmt.Sprintf("stat: reading %s failed", r.raw), "Try again."), nil
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	// The record is what was kept; it describes this file only while the
	// bytes are still those bytes. A file changed since (the inbox is
	// writable to the run) is reported for what it is now, origin unknown.
	if rec, ok := recordFor(ctx, r, rel, digest); ok {
		return okJSON(rec)
	}
	out := map[string]any{
		"ref":        r.raw,
		"path":       filepath.ToSlash(rel),
		"media_type": inbox.Sniff("", head[:n], info.Name()),
		"size":       info.Size(),
		"sha256":     digest,
		"origin":     "unknown",
		"modified":   info.ModTime().UTC().Format(time.RFC3339),
	}
	return okJSON(out)
}

// recordFor returns the capture record of an inbox file whose bytes still
// hash to what was recorded.
func recordFor(ctx context.Context, r dataRef, rel, digest string) (inbox.Record, bool) {
	if r.volume != inbox.Name {
		return inbox.Record{}, false
	}
	rec, ok := inbox.Lookup(ctx, rel)
	return rec, ok && rec.SHA256 == digest
}

// ---- ls ----

type fileEntry struct {
	Ref       string `json:"ref"`
	Size      int64  `json:"size,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Dir       bool   `json:"dir,omitempty"`
	Origin    string `json:"origin,omitempty"`
}

func (f *File) ls(ctx context.Context, in fileInput) (tools.Result, error) {
	p := in.Path
	if p == "" {
		p = inbox.Ref(".")
	}
	r, err := fileRef(p)
	if err != nil {
		return errValidation("ls: "+err.Error(), "Pass path, e.g. volume:inbox:documents."), nil
	}
	if inboxMissing(ctx, r) {
		return okJSON(map[string]any{"path": r.raw, "entries": []fileEntry{}, "note": "nothing has been kept in this run's inbox yet"})
	}
	vfs, rel, res, ok := openRef(ctx, r, false)
	if !ok {
		return res, nil
	}
	defer vfs.Close()

	prefix := refPrefix(r)
	entries := []fileEntry{}
	truncated := false
	add := func(relPath string, d fs.DirEntry, size int64) {
		if len(entries) >= fileMaxListed {
			truncated = true
			return
		}
		e := fileEntry{Ref: prefix + path.Join(filepath.ToSlash(rel), relPath), Dir: d.IsDir()}
		if !e.Dir {
			e.Size = size
			if r.volume == inbox.Name {
				if rec, ok := inbox.Lookup(ctx, path.Join(filepath.ToSlash(rel), relPath)); ok {
					e.MediaType, e.Origin = rec.MediaType, rec.Origin
				}
			}
		}
		entries = append(entries, e)
	}
	if in.Recursive {
		vfs.walk(rel, func(dir *os.Root, p string, d fs.DirEntry) {
			if hiddenPath(p) {
				return
			}
			var size int64
			if fi, err := dir.Lstat(filepath.Base(p)); err == nil {
				size = fi.Size()
			}
			add(filepath.ToSlash(p), d, size)
		})
	} else {
		dir, err := vfs.openDir(rel)
		if err != nil {
			return errNotFound(fmt.Sprintf("ls: no folder at %s", r.raw), "List its parent."), nil
		}
		defer dir.Close()
		d, err := dir.Open(".")
		if err != nil {
			return errNotFound(fmt.Sprintf("ls: no folder at %s", r.raw), "List its parent."), nil
		}
		ents, _ := d.ReadDir(-1)
		d.Close()
		for _, e := range ents {
			if hiddenPath(e.Name()) {
				continue
			}
			var size int64
			if fi, err := e.Info(); err == nil {
				size = fi.Size()
			}
			add(e.Name(), e, size)
		}
	}
	return okJSON(map[string]any{"path": r.raw, "entries": entries, "truncated": truncated})
}

// refPrefix is the part of a reference before its path.
func refPrefix(r dataRef) string {
	if r.volume == "" {
		return "volume:"
	}
	return "volume:" + r.volume + ":"
}

// hiddenPath skips dotfiles: the inbox writes a file under a hidden
// temporary name before it is complete.
func hiddenPath(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if strings.HasPrefix(seg, ".") && seg != "." {
			return true
		}
	}
	return false
}

// ---- keep ----

func (f *File) keep(ctx context.Context, in fileInput) (tools.Result, error) {
	from, err := fileRef(in.From)
	if err != nil {
		return errValidation("keep: from: "+err.Error(), "Pass from, e.g. volume:inbox:documents/a.pdf."), nil
	}
	if from.volume != inbox.Name {
		return errValidation("keep: from must be a file in the inbox", "Copy between your own volumes with Read and Write."), nil
	}
	to, err := parseDataRef(strings.TrimSpace(in.To))
	if err != nil || to.kind != refVolume {
		return errValidation("keep: to must be a volume reference", "Pass to, e.g. volume:docs:board/2026/."), nil
	}
	if to.volume == inbox.Name {
		return errValidation("keep: to is in the inbox, which is deleted with the run", "Name one of your read-write volumes."), nil
	}

	src, srcRel, res, ok := openRef(ctx, from, false)
	if !ok {
		return res, nil
	}
	defer src.Close()
	in1, err := src.open(srcRel)
	if err != nil {
		return errNotFound(fmt.Sprintf("keep: no file at %s", from.raw), "List the inbox with op=ls."), nil
	}
	defer in1.Close()
	info, err := in1.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errValidation(fmt.Sprintf("keep: %s is not a file", from.raw), ""), nil
	}

	if res, ok := checkRefGrant(ctx, to, true, FileToolName, "Leave it in the inbox"); !ok {
		return res, nil
	}
	root, fence, err := effectiveRoot(ctx, to.volume, true)
	if err != nil {
		return errPermission("keep: "+err.Error(), "Name a read-write volume you are bound to."), nil
	}
	toPath := to.path
	if strings.HasSuffix(toPath, "/") {
		toPath += filepath.Base(srcRel)
	}
	target, err := resolveParentInsideRoot(root, toPath)
	if err == nil {
		err = outsideFence(fence, target)
	}
	if err != nil {
		return errPermission("keep: "+err.Error(), "Create the folder first, or name one that exists."), nil
	}
	dst, err := openVolumeFS(root, fence)
	if err != nil {
		return errPermission("keep: "+err.Error(), ""), nil
	}
	defer dst.Close()
	rel, err := dst.rel(target)
	if err != nil {
		return errPermission("keep: "+err.Error(), ""), nil
	}
	dir, err := dst.openDir(filepath.Dir(rel))
	if err != nil {
		return errNotFound("keep: the folder for "+in.To+" does not exist", "Create it first, or name one that exists."), nil
	}
	defer dir.Close()
	name := filepath.Base(rel)
	if fi, err := dir.Lstat(name); err == nil {
		if fi.IsDir() {
			// An existing folder: keep the file's own name inside it.
			sub, err := dst.step(dir, name)
			if err != nil {
				return errPermission("keep: "+err.Error(), ""), nil
			}
			defer sub.Close()
			dir, name, rel = sub, filepath.Base(srcRel), filepath.Join(rel, filepath.Base(srcRel))
		}
	}
	if fi, err := dir.Lstat(name); err == nil {
		if !fi.Mode().IsRegular() || !in.Overwrite {
			return errBusiness(fmt.Sprintf("keep: %s already exists", refPrefix(to)+filepath.ToSlash(rel)), "Pass overwrite: true to replace it, or choose another name."), nil
		}
	}
	tmp, tmpName, err := createTemp(dir, ".loomcycle-keep-")
	if err != nil {
		return errTransient("keep: "+err.Error(), "Try again."), nil
	}
	defer dir.Remove(tmpName)
	sum := sha256.New()
	if _, err := io.Copy(tmp, io.TeeReader(in1, sum)); err != nil {
		tmp.Close()
		return errTransient("keep: copying failed: "+err.Error(), "Try again."), nil
	}
	if err := tmp.Close(); err != nil {
		return errTransient("keep: copying failed: "+err.Error(), "Try again."), nil
	}
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	if err := dir.Rename(tmpName, name); err != nil {
		return errTransient("keep: "+err.Error(), "Try again."), nil
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	out := map[string]any{"from": from.raw, "ref": refPrefix(to) + filepath.ToSlash(rel), "size": info.Size(), "sha256": digest}
	if rec, ok := recordFor(ctx, from, srcRel, digest); ok {
		out["media_type"], out["origin"] = rec.MediaType, rec.Origin
	}
	return okJSON(out)
}

// ---- rm / clear ----

func (f *File) rm(ctx context.Context, in fileInput) (tools.Result, error) {
	r, err := fileRef(in.Path)
	if err != nil {
		return errValidation("rm: "+err.Error(), "Pass path, e.g. volume:inbox:documents/a.pdf."), nil
	}
	if r.volume != inbox.Name {
		return errValidation("rm: only a file in the inbox can be removed here", "Remove a file of your own volumes with the tool that wrote it."), nil
	}
	vfs, rel, res, ok := openRef(ctx, r, true)
	if !ok {
		return res, nil
	}
	defer vfs.Close()
	dir, err := vfs.openDir(filepath.Dir(rel))
	if err != nil {
		return errNotFound(fmt.Sprintf("rm: no file at %s", r.raw), "List the inbox with op=ls."), nil
	}
	defer dir.Close()
	fi, err := dir.Lstat(filepath.Base(rel))
	if err != nil {
		return errNotFound(fmt.Sprintf("rm: no file at %s", r.raw), "List the inbox with op=ls."), nil
	}
	if fi.IsDir() {
		return errValidation(fmt.Sprintf("rm: %s is a folder", r.raw), "Empty a folder with op=clear and its prefix."), nil
	}
	if err := dir.Remove(filepath.Base(rel)); err != nil {
		return errTransient("rm: "+err.Error(), "Try again."), nil
	}
	inbox.Forget(ctx, rel)
	return okJSON(map[string]any{"removed": r.raw, "bytes": fi.Size()})
}

func (f *File) clear(ctx context.Context, in fileInput) (tools.Result, error) {
	prefix := strings.Trim(strings.TrimSpace(in.Prefix), "/")
	if prefix == "" {
		prefix = "."
	}
	r := dataRef{raw: inbox.Ref(prefix), kind: refVolume, volume: inbox.Name, path: prefix}
	vfs, rel, res, ok := openRef(ctx, r, true)
	if !ok {
		return res, nil
	}
	defer vfs.Close()
	var files, bytes int64
	vfs.walk(rel, func(dir *os.Root, p string, d fs.DirEntry) {
		if fi, err := dir.Lstat(filepath.Base(p)); err == nil && fi.Mode().IsRegular() {
			files++
			bytes += fi.Size()
		}
	})
	if rel == "." || rel == "" {
		d, err := vfs.root.Open(".")
		if err != nil {
			return errTransient("clear: "+err.Error(), "Try again."), nil
		}
		ents, _ := d.ReadDir(-1)
		d.Close()
		for _, e := range ents {
			if err := vfs.root.RemoveAll(e.Name()); err != nil {
				return errTransient("clear: "+err.Error(), "Try again."), nil
			}
		}
	} else if err := vfs.root.RemoveAll(rel); err != nil {
		return errTransient("clear: "+err.Error(), "Try again."), nil
	}
	inbox.Forget(ctx, rel)
	return okJSON(map[string]any{"cleared": r.raw, "files": files, "bytes": bytes})
}
