package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/files/filenames"
	"github.com/denn-gubsky/loomcycle/internal/files/unpack"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// File op=unpack of an archive in a workspace (RFC GW D34). The archive is
// walked in its worker's conversion child, under the same bounds as an
// inbox archive's; the core never holds it. Into a workspace on the same
// worker, the files are written into a stage there and never cross; into
// the inbox, a volume or a workspace on another worker, the child's tar
// streams through the core, an entry at a time, and is written as an inbox
// archive's files are.
//
// Into a workspace an archive is unpacked whole or not at all: a walk that
// stopped at a bound writes nothing, and every file's final path is judged
// by the Workspace hooks before the stage is committed, one refusal
// refusing the unpack. Into the inbox or a volume, what was written before
// a bound stays and a hook skips one file, as for an inbox archive.

// unpackWorkspace unpacks the archive r, in a workspace, where in.To says.
func (f *File) unpackWorkspace(ctx context.Context, in fileInput, r dataRef, lim unpack.Limits) (tools.Result, error) {
	l, info, res, ok := f.wsConvertSource(ctx, "unpack", r)
	if !ok {
		return res, nil
	}
	perFile := inbox.MaxFileBytes(ctx)
	args := &workspace.ConvertUnpackArgs{
		ConvertSource: workspace.ConvertSource{Path: l.path, SHA256: info.SHA256, MaxBytes: perFile},
		All:           in.All, DryRun: in.DryRun, MaxListed: lim.Listed, MaxWritten: lim.Written, MaxTotal: lim.Total,
		Budget: lim.Total, PerFile: perFile,
	}
	to := strings.TrimSpace(in.To)
	switch {
	case strings.HasPrefix(to, "workspace:"):
		return f.unpackIntoWorkspace(ctx, in, r, l, info, args)
	case to != "":
		vd, res, ok := f.unpackVolumeDest(ctx, to)
		if !ok {
			return res, nil
		}
		defer vd.vfs.Close()
		return unpackStreamed(ctx, r, l, info, args, vd, vd.prefix+vd.base)
	}
	if !inbox.Available(ctx) {
		return errBusiness("unpack: this run cannot keep files, so there is nowhere to unpack to",
			"An operator must configure a dynamic_root volume for the run's inbox, or pass to: a read-write volume or a workspace folder of yours."), nil
	}
	dir := path.Join(unpackDir, unpackSlug(l.path))
	// min is not used: a test file of this package declares its own.
	if rest := inbox.Remaining(ctx); rest < args.Budget {
		args.Budget = rest
	}
	return unpackStreamed(ctx, r, l, info, args, &inboxDest{ctx: ctx, archive: r.raw, dir: dir}, inbox.Ref(dir)+"/")
}

// unpackMaxOutput bounds what an unpack may stream back: the bytes it may
// inflate, a header per file and the answer.
func unpackMaxOutput(args *workspace.ConvertUnpackArgs) int64 {
	return max(args.Budget, 0) + int64(args.MaxWritten+2)*(16<<10) + 16<<20
}

// unpackWrite keeps one file of a streamed unpack.
type unpackWrite func(e unpack.Entry, head []byte, body io.Reader) (unpack.Written, string, error)

// destWrite is an unpack.Dest's write as a streamed unpack's.
func destWrite(d unpack.Dest) unpackWrite {
	return func(e unpack.Entry, head []byte, body io.Reader) (unpack.Written, string, error) {
		return d.Write(e.Segs, body, head, e.Display)
	}
}

// unpackStreamed runs the unpack with its tar streamed back and writes each
// file through dest, as an inbox archive's files are written.
func unpackStreamed(ctx context.Context, r dataRef, l *workspaceLoc, info workspace.FileInfo, args *workspace.ConvertUnpackArgs, dest unpack.Dest, to string) (tools.Result, error) {
	kind, res, fail, err := wsUnpackRun(ctx, r, l, info, args, dest.Planned, destWrite(dest))
	if err != nil || fail.IsError {
		return fail, err
	}
	return okJSON(res.Answer(r.raw, kind, to))
}

