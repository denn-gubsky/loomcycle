package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// Workspace is the agent's way into workspaces (RFC GW): directories on a
// workspace worker, away from the runtime, where it runs commands and works
// on files. A workspace belongs to the run that opened it (the pin) and is
// removed when that run ends.
//
// Every op checks, in this order: the run's workspace grant, then the pin
// (the workspace is the calling run's own: anything else is "not found",
// never a hint that it exists), then the worker. The checks live here and in
// lifecycle; the worker trusts the core.
type Workspace struct {
	// Manager is nil while workspace.mode is off: every op is refused.
	Manager *lifecycle.Manager
	// ExecDefault and ExecMax are workspace.exec.default_timeout_s and
	// max_timeout_s; zero is config's default.
	ExecDefault, ExecMax time.Duration
}

func (w *Workspace) Name() string { return "Workspace" }

func (w *Workspace) Description() string {
	return "Open a workspace — a directory on a workspace worker, away from the runtime — and run shell commands and file operations in it. " +
		"Pick the operation with `op`: open (`name`, optional `toolchain`), list, status, release; exec (`command`, optional `cwd`, `timeout_s`); " +
		"read, write, edit, glob, grep with the same arguments as the Read, Write, Edit, Glob and Grep tools (no `volume`). " +
		"A workspace belongs to the run that opened it and is deleted when that run ends; copy out what you need to keep with read. " +
		"Commands run on the worker's host engine as the worker's user with no isolation: it is not a sandbox, and has no network guarantees either way. " +
		"Use this for work that needs a real shell and toolchains (python, node); use Read/Write/Edit on your volumes for files you keep. " +
		"Paths are relative to the workspace; `..`, absolute paths elsewhere and symlinks out of it are refused. See the `Workspace` help topic."
}

