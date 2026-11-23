package builtin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// File's transfers with a workspace (RFC GW P2 §3.2–§3.4): import reads
// Markdown out of one, download writes into one, upload sends from one, and
// Crypto reads and writes through the same references. Each goes through the
// workspace grant and the Workspace pre-hooks (workspaceLoc), and none holds
// a workspace's file whole on the core: Markdown is read within the import
// cap, an upload is served by ranged gets, a download streams into a put.

// ---- reading a workspace file at random: upload ----

// wsReadWindow is how much of a file one ranged get fetches for a
// wsReaderAt. An upload reads its body sequentially through a SectionReader
// in the HTTP client's small reads; a window turns those into one get per
// MiB rather than one per read, and is all the core holds of the file.
const wsReadWindow = 1 << 20

// wsReaderAt reads a workspace file at any offset through ranged gets of
// wsReadWindow, keeping the last window. It is safe for concurrent use.
type wsReaderAt struct {
	ctx  context.Context
	l    *workspaceLoc
	size int64

	mu  sync.Mutex
	buf []byte
	off int64
}

func (l *workspaceLoc) readerAt(ctx context.Context, size int64) *wsReaderAt {
	return &wsReaderAt{ctx: ctx, l: l, size: size}
}

func (r *wsReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for n < len(p) {
		if off >= r.size {
			return n, io.EOF
		}
		if off < r.off || off >= r.off+int64(len(r.buf)) {
			if err := r.fill(off); err != nil {
				return n, err
			}
		}
		c := copy(p[n:], r.buf[off-r.off:])
		n += c
		off += int64(c)
	}
	return n, nil
}

// fill fetches the window at off, reusing the buffer.
func (r *wsReaderAt) fill(off int64) error {
	length := r.size - off
	if length > wsReadWindow {
		length = wsReadWindow
	}
	b := bytes.NewBuffer(r.buf[:0])
	r.buf = nil
	got, err := r.l.m.GetFile(r.ctx, r.l.o, r.l.name, workspace.GetArgs{Path: r.l.path, Offset: off, Length: length}, b, 0)
	if err != nil {
		return err
	}
	if got != length {
		return fmt.Errorf("%s changed while it was read (%d bytes at %d, expected %d)", r.l.Ref(), got, off, length)
	}
	r.buf, r.off = b.Bytes(), off
	return nil
}

// uploadSource resolves an upload's workspace source: the read grant, the
// Workspace hook on the file, and its size and sha256 from fstat, so the
// HTTP hook sees {from, size, sha256} without the core reading the file.
func (f *File) uploadSource(ctx context.Context, from dataRef) (ra io.ReaderAt, info workspace.FileInfo, res tools.Result, ok bool) {
	l, res, ok := f.workspaceLoc(ctx, from, false, FileToolName)
	if !ok {
		return nil, info, res, false
	}
	if res, ok := l.hook(ctx, wsHookRead); !ok {
		return nil, info, res, false
	}
	info, err := l.m.Stat(ctx, l.o, l.name, workspace.FStatArgs{Path: l.path, SHA256: true})
	if err != nil {
		return nil, info, prefixed("upload", workspaceRefError(err, l.name)), false
	}
	if info.Type != workspace.EntryFile {
		return nil, info, errValidation(fmt.Sprintf("upload: %s is not a file", from.raw), ""), false
	}
	return l.readerAt(ctx, info.Size), info, tools.Result{}, true
}

// ---- download into a workspace ----

// workspaceDownloadTarget judges a download's workspace target before the
// request when its final path is already known: a file path, not a folder.
// judged is false for a folder target (a path ending in "/", the root, or an
// existing folder), whose file name only the response gives.
func workspaceDownloadTarget(ctx context.Context, l *workspaceLoc, to dataRef) (judged bool, res tools.Result, ok bool) {
	// The stat also asks the pin: another run's workspace is not found
	// before anything is requested.
	e, res, ok := l.Stat(ctx)
	if !ok {
		return false, prefixed("download", res), false
	}
	if strings.HasSuffix(to.path, "/") || l.path == "." || e.Type == workspace.EntryDir {
		return false, tools.Result{}, true
	}
	if res, ok := l.hook(ctx, wsHookWrite); !ok {
		return false, res, false
	}
	return true, tools.Result{}, true
}

