package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
)

// maxExecTimeout bounds the timeout an exec op may carry. The core decides
// the timeout from the operator's workspace.exec bounds; this only keeps a
// malformed op from holding a command for ever.
const maxExecTimeout = 24 * time.Hour

// inWorkspace runs a command or a file op in the workspace whose directory is
// dir. The file ops are the file tools' own code (fsops) with the workspace
// as the root and no fence: the workspace is the whole of what the op may
// reach, and fsops refuses `..`, an absolute path elsewhere and a symlink out
// of it.
func (w *worker) inWorkspace(ctx context.Context, dir, op string, args, input []byte) answer {
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() {
		return refuse(workspace.ClassNotFound, "the workspace is not on this worker")
	}
	// fsops resolves only the root; the workspace's real path is derived
	// from the worker's root and the id, and used to keep the worker's own
	// disk layout out of what the op returns.
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return refuse(workspace.ClassFailed, "reading the workspace: %v", err)
	}
	raw := args
	if len(raw) == 0 {
		raw = input
	}
	var a answer
	switch op {
	case workspace.OpExec:
		a = w.execInQuota(ctx, filepath.Base(dir), real, raw)
	case workspace.OpRead:
		a = readOp(ctx, real, raw)
	case workspace.OpWrite:
		var in workspace.WriteArgs
		if a = decodeArgs(raw, &in); a.class == "" {
			a = w.inQuota(ctx, filepath.Base(dir), real, in.Path, func(*fsops.VolumeFS) int64 { return int64(len(in.Content)) }, func() answer {
				return textAnswer(fsops.Write(ctx, real, "", in.Path, in.Content, 0))
			})
		}
	case workspace.OpEdit:
		var in workspace.EditArgs
		if a = decodeArgs(raw, &in); a.class == "" {
			a = w.inQuota(ctx, filepath.Base(dir), real, in.Path, func(v *fsops.VolumeFS) int64 { return editGrowth(v, real, in) }, func() answer {
				return textAnswer(fsops.Edit(ctx, real, "", fsops.EditRequest{Path: in.Path, OldString: in.OldString, NewString: in.NewString, ReplaceAll: in.ReplaceAll}))
			})
		}
	case workspace.OpGlob:
		var in workspace.GlobArgs
		if a = decodeArgs(raw, &in); a.class == "" {
			a = countedAnswer(fsops.Glob(real, "", fsops.GlobRequest{Pattern: in.Pattern, Path: in.Path}))
		}
	case workspace.OpGrep:
		var in workspace.GrepArgs
		if a = decodeArgs(raw, &in); a.class == "" {
			a = countedAnswer(fsops.Grep(real, "", fsops.GrepRequest{
				Pattern: in.Pattern, Path: in.Path, Glob: in.Glob, OutputMode: in.OutputMode,
				CaseInsensitive: in.CaseInsensitive, Multiline: in.Multiline, HeadLimit: in.HeadLimit,
				Before: in.Before, After: in.After, Context: in.Context,
			}))
		}
	default:
		return refuse(workspace.ClassUnsupportedOp, "this worker does not run %q", op)
	}
	// Only text the runtime wrote is rewritten: a refusal's detail, and the
	// result line of a write or an edit. A file's own text (read, grep) is
	// returned as it is, even where it holds the path.
	if op != workspace.OpExec {
		a = hidePaths(a, op == workspace.OpWrite || op == workspace.OpEdit, real, dir)
	}
	return a
}

// decodeArgs decodes an op's arguments strictly: an argument the op does not
// take is refused, not dropped, as the Read tool refuses one.
func decodeArgs(raw []byte, into any) answer {
	if len(raw) == 0 {
		return refuse(workspace.ClassInvalid, "the op has no arguments")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return refuse(workspace.ClassInvalid, "arguments: %v", err)
	}
	return answer{}
}

func readOp(ctx context.Context, root string, raw []byte) answer {
	var in workspace.ReadArgs
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	if in.Offset != nil && *in.Offset < 1 {
		return refuse(workspace.ClassInvalid, "offset must be 1 or more (lines are counted from 1)")
	}
	if in.Limit != nil && *in.Limit < 1 {
		return refuse(workspace.ClassInvalid, "limit must be 1 or more")
	}
	req := fsops.ReadRequest{Path: in.Path}
	if in.Offset != nil || in.Limit != nil || in.LineNumbers {
		win := fsops.LineWindow{Offset: 1, Numbers: in.LineNumbers}
		if in.Offset != nil {
			win.Offset = *in.Offset
		}
		if in.Limit != nil {
			win.Limit = *in.Limit
		}
		req.Lines = &win
	}
	text, err := fsops.Read(ctx, root, "", req)
	if errors.Is(err, fsops.ErrBinaryLineRange) {
		return refuse(workspace.ClassInvalid, "this looks like a binary file; a line range reads text only: read it without offset, limit and line_numbers")
	}
	return textAnswer(text, err)
}

func textAnswer(text string, err error) answer {
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	out, _ := json.Marshal(workspace.FileAnswer{Text: text})
	return answer{output: out}
}

func countedAnswer(text string, n int, err error) answer {
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	out, _ := json.Marshal(workspace.FileAnswer{Text: text, Count: &n})
	return answer{output: out}
}