// wsUnpackRun runs the unpack on the source's worker with its tar streamed
// back, an entry at a time, each file kept by write. A write that fails
// stops it there, with what was written before it in the result. fail is
// the unpack refused; err the call's own context ending.
func wsUnpackRun(ctx context.Context, r dataRef, l *workspaceLoc, info workspace.FileInfo, args *workspace.ConvertUnpackArgs, planned func([]string) string, write unpackWrite) (string, *unpack.Result, tools.Result, error) {
	rp := &unpack.Replayer{PerFile: args.PerFile, MaxWritten: args.MaxWritten, Planned: planned, Write: write}
	req := lifecycle.ConvertRequest{Op: workspace.OpConvertUnpack, Args: args, Source: l.name, SHA256: info.SHA256, MaxOutput: unpackMaxOutput(args)}
	err := wsStream(ctx, l, req, rp.Entry)
	if cerr := ctx.Err(); cerr != nil {
		return "", nil, tools.Result{}, cerr
	}
	// A write that failed ended the stream on purpose; the files written
	// before it stay, as for an inbox archive.
	if err != nil && !rp.Stopped() {
		return "", nil, unpackWorkspaceError(r, err, l), nil
	}
	kind, res, err := rp.Result()
	if err != nil {
		return "", nil, errTransient("unpack: "+err.Error(), "Try again."), nil
	}
	return kind, res, tools.Result{}, nil
}

// unpackWorkspaceError explains an unpack's failure on a workspace's
// worker: an archive refused whole as an inbox archive's refusal reads.
func unpackWorkspaceError(r dataRef, err error, l *workspaceLoc) tools.Result {
	var oe *dispatch.OpError
	if errors.As(err, &oe) && oe.Class == workspace.ClassUnpackRefused {
		hint := unpackRefusedHint
		if strings.HasPrefix(oe.Detail, "is not a ZIP or RAR archive") {
			hint = unpackNotArchiveHint
		}
		return errValidation(fmt.Sprintf("unpack: %s %s", r.raw, oe.Detail), hint)
	}
	if errors.As(err, &oe) || errors.Is(err, lifecycle.ErrQuarantined) || errors.Is(err, lifecycle.ErrNotFound) || errors.Is(err, lifecycle.ErrNotReady) {
		return wsConvertError("unpack", r.raw, err, l)
	}
	return errTransient("unpack: "+err.Error(), "Try again.")
}

// unpackIntoWorkspace unpacks into a folder of a workspace of the run: in a
// stage on the target's worker, judged file by file, then committed at
// once.
func (f *File) unpackIntoWorkspace(ctx context.Context, in fileInput, r dataRef, l *workspaceLoc, info workspace.FileInfo, args *workspace.ConvertUnpackArgs) (tools.Result, error) {
	to, res, ok := f.wsConvertTarget(ctx, "unpack", in.To)
	if !ok {
		return res, nil
	}
	if args.DryRun {
		return unpackStreamed(ctx, r, l, info, args, plannedOnly{to}, to.Ref()+"/")
	}
	same, err := l.m.SameWorker(ctx, l.o, l.name, to.name)
	if err != nil {
		return prefixed("unpack", workspaceRefError(err, to.name)), nil
	}
	if !same {
		// Streamed through the core into a stage of the target's worker.
		sd := &wsStageDest{ctx: ctx, l: to, taken: map[string]bool{}}
		kind, ures, fail, err := wsUnpackRun(ctx, r, l, info, args, func(segs []string) string { return path.Join(segs...) }, sd.write)
		switch {
		case err != nil || fail.IsError:
			sd.discard(ctx)
			return fail, err
		case sd.err != nil:
			// Over the target's quota, or a path its stage cannot hold.
			sd.discard(ctx)
			return prefixed("unpack", workspaceRefError(sd.err, to.name)), nil
		}
		return commitUnpack(ctx, r, to, sd.stage, kind, ures, args.MaxWritten, in.Overwrite)
	}
	args.To = workspace.ConvertTarget{Path: to.path}
	var ans workspace.ConvertUnpackAnswer
	if err := l.m.Convert(ctx, l.o, lifecycle.ConvertRequest{Op: workspace.OpConvertUnpack, Args: args, Source: l.name, SHA256: info.SHA256, To: to.name}, nil, &ans); err != nil {
		if dispatch.ClassOf(err) == workspace.ClassOverQuota {
			// The target's refusal, not the archive's.
			return prefixed("unpack", workspaceRefError(err, to.name)), nil
		}
		return unpackWorkspaceError(r, err, l), nil
	}
	ures := &unpack.Result{}
	if err := json.Unmarshal(ans.Result, ures); err != nil {
		if ans.Stage != "" {
			discardStage(ctx, to, ans.Stage)
		}
		return errTransient("unpack: the worker's answer is not JSON", "Try again."), nil
	}
	return commitUnpack(ctx, r, to, ans.Stage, ans.Format, ures, args.MaxWritten, in.Overwrite)
}

// discardStage drops a stage of an unpack that is not committed, past the
// call's own end.
func discardStage(ctx context.Context, to *workspaceLoc, stage string) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = to.m.Discard(dctx, to.o, to.name, stage)
}

