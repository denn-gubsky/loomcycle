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
	// max_timeout_s; zero is config's default. They bound a cell too.
	ExecDefault, ExecMax time.Duration
	// KernelGrace is workspace.kernel.interrupt_grace_seconds: how long an
	// interrupted cell has to stop before its kernel is killed. Zero is
	// config's default.
	KernelGrace time.Duration
}

func (w *Workspace) Name() string { return "Workspace" }

func (w *Workspace) Description() string {
	return "Open a workspace — a directory on a workspace worker, away from the runtime — and run shell commands and file operations in it. " +
		"Pick the operation with `op`: open (`name`, optional `toolchain`, optional `egress`: the routes it may reach the network through, default all your grant names), list, status, release; exec (`command`, optional `cwd`, `timeout_s`); " +
		"read, write, edit, glob, grep with the same arguments as the Read, Write, Edit, Glob and Grep tools (no `volume`); " +
		"run_cell (`code`, optional `kernel` python|node, `timeout_s`) runs code in an interpreter that keeps its variables between cells, reset_kernel starts it clean. " +
		"view (`path`) shows you a PNG, JPEG, GIF or WebP from the workspace as a picture, e.g. a chart you drew. " +
		"start (`command`, optional `cwd`, `port`) runs a command that keeps running after the call, a server, and returns its id; stop (`server`) ends it; logs (`server`) reads its latest output, or without server lists the servers; request (`port`, `method`, `path`, optional `headers`, `body`) sends one HTTP request to a port a running server of the workspace declared and returns its status, headers and body (cut at 1 MiB); expose (`port`, optional `title`, `kind` page|panel) publishes that port to your user as a page in their chat and returns its base_path, unexpose (`port`) withdraws it. " +
		"share (`name`, `with`: another run of your user by its agent id, `access`: read or read_write, optional `connect`: also its exposed pages) and unshare (`name`, `with`). " +
		"checkpoint (`name`, optional `label`) records the workspace's files and answers an id; checkpoints lists them; restore (`name`, `checkpoint`) puts the files back and stops its interpreters and commands; delete_checkpoint frees one; fork (`name`, `as`, optional `checkpoint`) opens a second workspace of yours as a copy. Checkpoints count against the workspace's disk quota. " +
		"A workspace another agent shared with you is named `<name>@<its agent id>` (list shows them): read, view, glob and grep on it, write and edit with read_write, and request to a port its owner exposed when shared with connect; never exec or run_cell. " +
		"A workspace belongs to the run that opened it and is deleted when that run ends; copy out what you need to keep with read. " +
		"Commands run on the worker's host engine as the worker's user with no isolation: it is not a sandbox, and has no network guarantees either way. " +
		"Use this for work that needs a real shell and toolchains (python, node); use Read/Write/Edit on your volumes for files you keep. " +
		"Paths are relative to the workspace; `..`, absolute paths elsewhere and symlinks out of it are refused. See the `Workspace` help topic."
}