func (w *Workspace) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"op":               {"type": "string", "enum": ["open","release","list","status","exec","read","write","edit","glob","grep"]},
			"name":             {"type": "string", "description": "The workspace's name, your label for it within this run: 1-64 letters, digits, '.', '-', '_'. Every op but list takes it."},
			"toolchain":        {"type": "string", "enum": ["python","node"], "description": "open: the toolchain the worker must have. Omit for a general workspace, on a worker with both."},
			"command":          {"type": "string", "description": "exec: a shell command line, run with /bin/sh -c."},
			"cwd":              {"type": "string", "description": "exec: the directory to start in, relative to the workspace (default its root)."},
			"timeout_s":        {"type": "integer", "minimum": 1, "description": "exec: seconds before the command and everything it started are killed. Default and ceiling are the operator's."},
			"path":             {"type": "string", "description": "read/write/edit: the file; glob/grep: the directory to search. Relative to the workspace."},
			"content":          {"type": "string", "description": "write: the whole new file, up to 1 MiB."},
			"old_string":       {"type": "string", "description": "edit: the exact text to replace; unique unless replace_all."},
			"new_string":       {"type": "string", "description": "edit: the replacement."},
			"replace_all":      {"type": "boolean", "description": "edit: replace every occurrence."},
			"offset":           {"type": "integer", "minimum": 1, "description": "read: first line, from 1. Turns on line mode."},
			"limit":            {"type": "integer", "minimum": 1, "description": "read: how many lines. Turns on line mode."},
			"line_numbers":     {"type": "boolean", "description": "read: prefix each line with its number. Turns on line mode."},
			"pattern":          {"type": "string", "description": "glob: a glob (*, ?, [..], **); grep: an RE2 regular expression."},
			"glob":             {"type": "string", "description": "grep: only files whose name matches this one-segment pattern."},
			"output_mode":      {"type": "string", "enum": ["files_with_matches","content","count"], "description": "grep: what to return (default files_with_matches)."},
			"case_insensitive": {"type": "boolean", "description": "grep: match without regard to case."},
			"multiline":        {"type": "boolean", "description": "grep: . matches newlines, ^ and $ match at line boundaries."},
			"head_limit":       {"type": "integer", "description": "grep: at most this many results (default 100)."},
			"-A":               {"type": "integer", "description": "grep content: lines after each match."},
			"-B":               {"type": "integer", "description": "grep content: lines before each match."},
			"-C":               {"type": "integer", "description": "grep content: lines before and after each match."}
		},
		"required": ["op"]
	}`)
}

// workspaceOpArgs are the arguments each op takes besides op. An argument
// another op takes is refused, as a misspelt one is: silently dropping it
// would do something the caller did not ask for.
var workspaceOpArgs = map[string][]string{
	"open":    {"name", "toolchain"},
	"release": {"name"},
	"list":    {},
	"status":  {"name"},
	"exec":    {"name", "command", "cwd", "timeout_s"},
	"read":    {"name", "path", "offset", "limit", "line_numbers"},
	"write":   {"name", "path", "content"},
	"edit":    {"name", "path", "old_string", "new_string", "replace_all"},
	"glob":    {"name", "pattern", "path"},
	"grep":    {"name", "pattern", "path", "glob", "output_mode", "case_insensitive", "multiline", "head_limit", "-A", "-B", "-C"},
}

// workspaceOpGrant is the grant each op needs: an operation of the run's
// workspace.ops, "open" for workspace.open, or "any" for any grant at all
// (release: only an owner reaches a workspace to release, and an owner was
// granted open).
var workspaceOpGrant = map[string]string{
	"open": "open", "release": "any", "list": config.WorkspaceOpRead, "status": config.WorkspaceOpRead,
	"exec": config.WorkspaceOpExec, "read": config.WorkspaceOpRead, "glob": config.WorkspaceOpRead, "grep": config.WorkspaceOpRead,
	"write": config.WorkspaceOpWrite, "edit": config.WorkspaceOpWrite,
}

type workspaceInput struct {
	Op              string `json:"op"`
	Name            string `json:"name"`
	Toolchain       string `json:"toolchain"`
	Command         string `json:"command"`
	Cwd             string `json:"cwd"`
	TimeoutS        int    `json:"timeout_s"`
	Path            string `json:"path"`
	Content         string `json:"content"`
	OldString       string `json:"old_string"`
	NewString       string `json:"new_string"`
	ReplaceAll      bool   `json:"replace_all"`
	Offset          *int   `json:"offset"`
	Limit           *int   `json:"limit"`
	LineNumbers     bool   `json:"line_numbers"`
	Pattern         string `json:"pattern"`
	Glob            string `json:"glob"`
	OutputMode      string `json:"output_mode"`
	CaseInsensitive bool   `json:"case_insensitive"`
	Multiline       bool   `json:"multiline"`
	HeadLimit       int    `json:"head_limit"`
	After           int    `json:"-A"`
	Before          int    `json:"-B"`
	Context         int    `json:"-C"`
}

// workspaceRefusal is a failure the model sees: a class and a detail.
func workspaceRefusal(class, detail string) tools.Result {
	out := map[string]string{"error": class}
	if detail != "" {
		out["detail"] = detail
	}
	b, _ := json.Marshal(out)
	return tools.Result{Text: string(b), IsError: true}
}

func workspaceOK(v any) tools.Result {
	b, err := json.Marshal(v)
	if err != nil {
		return workspaceRefusal("failed", "could not encode the result")
	}
	return tools.Result{Text: string(b)}
}

func (w *Workspace) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	in, res, ok := decodeWorkspaceInput(input)
	if !ok {
		return res, nil
	}
	// 1. The grant.
	pol := tools.PolicyOf(ctx).Workspace
	granted := false
	switch need := workspaceOpGrant[in.Op]; need {
	case "open":
		granted = pol.Open
	case "any":
		granted = pol.Grants()
	default:
		granted = pol.Allows(need)
	}
	if !granted {
		return workspaceRefusal("not_granted", fmt.Sprintf("this agent's workspace grant does not allow %s", in.Op)), nil
	}
	if w.Manager == nil {
		return workspaceRefusal("workspaces_off", "this runtime serves no workspaces (workspace.mode is off)"), nil
	}
	ident := tools.RunIdentity(ctx)
	owner := lifecycle.Owner{TenantID: ident.TenantID, UserID: ident.UserID, RunID: tools.RunID(ctx), AgentID: ident.AgentID, RootRunID: ident.RootRunID}
	if owner.RunID == "" {
		return workspaceRefusal("no_run", "a workspace belongs to a run, and this call has none"), nil
	}
	switch in.Op {
	case "open":
		return w.open(ctx, owner, pol, in), nil
	case "list":
		return w.list(ctx, owner), nil
	}
	// 2. The pin: the calling run's own workspace, or not found.
	row, err := w.Manager.Get(ctx, owner, in.Name)
	if err != nil {
		return workspaceError(err, in.Name), nil
	}
	// 3. The worker.
	switch in.Op {
	case "release":
		if err := w.Manager.Release(ctx, row); err != nil {
			return workspaceError(err, in.Name), nil
		}
		return workspaceOK(map[string]any{"name": row.Name, "released": true}), nil
	case "status":
		out := map[string]any{"name": row.Name, "state": string(row.State), "engine": row.Engine}
		if row.State == store.WorkspaceReady {
			exists, err := w.Manager.Status(ctx, row)
			if err != nil {
				return workspaceError(err, in.Name), nil
			}
			out["exists"] = exists
		}
		return workspaceOK(out), nil
	case "exec":
		return w.exec(ctx, row, in), nil
	}
	return w.fileOp(ctx, row, in), nil
}

// decodeWorkspaceInput decodes the call strictly: an unknown argument, and
// one the op does not take, are refused.
func decodeWorkspaceInput(input json.RawMessage) (workspaceInput, tools.Result, bool) {
	var in workspaceInput
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, workspaceRefusal("invalid_argument", "input: "+err.Error()), false
	}
	allowed, ok := workspaceOpArgs[in.Op]
	if !ok {
		return in, workspaceRefusal("invalid_argument", fmt.Sprintf("unknown op %q", in.Op)), false
	}
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(input, &keys)
	var extra []string
	for k := range keys {
		if k != "op" && !slices.Contains(allowed, k) {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return in, workspaceRefusal("invalid_argument", fmt.Sprintf("%s does not take %s (it takes %s)", in.Op, strings.Join(extra, ", "), strings.Join(append([]string{"op"}, allowed...), ", "))), false
	}
	if in.Op != "list" && in.Name == "" {
		return in, workspaceRefusal("invalid_argument", in.Op+" needs name"), false
	}
	return in, tools.Result{}, true
}

// workspaceError turns a lifecycle or dispatch error into a refusal. A
// worker's own words come back as its class and detail; the core's are
// explained.
func workspaceError(err error, name string) tools.Result {
	switch {
	case errors.Is(err, lifecycle.ErrNotFound):
		return workspaceRefusal("not_found", fmt.Sprintf("this run has no workspace named %q; open it first (list shows yours)", name))
	case errors.Is(err, lifecycle.ErrInvalidName):
		return workspaceRefusal("invalid_argument", err.Error())
	case errors.Is(err, lifecycle.ErrLimit):
		return workspaceRefusal("limit", err.Error())
	case errors.Is(err, lifecycle.ErrNotReady):
		return workspaceRefusal("not_ready", err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return workspaceRefusal("cancelled", "the call was cancelled before the worker answered")
	}
	var oe *dispatch.OpError
	if !errors.As(err, &oe) {
		// A store fault: its text may name tables or hosts, so only its class.
		return workspaceRefusal("failed", "the runtime could not complete the op; try again")
	}
	switch oe.Class {
	case workspace.ClassWorkerLost:
		return workspaceRefusal(oe.Class, "the worker holding this workspace went away; whether the op took effect is unknown, and the workspace may be gone")
	case workspace.ClassWorkerRevoked:
		return workspaceRefusal(oe.Class, "the worker holding this workspace was detached by the operator; whether the op took effect is unknown")
	case workspace.ClassNotFound:
		return workspaceRefusal("lost", "the workspace's directory is no longer on its worker")
	}
	return workspaceRefusal(oe.Class, oe.Detail)
}

func (w *Workspace) open(ctx context.Context, o lifecycle.Owner, pol tools.WorkspacePolicyValue, in workspaceInput) tools.Result {
	req := lifecycle.OpenRequest{Name: in.Name, Engine: pol.Engine}
	switch in.Toolchain {
	case "":
		// A general workspace (RFC GW §4.4) is placed where both are.
		req.Toolchains = []string{"python", "node"}
	case "python", "node":
		req.Toolchains = []string{in.Toolchain}
	default:
		return workspaceRefusal("invalid_argument", fmt.Sprintf("toolchain %q: want python or node", in.Toolchain))
	}
	row, err := w.Manager.Open(ctx, o, req)
	if err != nil {
		return workspaceError(err, in.Name)
	}
	return workspaceOK(map[string]any{"name": row.Name, "state": string(row.State), "engine": row.Engine})
}

func (w *Workspace) list(ctx context.Context, o lifecycle.Owner) tools.Result {
	rows, err := w.Manager.List(ctx, o)
	if err != nil {
		return workspaceError(err, "")
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{"name": r.Name, "state": string(r.State), "engine": r.Engine, "created_at": r.CreatedAt.Format(time.RFC3339)})
	}
	n := len(out)
	res := workspaceOK(map[string]any{"workspaces": out})
	res.Count = &n
	return res
}

func (w *Workspace) exec(ctx context.Context, row store.WorkspaceRow, in workspaceInput) tools.Result {
	def, ceiling := w.ExecDefault, w.ExecMax
	if def <= 0 {
		def = config.DefaultWorkspaceExecTimeoutS * time.Second
	}
	if ceiling <= 0 {
		ceiling = config.DefaultWorkspaceExecMaxTimeout * time.Second
	}
	timeout := def
	if in.TimeoutS != 0 {
		timeout = time.Duration(in.TimeoutS) * time.Second
	}
	if in.TimeoutS < 0 || timeout > ceiling {
		return workspaceRefusal("invalid_argument", fmt.Sprintf("timeout_s must be from 1 to %d", int(ceiling/time.Second)))
	}
	if strings.TrimSpace(in.Command) == "" {
		return workspaceRefusal("invalid_argument", "exec needs command")
	}
	args, _ := json.Marshal(workspace.ExecArgs{Command: in.Command, Cwd: in.Cwd, TimeoutMS: timeout.Milliseconds()})
	res, err := w.Manager.Do(ctx, row, dispatch.Request{Op: workspace.OpExec, Args: args})
	if err != nil {
		return workspaceError(err, in.Name)
	}
	var ea workspace.ExecAnswer
	if json.Unmarshal(res.Output, &ea) != nil {
		return workspaceRefusal(workspace.ClassProtocol, "the worker's answer was not an exec result")
	}
	out := workspaceOK(ea)
	// A command that failed is still an answer to read; it is marked so the
	// model sees it failed, as the Bash tool does.
	out.IsError = ea.ExitCode != 0 || ea.TimedOut
	return out
}

func (w *Workspace) fileOp(ctx context.Context, row store.WorkspaceRow, in workspaceInput) tools.Result {
	req := dispatch.Request{Op: in.Op}
	var args any
	switch in.Op {
	case workspace.OpRead:
		args = workspace.ReadArgs{Path: in.Path, Offset: in.Offset, Limit: in.Limit, LineNumbers: in.LineNumbers}
	case workspace.OpWrite:
		args = workspace.WriteArgs{Path: in.Path, Content: in.Content}
	case workspace.OpEdit:
		args = workspace.EditArgs{Path: in.Path, OldString: in.OldString, NewString: in.NewString, ReplaceAll: in.ReplaceAll}
	case workspace.OpGlob:
		args = workspace.GlobArgs{Pattern: in.Pattern, Path: in.Path}
	case workspace.OpGrep:
		args = workspace.GrepArgs{Pattern: in.Pattern, Path: in.Path, Glob: in.Glob, OutputMode: in.OutputMode,
			CaseInsensitive: in.CaseInsensitive, Multiline: in.Multiline, HeadLimit: in.HeadLimit,
			After: in.After, Before: in.Before, Context: in.Context}
	}
	b, _ := json.Marshal(args)
	if in.Op == workspace.OpWrite || in.Op == workspace.OpEdit {
		// A file body can be larger than an OpStart should carry.
		req.Input = bytes.NewReader(b)
	} else {
		req.Args = b
	}
	res, err := w.Manager.Do(ctx, row, req)
	if err != nil {
		return workspaceError(err, in.Name)
	}
	var fa workspace.FileAnswer
	if json.Unmarshal(res.Output, &fa) != nil {
		return workspaceRefusal(workspace.ClassProtocol, "the worker's answer was not a file result")
	}
	return tools.Result{Text: fa.Text, Count: fa.Count}
}
