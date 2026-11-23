package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// A workspace reference in the File tool (RFC GW P2 §3.2, ruling Q4). It
// needs the run's workspace grant — read to read from a workspace, write to
// write into or remove from one — and the asking tool in the run's tools;
// the Workspace tool itself need not be there, and the Read and Write grants
// that govern volumes are not involved. The run's Workspace pre-hooks judge
// it as a call {op: read|write|remove, name, path, via}, on the final path,
// so an operator writes one set of rules for everything that touches a
// workspace. The pin is the manager's: a workspace another run opened is
// not found, as one nobody opened is.

// The hook ops a workspace reference is judged as.
const (
	wsHookRead   = "read"
	wsHookWrite  = "write"
	wsHookRemove = "remove"
)

// workspaceLoc is a file or folder in a workspace of the calling run.
type workspaceLoc struct {
	m    *lifecycle.Manager
	o    lifecycle.Owner
	name string
	path string // cleaned, "." for the root
	via  string
}

// workspaceLoc resolves r for a use: the run, the asking tool and the
// workspace grant are checked here, before anything reaches the worker; the
// hook is judged separately (hook), on the final path.
func (f *File) workspaceLoc(ctx context.Context, r dataRef, write bool, via string) (*workspaceLoc, tools.Result, bool) {
	return resolveWorkspaceLoc(ctx, f.Workspaces, r, write, via)
}

// resolveWorkspaceLoc is workspaceLoc for any tool holding the bookkeeping m
// (File, Crypto): via names the asking tool, which the run must hold.
func resolveWorkspaceLoc(ctx context.Context, m *lifecycle.Manager, r dataRef, write bool, via string) (*workspaceLoc, tools.Result, bool) {
	if tools.IsOffRunPlane(ctx) || tools.RunID(ctx) == "" {
		return nil, errPermission(r.raw+": a workspace belongs to a run, and this call has none",
			"Use the reference inside the run that opened the workspace."), false
	}
	if !slices.Contains(tools.PolicyOf(ctx).Tools, via) {
		return nil, errPermission(fmt.Sprintf("%s needs the %s tool, which this run does not have", r.raw, via), ""), false
	}
	pol := tools.PolicyOf(ctx).Workspace
	need, granted := "read", pol.Read
	if write {
		need, granted = "write", pol.Write
	}
	if !granted {
		return nil, errPermission(fmt.Sprintf("%s: this agent's workspace grant does not allow %s", r.raw, need),
			"Ask an operator to add "+need+" to the agent's workspace.ops."), false
	}
	if m == nil {
		return nil, errBusiness(r.raw+": this runtime serves no workspaces (workspace.mode is off)", ""), false
	}
	ident := tools.RunIdentity(ctx)
	o := lifecycle.Owner{TenantID: ident.TenantID, UserID: ident.UserID, RunID: tools.RunID(ctx), AgentID: ident.AgentID, RootRunID: ident.RootRunID}
	// A workspace another run shared with this one (RFC GW P8): the share
	// must cover the use, and the use is recorded once here; every op the
	// call then makes is checked again in the manager.
	if _, _, shared := lifecycle.SplitShared(r.workspace); shared {
		op := wsHookRead
		if write {
			op = wsHookWrite
		}
		if err := m.UseShared(ctx, o, r.workspace, write, op, via); err != nil {
			return nil, workspaceRefError(err, r.workspace), false
		}
	}
	return &workspaceLoc{m: m, o: o, name: r.workspace, path: wsClean(r.path), via: via}, tools.Result{}, true
}

// hook runs the run's Workspace pre-hooks on the call this reference stands
// for. The data is not in it: a hook sees where, not what.
func (l *workspaceLoc) hook(ctx context.Context, op string) (tools.Result, bool) {
	in, _ := json.Marshal(map[string]any{"op": op, "name": l.name, "path": l.path, "via": l.via})
	if res, ok := tools.HookPreCheck(ctx, "Workspace", in); !ok {
		return classified(fmt.Sprintf("%s: refused by a Workspace hook: %s", l.Ref(), res.Text), tools.CategoryPermission, false,
			"Ask the operator about the hook."), false
	}
	return tools.Result{}, true
}

func (l *workspaceLoc) Ref() string { return wsRef(l.name, l.path) }