// commitUnpack commits an unpack's stage at the target folder once every
// file's final path passed the Workspace hooks; a walk that stopped at a
// bound, or one refused path, leaves nothing.
func commitUnpack(ctx context.Context, r dataRef, to *workspaceLoc, stage, kind string, res *unpack.Result, maxWritten int, overwrite bool) (tools.Result, error) {
	committed := false
	defer func() {
		if !committed && stage != "" {
			discardStage(ctx, to, stage)
		}
	}()
	if res.Stopped != "" {
		return errValidation(fmt.Sprintf("unpack: %s: %s; nothing was written to %s, as an archive goes into a workspace whole or not at all", r.raw, res.Stopped, to.Ref()),
			"Unpack it into the inbox or a volume, where what was written before the bound stays, or unpack fewer files (without all: true)."), nil
	}
	if stage == "" || len(res.Written) == 0 {
		return errValidation(fmt.Sprintf("unpack: %s holds no file to unpack into %s", r.raw, to.Ref()), "Pass all: true to unpack every file; op=unpack with dry_run lists what is skipped and why."), nil
	}
	if len(res.Written) > maxWritten {
		return errTransient("unpack: the worker staged more files than the unpack allows", "Try again."), nil
	}
	for i, w := range res.Written {
		segs, reason := unpack.Path(w.Ref)
		if reason != "" || path.Join(segs...) != w.Ref {
			return errTransient(fmt.Sprintf("unpack: the worker staged a path %q an unpack never makes", w.Ref), "Try again."), nil
		}
		if hr, ok := to.child(w.Ref).hook(ctx, wsHookWrite); !ok {
			return prefixed("unpack", hr), nil
		}
		res.Written[i].Ref = wsRef(to.name, path.Join(to.path, w.Ref))
		res.Written[i].Name = unpack.DisplayName(w.Name)
	}
	if _, err := to.m.Commit(ctx, to.o, to.name, workspace.CommitArgs{Stage: stage, Path: to.path, Overwrite: overwrite}); err != nil {
		return prefixed("unpack", workspaceRefError(err, to.name)), nil
	}
	committed = true
	return okJSON(res.Answer(r.raw, kind, to.Ref()+"/"))
}

// plannedOnly is the target of a dry run into a workspace: it names, and
// never writes.
type plannedOnly struct{ l *workspaceLoc }

func (p plannedOnly) Planned(segs []string) string {
	return wsRef(p.l.name, path.Join(p.l.path, path.Join(segs...)))
}

func (p plannedOnly) Write([]string, io.Reader, []byte, string) (unpack.Written, string, error) {
	return unpack.Written{}, "", errors.New("a dry run writes nothing")
}

// wsStageDest writes a streamed unpack into a stage of a workspace on
// another worker, each file a put the target's quota counts. err is the
// first write's failure; commitUnpack commits the stage.
type wsStageDest struct {
	ctx   context.Context
	l     *workspaceLoc
	stage string
	taken map[string]bool
	err   error
}

func (d *wsStageDest) write(e unpack.Entry, head []byte, body io.Reader) (unpack.Written, string, error) {
	if d.stage == "" {
		st, err := d.l.m.Stage(d.ctx, d.l.o, d.l.name)
		if err != nil {
			d.err = err
			return unpack.Written{}, "", err
		}
		d.stage = st
	}
	rel := unusedUnpackPath(d.taken, path.Join(e.Segs...))
	if _, err := d.l.m.PutFile(d.ctx, d.l.o, d.l.name, workspace.PutArgs{Path: rel, Size: e.Size, Stage: d.stage}, io.MultiReader(bytes.NewReader(head), body)); err != nil {
		d.err = err
		return unpack.Written{}, "", err
	}
	return unpack.Written{Ref: rel, Name: e.Display, Size: e.Size, MediaType: filenames.Sniff("", head, path.Base(rel))}, "", nil
}

func (d *wsStageDest) discard(ctx context.Context) {
	if d.stage != "" {
		discardStage(ctx, d.l, d.stage)
		d.stage = ""
	}
}

// unusedUnpackPath is rel, or rel with -2, -3, ... before its extension
// when an earlier file of the archive took it, as the worker names one.
func unusedUnpackPath(taken map[string]bool, rel string) string {
	out := rel
	dir, name := path.Split(rel)
	ext := filenames.FileExt(name)
	for i := 2; taken[out]; i++ {
		out = dir + strings.TrimSuffix(name, ext) + "-" + strconv.Itoa(i) + ext
	}
	taken[out] = true
	return out
}
