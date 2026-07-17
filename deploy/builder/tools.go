package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// toolDescriptor is one MCP tool the sidecar advertises via tools/list. Matches
// loomcycle's mcp.ToolDescriptor shape ({name, description, inputSchema}).
type toolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Dispatcher answers tools/list and tools/call.
type Dispatcher struct {
	cfg   *Config
	eng   *Engine
	store *Store
	nowFn func() time.Time
}

func NewDispatcher(cfg *Config, eng *Engine, store *Store) *Dispatcher {
	return &Dispatcher{cfg: cfg, eng: eng, store: store, nowFn: time.Now}
}

func (d *Dispatcher) now() time.Time { return d.nowFn() }

// Tools returns the advertised descriptors. Descriptions are model-facing — kept
// concrete and free of internal jargon.
func (d *Dispatcher) Tools() []toolDescriptor {
	return []toolDescriptor{
		{Name: "sandbox_open", Description: "Open an isolated sandbox container with a dev toolchain (Python/Go/Rust/C++/Node) and a /work directory. Returns a session_id to pass to the other sandbox tools. /work is in-memory by default; pass a `workspace` name for a PERSISTENT /work that survives close + restart (reopen the same name to resume). Reuse one session across a compile/test/fix loop; close it when done.", InputSchema: schemaOpen},
		{Name: "sandbox_exec", Description: "Run a shell command inside a sandbox session (working directory /work). Returns combined stdout+stderr and the exit code. Non-zero exit is reported so you can read the error and fix it.", InputSchema: schemaExec},
		{Name: "sandbox_write", Description: "Write a file into the sandbox workspace at a path relative to /work (parent dirs are created). Use this to put source files in before compiling.", InputSchema: schemaWrite},
		{Name: "sandbox_read", Description: "Read a file from the sandbox workspace (path relative to /work). Use this to pull out build output or a generated artifact.", InputSchema: schemaRead},
		{Name: "sandbox_close", Description: "Destroy a sandbox session and free its resources. Always close a session when finished; sessions also expire on their own after an idle timeout.", InputSchema: schemaClose},
		{Name: "sandbox_touch", Description: "Reset a session's idle timer (a heartbeat) to keep its container alive across a gap between commands. Use it when you'll return to a session after a pause longer than the idle timeout.", InputSchema: schemaTouch},
		{Name: "sandbox_close_run", Description: "Close ALL of your sandbox sessions that belong to a given run (bulk teardown by root_run_id). Use it to tear down a whole run's containers at once.", InputSchema: schemaCloseRun},
		{Name: "sandbox_list", Description: "List your currently open sandbox sessions.", InputSchema: schemaList},
	}
}

// caller carries the server-attested identity of an MCP request: the principal
// derived from the bearer, plus the non-secret run identifiers loomcycle forwards
// as headers (X-Loom-Root-Run / X-Loom-Tenant, RFC BI P2b). RootRun and Tenant are
// "" when the caller didn't send them (e.g. a direct client, or loomcycle without
// the run-id substitution). Unforgeable — they come from the request headers the
// MCP handler read, not from tool arguments.
type caller struct {
	Principal string
	RootRun   string
	Tenant    string
}

// Call dispatches a tools/call. It returns model-facing text and an isError flag
// (a tool-level failure the model should see + react to), or a non-nil error for
// a protocol-level fault (malformed arguments) which becomes a JSON-RPC error.
func (d *Dispatcher) Call(ctx context.Context, c caller, name string, args json.RawMessage) (text string, isError bool, err error) {
	switch name {
	case "sandbox_open":
		return d.open(ctx, c, args)
	case "sandbox_exec":
		return d.exec(ctx, c.Principal, args)
	case "sandbox_write":
		return d.write(ctx, c.Principal, args)
	case "sandbox_read":
		return d.read(ctx, c.Principal, args)
	case "sandbox_close":
		return d.close(ctx, c.Principal, args)
	case "sandbox_close_run":
		return d.closeRun(ctx, c.Principal, args)
	case "sandbox_touch":
		return d.touch(c.Principal, args)
	case "sandbox_list":
		return d.list(c.Principal)
	default:
		return "", false, fmt.Errorf("unknown tool %q", name)
	}
}