// downloadToWorkspace streams a response body into a workspace file. A
// folder target takes the response's file name inside it, judged by the
// Workspace hook on that final path before a byte of the body is read (a
// file target was judged before the request). The put declares the body's
// length, so the workspace's quota refuses it before anything is sent and
// nothing is left. A response that gives no length is put with an unknown
// size bounded by the download's limit, which the quota must hold ahead;
// it is never spooled on the core.
func (f *File) downloadToWorkspace(ctx context.Context, l *workspaceLoc, judged bool, to dataRef, name string, overwrite bool, body io.Reader, resp *http.Response, shown string, limit int64) (tools.Result, error) {
	fileName := inbox.SanitizeName(name)
	if fileName == "" {
		fileName = "download"
	}
	if !judged {
		l = l.child(fileName)
		if res, ok := l.hook(ctx, wsHookWrite); !ok {
			return res, nil
		}
	}
	head := &headWriter{max: 512}
	src := io.TeeReader(body, head)
	var w locWritten
	if size := resp.ContentLength; size >= 0 {
		if size > limit {
			return errValidation(fmt.Sprintf("download: %s is %d bytes, over the limit of %d", shown, size, limit), ""), nil
		}
		var res tools.Result
		var ok bool
		if w, res, ok = l.Create(ctx, size, src, overwrite); !ok {
			return prefixed("download", res), nil
		}
	} else {
		var err error
		if w, err = l.createUnsized(ctx, limit, src, overwrite); err != nil {
			return unsizedDownloadError(err, shown, limit), nil
		}
	}
	return okJSON(map[string]any{"url": shown, "status": resp.StatusCode, "ref": w.Ref, "size": w.Size,
		"sha256": w.SHA256, "media_type": inbox.Sniff(resp.Header.Get("Content-Type"), head.b, path.Base(l.path))})
}

// unsizedDownloadError words a failed put of a body of unknown length: past
// the limit it is over the limit (the core's check, or the worker's), and a
// quota refusal says the room needed is the limit's, since the size was not
// known ahead.
func unsizedDownloadError(err error, shown string, limit int64) tools.Result {
	switch dispatch.ClassOf(err) {
	case workspace.ClassSizeMismatch:
		return errValidation(fmt.Sprintf("download: %s is over the limit of %d bytes", shown, limit), "")
	case workspace.ClassOverQuota:
		res := prefixed("download", copyReadError(err))
		res.Text += fmt.Sprintf(" (the response gave no size, so the download needs room for its limit of %d bytes ahead)", limit)
		return res
	}
	return prefixed("download", copyReadError(err))
}

// ---- import from a workspace ----

// readWorkspaceMarkdown reads the Markdown an import builds a document
// from: a regular file, at most fileImportMaxBytes, UTF-8. Its sha256 is
// fstat's, and the bytes read must hash to it: a file changed while it was
// read is refused rather than imported under a digest it does not have.
func readWorkspaceMarkdown(ctx context.Context, l *workspaceLoc, r dataRef) ([]byte, workspace.FileInfo, tools.Result, bool) {
	info, err := l.m.Stat(ctx, l.o, l.name, workspace.FStatArgs{Path: l.path, SHA256: true})
	if err != nil {
		return nil, info, prefixed("import", workspaceRefError(err, l.name)), false
	}
	if info.Type != workspace.EntryFile {
		return nil, info, errValidation(fmt.Sprintf("import: %s is not a file", r.raw), ""), false
	}
	if info.Size > fileImportMaxBytes {
		return nil, info, errValidation(fmt.Sprintf("import: %s is over %d bytes", r.raw, fileImportMaxBytes), "Split the document."), false
	}
	rc, res, ok := l.Open(ctx, ".")
	if !ok {
		return nil, info, prefixed("import", res), false
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, fileImportMaxBytes+1))
	if err != nil {
		return nil, info, prefixed("import", copyReadError(err)), false
	}
	if len(b) > fileImportMaxBytes {
		return nil, info, errValidation(fmt.Sprintf("import: %s is over %d bytes", r.raw, fileImportMaxBytes), "Split the document."), false
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != info.SHA256 {
		return nil, info, errTransient(fmt.Sprintf("import: %s changed while it was read", r.raw), "Try again when nothing writes it."), false
	}
	if !utf8.Valid(b) {
		return nil, info, errValidation(fmt.Sprintf("import: %s is not UTF-8 text", r.raw), "Import a Markdown file."), false
	}
	return b, info, tools.Result{}, true
}