// Judge runs the Workspace hooks on one entry under the place; the grant
// was checked when the place was resolved and covers every path in it.
func (l *workspaceLoc) Judge(ctx context.Context, rel string, write bool) (tools.Result, bool) {
	op := wsHookRead
	if write {
		op = wsHookWrite
	}
	return l.child(rel).hook(ctx, op)
}

func (l *workspaceLoc) Close() {}

func (l *workspaceLoc) child(name string) *workspaceLoc {
	c := *l
	c.path = wsClean(path.Join(l.path, name))
	return &c
}

func (l *workspaceLoc) Stat(ctx context.Context) (locEntry, tools.Result, bool) {
	info, err := l.m.Stat(ctx, l.o, l.name, workspace.FStatArgs{Path: l.path})
	if dispatch.ClassOf(err) == workspace.ClassPathNotFound {
		return locEntry{Path: l.path}, tools.Result{}, true
	}
	if err != nil {
		return locEntry{}, workspaceRefError(err, l.name), false
	}
	return locEntry{Path: l.path, Type: info.Type, Size: info.Size}, tools.Result{}, true
}

// Walk lists through fls, which counts folders against its limit too: a
// listing it cut short is reported as truncated, so a copy refuses it
// rather than copying part of the folder.
func (l *workspaceLoc) Walk(ctx context.Context, limit int) ([]locEntry, bool, tools.Result, bool) {
	ans, err := l.m.ListFiles(ctx, l.o, l.name, workspace.FListArgs{Path: l.path, Recursive: true, Limit: workspace.MaxListLimit})
	if err != nil {
		return nil, false, workspaceRefError(err, l.name), false
	}
	prefix := l.path + "/"
	var out []locEntry
	files := 0
	for _, e := range ans.Entries {
		if e.Type == workspace.EntryDir {
			continue
		}
		rel := e.Path
		if l.path != "." {
			rel = strings.TrimPrefix(rel, prefix)
		}
		if e.Type == workspace.EntryFile {
			if files >= limit {
				return out, true, tools.Result{}, true
			}
			files++
		}
		out = append(out, locEntry{Path: rel, Type: e.Type, Size: e.Size})
	}
	return out, ans.Truncated, tools.Result{}, true
}

// wsReader is a file streamed out of a workspace: the get runs in its own
// goroutine writing into a pipe, so the bytes pass through the core a chunk
// at a time. Close stops the get and waits for it.
type wsReader struct {
	pr     *io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
}

func (r *wsReader) Read(p []byte) (int, error) { return r.pr.Read(p) }

func (r *wsReader) Close() error {
	r.cancel()
	_ = r.pr.Close()
	<-r.done
	return nil
}

func (l *workspaceLoc) Open(ctx context.Context, rel string) (io.ReadCloser, tools.Result, bool) {
	full := wsClean(path.Join(l.path, rel))
	gctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	done := make(chan struct{})
	maxBytes := inbox.MaxFileBytes(ctx)
	safego.Go(gctx, "file.workspace_get", func() {
		defer close(done)
		// Failure until the get says otherwise: a panic recovered by
		// safego must still end the reader, or the copy waits forever.
		err := errors.New("the workspace read stopped")
		defer func() { pw.CloseWithError(err) }()
		_, err = l.m.GetFile(gctx, l.o, l.name, workspace.GetArgs{Path: full}, pw, maxBytes)
	})
	return &wsReader{pr: pr, cancel: cancel, done: done}, tools.Result{}, true
}

func (l *workspaceLoc) Create(ctx context.Context, size int64, r io.Reader, overwrite bool) (locWritten, tools.Result, bool) {
	ans, err := l.m.PutFile(ctx, l.o, l.name, workspace.PutArgs{Path: l.path, Size: size, Overwrite: overwrite}, r)
	if err != nil {
		return locWritten{}, copyReadError(err), false
	}
	return locWritten{Ref: l.Ref(), Size: ans.Size, SHA256: ans.SHA256}, tools.Result{}, true
}