func (d *Dispatcher) open(ctx context.Context, c caller, raw json.RawMessage) (string, bool, error) {
	var in struct {
		Network   string  `json:"network"`
		TmpfsMB   int64   `json:"tmpfs_mb"`
		CPUs      float64 `json:"cpu"`
		MemMB     int64   `json:"mem_mb"`
		Pids      int64   `json:"pids"`
		Workspace string  `json:"workspace"`
	}
	if err := unmarshalArgs(raw, &in); err != nil {
		return "", false, err
	}
	if d.store.Count() >= d.cfg.MaxSessions {
		return fmt.Sprintf("sandbox capacity reached (%d open sessions); close one first", d.cfg.MaxSessions), true, nil
	}
	o := clampOpen(d.cfg, in.Network, in.TmpfsMB, in.CPUs, in.MemMB, in.Pids)
	o.Image = d.cfg.Image

	// RFC BI P2a — a named durable workspace: bind-mount a persistent host dir at
	// /work instead of tmpfs, so a checkout + build cache survive container churn.
	// The dir is derived + fenced from the operator root + the caller's principal
	// (never a raw caller path); the request is refused when durable workspaces are
	// not enabled.
	if in.Workspace != "" {
		dir, werr := d.resolveWorkspaceDir(c.Principal, in.Workspace)
		if werr != nil {
			return werr.Error(), true, nil
		}
		o.WorkspaceHostDir = dir
	}

	id, err := newID()
	if err != nil {
		return "", false, err
	}
	name := "loom-sbx-" + id
	if err := d.eng.Open(ctx, name, o); err != nil {
		return "opening sandbox failed: " + err.Error(), true, nil
	}
	now := d.now()
	sess := &Session{ID: id, Name: name, Principal: c.Principal, Image: o.Image, Network: o.Network, Workspace: in.Workspace, RootRun: c.RootRun, CreatedAt: now, LastUsed: now}
	d.store.Add(sess)

	respObj := map[string]any{
		"session_id":     id,
		"workspace_path": workDir,
		"network":        o.Network,
		"expires_at":     now.Add(d.cfg.SessionMaxTTL).UTC().Format(time.RFC3339),
	}
	if in.Workspace != "" {
		respObj["workspace"] = in.Workspace
		respObj["persistent"] = true // /work survives close/reap; reopen with the same workspace to resume
	}
	resp, _ := json.Marshal(respObj)
	return string(resp), false, nil
}

// resolveWorkspaceDir derives + provisions the durable /work host directory for a
// named workspace (RFC BI P2a), fenced under the operator's SANDBOX_WORKSPACE_ROOT
// and the caller's principal. The name is charset-gated and the resolved path is
// asserted strictly inside the root (symlink-escape defence), so one principal can
// never reach another's workspace and a hostile name can't escape the root — the
// same posture as loomcycle's VolumeDef fencing. Returns the host path to mount.
func (d *Dispatcher) resolveWorkspaceDir(principal, name string) (string, error) {
	root := d.cfg.WorkspaceRoot
	if root == "" {
		return "", fmt.Errorf("durable workspaces are not enabled (the operator must set SANDBOX_WORKSPACE_ROOT); omit `workspace` for an in-memory session")
	}
	if err := validateWorkspaceName(name); err != nil {
		return "", err
	}
	dir := filepath.Join(root, principalSegment(principal), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("provision workspace: %w", err)
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("workspace root: %w", err)
	}
	dirResolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("workspace dir: %w", err)
	}
	if dirResolved != rootResolved && !strings.HasPrefix(dirResolved, rootResolved+string(os.PathSeparator)) {
		return "", fmt.Errorf("workspace path escapes the workspace root")
	}
	// The session runs as the non-root container user; make the workspace writable
	// by it. Best-effort: it works in the docker-socket model (the sidecar runs as
	// root and can chown); on a rootless nested sidecar the operator pre-chowns the
	// root (documented).
	if uid, gid, ok := parseUserGID(d.cfg.CtrUser); ok {
		_ = os.Chown(dirResolved, uid, gid)
	}
	return dirResolved, nil
}

