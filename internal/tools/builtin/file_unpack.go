package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/files/unpack"
	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// File op=unpack (RFC GV P5, D12): the files of a ZIP or RAR archive are
// copied, by the runtime, into the inbox (or a read-write volume), each to be
// extracted on its own afterwards. Nothing is extracted here: an archive's
// directory is read and its entries inflated, and that is the work the
// extraction slot bounds.

// The walk itself, its bounds and its naming rules live in
// internal/files/unpack, which a workspace worker's conversion child runs
// too (RFC GW D34); what stays here is where an archive's files go.

const (
	// unpackDir is the inbox folder unpacked archives go to.
	unpackDir = "unpacked"
	// unpackMaxOrigin bounds a capture record's origin.
	unpackMaxOrigin = 512
)

// ---- the op ----

func (f *File) unpack(ctx context.Context, in fileInput) (tools.Result, error) {
	r, err := fileRef(in.Path)
	if err != nil {
		return errValidation("unpack: "+err.Error(), "Pass path, e.g. volume:inbox:documents/books.zip."), nil
	}
	lim := unpack.DefaultLimits
	if f.unpackLimits != nil {
		lim = *f.unpackLimits
	}
	if r.kind == refWorkspace {
		return f.unpackWorkspace(ctx, in, r, lim)
	}
	vfs, rel, res, ok := openRef(ctx, r, false)
	if !ok {
		return res, nil
	}
	defer vfs.Close()
	fh, err := vfs.Open(rel)
	if err != nil {
		return errNotFound(fmt.Sprintf("unpack: no file at %s", r.raw), "List the folder with op=ls."), nil
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errValidation(fmt.Sprintf("unpack: %s is not a file", r.raw), ""), nil
	}
	head := make([]byte, 512)
	n, _ := fh.ReadAt(head, 0)
	head = head[:n]
	kind, why := unpack.Kind(head)
	if kind == "" {
		return errValidation(fmt.Sprintf("unpack: %s is not a ZIP or RAR archive (%s)", r.raw, why), unpackNotArchiveHint), nil
	}
	if kind == "rar" {
		if refusal := unpack.RARRefusal(head); refusal != "" {
			return errValidation(fmt.Sprintf("unpack: %s %s", r.raw, refusal), unpackRefusedHint), nil
		}
	}

	slug := unpackSlug(rel)
	opt := unpack.Options{Limits: lim, PerFile: inbox.MaxFileBytes(ctx), Budget: lim.Total, DryRun: in.DryRun, All: in.All}
	var dest unpack.Dest
	var to string
	if strings.TrimSpace(in.To) != "" {
		vd, res, ok := f.unpackVolumeDest(ctx, in.To)
		if !ok {
			return res, nil
		}
		defer vd.vfs.Close()
		dest, to = vd, vd.prefix+vd.base
	} else {
		if !inbox.Available(ctx) {
			return errBusiness("unpack: this run cannot keep files, so there is nowhere to unpack to",
				"An operator must configure a dynamic_root volume for the run's inbox, or pass to: a read-write volume of yours."), nil
		}
		dir := path.Join(unpackDir, slug)
		dest, to = &inboxDest{ctx: ctx, archive: r.raw, dir: dir}, inbox.Ref(dir)+"/"
		// min is not used: a test file of this package declares its own.
		if rest := inbox.Remaining(ctx); rest < opt.Budget {
			opt.Budget = rest
		}
	}

	release, err := f.extractSlot(ctx)
	if err != nil {
		return slotBusy(ctx, "unpack", f.slotWait)
	}
	defer release()
	ures, err := unpack.Run(ctx, kind, fh, info.Size(), opt, dest)
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	var refused *unpack.Refusal
	if errors.As(err, &refused) {
		return errValidation(fmt.Sprintf("unpack: %s %s", r.raw, refused.Reason), unpackRefusedHint), nil
	}
	if err != nil {
		return tools.Result{}, err
	}
	return okJSON(ures.Answer(r.raw, kind, to))
}