func (w *Workspace) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"op":               {"type": "string", "enum": ["open","release","list","status","exec","read","write","edit","glob","grep","share","unshare","run_cell","reset_kernel","view","start","stop","logs","request","expose","unexpose","checkpoint","checkpoints","restore","delete_checkpoint","fork","save_template","delete_template","templates"]},
			"name":             {"type": "string", "description": "The workspace's name, your label for it within this run: 1-64 letters, digits, '.', '-', '_'. Every op but list takes it. A workspace shared with you: <name>@<owner agent id> (read, write, edit, glob, grep only)."},
			"with":             {"type": "string", "description": "share/unshare: the agent id of the run to share with: your parent, a child, a sibling or another run of your user."},
			"access":           {"type": "string", "enum": ["read","read_write"], "description": "share: read (read, glob, grep and copying files out) or read_write (also writing files in). Never commands."},
			"connect":          {"type": "boolean", "description": "share: also let the peer send requests to the ports you exposed (needs your workspace.connect grant; default false)."},
			"toolchain":        {"type": "string", "enum": ["python","node"], "description": "open: the toolchain the worker must have. Omit for a general workspace, on a worker with both."},
			"egress":           {"type": "array", "items": {"type": "string"}, "description": "open: the egress routes the workspace may send requests out through, at http://egress/<route>/…, from those your grant names. Omit for all of them; [] for none."},
			"command":          {"type": "string", "description": "exec, start: a shell command line, run with /bin/sh -c."},
			"cwd":              {"type": "string", "description": "exec, start: the directory to start in, relative to the workspace (default its root)."},
			"port":             {"type": "integer", "minimum": 1, "maximum": 65535, "description": "start: the port the server listens on, recorded with it; request: the port to send to, one a running server of this workspace declared at start (through a share: one its owner exposed); expose, unexpose: the port to publish to your user or withdraw."},
			"method":           {"type": "string", "description": "request: the HTTP method, upper case (GET, POST, ...)."},
			"headers":          {"type": "object", "additionalProperties": {"type": "string"}, "description": "request: headers to send, name to value. Content-Length, Transfer-Encoding and Connection are the runtime's."},
			"body":             {"type": "string", "description": "request: the body to send, up to 1 MiB."},
			"title":            {"type": "string", "description": "expose: the page's title, shown to the user (one line, up to 200 bytes)."},
			"kind":             {"type": "string", "enum": ["page","panel"], "description": "expose: how the chat shows it (default page)."},
			"server":           {"type": "string", "description": "stop, logs: the server's id, as start returned it (srv-1, ...)."},
			"tail_bytes":       {"type": "integer", "minimum": 1, "description": "logs with server: how much of its latest output to return (default 65536, at most 262144)."},
			"timeout_s":        {"type": "integer", "minimum": 1, "description": "exec: seconds before the command and everything it started are killed; run_cell: seconds before the cell is interrupted. Default and ceiling are the operator's."},
			"code":             {"type": "string", "description": "run_cell: the code to run, up to 1 MiB. The last expression's value is returned."},
			"kernel":           {"type": "string", "enum": ["python","node"], "description": "run_cell, reset_kernel: the interpreter (default python)."},
			"label":            {"type": "string", "description": "checkpoint: a note to tell it by, up to 200 characters."},
			"checkpoint":       {"type": "string", "description": "restore, delete_checkpoint: the checkpoint's id (checkpoints lists them); fork: the checkpoint to start from (default the present files); save_template: the checkpoint to save."},
			"as":               {"type": "string", "description": "fork: the new workspace's name; save_template: the template's name."},
			"template":         {"type": "string", "description": "open: a template to start the workspace from, by name (templates lists them; your own first, then your tenant's); delete_template: the template to delete."},
			"scope":            {"type": "string", "enum": ["user","tenant"], "description": "save_template, delete_template: user (your runs only) or tenant (every user of your tenant; needs the tenant value in memory_scopes)."},
			"path":             {"type": "string", "description": "read/write/edit: the file; glob/grep: the directory to search. Relative to the workspace. request: the request target, starting with / (e.g. /api/items?page=2)."},
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
	"open":         {"name", "toolchain", "egress", "template"},
	"release":      {"name"},
	"list":         {},
	"status":       {"name"},
	"exec":         {"name", "command", "cwd", "timeout_s"},
	"read":         {"name", "path", "offset", "limit", "line_numbers"},
	"write":        {"name", "path", "content"},
	"edit":         {"name", "path", "old_string", "new_string", "replace_all"},
	"glob":         {"name", "pattern", "path"},
	"grep":         {"name", "pattern", "path", "glob", "output_mode", "case_insensitive", "multiline", "head_limit", "-A", "-B", "-C"},
	"run_cell":     {"name", "code", "kernel", "timeout_s"},
	"reset_kernel": {"name", "kernel"},
	// RFC GW §5.4: checkpoints of the run's own workspace, and a fork of it.
	"checkpoint":        {"name", "label"},
	"checkpoints":       {"name"},
	"restore":           {"name", "checkpoint"},
	"delete_checkpoint": {"name", "checkpoint"},
	"fork":              {"name", "as", "checkpoint"},
	// RFC GW §5.5: a checkpoint of the run's own workspace saved as a
	// template, a template deleted, and the templates the run may open.
	"save_template":   {"name", "checkpoint", "as", "scope"},
	"delete_template": {"template", "scope"},
	"templates":       {},
	"view":            {"name", "path"},
	"start":           {"name", "command", "cwd", "port"},
	"stop":            {"name", "server"},
	"logs":            {"name", "server", "tail_bytes"},
	// RFC GW §5.3, D26: one HTTP request to a port of a server of the
	// run's own workspace (P9 plan §3.2).
	"request": {"name", "port", "method", "path", "headers", "body"},
	// RFC GW §5.3: publish a port to the run's user, and withdraw it.
	"expose":   {"name", "port", "title", "kind"},
	"unexpose": {"name", "port"},
	"share":    {"name", "with", "access", "connect"},
	"unshare":  {"name", "with"},
}