// workspaceImportProvenance is what an import from a workspace records: the
// file, its type and sha256 (fstat's), the workspace it was captured from
// by name and id, when, and by which run.
func workspaceImportProvenance(ctx context.Context, l *workspaceLoc, info workspace.FileInfo, b []byte) importProvenance {
	prov := newImportProvenance()
	f := prov.fields
	name := path.Base(l.path)
	f[importFieldFile] = name
	head := b
	if len(head) > 512 {
		head = head[:512]
	}
	f[importFieldMediaType] = inbox.Sniff("", head, name)
	f[importFieldSHA256] = info.SHA256
	f[importFieldCapturedFrom] = "workspace:" + l.name
	f[importFieldCapturedAt] = time.Now().UTC().Format(time.RFC3339)
	f[importFieldRunID] = l.o.RunID
	if row, err := l.m.Get(ctx, l.o, l.name); err == nil {
		f[importFieldWorkspaceID] = row.ID
	}
	return prov
}

// mdImageLineRe is a line that is exactly one image embed, the form
// import_md attaches as an image chunk. A link with a title, spaces or
// parentheses is not matched and stays text.
var mdImageLineRe = regexp.MustCompile(`^!\[([^\]\n\r]*)\]\(([^\s()<>]+)\)$`)

// workspaceImageLinks rewrites each line of md that is one image embed with
// a relative link (resolved against the folder of the file at mdPath) into
// a reference to that file in the same workspace, at most
// maxImportVolumeRefs distinct files. It returns the references it wrote:
// the only ones the import's image reader follows, so a workspace reference
// the text spelled out itself is never read. Any other link — absolute,
// with a scheme, with `..` — is left as text. Fenced code is left alone.
func workspaceImageLinks(md, name, mdPath string) (string, map[string]bool) {
	dir := path.Dir(wsClean(mdPath))
	refs := map[string]bool{}
	lines := strings.Split(md, "\n")
	var fenceChar byte
	fenceRun := 0
	for i, l := range lines {
		if c, run := mdFenceDelim(l); run > 0 {
			switch {
			case fenceRun == 0:
				fenceChar, fenceRun = c, run
			case c == fenceChar && run >= fenceRun:
				fenceRun = 0
			}
			continue
		}
		if fenceRun > 0 || strings.TrimSpace(l) != l {
			continue
		}
		m := mdImageLineRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		target, ok := workspaceImageTarget(dir, m[2])
		if !ok {
			continue
		}
		ref := wsRef(name, target)
		if !refs[ref] && len(refs) >= maxImportVolumeRefs {
			continue
		}
		refs[ref] = true
		lines[i] = "![" + m[1] + "](" + ref + ")"
	}
	return strings.Join(lines, "\n"), refs
}

// workspaceImageTarget resolves a relative image link against dir; ok is
// false for anything that is not a plain relative path inside the
// workspace: a scheme or another reference kind, an absolute path, `..`, a
// query or fragment.
func workspaceImageTarget(dir, link string) (string, bool) {
	if link == "" || strings.HasPrefix(link, "/") || strings.ContainsAny(link, ":?#\\\x00") {
		return "", false
	}
	for _, seg := range strings.Split(link, "/") {
		if seg == ".." {
			return "", false
		}
	}
	t := wsClean(path.Join(dir, link))
	if t == "." {
		return "", false
	}
	return t, true
}