// validateWorkspaceName gates a caller-supplied workspace name: lowercase alnum +
// `_`/`-`, starting alnum, ≤64 chars — no `/`, no `.`, no `..`, no control bytes.
// The first line of the no-caller-controlled-path defence (mirrors VolumeDef).
func validateWorkspaceName(p string) error {
	if p == "" {
		return fmt.Errorf("workspace name is required")
	}
	if len(p) > 64 {
		return fmt.Errorf("workspace name too long (max 64)")
	}
	for i, r := range p {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok {
			return fmt.Errorf("workspace name must be [a-z0-9_-] (no '/', '.', or '..')")
		}
		if i == 0 && (r == '_' || r == '-') {
			return fmt.Errorf("workspace name must start with a letter or digit")
		}
	}
	return nil
}

// principalSegment renders a principal as a filesystem-safe path segment so each
// principal's workspaces live in their own subtree.
func principalSegment(principal string) string {
	if principal == "" {
		return "_anon"
	}
	var b strings.Builder
	for _, r := range principal {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// parseUserGID parses a "uid[:gid]" string (e.g. the container user "1000:1000").
func parseUserGID(s string) (uid, gid int, ok bool) {
	parts := strings.SplitN(s, ":", 2)
	u, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, false
	}
	g := u
	if len(parts) == 2 {
		if pg, err2 := strconv.Atoi(strings.TrimSpace(parts[1])); err2 == nil {
			g = pg
		}
	}
	return u, g, true
}

func (d *Dispatcher) exec(ctx context.Context, principal string, raw json.RawMessage) (string, bool, error) {
	var in struct {
		SessionID      string `json:"session_id"`
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
		MaxOutputBytes int64  `json:"max_output_bytes"`
	}
	if err := unmarshalArgs(raw, &in); err != nil {
		return "", false, err
	}
	if in.Command == "" {
		return "command is required", true, nil
	}
	sess, ok := d.store.Get(in.SessionID, principal, d.now())
	if !ok {
		return "no such session (open one with sandbox_open, or it may have expired)", true, nil
	}

	timeout := d.cfg.DefTimeout
	if in.TimeoutSeconds > 0 {
		timeout = time.Duration(in.TimeoutSeconds) * time.Second
	}
	if timeout > d.cfg.MaxTimeout {
		timeout = d.cfg.MaxTimeout
	}
	maxOut := d.cfg.MaxOutBytes
	if in.MaxOutputBytes > 0 && in.MaxOutputBytes < maxOut {
		maxOut = in.MaxOutputBytes
	}

	out, code, timedOut, err := d.eng.Exec(ctx, sess.Name, in.Command, timeout, maxOut)
	if err != nil {
		return "exec failed: " + err.Error(), true, nil
	}
	text := string(out)
	if int64(len(out)) >= maxOut {
		text += fmt.Sprintf("\n[output truncated at %d bytes]", maxOut)
	}
	if timedOut {
		text += fmt.Sprintf("\n[timed out after %s]", timeout)
		return text, true, nil
	}
	if code != 0 {
		text += fmt.Sprintf("\n[exit: %d]", code)
		return text, true, nil
	}
	return text, false, nil
}

func (d *Dispatcher) write(ctx context.Context, principal string, raw json.RawMessage) (string, bool, error) {
	var in struct {
		SessionID string `json:"session_id"`
		Path      string `json:"path"`
		Content   string `json:"content"`
		Encoding  string `json:"encoding"`
	}
	if err := unmarshalArgs(raw, &in); err != nil {
		return "", false, err
	}
	rel, perr := safeRelPath(in.Path)
	if perr != nil {
		return perr.Error(), true, nil
	}
	sess, ok := d.store.Get(in.SessionID, principal, d.now())
	if !ok {
		return "no such session", true, nil
	}
	content, derr := decodeContent(in.Content, in.Encoding)
	if derr != nil {
		return derr.Error(), true, nil
	}
	if err := d.eng.Write(ctx, sess.Name, rel, content); err != nil {
		return "write failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("wrote %d bytes to %s/%s", len(content), workDir, rel), false, nil
}

func (d *Dispatcher) read(ctx context.Context, principal string, raw json.RawMessage) (string, bool, error) {
	var in struct {
		SessionID string `json:"session_id"`
		Path      string `json:"path"`
		MaxBytes  int64  `json:"max_bytes"`
		Encoding  string `json:"encoding"`
	}
	if err := unmarshalArgs(raw, &in); err != nil {
		return "", false, err
	}
	rel, perr := safeRelPath(in.Path)
	if perr != nil {
		return perr.Error(), true, nil
	}
	sess, ok := d.store.Get(in.SessionID, principal, d.now())
	if !ok {
		return "no such session", true, nil
	}
	maxBytes := d.cfg.MaxOutBytes
	if in.MaxBytes > 0 && in.MaxBytes < maxBytes {
		maxBytes = in.MaxBytes
	}
	data, err := d.eng.Read(ctx, sess.Name, rel, maxBytes)
	if err != nil {
		return "read failed: " + err.Error(), true, nil
	}
	if in.Encoding == "base64" {
		return base64.StdEncoding.EncodeToString(data), false, nil
	}
	return string(data), false, nil
}

func (d *Dispatcher) close(ctx context.Context, principal string, raw json.RawMessage) (string, bool, error) {
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := unmarshalArgs(raw, &in); err != nil {
		return "", false, err
	}
	sess, ok := d.store.Get(in.SessionID, principal, d.now())
	if !ok {
		return "no such session (already closed?)", false, nil // idempotent
	}
	d.store.Remove(sess.ID)
	if err := d.eng.Close(ctx, sess.Name); err != nil {
		return "close reported: " + err.Error(), true, nil
	}
	return "closed " + sess.ID, false, nil
}

// touch resets a session's idle timer (RFC BI P2b keepalive) — a driver holding a
// container across a known gap calls it so the idle TTL doesn't reap the session
// mid-wait. Get() already refreshes LastUsed as a side effect; this is the explicit
// keepalive surface.
func (d *Dispatcher) touch(principal string, raw json.RawMessage) (string, bool, error) {
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := unmarshalArgs(raw, &in); err != nil {
		return "", false, err
	}
	sess, ok := d.store.Get(in.SessionID, principal, d.now())
	if !ok {
		return "no such session (open one with sandbox_open, or it may have expired)", true, nil
	}
	return "touched " + sess.ID + " — idle timer reset", false, nil
}

// closeRun bulk-closes every session the caller owns for a given loomcycle run
// tree (RFC BI P2b). Principal-scoped (a caller only closes its own) and requires
// a non-empty root_run_id. Idempotent — a run with no live sessions returns 0.
func (d *Dispatcher) closeRun(ctx context.Context, principal string, raw json.RawMessage) (string, bool, error) {
	var in struct {
		RootRunID string `json:"root_run_id"`
	}
	if err := unmarshalArgs(raw, &in); err != nil {
		return "", false, err
	}
	if in.RootRunID == "" {
		return "root_run_id is required", true, nil
	}
	closed := d.store.RemoveByRun(principal, in.RootRunID)
	for _, s := range closed {
		_ = d.eng.Close(ctx, s.Name) // best-effort; store row already removed
	}
	return fmt.Sprintf("closed %d session(s) for run %s", len(closed), in.RootRunID), false, nil
}

func (d *Dispatcher) list(principal string) (string, bool, error) {
	sessions := d.store.ListByPrincipal(principal)
	out := make([]map[string]any, 0, len(sessions))
	for _, s := range sessions {
		row := map[string]any{
			"session_id": s.ID,
			"network":    s.Network,
			"created_at": s.CreatedAt.UTC().Format(time.RFC3339),
			"last_used":  s.LastUsed.UTC().Format(time.RFC3339),
		}
		if s.Workspace != "" {
			row["workspace"] = s.Workspace
		}
		out = append(out, row)
	}
	b, _ := json.Marshal(map[string]any{"sessions": out})
	return string(b), false, nil
}

// clampOpen turns caller-requested values into an openOpts within operator
// ceilings. Zero / negative requests fall back to the defaults; over-ceiling
// requests are clamped down (never up).
func clampOpen(cfg *Config, network string, tmpfsMB int64, cpus float64, memMB, pids int64) openOpts {
	o := openOpts{
		Network: "none",
		TmpfsMB: cfg.DefTmpfsMB,
		CPUs:    cfg.DefCPUs,
		MemMB:   cfg.DefMemMB,
		Pids:    cfg.DefPids,
	}
	if network == "egress" {
		o.Network = "egress"
	}
	if tmpfsMB > 0 {
		o.TmpfsMB = tmpfsMB
	}
	if o.TmpfsMB > cfg.MaxTmpfsMB {
		o.TmpfsMB = cfg.MaxTmpfsMB
	}
	if cpus > 0 {
		o.CPUs = cpus
	}
	if o.CPUs > cfg.MaxCPUs {
		o.CPUs = cfg.MaxCPUs
	}
	if memMB > 0 {
		o.MemMB = memMB
	}
	if o.MemMB > cfg.MaxMemMB {
		o.MemMB = cfg.MaxMemMB
	}
	if pids > 0 {
		o.Pids = pids
	}
	if o.Pids > cfg.MaxPids {
		o.Pids = cfg.MaxPids
	}
	return o
}

// safeRelPath validates a caller-supplied workspace path. It must be a relative
// path that stays under /work — no absolute paths, no "..", no NUL/control
// bytes. Returns the cleaned relative path.
func safeRelPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path must be relative to %s (no leading /)", workDir)
	}
	for _, r := range p {
		if r == 0 || r < 0x20 {
			return "", fmt.Errorf("path contains a control character")
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path may not contain '..'")
		}
	}
	return p, nil
}

