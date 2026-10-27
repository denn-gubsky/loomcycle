package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/dynvol"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// The File tool outside a run (RFC FF D16). An operator session has no inbox
// and no volume bindings of its own, so it acts on the inbox of a run it may
// read, named by run_id. The File tool does the work: this file builds the
// context it runs in, which holds that inbox as the tree's only ephemeral
// volume and nothing the caller did not earn.

// fileTwinOwnerOps are the ops that write: extract, rm and clear change the
// run's inbox, and keep writes into one of the tenant's volumes. Only the
// run's owner or an admin may make them; a tenant operator is the admin of
// its own tenant's runs (fileTwinAdmin). Anyone else who may read the run
// could otherwise empty, or fill, an inbox its run is still using, or write
// (and with overwrite replace) files in volumes it has no write grant on.
var fileTwinOwnerOps = map[string]bool{"extract": true, "rm": true, "clear": true, "keep": true}

// fileTwinArgs is the part of a twin call the File tool receives: the File
// tool's own input, less download and upload's fields. Its json names, with
// run_id, are connector.FileTwinFields (TestFileTwin_FieldsAreTheArgs). It is what is
// authorised and what is passed on, re-encoded from this struct, so the tool
// never reads a byte of the caller's input that was not checked here.
type fileTwinArgs struct {
	Op           string   `json:"op"`
	Path         string   `json:"path,omitempty"`
	From         string   `json:"from,omitempty"`
	To           string   `json:"to,omitempty"`
	Overwrite    bool     `json:"overwrite,omitempty"`
	Recursive    bool     `json:"recursive,omitempty"`
	Prefix       string   `json:"prefix,omitempty"`
	Images       *bool    `json:"images,omitempty"`
	MaxRows      int      `json:"max_rows,omitempty"`
	ExcerptChars int      `json:"excerpt_chars,omitempty"`
	Title        string   `json:"title,omitempty"`
	DocPath      string   `json:"doc_path,omitempty"`
	Scope        string   `json:"scope,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	// Source is import's link to where the document came from; the File
	// tool checks and masks it.
	Source *builtin.ImportSource `json:"source,omitempty"`
}

type fileTwinInput struct {
	RunID string `json:"run_id"`
	fileTwinArgs
}

// decodeFileTwinInput decodes a call strictly. encoding/json matches a key
// to a field whatever its case and lets a later duplicate win, so the struct
// a lenient decode authorises and the bytes a second decode runs can name
// different ops. Here every top-level key must be one of
// connector.FileTwinFields, spelled exactly, given once, with nothing after
// the object.
func decodeFileTwinInput(input json.RawMessage) (fileTwinInput, error) {
	var in fileTwinInput
	dec := json.NewDecoder(bytes.NewReader(input))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return in, errors.New("the input must be a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return in, err
		}
		key, _ := tok.(string)
		if !slices.Contains(connector.FileTwinFields, key) {
			return in, fmt.Errorf("unknown field %q", key)
		}
		if seen[key] {
			return in, fmt.Errorf("field %q more than once", key)
		}
		seen[key] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return in, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return in, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return in, errors.New("trailing data after the object")
	}
	// Every key is now an exact field name, given once, so this decode reads
	// the same fields the scan above checked.
	if err := json.Unmarshal(input, &in); err != nil {
		return in, err
	}
	return in, nil
}

// File runs one File-tool op on the inbox of the run named by run_id. The
// rules, in the order they are checked:
//
//   - The run is read through the gate every run read uses (get_run's): the
//     caller's tenant, an isolated member's own runs, a restricted token's
//     agents. Anything else is the same not-found a missing run gets.
//   - The inbox is the run tree's, at the path the runtime derives from the
//     dynamic root and the tree's root run id, and it is used only while the
//     tree's record still names it. A finished tree's inbox was deleted with
//     it, and the answer says so.
//   - extract, rm, clear and keep need the run's owner (the principal whose
//     subject is the run's user, in its tenant) or an admin, which a tenant
//     operator is for a run of its own tenant.
//   - import builds the Document in the caller's own scopes, the ones the MCP
//     document tool writes for this principal.
//   - keep copies into a dynamic volume of the caller's tenant, by name, which
//     must exist, be read-write, and be writable under the caller's access
//     rules. A configured volume is not reachable: the operator binds those
//     to agents, not to sessions.
func (s *Server) File(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	in, err := decodeFileTwinInput(input)
	if err != nil {
		return fileTwinRefusal(tools.CategoryValidation, "file: invalid input: "+err.Error(), "Pass a JSON object with run_id, op and the op's own fields."), nil
	}
	if !slices.Contains(connector.FileTwinOps, in.Op) {
		if in.Op == "download" || in.Op == "upload" {
			return fileTwinRefusal(tools.CategoryValidation, "file: "+in.Op+" is not available here: it needs a run's HTTP host policy",
				"Give the agent the File and HTTP tools and let the run do it."), nil
		}
		return fileTwinRefusal(tools.CategoryValidation, fmt.Sprintf("file: unknown op %q", in.Op),
			"One of stat, ls, extract, import, keep, rm, clear."), nil
	}
	if !validIdent(in.RunID) {
		return fileTwinRefusal(tools.CategoryValidation, "file: run_id must match [A-Za-z0-9_-]{1,128}", "Pass the run's id, as get_run and list_runs report it."), nil
	}
	if res, ok := fileTwinInboxOnly(in); !ok {
		return res, nil
	}
	run, res, ok, err := s.fileTwinReadableRun(ctx, in.RunID)
	if err != nil || !ok {
		return res, err
	}

	p, hasPrincipal := auth.PrincipalFromContext(ctx)
	admin := fileTwinAdmin(p, hasPrincipal, run)
	owner := hasPrincipal && p.Subject != "" && p.Subject == run.UserID && p.TenantID == run.TenantID
	if fileTwinOwnerOps[in.Op] && !admin && !owner {
		return fileTwinRefusal(tools.CategoryPermission, "file: only the run's owner or an admin may "+in.Op+" in its inbox",
			"Read it with stat or ls, or import a Markdown file of it into your own documents."), nil
	}

	box, res, ok := s.fileTwinHoldInbox(ctx, run)
	if !ok {
		return res, nil
	}
	defer box.release()

	tctx, res, ok := s.fileTwinCtx(ctx, in, box.root, box.dir, box.dynRoot, p, admin || owner)
	if !ok {
		return res, nil
	}
	body, err := json.Marshal(in.fileTwinArgs)
	if err != nil {
		return connector.ToolResult{}, err
	}
	return s.dispatchBuiltin(tctx, builtin.FileToolName, body)
}

// fileTwinReadableRun reads the run named runID through the gate every run
// read uses (get_run's): the caller's tenant, an isolated member's own runs, a
// restricted token's agents. A run the caller may not read is the same
// not-found a missing run gets. err is a store fault.
func (s *Server) fileTwinReadableRun(ctx context.Context, runID string) (store.Run, connector.ToolResult, bool, error) {
	if s.store == nil {
		return store.Run{}, connector.ToolResult{}, false, errors.New("file requires persistence (no Store configured)")
	}
	run, err := s.tenantStore(ctx).GetRun(ctx, runID)
	var nf *store.ErrNotFound
	switch {
	case errors.As(err, &nf):
		return store.Run{}, fileTwinNotFound(runID), false, nil
	case err != nil:
		return store.Run{}, connector.ToolResult{}, false, err
	}
	if !runOwnershipOK(ctx, run) || !s.allowlistReachesRow(ctx, run) {
		return store.Run{}, fileTwinNotFound(runID), false, nil
	}
	return run, connector.ToolResult{}, true, nil
}

// fileTwinInbox is a readable run's tree inbox, held: release must be called.
type fileTwinInbox struct {
	root, dir, dynRoot string
	release            func()
}

// fileTwinHoldInbox finds the inbox of run's tree and holds the tree, so a
// tree that ends meanwhile is purged after the caller is done rather than
// under it (the last hold pays the purge). When it is not ok, nothing is held.
func (s *Server) fileTwinHoldInbox(ctx context.Context, run store.Run) (fileTwinInbox, connector.ToolResult, bool) {
	dynRoot, ok := builtin.DynamicVolumeRoot(s.cfg())
	if !ok {
		return fileTwinInbox{}, fileTwinRefusal(tools.CategoryBusiness, "file: no dynamic_root volume is configured, so no run keeps files",
			"An operator must mark one volume dynamic_root: true."), false
	}
	// A sub-run shares its tree's inbox. A chain that cannot be followed
	// leaves the run its own root, where no inbox is found: never a guess at
	// another tree's.
	root, _ := resumedRootRunID(ctx, s.store.GetRun, run)
	// The root's id names a folder under dynamic_root. The run's own id was
	// checked at the wire, but a parent's comes from a stored row, so check it
	// again here rather than trust the store with a path segment.
	dir := dynvol.DerivedEphemeralPath(dynRoot, root, inbox.Name)
	if !validIdent(root) || dynvol.AssertInsideRoot(dynRoot, dir) != nil {
		return fileTwinInbox{}, fileTwinNotFound(run.ID), false
	}
	release := s.holdRunTree(root)
	if res, ok := s.fileTwinInboxPresent(ctx, run, root, dir); !ok {
		release()
		return fileTwinInbox{}, res, false
	}
	return fileTwinInbox{root: root, dir: dir, dynRoot: dynRoot, release: release}, connector.ToolResult{}, true
}

// fileTwinAdmin reports whether the caller is an admin of the run for the
// writing ops: no principal (open mode), the legacy operator, substrate:admin,
// or a substrate:tenant operator of the run's own tenant. The tenant check is
// repeated here although the read gate already folds another tenant's run
// into not-found, so this rule alone never reaches across tenants.
func fileTwinAdmin(p auth.Principal, hasPrincipal bool, run store.Run) bool {
	if !hasPrincipal || p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return true
	}
	return auth.HasScope(p.Scopes, auth.ScopeTenant) && p.TenantID == run.TenantID
}

// fileTwinInboxOnly refuses a path outside the inbox before anything is read:
// over this surface the run's inbox is the one volume there is, besides the
// target of keep.
func fileTwinInboxOnly(in fileTwinInput) (connector.ToolResult, bool) {
	check := func(field, v string) (connector.ToolResult, bool) {
		v = strings.TrimSpace(v)
		if v == "" || !strings.HasPrefix(v, "volume:") || strings.HasPrefix(v, "volume:"+inbox.Name+":") {
			return connector.ToolResult{}, true
		}
		return fileTwinRefusal(tools.CategoryValidation, fmt.Sprintf("file: %s %q is not in the run's inbox", field, v),
			"Name a file of the inbox: volume:inbox:<path>, or a bare path."), false
	}
	switch in.Op {
	case "keep":
		return check("from", in.From)
	default:
		return check("path", in.Path)
	}
}

// fileTwinInboxPresent reports whether the tree's inbox exists here: recorded
// for the tree and present at its derived path as a real directory. Absent,
// the answer says why: the tree ended (the inbox went with it), nothing was
// kept yet, or it was made on another server.
func (s *Server) fileTwinInboxPresent(ctx context.Context, run store.Run, root, dir string) (connector.ToolResult, bool) {
	rows, err := s.store.EphemeralVolumeListByRun(ctx, root)
	if err != nil {
		return fileTwinRefusal(tools.CategoryTransient, "file: could not read the run's volumes", "Try again."), false
	}
	recorded := false
	for _, r := range rows {
		if r.Name == inbox.Name {
			recorded = true
			break
		}
	}
	// #nosec G703 -- dir is derived, never taken from input: dynamic_root (operator
	// config) + "_ephemeral" + a run id checked against [A-Za-z0-9_-] and kept
	// inside the root (fileTwinHoldInbox) + the reserved name "inbox".
	fi, statErr := os.Lstat(dir)
	if recorded && statErr == nil && fi.IsDir() {
		return connector.ToolResult{}, true
	}
	if recorded && errors.Is(statErr, os.ErrNotExist) {
		return fileTwinRefusal(tools.CategoryBusiness, fmt.Sprintf("file: run %s's inbox was made on another server and is not on this one", run.ID),
			"Ask the server that runs it."), false
	}
	if recorded {
		return fileTwinRefusal(tools.CategoryBusiness, fmt.Sprintf("file: run %s's inbox is not a directory here", run.ID), ""), false
	}
	status := run.Status
	if root != run.ID {
		if r, err := s.store.GetRun(ctx, root); err == nil {
			status = r.Status
		}
	}
	if store.IsTerminalRunStatus(status) {
		return fileTwinRefusal(tools.CategoryValidation, fmt.Sprintf("file: run %s's inbox is gone: it is deleted when the run ends", run.ID),
			"Copy a file out with keep while the run is still going, or have the agent keep it."), false
	}
	return fileTwinRefusal(tools.CategoryValidation, fmt.Sprintf("file: nothing has been kept in run %s's inbox yet", run.ID),
		"A file one of its tools returns lands there; until one does, the inbox does not exist."), false
}

// fileTwinCtx is the context the File tool runs in for one twin call:
//
//   - the caller's own identity and policies (as the MCP session stamped
//     them), with the tree's root run id, so the inbox writer that extract
//     uses finds the tree;
//   - an ephemeral volume set holding only the inbox, at its derived path.
//     It is read-only unless the call is rm or clear by the owner or an admin;
//     extract writes through the inbox writer (a runtime write), never
//     through the set. No reader of the tree's other ephemeral volumes is
//     attached, so no other name of the tree resolves;
//   - the tree's inbox state when a run of the tree is live in this process
//     (its index and its cap), else a fresh one that counts the files on disk;
//   - the tools the op needs and nothing else: File, plus Document for import
//     (the Document tool builds it, with its own scope checks) and Write for
//     keep, whose only volume is the named target.
func (s *Server) fileTwinCtx(ctx context.Context, in fileTwinInput, root, dir, dynRoot string, caller auth.Principal, mayChange bool) (context.Context, connector.ToolResult, bool) {
	ident := tools.RunIdentity(ctx)
	ident.RootRunID = root
	ctx = tools.WithRunIdentity(ctx, ident)

	writable := mayChange && (in.Op == "rm" || in.Op == "clear")
	set := tools.NewEphemeralVolumeSet()
	set.Add(inbox.Name, tools.EphemeralVolumeRef{Root: dir, ReadOnly: !writable})
	ctx = tools.WithEphemeralVolumes(ctx, set)
	// A write asks the tree's record before it trusts the set: an inbox made
	// read-only by its run stays read-only here.
	ctx = tools.WithEphemeralModeCheck(ctx, s.ephemeralModeCheck(root))

	tree, live := s.treeSets.liveInbox(root)
	if !live {
		tree = inbox.NewTree()
	}
	ctx = inbox.With(ctx, s.inboxService(), tree)

	granted := []string{builtin.FileToolName}
	vols := tools.VolumePolicyValue{Active: true}
	switch in.Op {
	case "import":
		granted = append(granted, "Document")
	case "keep":
		binding, res, ok := s.fileTwinKeepTarget(ctx, in.To, dynRoot, caller)
		if !ok {
			return nil, res, false
		}
		granted = append(granted, "Write")
		vols.Bindings = []tools.VolumeBinding{binding}
	}
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Tools = granted
		rp.Volumes = vols
		rp.EphemeralWritable = writable
	})
	return ctx, connector.ToolResult{}, true
}

// fileTwinKeepTarget resolves keep's "to" to a dynamic volume of the caller's
// tenant: named, recorded, read-write, at the path the runtime derives for it
// (never the one a row says), and writable under the caller's access rules —
// the narrowing a run of the caller gets (tools.NarrowVolumesByACL), so a
// rule that lets the caller only read the volume refuses the copy here too.
// The auth model has no scope for writing a volume; the rules are where that
// grant lives.
func (s *Server) fileTwinKeepTarget(ctx context.Context, to, dynRoot string, caller auth.Principal) (tools.VolumeBinding, connector.ToolResult, bool) {
	tenant := caller.TenantID
	refuse := func(cat tools.ErrorCategory, msg, next string) (tools.VolumeBinding, connector.ToolResult, bool) {
		return tools.VolumeBinding{}, fileTwinRefusal(cat, msg, next), false
	}
	rest, ok := strings.CutPrefix(strings.TrimSpace(to), "volume:")
	name, _, named := strings.Cut(rest, ":")
	if !ok || !named || name == "" {
		return refuse(tools.CategoryValidation, "file: keep: to must name a volume: volume:<name>:<path>",
			"Name a dynamic volume of your tenant (volumedef list shows them).")
	}
	if name == inbox.Name {
		return refuse(tools.CategoryValidation, "file: keep: to is in the inbox, which is deleted with the run", "Name a dynamic volume of your tenant.")
	}
	// A dynamic volume is shared by the tenant, and an isolated user is held
	// to its own scope. Its baseline allows writing volumes because inside a
	// run the agent definition picks them; here the caller would pick any.
	if auth.IsIsolated(caller, true) {
		return refuse(tools.CategoryPermission, "file: keep: an isolated user may not write to its tenant's volumes",
			"Import a Markdown file of it into your own documents, or run an agent bound to the volume.")
	}
	if _, static := s.cfg().Volumes[name]; static {
		return refuse(tools.CategoryPermission, fmt.Sprintf("file: keep: %q is a configured volume; from here keep copies only into a dynamic volume of your tenant", name),
			"Create one with volumedef, or have an agent bound to that volume keep the file.")
	}
	if err := dynvol.ValidName(name); err != nil {
		return refuse(tools.CategoryValidation, "file: keep: "+err.Error(), "")
	}
	row, err := s.store.VolumeDefGetByName(ctx, tenant, name)
	var nf *store.ErrNotFound
	switch {
	case errors.As(err, &nf):
		return refuse(tools.CategoryValidation, fmt.Sprintf("file: keep: your tenant has no volume named %q", name),
			"Create it with volumedef op=create, or name one volumedef list shows.")
	case err != nil:
		return refuse(tools.CategoryTransient, "file: keep: could not read the volume", "Try again.")
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(row.Definition, &body); err != nil {
		return refuse(tools.CategoryBusiness, fmt.Sprintf("file: keep: volume %q has an unreadable record", name), "")
	}
	if body.Mode == "ro" {
		return refuse(tools.CategoryPermission, fmt.Sprintf("file: keep: volume %q is read-only", name), "Name a read-write volume.")
	}
	binding := tools.VolumeBinding{Name: name, Root: dynvol.DerivedPath(dynRoot, tenant, name)}
	narrowed := tools.NarrowVolumesByACL(ctx, caller.Effective, tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{binding}})
	if len(narrowed.Bindings) != 1 || narrowed.Bindings[0].ReadOnly {
		return refuse(tools.CategoryPermission, fmt.Sprintf("file: keep: your access rules do not let you write to volume %q", name),
			"Ask an operator for write access to the volume, or name another.")
	}
	return binding, connector.ToolResult{}, true
}

func fileTwinNotFound(runID string) connector.ToolResult {
	return fileTwinRefusal(tools.CategoryValidation, fmt.Sprintf("file: run %s not found", runID), "Check the id with list_runs.")
}

func fileTwinRefusal(cat tools.ErrorCategory, msg, next string) connector.ToolResult {
	return connector.ToolResult{Text: msg, IsError: true, ErrorInfo: &tools.ErrorInfo{
		Category: cat, Retryable: cat == tools.CategoryTransient, Description: next,
	}}
}