// The hints of an archive unpack will not read.
const (
	unpackNotArchiveHint = "unpack reads ZIP and RAR archives only; 7z, tar, gzip and the rest are not read."
	unpackRefusedHint    = "Unpack it with a tool of your own and upload the files."
)

// unpackSlug is the inbox folder an archive at rel unpacks into, after its
// name.
func unpackSlug(rel string) string {
	name := path.Base(filepath.ToSlash(rel))
	slug := inbox.SanitizeName(strings.TrimSuffix(name, path.Ext(name)))
	if slug == "" {
		slug = "archive"
	}
	return slug
}

// unpackOrigin is the capture record's origin of an entry: the archive and
// the entry, so extract, import and provenance name where it came from.
func unpackOrigin(archive, display string) string {
	o := "unpack:" + archive + "#" + display
	if len(o) <= unpackMaxOrigin {
		return o
	}
	cut := unpackMaxOrigin
	for cut > 0 && !utf8.RuneStart(o[cut]) {
		cut--
	}
	return o[:cut]
}

// ---- destinations ----

// inboxDest writes through the inbox writer, a runtime write: a sub-agent
// whose inbox is read-only still unpacks into it, and each file gets a
// capture record naming the archive.
type inboxDest struct {
	ctx     context.Context
	archive string
	dir     string
}

func (d *inboxDest) Planned(segs []string) string {
	return inbox.Ref(path.Join(append([]string{d.dir}, segs...)...))
}

func (d *inboxDest) Write(segs []string, r io.Reader, head []byte, display string) (unpack.Written, string, error) {
	dir := path.Join(append([]string{d.dir}, segs[:len(segs)-1]...)...)
	src := io.MultiReader(bytes.NewReader(head), r)
	res := inbox.CaptureReader(d.ctx, inbox.Item{Name: segs[len(segs)-1], Dir: dir, Origin: unpackOrigin(d.archive, display)}, src, -1)
	if !res.Kept {
		// The reader's own bound wins over the writer's account of it.
		if b := unpack.BoundOf(r); b != nil {
			return unpack.Written{}, "", b
		}
		return unpack.Written{}, "", errors.New(strings.Trim(res.Stub, "[]"))
	}
	return unpack.Written{Ref: res.Record.Ref, Name: display, Size: res.Record.Size, MediaType: res.Record.MediaType}, "", nil
}

// volumeDest writes into a folder of a read-write volume, as keep does: the
// run needs Write, and Write's hooks judge each file it writes.
type volumeDest struct {
	ctx    context.Context
	vfs    *fsops.VolumeFS
	volume string
	prefix string // volume:<name>:
	base   string // the folder, relative to the volume root, slash-separated, ending in /
}

func (f *File) unpackVolumeDest(ctx context.Context, to string) (*volumeDest, tools.Result, bool) {
	r, err := parseDataRef(strings.TrimSpace(to))
	if err != nil || r.kind != refVolume {
		return nil, errValidation("unpack: to must be a volume folder", "Pass to, e.g. volume:books:library/, or omit it to unpack into the inbox."), false
	}
	if r.volume == inbox.Name {
		return nil, errValidation("unpack: to is in the inbox", "Omit to to unpack into volume:inbox:unpacked/."), false
	}
	if res, ok := checkRefGrant(ctx, r, true, FileToolName, "Unpack into the inbox"); !ok {
		return nil, res, false
	}
	root, fence, err := effectiveRoot(ctx, r.volume, true)
	if err != nil {
		return nil, errPermission("unpack: "+err.Error(), "Name a read-write volume you are bound to."), false
	}
	resolved, err := fsops.ResolveInsideRoot(root, r.path)
	if err == nil {
		err = fsops.OutsideFence(fence, resolved)
	}
	if err != nil {
		return nil, errPermission("unpack: "+err.Error(), "Create the folder first, or name one that exists."), false
	}
	vfs, err := fsops.OpenVolumeFS(root, fence)
	if err != nil {
		return nil, errPermission("unpack: "+err.Error(), ""), false
	}
	rel, err := vfs.Rel(resolved)
	if err == nil {
		var d *os.Root
		if d, err = vfs.OpenDir(rel); err == nil {
			d.Close()
		}
	}
	if err != nil {
		vfs.Close()
		return nil, errNotFound("unpack: "+to+" is not a folder", "Create it first, or name one that exists."), false
	}
	base := filepath.ToSlash(rel)
	if base == "." {
		base = ""
	} else {
		base += "/"
	}
	return &volumeDest{ctx: ctx, vfs: vfs, volume: r.volume, prefix: refPrefix(r), base: base}, tools.Result{}, true
}