// CreateFolder builds the folder in a staging folder of the workspace and
// commits it in one rename; on any failure the stage is discarded, so
// nothing of the copy is left, a quota refusal included.
func (l *workspaceLoc) CreateFolder(ctx context.Context, src fileLoc, files []locEntry, overwrite bool) (tools.Result, bool) {
	stage, err := l.m.Stage(ctx, l.o, l.name)
	if err != nil {
		return workspaceRefError(err, l.name), false
	}
	committed := false
	defer func() {
		if !committed {
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = l.m.Discard(dctx, l.o, l.name, stage)
		}
	}()
	for _, f := range files {
		rc, res, ok := src.Open(ctx, f.Path)
		if !ok {
			return res, false
		}
		_, err := l.m.PutFile(ctx, l.o, l.name, workspace.PutArgs{Path: f.Path, Size: f.Size, Stage: stage}, rc)
		rc.Close()
		if err != nil {
			return copyReadError(err), false
		}
	}
	if _, err := l.m.Commit(ctx, l.o, l.name, workspace.CommitArgs{Stage: stage, Path: l.path, Overwrite: overwrite}); err != nil {
		return workspaceRefError(err, l.name), false
	}
	committed = true
	return tools.Result{}, true
}

func (l *workspaceLoc) Remove(ctx context.Context) (locRemoved, tools.Result, bool) {
	if l.path == "." {
		return locRemoved{}, errValidation(l.Ref()+" is the workspace's root, which is not removed here",
			"Empty it with op=clear, or release the workspace with Workspace op=release."), false
	}
	ans, err := l.m.RemoveFile(ctx, l.o, l.name, l.path)
	if err != nil {
		return locRemoved{}, workspaceRefError(err, l.name), false
	}
	return locRemoved{Files: ans.Files, Bytes: ans.Bytes}, tools.Result{}, true
}

// workspaceFileError explains a workspace's refusal as a File result, its
// class leading the text. ok is false for an error that is not a
// workspace's at all.
func workspaceFileError(err error) (tools.Result, bool) {
	switch {
	case errors.Is(err, lifecycle.ErrNotReady):
		return errBusiness(err.Error(), "Wait until the workspace is ready; Workspace op=status shows it."), true
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errTransient("the call was cancelled before the worker answered", ""), true
	}
	var oe *dispatch.OpError
	if !errors.As(err, &oe) {
		return tools.Result{}, false
	}
	msg := oe.Class
	if oe.Detail != "" {
		msg += ": " + oe.Detail
	}
	switch oe.Class {
	case workspace.ClassPathNotFound:
		return errNotFound(msg, "List the folder with op=ls."), true
	case workspace.ClassExists:
		return errBusiness(msg, "Pass overwrite: true to replace it, or choose another name."), true
	case workspace.ClassOverQuota:
		return errBusiness(msg, "Remove files from the workspace with File op=rm or op=clear, or copy less."), true
	case workspace.ClassCopyLimit, workspace.ClassInvalid, workspace.ClassNotRegular, workspace.ClassOutputTooLarge:
		return errValidation(msg, ""), true
	case workspace.ClassNotFound:
		return errBusiness("lost: the workspace's directory is no longer on its worker", ""), true
	}
	return errTransient(msg, "Try again."), true
}

// workspaceRefError is workspaceFileError with the run's own errors: a
// workspace this run does not hold is not found, and a store fault says
// only that it failed (its text may name tables or hosts).
func workspaceRefError(err error, name string) tools.Result {
	if errors.Is(err, lifecycle.ErrNotFound) {
		if _, _, shared := lifecycle.SplitShared(name); shared {
			return errNotFound(fmt.Sprintf("no workspace %q is shared with this run", name), "Workspace op=list shows the workspaces shared with you.")
		}
		return errNotFound(fmt.Sprintf("this run has no workspace named %q", name), "Open it with Workspace op=open; Workspace op=list shows yours.")
	}
	if errors.Is(err, lifecycle.ErrShareReadOnly) {
		return errPermission(fmt.Sprintf("%s: %v", name, err), "Copy what you need out of it, or ask its owner for a read_write share.")
	}
	if res, ok := workspaceFileError(err); ok {
		return res
	}
	return errTransient("the runtime could not complete the op", "Try again.")
}

// ---- stat, ls, rm, clear on a workspace ----

func (f *File) statWorkspace(ctx context.Context, r dataRef) (tools.Result, error) {
	l, res, ok := f.workspaceLoc(ctx, r, false, FileToolName)
	if !ok {
		return res, nil
	}
	if res, ok := l.hook(ctx, wsHookRead); !ok {
		return res, nil
	}
	info, err := l.m.Stat(ctx, l.o, l.name, workspace.FStatArgs{Path: l.path, SHA256: true})
	if err != nil {
		return prefixed("stat", workspaceRefError(err, l.name)), nil
	}
	out := map[string]any{"ref": l.Ref(), "path": info.Path, "type": info.Type, "modified": info.MTime.UTC().Format(time.RFC3339)}
	if info.Type == workspace.EntryFile {
		out["size"], out["sha256"] = info.Size, info.SHA256
		// The type is sniffed from the file's first bytes, as the inbox's is.
		var head bytes.Buffer
		if _, err := l.m.GetFile(ctx, l.o, l.name, workspace.GetArgs{Path: l.path, Length: 512}, &head, 0); err == nil {
			out["media_type"] = inbox.Sniff("", head.Bytes(), path.Base(l.path))
		}
	}
	return okJSON(out)
}