// workspaceImageReader reads the images an import from l attaches: only the
// references workspaceImageLinks wrote, each judged by the Workspace hook
// as a read of that file, each a regular file (a link or a special file is
// refused, as the worker refuses it too) within max bytes.
func workspaceImageReader(l *workspaceLoc, refs map[string]bool) volumeAssetReader {
	return func(ctx context.Context, r dataRef, max int64) ([]byte, error) {
		if r.kind != refWorkspace || r.workspace != l.name || !refs[r.raw] {
			return nil, errors.New("only images the imported file links to by a relative path in its own workspace are attached")
		}
		c := *l
		c.path = wsClean(r.path)
		if res, ok := c.hook(ctx, wsHookRead); !ok {
			return nil, errors.New(res.Text)
		}
		info, err := l.m.Stat(ctx, l.o, l.name, workspace.FStatArgs{Path: c.path})
		if err != nil {
			return nil, fmt.Errorf("no file at %s", r.raw)
		}
		if info.Type != workspace.EntryFile {
			return nil, fmt.Errorf("%s is not a regular file", r.raw)
		}
		if info.Size > max {
			return nil, fmt.Errorf("%s is %d bytes, over the %d-byte limit", r.raw, info.Size, max)
		}
		var buf bytes.Buffer
		if _, err := l.m.GetFile(ctx, l.o, l.name, workspace.GetArgs{Path: c.path}, &buf, max); err != nil {
			return nil, fmt.Errorf("reading %s failed", r.raw)
		}
		return buf.Bytes(), nil
	}
}

// ---- Crypto ----

// cryptoWorkspaceLoc resolves a Crypto from (write false) or to on a
// workspace: the workspace grant and the asking Crypto tool, then the
// Workspace hook on the file, before any byte moves. e is what is at the
// place now.
func (c *Crypto) cryptoWorkspaceLoc(ctx context.Context, r dataRef, write bool) (l *workspaceLoc, e locEntry, res tools.Result, ok bool) {
	l, res, ok = resolveWorkspaceLoc(ctx, c.Workspaces, r, write, CryptoToolName)
	if !ok {
		return nil, e, res, false
	}
	if l.path == "." {
		return nil, e, errValidation(r.raw+" is the workspace's root, not a file", "Name a file in the workspace."), false
	}
	// The stat asks the pin (another run's workspace is not found) before
	// the op runs; a folder is no file to write.
	e, res, ok = l.Stat(ctx)
	if !ok {
		return nil, e, res, false
	}
	if write && e.Type == workspace.EntryDir {
		return nil, e, errValidation(r.raw+" is a folder, not a file", "Name a file in the workspace."), false
	}
	op := wsHookRead
	if write {
		op = wsHookWrite
	}
	if res, ok := l.hook(ctx, op); !ok {
		return nil, e, res, false
	}
	return l, e, tools.Result{}, true
}

// readWorkspaceRef reads at most max bytes of a workspace file; more is
// refused, never truncated.
func (c *Crypto) readWorkspaceRef(ctx context.Context, r dataRef, max int) ([]byte, tools.Result, bool) {
	l, e, res, ok := c.cryptoWorkspaceLoc(ctx, r, false)
	if !ok {
		return nil, res, false
	}
	switch {
	case e.Type == "":
		return nil, errNotFound(fmt.Sprintf("cannot open %s", r.raw), "Check the path."), false
	case e.Type != workspace.EntryFile:
		return nil, errValidation(fmt.Sprintf("%s is not a regular file", r.raw), ""), false
	case e.Size > int64(max):
		return nil, errValidation(fmt.Sprintf("%s holds more than %d bytes", r.raw, max), "Split the data."), false
	}
	rc, res, ok := l.Open(ctx, ".")
	if !ok {
		return nil, res, false
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, int64(max)+1))
	if err != nil {
		return nil, copyReadError(err), false
	}
	if len(b) > max {
		return nil, errValidation(fmt.Sprintf("%s holds more than %d bytes", r.raw, max), "Split the data."), false
	}
	return b, tools.Result{}, true
}

// writeWorkspaceRef writes data as the workspace file, replacing one there,
// as a volume `to` replaces its file; the put is atomic and quota-checked.
// The hook judged this write in readFrom, before the op ran, and is not
// asked twice.
func (c *Crypto) writeWorkspaceRef(ctx context.Context, r dataRef, data []byte) (tools.Result, bool) {
	l, res, ok := resolveWorkspaceLoc(ctx, c.Workspaces, r, true, CryptoToolName)
	if !ok {
		return res, false
	}
	if _, res, ok := l.Create(ctx, int64(len(data)), bytes.NewReader(data), true); !ok {
		return res, false
	}
	return tools.Result{}, true
}