func (d *volumeDest) Planned(segs []string) string {
	return d.prefix + d.base + path.Join(segs...)
}

func (d *volumeDest) ref(rel string) dataRef {
	return dataRef{raw: d.prefix + rel, kind: refVolume, volume: d.volume, path: rel}
}

func (d *volumeDest) Write(segs []string, r io.Reader, head []byte, display string) (unpack.Written, string, error) {
	// Asked before any folder is made for it, and again below of the name
	// it gets: a hook that refuses the path refuses its folders too.
	if res, ok := checkRefGrant(d.ctx, d.ref(d.base+path.Join(segs...)), true, FileToolName, "Unpack into the inbox"); !ok {
		return unpack.Written{}, res.Text, nil
	}
	dir, err := d.vfs.OpenDir(strings.TrimSuffix(d.base, "/"))
	if err != nil {
		return unpack.Written{}, "", err
	}
	folder := d.base
	for _, s := range segs[:len(segs)-1] {
		if err := dir.Mkdir(s, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			dir.Close()
			return unpack.Written{}, "", err
		}
		next, err := d.vfs.Step(dir, s)
		dir.Close()
		if err != nil {
			return unpack.Written{}, "", fmt.Errorf("%s is not a folder: %w", folder+s, err)
		}
		dir, folder = next, folder+s+"/"
	}
	defer dir.Close()
	name, err := reserveVolumeName(dir, segs[len(segs)-1])
	if err != nil {
		return unpack.Written{}, "", err
	}
	rel := folder + name
	if name != segs[len(segs)-1] {
		if res, ok := checkRefGrant(d.ctx, d.ref(rel), true, FileToolName, "Unpack into the inbox"); !ok {
			_ = dir.Remove(name)
			return unpack.Written{}, res.Text, nil
		}
	}
	tmp, tmpName, err := fsops.CreateTemp(dir, ".loomcycle-unpack-")
	if err != nil {
		_ = dir.Remove(name)
		return unpack.Written{}, "", err
	}
	n, copyErr := io.Copy(tmp, io.MultiReader(bytes.NewReader(head), r))
	closeErr := tmp.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr == nil {
		copyErr = d.ctx.Err()
	}
	if copyErr == nil {
		copyErr = dir.Rename(tmpName, name)
	}
	if copyErr != nil {
		_ = dir.Remove(tmpName)
		_ = dir.Remove(name)
		return unpack.Written{}, "", copyErr
	}
	return unpack.Written{Ref: d.prefix + rel, Name: display, Size: n, MediaType: inbox.Sniff("", head, name)}, "", nil
}

// reserveVolumeName creates name, or the first free name-<n> variant, empty
// and O_EXCL in dir, so no existing file is ever replaced.
func reserveVolumeName(dir *os.Root, name string) (string, error) {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i <= 1000; i++ {
		cand := name
		if i > 1 {
			cand = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		fh, err := dir.OpenFile(cand, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_ = fh.Close()
		return cand, nil
	}
	return "", fmt.Errorf("no free name for %s", name)
}