// WorkspaceMCPOps are the ops the Workspace tool takes over loomcycle's MCP
// server, where the owner is the session's principal (RFC GW D35, P9 plan
// §3.5): the lifecycle, commands and the file ops. Sharing, interpreters and
// servers are a run's.
var WorkspaceMCPOps = []string{"open", "release", "list", "status", "exec", "read", "write", "edit", "glob", "grep"}

var workspaceMCPOps = func() map[string]bool {
	m := map[string]bool{}
	for _, op := range WorkspaceMCPOps {
		m[op] = true
	}
	return m
}()

// workspaceSharedOps are the ops that take a workspace shared with the run
// (<name>@<agent id>): reading and, through a read_write share, writing
// files. Never a command, a status, a release or a share of it (RFC GW D20).
//
// view is a read (RFC GW §5.8: grant read): a read share allows it.
var workspaceSharedOps = map[string]bool{"read": true, "glob": true, "grep": true, "write": true, "edit": true, "view": true}

// workspaceOpGrant is the grant each op needs: an operation of the run's
// workspace.ops, "open" for workspace.open, or "any" for any grant at all
// (release: only an owner reaches a workspace to release, and an owner was
// granted open).
var workspaceOpGrant = map[string]string{
	"open": "open", "release": "any", "list": config.WorkspaceOpRead, "status": config.WorkspaceOpRead,
	"exec": config.WorkspaceOpExec, "read": config.WorkspaceOpRead, "glob": config.WorkspaceOpRead, "grep": config.WorkspaceOpRead,
	"view":  config.WorkspaceOpRead,
	"write": config.WorkspaceOpWrite, "edit": config.WorkspaceOpWrite,
	// A cell runs code as a command does (RFC GW §5.7: grants exec).
	"run_cell": config.WorkspaceOpExec, "reset_kernel": config.WorkspaceOpExec,
	// A server is a command that keeps running (RFC GW §7: exec covers
	// servers); its output is a command's, so reading it is exec's too.
	"start": config.WorkspaceOpExec, "stop": config.WorkspaceOpExec, "logs": config.WorkspaceOpExec,
	// A request reaches what a command in the workspace could reach (curl to
	// the same port), so it is exec's for the owner (P9 plan D-P9-20); a
	// request through a share is connect's instead (see Execute).
	"request": config.WorkspaceOpExec,
	// expose puts model-written active content in front of a person: its
	// own grant (RFC GW §7). Withdrawing it needs the same.
	"expose": "expose", "unexpose": "expose",
	// share needs the share grant and what it hands over: checked in share.
	"share": "share", "unshare": "share",
	// A checkpoint and a restore change what the workspace holds; a fork
	// opens a workspace (RFC GW §5.4).
	"checkpoint": config.WorkspaceOpWrite, "restore": config.WorkspaceOpWrite, "delete_checkpoint": config.WorkspaceOpWrite,
	"checkpoints": config.WorkspaceOpRead, "fork": "open",
	// Saving and deleting a template need workspace.templates (RFC GW
	// §5.5); listing them, the grant to open one or to save one.
	"save_template": "templates", "delete_template": "templates", "templates": "open_or_templates",
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
	Code            string `json:"code"`
	Kernel          string `json:"kernel"`
	With            string `json:"with"`
	Access          string `json:"access"`
	Connect         *bool  `json:"connect"`
	// Egress is open's routes: nil when absent (the whole grant), empty
	// for none.
	Egress     *[]string         `json:"egress"`
	Label      string            `json:"label"`
	Checkpoint string            `json:"checkpoint"`
	As         string            `json:"as"`
	Template   string            `json:"template"`
	Scope      string            `json:"scope"`
	Port       int               `json:"port"`
	Server     string            `json:"server"`
	TailBytes  int               `json:"tail_bytes"`
	Method     string            `json:"method"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	Title      string            `json:"title"`
	Kind       string            `json:"kind"`
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
	need := workspaceOpGrant[in.Op]
	if _, _, shared := lifecycle.SplitShared(in.Name); shared && in.Op == "request" {
		// A peer's request reaches only exposed ports, never a command's
		// reach: its own connect grant, not exec (RFC GW D20, §7).
		need = "connect"
	}
	switch need {
	case "open":
		granted = pol.Open
	case "any":
		granted = pol.Grants()
	case "share":
		granted = pol.Share
	case "expose":
		granted = pol.Expose
	case "connect":
		granted = pol.Connect
	case "templates":
		granted = pol.Templates
	case "open_or_templates":
		granted = pol.Open || pol.Templates
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
		// Outside a run, only the MCP server's twin names an owner: the
		// session's principal (RFC GW D35).
		po, ok := lifecycle.PrincipalOwnerFrom(ctx)
		if !ok {
			return workspaceRefusal("no_run", "a workspace belongs to a run, and this call has none"), nil
		}
		owner = po
		if !workspaceMCPOps[in.Op] {
			return workspaceRefusal("not_over_mcp", fmt.Sprintf("%s is done inside a run; over the MCP server the workspace tool takes %s", in.Op, strings.Join(WorkspaceMCPOps, ", "))), nil
		}
		if _, _, shared := lifecycle.SplitShared(in.Name); shared {
			return workspaceRefusal("not_over_mcp", "shared workspaces are reached inside a run; over the MCP server only your own"), nil
		}
	}
	switch in.Op {
	case "open":
		if in.Template != "" && owner.IsPrincipal() {
			return workspaceRefusal("not_over_mcp", "a workspace is opened from a template inside a run; over the MCP server open takes no template"), nil
		}
		return w.open(ctx, owner, pol, in), nil
	case "list":
		return w.list(ctx, owner), nil
	case "templates":
		return w.templates(ctx, owner), nil
	case "delete_template":
		return w.deleteTemplate(ctx, owner, in), nil
	}
	if _, _, shared := lifecycle.SplitShared(in.Name); shared {
		return w.sharedOp(ctx, owner, in), nil
	}
	switch in.Op {
	case "share":
		return w.share(ctx, owner, pol, in), nil
	case "unshare":
		return w.unshare(ctx, owner, in), nil
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
		withEgress(out, row.Egress)
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
	case "run_cell":
		return w.runCell(ctx, row, in), nil
	case "reset_kernel":
		return w.resetKernel(ctx, row, in), nil
	case "checkpoint", "checkpoints", "restore", "delete_checkpoint":
		return w.checkpointOp(ctx, row, in), nil
	case "fork":
		return w.fork(ctx, owner, pol, row, in), nil
	case "save_template":
		return w.saveTemplate(ctx, owner, row, in), nil
	case "view":
		return w.view(ctx, owner, in), nil
	case "start", "stop", "logs":
		return w.serverOp(ctx, row, in), nil
	case "request":
		return w.request(ctx, row, in), nil
	case "expose", "unexpose":
		return w.exposeOp(ctx, owner, in), nil
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
	if in.Op != "list" && in.Op != "templates" && in.Op != "delete_template" && in.Name == "" {
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
	case errors.Is(err, lifecycle.ErrShareNoConnect):
		return workspaceRefusal("not_granted", err.Error()+"; ask its owner to share it with connect")
	case errors.Is(err, lifecycle.ErrNotExposed):
		return workspaceRefusal("not_exposed", err.Error())
	case errors.Is(err, lifecycle.ErrShareReadOnly):
		return workspaceRefusal("read_only_share", err.Error()+"; ask its owner for a read_write share")
	case errors.Is(err, lifecycle.ErrShareTarget), errors.Is(err, lifecycle.ErrShareNoUser):
		return workspaceRefusal("invalid_share_target", err.Error())
	case errors.Is(err, lifecycle.ErrShareLimit):
		return workspaceRefusal("limit", err.Error())
	case errors.Is(err, lifecycle.ErrShareBusy):
		return workspaceRefusal("busy", err.Error())
	case errors.Is(err, lifecycle.ErrNameTaken):
		return workspaceRefusal("exists", err.Error()+"; fork as another name, or release that one first")
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
	routes, refusal, ok := openEgress(pol, in.Egress)
	if !ok {
		return refusal
	}
	req := lifecycle.OpenRequest{Name: in.Name, Engine: pol.Engine, Egress: routes}
	if in.Template != "" {
		if in.Toolchain != "" {
			return workspaceRefusal("invalid_argument", "a template names its own toolchains; open it without toolchain")
		}
		t, err := w.Manager.FindTemplate(ctx, o, in.Template)
		if err != nil {
			return templateError(err)
		}
		if !templateReachable(ctx, t) {
			return templateError(lifecycle.ErrTemplateNotFound)
		}
		req.Template = &t
		return w.finishOpen(ctx, o, req)
	}
	switch in.Toolchain {
	case "":
		// A general workspace (RFC GW §4.4) is placed where both are.
		req.Toolchains = []string{"python", "node"}
	case "python", "node":
		req.Toolchains = []string{in.Toolchain}
	default:
		return workspaceRefusal("invalid_argument", fmt.Sprintf("toolchain %q: want python or node", in.Toolchain))
	}
	return w.finishOpen(ctx, o, req)
}

func (w *Workspace) finishOpen(ctx context.Context, o lifecycle.Owner, req lifecycle.OpenRequest) tools.Result {
	row, err := w.Manager.Open(ctx, o, req)
	if err != nil {
		if req.Template != nil {
			return templateError(err)
		}
		return workspaceError(err, req.Name)
	}
	out := map[string]any{"name": row.Name, "state": string(row.State), "engine": row.Engine}
	if req.Template != nil {
		out["template"] = req.Template.Name
	}
	withEgress(out, row.Egress)
	return workspaceOK(out)
}

// openEgress is the routes an open records (RFC GW §6.3, O4 of the P4 plan):
// absent, every route of the run's grant; a list, that subset, each name
// checked against the grant, duplicates dropped. A name the grant does not
// hold is refused: an agent names routes, it never widens them.
func openEgress(pol tools.WorkspacePolicyValue, asked *[]string) ([]string, tools.Result, bool) {
	if asked == nil {
		return append([]string{}, pol.Egress...), tools.Result{}, true
	}
	out := []string{}
	for _, name := range *asked {
		if !config.ValidEgressRouteName(name) {
			return nil, workspaceRefusal("invalid_argument", fmt.Sprintf("egress: %q is not a route name (lower-case letters, digits and '-', starting with a letter)", name)), false
		}
		if !slices.Contains(pol.Egress, name) {
			return nil, workspaceRefusal("not_granted", fmt.Sprintf("this agent's workspace grant does not name the egress route %q", name)), false
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out, tools.Result{}, true
}

// withEgress names a workspace's routes in an answer, when it has any.
func withEgress(out map[string]any, routes []string) {
	if len(routes) > 0 {
		out["egress"] = routes
	}
}

func (w *Workspace) list(ctx context.Context, o lifecycle.Owner) tools.Result {
	rows, err := w.Manager.List(ctx, o)
	if err != nil {
		return workspaceError(err, "")
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		e := map[string]any{"name": r.Name, "state": string(r.State), "engine": r.Engine, "created_at": r.CreatedAt.Format(time.RFC3339)}
		withEgress(e, r.Egress)
		if len(r.Shares) > 0 {
			shares := make([]map[string]any, 0, len(r.Shares))
			for _, sh := range r.Shares {
				e := map[string]any{"with": sh.AgentID, "access": sh.Access}
				if sh.Connect {
					e["connect"] = true
				}
				shares = append(shares, e)
			}
			e["shared_with"] = shares
		}
		out = append(out, e)
	}
	n := len(out)
	body := map[string]any{"workspaces": out}
	// What other runs shared with this one, by the name to use.
	shared, err := w.Manager.SharedWith(ctx, o)
	if err != nil {
		return workspaceError(err, "")
	}
	if len(shared) > 0 {
		sw := make([]map[string]any, 0, len(shared))
		for _, s := range shared {
			e := map[string]any{"name": lifecycle.SharedName(s.Row.Name, s.Row.OwnerAgentID), "owner": s.Row.OwnerAgentID, "access": s.Share.Access, "engine": s.Row.Engine}
			if s.Share.Connect {
				e["connect"] = true
			}
			sw = append(sw, e)
		}
		body["shared_with_me"] = sw
	}
	res := workspaceOK(body)
	res.Count = &n
	return res
}

// timeout is a command's or a cell's timeout: timeout_s within the
// operator's ceiling, else the operator's default.
func (w *Workspace) timeout(in workspaceInput) (time.Duration, *tools.Result) {
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
		r := workspaceRefusal("invalid_argument", fmt.Sprintf("timeout_s must be from 1 to %d", int(ceiling/time.Second)))
		return 0, &r
	}
	return timeout, nil
}

func (w *Workspace) exec(ctx context.Context, row store.WorkspaceRow, in workspaceInput) tools.Result {
	timeout, bad := w.timeout(in)
	if bad != nil {
		return *bad
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

// share shares the run's own workspace with another run of its user (RFC GW
// §5.2). The run must hold share and what the access hands over: a share
// never gives a peer more than its owner may do (D-P8-3).
func (w *Workspace) share(ctx context.Context, o lifecycle.Owner, pol tools.WorkspacePolicyValue, in workspaceInput) tools.Result {
	connect := in.Connect != nil && *in.Connect
	if !lifecycle.ValidShareAccess(in.Access) {
		return workspaceRefusal("invalid_argument", fmt.Sprintf("access %q: want read or read_write", in.Access))
	}
	if !pol.Read || (in.Access == store.WorkspaceShareReadWrite && !pol.Write) {
		return workspaceRefusal("not_granted", fmt.Sprintf("a %s share hands over operations this agent's workspace grant does not hold", in.Access))
	}
	if connect && !pol.Connect {
		return workspaceRefusal("not_granted", "a share with connect hands over requests to your exposed ports, which this agent's workspace grant (workspace.connect) does not allow")
	}
	if in.With == "" {
		return workspaceRefusal("invalid_argument", "share needs with: the agent id of the run to share with")
	}
	peer, err := w.Manager.ResolvePeer(ctx, o, in.With)
	if err != nil {
		return workspaceError(err, in.Name)
	}
	row, err := w.Manager.Share(ctx, o, in.Name, peer, in.Access, connect)
	if err != nil {
		return workspaceError(err, in.Name)
	}
	return workspaceOK(map[string]any{"name": row.Name, "shared_with": peer.AgentID, "access": in.Access, "connect": connect,
		"their_name": lifecycle.SharedName(row.Name, o.AgentID)})
}

func (w *Workspace) unshare(ctx context.Context, o lifecycle.Owner, in workspaceInput) tools.Result {
	if in.With == "" {
		return workspaceRefusal("invalid_argument", "unshare needs with: the agent id the workspace was shared with")
	}
	removed, err := w.Manager.Unshare(ctx, o, in.Name, in.With)
	if err != nil {
		return workspaceError(err, in.Name)
	}
	return workspaceOK(map[string]any{"name": in.Name, "with": in.With, "unshared": removed})
}

// sharedOp is an op on a workspace another run shared with this one: only
// the file ops of workspaceSharedOps, a write only through a read_write
// share, and the run's own grant applies on top (checked in Execute).
func (w *Workspace) sharedOp(ctx context.Context, o lifecycle.Owner, in workspaceInput) tools.Result {
	if in.Op == "request" {
		// Through a share with connect, to a port the owner exposed; the
		// tunnel then checks the port at the dial as for the owner.
		row, err := w.Manager.ConnectShared(ctx, o, in.Name, in.Port)
		if err != nil {
			return workspaceSharedError(err, in.Name)
		}
		return w.request(ctx, row, in)
	}
	if !workspaceSharedOps[in.Op] {
		return workspaceRefusal("not_shared_op", fmt.Sprintf("%s is not done on a workspace shared with you: a share allows read, glob, grep, and write and edit with read_write; never commands", in.Op))
	}
	write := in.Op == workspace.OpWrite || in.Op == workspace.OpEdit
	hookOp := wsHookRead
	if write {
		hookOp = wsHookWrite
	}
	if err := w.Manager.UseShared(ctx, o, in.Name, write, hookOp, "Workspace"); err != nil {
		return workspaceSharedError(err, in.Name)
	}
	if in.Op == "view" {
		return w.view(ctx, o, in)
	}
	row, err := w.Manager.Resolve(ctx, o, in.Name, write)
	if err != nil {
		return workspaceSharedError(err, in.Name)
	}
	return w.fileOp(ctx, row, in)
}

// workspaceSharedError is workspaceError for a shared name: not found says
// so without telling a missing share from a missing workspace.
func workspaceSharedError(err error, name string) tools.Result {
	if errors.Is(err, lifecycle.ErrNotFound) {
		return workspaceRefusal("not_found", fmt.Sprintf("no workspace %q is shared with this run (list shows those that are)", name))
	}
	return workspaceError(err, name)
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