// hidePaths rewrites the workspace's absolute path out of an answer's detail,
// and out of its output text when inOutput, so a result reads relative to
// the workspace and does not tell the agent where the worker keeps it. It is
// presentation, not confinement: a command can still print its directory.
func hidePaths(a answer, inOutput bool, dirs ...string) answer {
	strip := func(s string) string {
		for _, d := range dirs {
			s = strings.ReplaceAll(s, d+string(filepath.Separator), "")
			s = strings.ReplaceAll(s, d, ".")
		}
		return s
	}
	a.detail = strip(a.detail)
	if inOutput && len(a.output) > 0 {
		var fa workspace.FileAnswer
		if json.Unmarshal(a.output, &fa) == nil {
			fa.Text = strip(fa.Text)
			a.output, _ = json.Marshal(fa)
		}
	}
	return a
}

func (w *worker) exec(ctx context.Context, root string, raw []byte) answer {
	var in workspace.ExecArgs
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	if in.TimeoutMS <= 0 || time.Duration(in.TimeoutMS)*time.Millisecond > maxExecTimeout {
		return refuse(workspace.ClassInvalid, "timeout_ms must be from 1 to %d", maxExecTimeout.Milliseconds())
	}
	dir := root
	if in.Cwd != "" {
		d, err := fsops.ResolveInsideRoot(root, in.Cwd)
		if err != nil {
			return hidePaths(refuse(workspace.ClassInvalid, "cwd: %v", err), false, root)
		}
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			return refuse(workspace.ClassInvalid, "cwd: %q is not a directory in the workspace", in.Cwd)
		}
		dir = d
	}
	res, err := w.engine.Exec(ctx, engine.ExecRequest{Dir: dir, Command: in.Command, Timeout: time.Duration(in.TimeoutMS) * time.Millisecond})
	switch {
	case errors.Is(err, engine.ErrRefused):
		return refuse(workspace.ClassRefused, "%v", err)
	case err != nil:
		return refuse(workspace.ClassFailed, "%v", err)
	}
	out, _ := json.Marshal(workspace.ExecAnswer{ExitCode: res.ExitCode, Output: strings.ToValidUTF8(string(res.Output), string(utf8.RuneError)),
		Truncated: res.Truncated, TimedOut: res.TimedOut})
	return answer{output: out, exit: int32(res.ExitCode)}
}

// execInQuota runs a command unless the workspace is marked full, then
// measures the workspace again (RFC GW P2, ruling Q2): a command may write
// past the quota while it runs, and the next write or command is refused
// until a measurement finds it back under.
func (w *worker) execInQuota(ctx context.Context, id, real string, raw []byte) answer {
	v, err := fsops.OpenVolumeFS(real, "")
	if err != nil {
		return refuse(workspace.ClassFailed, "opening the workspace: %v", err)
	}
	defer v.Close()
	u, release, err := w.writeLock(ctx, id, v)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	full := u.full.Load()
	snap := u.snapshot()
	release()
	if full {
		about := ""
		if snap.Estimate {
			about = " (at least: the count is an estimate)"
		}
		return refuse(workspace.ClassOverQuota, "the workspace holds %d bytes%s, over its quota of %d; remove files before running another command", snap.UsedBytes, about, snap.QuotaBytes)
	}
	a := w.exec(ctx, real, raw)
	// Measured even when the core stopped waiting: the command may have
	// written before it was killed.
	mctx := context.WithoutCancel(ctx)
	if u, release, err := w.writeLock(mctx, id, v); err == nil {
		w.measureLocked(mctx, u, v)
		release()
	}
	return a
}

// inQuota runs a write or an edit of the Workspace tool under the
// workspace's write lock (ruling Q2: they are writes): refused with
// workspace_over_quota when the workspace is marked full or when need bytes
// more would pass its quota, nothing written; after it, the count moves by
// the file's change in size.
func (w *worker) inQuota(ctx context.Context, id, real, path string, need func(*fsops.VolumeFS) int64, do func() answer) answer {
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
	if bad := u.admit(max(need(v), 0)); bad != nil {
		return *bad
	}
	before := fileSize(v, real, path)
	a := do()
	if a.class == "" {
		u.used = max(u.used+fileSize(v, real, path)-before, 0)
		w.settleLocked(ctx, u, v)
	}
	return a
}

// fileSize is the size of the regular file path names in the workspace, 0
// when there is none; for the count only, so a path the op will refuse
// counts 0.
func fileSize(v *fsops.VolumeFS, real, path string) int64 {
	resolved, err := fsops.ResolveInsideRoot(real, path)
	if err != nil {
		return 0
	}
	rel, err := v.Rel(resolved)
	if err != nil {
		return 0
	}
	f, err := v.Open(rel)
	if err != nil {
		return 0
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return 0
	}
	return st.Size()
}

// editGrowth is how many bytes an edit adds to its file: the replacements
// it would make times the growth of each. An edit that will fail (no file,
// no match, over the edit cap) adds nothing; fsops.Edit refuses it.
func editGrowth(v *fsops.VolumeFS, real string, in workspace.EditArgs) int64 {
	if fileSize(v, real, in.Path) > fsops.DefaultEditMaxBytes {
		return 0
	}
	resolved, err := fsops.ResolveInsideRoot(real, in.Path)
	if err != nil {
		return 0
	}
	rel, err := v.Rel(resolved)
	if err != nil {
		return 0
	}
	f, err := v.Open(rel)
	if err != nil {
		return 0
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, fsops.DefaultEditMaxBytes))
	if err != nil {
		return 0
	}
	_, n, err := fsops.Replace(string(body), in.OldString, in.NewString, in.ReplaceAll)
	if err != nil {
		return 0
	}
	return int64(n) * int64(len(in.NewString)-len(in.OldString))
}