func (f *File) lsWorkspace(ctx context.Context, r dataRef, recursive bool) (tools.Result, error) {
	l, res, ok := f.workspaceLoc(ctx, r, false, FileToolName)
	if !ok {
		return res, nil
	}
	if res, ok := l.hook(ctx, wsHookRead); !ok {
		return res, nil
	}
	ans, err := l.m.ListFiles(ctx, l.o, l.name, workspace.FListArgs{Path: l.path, Recursive: recursive, Limit: fileMaxListed})
	if err != nil {
		return prefixed("ls", workspaceRefError(err, l.name)), nil
	}
	entries := make([]fileEntry, 0, len(ans.Entries))
	for _, e := range ans.Entries {
		fe := fileEntry{Ref: wsRef(l.name, e.Path), Dir: e.Type == workspace.EntryDir}
		switch e.Type {
		case workspace.EntryFile:
			fe.Size = e.Size
		case workspace.EntryLink, workspace.EntryOther:
			fe.Type = e.Type
		}
		entries = append(entries, fe)
	}
	return okJSON(map[string]any{"path": l.Ref(), "entries": entries, "truncated": ans.Truncated})
}

func (f *File) rmWorkspace(ctx context.Context, r dataRef) (tools.Result, error) {
	l, res, ok := f.workspaceLoc(ctx, r, true, FileToolName)
	if !ok {
		return res, nil
	}
	if l.path == "." {
		_, res, _ := l.Remove(ctx)
		return prefixed("rm", res), nil
	}
	if res, ok := l.hook(ctx, wsHookRemove); !ok {
		return res, nil
	}
	rem, res, ok := l.Remove(ctx)
	if !ok {
		return prefixed("rm", res), nil
	}
	return okJSON(map[string]any{"removed": l.Ref(), "files": rem.Files, "bytes": rem.Bytes})
}

// clearWorkspace empties a folder of a workspace (its root included) and
// keeps the folder. It removes what one listing shows, and lists again
// while entries remain, up to a bound: a command writing into the folder
// meanwhile cannot keep it busy forever.
func (f *File) clearWorkspace(ctx context.Context, r dataRef) (tools.Result, error) {
	l, res, ok := f.workspaceLoc(ctx, r, true, FileToolName)
	if !ok {
		return res, nil
	}
	if res, ok := l.hook(ctx, wsHookRemove); !ok {
		return res, nil
	}
	e, res, ok := l.Stat(ctx)
	if !ok {
		return prefixed("clear", res), nil
	}
	if e.Type != workspace.EntryDir {
		return errValidation(fmt.Sprintf("clear: %s is not a folder", l.Ref()), "Remove a file with op=rm."), nil
	}
	var files int
	var bytes int64
	for range 100 {
		ans, err := l.m.ListFiles(ctx, l.o, l.name, workspace.FListArgs{Path: l.path, Limit: workspace.MaxListLimit})
		if err != nil {
			return prefixed("clear", workspaceRefError(err, l.name)), nil
		}
		if len(ans.Entries) == 0 {
			return okJSON(map[string]any{"cleared": l.Ref(), "files": files, "bytes": bytes})
		}
		for _, ent := range ans.Entries {
			rem, err := l.m.RemoveFile(ctx, l.o, l.name, ent.Path)
			if err != nil && dispatch.ClassOf(err) != workspace.ClassPathNotFound {
				return prefixed("clear", workspaceRefError(err, l.name)), nil
			}
			files += rem.Files
			bytes += rem.Bytes
		}
	}
	return errTransient(fmt.Sprintf("clear: %s kept filling while it was emptied", l.Ref()), "Try again when nothing writes into it."), nil
}

// prefixed puts the op's name before a refusal's text.
func prefixed(op string, res tools.Result) tools.Result {
	res.Text = op + ": " + res.Text
	return res
}