func decodeContent(s, encoding string) ([]byte, error) {
	if encoding == "base64" {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("content is not valid base64: %w", err)
		}
		return b, nil
	}
	return []byte(s), nil
}

func unmarshalArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// newID returns a 128-bit random hex id for a session — unguessable within a
// principal (and the store's principal check makes it useless across principals).
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Input schemas. Kept as raw JSON so they pass through tools/list verbatim.
var (
	schemaOpen = json.RawMessage(`{
		"type": "object",
		"properties": {
			"network": {"type": "string", "enum": ["none", "egress"], "description": "Network access. 'none' (default) fully isolates the sandbox; 'egress' allows outbound network only if the operator enabled it."},
			"tmpfs_mb": {"type": "integer", "description": "Size of the in-memory /work workspace in MiB (clamped to the operator maximum)."},
			"cpu": {"type": "number", "description": "CPU cores (clamped to the operator maximum)."},
			"mem_mb": {"type": "integer", "description": "Memory limit in MiB (clamped to the operator maximum)."},
			"pids": {"type": "integer", "description": "Max process count (clamped to the operator maximum)."},
			"workspace": {"type": "string", "description": "Name of a PERSISTENT workspace (durable /work that survives close + restart, for iterative dev — reopen with the same name to resume with your checkout + build cache intact). Omit for an in-memory tmpfs /work. Requires the operator to have enabled durable workspaces; [a-z0-9_-], starts alnum."}
		}
	}`)
	schemaExec = json.RawMessage(`{
		"type": "object",
		"properties": {
			"session_id": {"type": "string", "description": "The session from sandbox_open."},
			"command": {"type": "string", "description": "Shell command to run in /work."},
			"timeout_seconds": {"type": "integer", "description": "Per-command timeout (clamped to the operator maximum)."},
			"max_output_bytes": {"type": "integer", "description": "Cap on returned output bytes."}
		},
		"required": ["session_id", "command"]
	}`)
	schemaWrite = json.RawMessage(`{
		"type": "object",
		"properties": {
			"session_id": {"type": "string"},
			"path": {"type": "string", "description": "Destination path relative to /work."},
			"content": {"type": "string", "description": "File content (UTF-8, or base64 when encoding=base64)."},
			"encoding": {"type": "string", "enum": ["utf8", "base64"], "description": "Content encoding (default utf8)."}
		},
		"required": ["session_id", "path", "content"]
	}`)
	schemaRead = json.RawMessage(`{
		"type": "object",
		"properties": {
			"session_id": {"type": "string"},
			"path": {"type": "string", "description": "Source path relative to /work."},
			"max_bytes": {"type": "integer"},
			"encoding": {"type": "string", "enum": ["utf8", "base64"], "description": "Return encoding (base64 for binary files)."}
		},
		"required": ["session_id", "path"]
	}`)
	schemaClose = json.RawMessage(`{
		"type": "object",
		"properties": {"session_id": {"type": "string"}},
		"required": ["session_id"]
	}`)
	schemaTouch = json.RawMessage(`{
		"type": "object",
		"properties": {"session_id": {"type": "string", "description": "The session to keep alive (resets its idle timer)."}},
		"required": ["session_id"]
	}`)
	schemaCloseRun = json.RawMessage(`{
		"type": "object",
		"properties": {"root_run_id": {"type": "string", "description": "Close every session belonging to this run."}},
		"required": ["root_run_id"]
	}`)
	schemaList = json.RawMessage(`{"type": "object", "properties": {}}`)
)
