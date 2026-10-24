package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/dynvol"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// VolumeDef is the RFC AH Phase 2a substrate primitive for runtime-mutable,
// tenant-scoped, CONFINED filesystem volumes. Unlike the static `volumes:`
// config (operator-authored, may map anywhere), a dynamic volume is
// provisioned by the runtime INSIDE an operator-blessed parent (the static
// volume marked `dynamic_root: true`) and can never escape it.
//
// The security crux: `create` takes a NAME + MODE only — NEVER a
// caller-supplied host path. The runtime derives
//
//	path = <dynamic_root>/<tenant-segment>/<name>
//
// where tenant-segment is the tenant id, or "_shared" for the shared tenant
// "". So there is NO caller-controlled path anywhere — the os.RemoveAll in
// `purge` can only ever target a runtime-derived path under the dynamic
// root (and it RE-DERIVES rather than trusts the stored path; see purge).
//
// Op set is flat (create / get / list / delete / purge) — NOT the
// content-addressed retire/promote/fork lifecycle (a Volume points at
// mutable state outside the def, so versioning is meaningless).
//
//   - create {name, mode}  — capability-gated; derive + MkdirAll the path
//     inside the dynamic root; persist {path,mode}. Idempotent: identical
//     re-create is a no-op-OK; a different mode updates the row.
//   - get {name} / list    — tenant-scoped reads; opaque-404 cross-tenant.
//   - delete {name}        — capability-gated; remove the row only (leave
//     files on disk).
//   - purge {name}         — capability-gated; remove the row AND
//     os.RemoveAll the directory, behind the four-way fence (see execPurge).
//
// Tenant is tools.RunIdentity(ctx).TenantID (authoritative from ctx, never
// the wire), exactly like the other Def families.
type VolumeDef struct {
	// Store is the persistence backend. Required.
	Store store.Store
	// Cfg is the loaded operator config — used to resolve the dynamic_root
	// (the operator-blessed parent) and to refuse a name colliding with a
	// static cfg.Volumes entry. Required.
	Cfg *config.Config
	// MaxNameLen caps the volume name length. 0 → the regex's own 64-char
	// ceiling applies.
	MaxNameLen int
	// Git is what the operator allows for a volume created from a git source
	// (volumedef_gitsource.go). Nil means the feature is off.
	Git *GitSourceConfig
}

const volumeDefDescription = `Provision, inspect, and remove CONFINED dynamic filesystem volumes at runtime (see Context op=help topic=volumedef). ` +
	`A volume is created by NAME + MODE only — the runtime derives the path inside an operator-blessed ` +
	`parent (<dynamic_root>/<tenant>/<name>); you never supply a host path. Static yaml volumes: are the ` +
	`operator's ground truth and cannot be created over. Operations: create, get, list, delete, purge, set_mode. ` +
	`delete removes the mapping but LEAVES files on disk; purge removes the mapping AND deletes the ` +
	`directory tree (fenced: it can only ever delete a runtime-derived path strictly inside the dynamic root). ` +
	`set_mode {name, mode: "ro"} makes one of this run's EPHEMERAL volumes read-only for every agent of the run, ` +
	`yourself included, and cannot be undone. create with ephemeral: true and a source {git: {url, commit, credential}} ` +
	`fills the new volume with one commit of a repository: the runtime fetches it, so the token never reaches an agent.`

const volumeDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":   {"type": "string", "enum": ["create","get","list","delete","purge","set_mode"]},
    "name": {"type": "string", "description": "Dynamic volume name (required for create/get/delete/purge/set_mode). Charset ^[a-z0-9][a-z0-9_-]{0,63}$ — no slashes or dots."},
    "mode": {"type": "string", "enum": ["rw","ro"], "description": "Access mode for create (default rw). rw allows Write/Edit/Bash; ro is read-only. For set_mode it must be ro: an ephemeral volume can be made read-only and never writable again."},
    "ephemeral": {"type": "boolean", "description": "create only: make a RUN-SCOPED ephemeral volume instead of a persistent tenant volume. Resolvable by this run + its sub-agents; auto-purged when the top-level run completes. Requires an active run."},
    "source": {"type": "object", "description": "create with ephemeral: true only: fill the new volume with one commit of a git repository, fetched by the runtime so no agent holds the token. Shape: {\"git\": {\"url\": \"https://host/org/repo\", \"commit\": \"<40-digit sha>\", \"ref\": \"refs/heads/x\" (optional, where to find the commit), \"credential\": \"$cred:<name>\" or \"$ghapp:<name>\" (omit for a public repository), \"max_bytes\": <n> (optional, may only lower the limit), \"diff_base\": \"merge-base:default\" or a 40-digit sha (optional: also write a diff from that commit to this one, as _meta/diff.patch and _meta/numstat.json, or the names given in \"outputs\": {\"diff\", \"numstat\"} under _meta/)}}. The credential must be restricted to URLs covering this one. The answer gives counts, never file content; what was left out of the tree is listed in _meta/omitted.json. Needs \"source:git\" in volume_def_scopes and a host the operator allows."}
  },
  "required": ["op"]
}`

type volumeDefInput struct {
	Op        string `json:"op"`
	Name      string `json:"name,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Ephemeral bool   `json:"ephemeral,omitempty"`
	// MountAt (RFC AL) is the Path-tree mount point for this volume; defaults
	// to "/vol/<name>". Registers a volume_mount dirent in the tenant scope.
	MountAt string `json:"mount_at,omitempty"`
	// Source fills a new ephemeral volume from elsewhere; today, one commit
	// of a git repository (volumedef_gitsource.go). Kept raw so it can be
	// decoded strictly, apart from the tool's other arguments.
	Source json.RawMessage `json:"source,omitempty"`
}

// Name implements tools.Tool.
func (v *VolumeDef) Name() string { return "VolumeDef" }

// Description implements tools.Tool.
func (v *VolumeDef) Description() string { return volumeDefDescription }

// InputSchema implements tools.Tool.
func (v *VolumeDef) InputSchema() json.RawMessage { return json.RawMessage(volumeDefInputSchema) }

// Execute implements tools.Tool.
func (v *VolumeDef) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if v.Store == nil {
		return errResult("VolumeDef tool: not configured (no Store backend)"), nil
	}
	if v.Cfg == nil {
		return errResult("VolumeDef tool: not configured (no Config — dynamic root unavailable)"), nil
	}
	// The shared tenant ("") maps to the reserved on-disk segment
	// dynvol.SharedTenantSegment. Refuse a real tenant whose minted id is literally
	// that string, so the reserved segment can never be shared by two distinct
	// tenants (which would let one purge the other's volume tree). The tenant
	// is authoritative from the principal, never the wire.
	//
	// _ephemeral is RESERVED the same way: a tenant id literally equal to it
	// would let a persistent volume's <root>/_ephemeral/<name> tree collide
	// with the run-scoped <root>/_ephemeral/<run_id>/ subtree, blurring the
	// two purge fences. Reject it up front.
	//
	// The same check refuses a tenant id that is not one charset-clean path
	// segment ("x/.." derives another tenant's tree), before ANY op — the
	// ephemeral ops record the tenant too, and purge derives its path from it.
	if err := dynvol.ValidTenant(tools.RunIdentity(ctx).TenantID); err != nil {
		return errResult(fmt.Sprintf("VolumeDef tool: %s", err)), nil
	}
	var in volumeDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	// RFC EZ §1.2: the op's own verb, asked of the volume definition it names.
	if res, refused := tools.AuthorizeDefOp(ctx, "volume", in.Op, in.Name); refused {
		return res, nil
	}
	switch in.Op {
	case "create":
		return v.execCreate(ctx, in)
	case "get":
		return v.execGet(ctx, in)
	case "list":
		return v.execList(ctx, in)
	case "delete":
		return v.execDelete(ctx, in)
	case "purge":
		return v.execPurge(ctx, in)
	case "set_mode":
		return v.execSetMode(ctx, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, get, list, delete, purge, set_mode)", in.Op)), nil
	}
}

// ---- create ----

func (v *VolumeDef) execCreate(ctx context.Context, in volumeDefInput) (tools.Result, error) {
	if err := v.checkScopeForName(ctx, in.Name); err != nil {
		return errResult(err.Error()), nil
	}
	if err := v.validateName(in.Name); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	// RFC FF D9: the run inbox is an ephemeral volume the runtime makes under
	// this name. One an agent made would be resolved as the inbox (and a
	// persistent one would refuse every capture), so no create may take it.
	if in.Name == inbox.Name {
		return errResult(fmt.Sprintf("create: the name %q is reserved for the run's inbox, where the runtime keeps files your tools return", inbox.Name)), nil
	}
	mode := in.Mode
	if mode == "" {
		mode = "rw"
	}
	if dynvol.ValidMode(mode) != nil {
		return errResult(fmt.Sprintf("create: invalid mode %q (want rw or ro)", in.Mode)), nil
	}
	// Static cfg.Volumes is ground truth — refuse a name that collides with
	// an operator-declared static volume (mirrors MCPServerDef refusing a
	// static-name collision). Applies to BOTH persistent and ephemeral
	// volumes: an ephemeral volume must never shadow an operator-blessed
	// static name. The resolver also puts static first, so this is
	// belt-and-braces.
	if _, ok := v.Cfg.Volumes[in.Name]; ok {
		return errResult(fmt.Sprintf("create: name %q matches a static volumes: entry — yaml is ground truth; use a different name", in.Name)), nil
	}
	// A cancelled run can still dispatch a tool (a background child cancelled
	// as its root ends). Both paths below make the directory BEFORE the
	// ctx-bound row insert, which would then fail and leave a directory no row
	// names; for an ephemeral volume that lands after the root's purge, where
	// nothing sweeps it. Refuse before any filesystem side effect.
	if err := ctx.Err(); err != nil {
		return errResult(fmt.Sprintf("create: run is ending: %s", err)), nil
	}

	if len(in.Source) > 0 {
		return v.execCreateFromGit(ctx, in, mode)
	}
	if in.Ephemeral {
		return v.execCreateEphemeral(ctx, in, mode)
	}

	dynRoot, err := v.dynamicRoot()
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	tenantID := tools.RunIdentity(ctx).TenantID
	// Derive + fence + MkdirAll through the one provisioning helper snapshot
	// restore also uses, so a restored volume lands exactly where this would.
	path, created, err := dynvol.Provision(dynRoot, tenantID, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	// A directory this call made but could not record is one no row names.
	// Remove only that one, and only while empty: a re-create that reused an
	// existing directory must leave it (and its content) where it was. And
	// only while no row names it — an insert can commit before its re-read
	// fails, and a concurrent create of the same name can record the
	// directory this call made — looked up on a ctx that a cancel cannot
	// turn into a false "not found".
	undo := func() {
		if !created {
			return
		}
		var nf *store.ErrNotFound
		if _, err := v.Store.VolumeDefGetByName(context.WithoutCancel(ctx), tenantID, in.Name); errors.As(err, &nf) {
			dynvol.RemoveEmptyFenced(dynRoot, path)
		}
	}

	body, err := json.Marshal(dynvol.Body{Path: path, Mode: mode})
	if err != nil {
		undo()
		return errResult(fmt.Sprintf("create: marshal: %s", err)), nil
	}
	row, err := v.Store.VolumeDefCreate(ctx, store.VolumeDefRow{
		TenantID:   tenantID,
		Name:       in.Name,
		Definition: body,
	})
	if err != nil {
		undo()
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	resp := volumeDefRowResponse(row, mode)
	// RFC AL — register the volume's Path-tree mount (tenant-scoped). The
	// VolumeDef row is already durable; a dirent failure is a warning so the
	// volume isn't lost. Default mount is /vol/<name>.
	mountAt := in.MountAt
	if mountAt == "" {
		mountAt = "/vol/" + in.Name
	}
	if mp, perr := v.registerVolumeMount(ctx, tenantID, in.Name, mode, mountAt); perr != nil {
		resp["mount_warning"] = fmt.Sprintf("volume created but mount registration failed: %s", perr)
	} else {
		resp["mount_at"] = mp
	}
	return okJSON(resp)
}

// registerVolumeMount registers (upserts) a volume_mount dirent for the volume
// in the tenant-scoped Path tree. Returns the canonical mount path.
func (v *VolumeDef) registerVolumeMount(ctx context.Context, tenantID, name, mode, rawMount string) (string, error) {
	canonical, err := normalizePath(rawMount)
	if err != nil {
		return "", err
	}
	parent, leaf, isRoot := splitPath(canonical)
	if isRoot {
		return "", fmt.Errorf("mount_at may not be the root path")
	}
	ref, _ := json.Marshal(map[string]any{"volume_name": name, "mode": mode})
	if _, err := v.Store.DirentCreate(ctx, store.DirentRow{
		TenantID: tenantID, Scope: "tenant", ScopeID: "",
		ParentPath: parent, Name: leaf, Kind: "volume_mount", ResourceRef: ref,
	}); err != nil {
		return "", err
	}
	return canonical, nil
}

// execCreateEphemeral provisions a RUN-TREE-SCOPED ephemeral volume (RFC AH
// Phase 2b). Derivation: <dynamic_root>/_ephemeral/<root_run_id>/<name>.
// Run ids are globally unique, so two runs (any tenant) never collide. The
// volume is added to the run's in-memory EphemeralVolumeSet (the resolution
// source) AND persisted to ephemeral_volume_defs for durable crash-cleanup;
// it gets NO tenant-shared active pointer. name/mode/static-collision were
// already validated by execCreate.
func (v *VolumeDef) execCreateEphemeral(ctx context.Context, in volumeDefInput, mode string) (tools.Result, error) {
	rootRunID := tools.RunIdentity(ctx).RootRunID
	if rootRunID == "" {
		return errResult("create: ephemeral volumes require an active run (no root run id on context)"), nil
	}
	set := tools.EphemeralVolumes(ctx)
	if set == nil {
		return errResult("create: ephemeral volumes are unavailable for this run (no ephemeral set on context)"), nil
	}
	if res, refused := v.refuseEphemeralDuplicate(ctx, set, rootRunID, in.Name); refused {
		return res, nil
	}

	dynRoot, err := v.dynamicRoot()
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	// Same fenced provisioning as the persistent path.
	path, _, undo, err := dynvol.ProvisionEphemeral(dynRoot, rootRunID, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	resp, err := v.recordEphemeral(ctx, set, rootRunID, in.Name, path, mode)
	if err != nil {
		// The sweeper finds ephemeral directories by their rows, so one this
		// call made but could not record would never be removed.
		undo()
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	return okJSON(resp)
}

// refuseEphemeralDuplicate reports whether an ephemeral create of name must
// be refused because the name is taken: by an ephemeral volume of this run
// tree, or by a persistent volume the name would shadow.
func (v *VolumeDef) refuseEphemeralDuplicate(ctx context.Context, set *tools.EphemeralVolumeSet, rootRunID, name string) (tools.Result, bool) {
	// Refuse a duplicate ephemeral name within THIS run tree. (A different
	// run's identically-named ephemeral volume is fine — they're run-scoped.)
	if set.Has(name) {
		return errResult(fmt.Sprintf("create: ephemeral volume %q already exists in this run", name)), true
	}
	// The set can lack a volume the run tree does have: a resumed run whose
	// volumes could not be read back starts with an empty one. Creating over
	// the stored volume would hand out its directory again under whatever
	// mode this call asks for, a read-only volume included. So the rows
	// decide: the create is refused like any other duplicate, and the volume
	// is taken into the set where THIS replica places it (the records
	// reader), never at the row's path, which is whatever the replica that
	// wrote it had; one whose files are on another replica stays out. A
	// failed lookup is not evidence that there is none, so it refuses too.
	recorded, err := v.Store.EphemeralVolumeListByRun(ctx, rootRunID)
	if err != nil {
		return errResult(fmt.Sprintf("create: could not check this run's ephemeral volumes: %s", err)), true
	}
	for _, r := range recorded {
		if r.Name != name {
			continue
		}
		if read := tools.EphemeralRecordsFrom(ctx); read != nil {
			if recs, err := read(ctx); err == nil {
				for _, rec := range recs {
					if rec.Name == name && !rec.Elsewhere && rec.Root != "" {
						set.Add(name, tools.EphemeralVolumeRef{Root: rec.Root, ReadOnly: rec.ReadOnly})
					}
				}
			}
		}
		return errResult(fmt.Sprintf("create: ephemeral volume %q already exists in this run", name)), true
	}
	// Refuse a name that collides with a PERSISTENT dynamic volume (the static
	// collision is already refused in execCreate above). effectiveRoot resolves
	// ephemeral FIRST, so an ephemeral volume shadowing a durable same-named one
	// would silently route the agent's writes into the run-scoped tree — which
	// is purged at run completion (silent data loss). Names must be unambiguous.
	// Only not-found clears the check: any other fault (a cancelled ctx among
	// them) is not evidence there is no collision, so it refuses.
	tenantID := tools.RunIdentity(ctx).TenantID
	if res, refused := v.refuseEphemeralCollision(ctx, tenantID, name, "an existing"); refused {
		return res, true
	}
	if tenantID != "" {
		if res, refused := v.refuseEphemeralCollision(ctx, "", name, "a shared"); refused {
			return res, true
		}
	}
	return tools.Result{}, false
}

// recordEphemeral records an ephemeral volume whose directory is in place:
// the row first, then the run tree's set, so a failed persist never leaves a
// volume that resolves and is not recorded. It returns the create's answer.
func (v *VolumeDef) recordEphemeral(ctx context.Context, set *tools.EphemeralVolumeSet, rootRunID, name, path, mode string) (map[string]any, error) {
	body, err := json.Marshal(dynvol.Body{Path: path, Mode: mode})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	row, err := v.Store.EphemeralVolumeCreate(ctx, store.EphemeralVolumeDefRow{
		RootRunID:  rootRunID,
		Name:       name,
		TenantID:   tools.RunIdentity(ctx).TenantID,
		Definition: body,
	})
	if err != nil {
		return nil, err
	}
	// The mode comes from the stored row, not from this call: two creates of
	// one name can race past the checks above, and the row keeps read-only.
	var stored dynvol.Body
	if err := json.Unmarshal(row.Definition, &stored); err == nil && stored.Mode != "" {
		mode = stored.Mode
	}
	set.Add(name, tools.EphemeralVolumeRef{Root: path, ReadOnly: mode == "ro"})
	return map[string]any{
		"name":       row.Name,
		"path":       path,
		"mode":       mode,
		"ephemeral":  true,
		"created_at": row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
	}, nil
}

// ---- set_mode ----

// execSetMode makes one of the run's ephemeral volumes read-only.
//
// It goes one way only. An agent that fills a volume and then hands it to
// others needs to fix it; nothing needs the reverse, and a way back would let
// any run of the tree undo a guarantee another run relies on. Because the
// operation can only take access away, it does not ask who created the
// volume: the usual volume_def_scopes grant for the name is enough.
//
// The mode lives in two places and both change: the row, which a resumed run
// rebuilds its volumes from, and the set every run of the tree resolves
// through, so the caller and children already running lose write access on
// their next call. The row goes first, so a failed write leaves the volume
// as it was everywhere.
func (v *VolumeDef) execSetMode(ctx context.Context, in volumeDefInput) (tools.Result, error) {
	if err := v.checkScopeForName(ctx, in.Name); err != nil {
		return errResult(err.Error()), nil
	}
	if err := v.validateName(in.Name); err != nil {
		return errResult(fmt.Sprintf("set_mode: %s", err)), nil
	}
	switch in.Mode {
	case "ro":
	case "rw":
		return errResult("set_mode: a volume cannot be made writable again; set_mode only takes mode \"ro\""), nil
	default:
		return errResult(fmt.Sprintf("set_mode: mode must be \"ro\" (got %q)", in.Mode)), nil
	}
	rootRunID := tools.RunIdentity(ctx).RootRunID
	set := tools.EphemeralVolumes(ctx)
	if rootRunID == "" || set == nil {
		return errResult("set_mode: ephemeral volumes exist only inside a run, and this call is not in one"), nil
	}
	ref, ok := set.Get(in.Name)
	if !ok {
		// Possibly created by a run of the tree on another replica after
		// this one started: the record has it. The switch below writes the
		// record, so a volume whose files are elsewhere is still closed for
		// every replica.
		var rerr error
		if ref, ok, rerr = recordedEphemeralForMode(ctx, in.Name); rerr != nil {
			return errResult(fmt.Sprintf("set_mode: %s", rerr)), nil
		}
	}
	if !ok {
		return errResult(fmt.Sprintf("set_mode: this run has no ephemeral volume %q; set_mode applies to ephemeral volumes only (re-create a persistent volume with the mode you want)", in.Name)), nil
	}
	resp := map[string]any{"name": in.Name, "mode": "ro", "ephemeral": true}
	if ref.ReadOnly {
		return okJSON(resp) // already read-only: nothing to change
	}
	found, err := v.Store.EphemeralVolumeSetReadOnly(ctx, rootRunID, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("set_mode: %s", err)), nil
	}
	if !found {
		return errResult(fmt.Sprintf("set_mode: ephemeral volume %q is no longer recorded; the run is ending", in.Name)), nil
	}
	set.SetReadOnly(in.Name)
	return okJSON(resp)
}

// refuseEphemeralCollision reports whether an ephemeral create of name must be
// refused because of a persistent dynamic volume in tenantID: it exists, or
// the lookup failed for any reason other than not-found. which names the
// tenant in the message ("an existing" / "a shared").
func (v *VolumeDef) refuseEphemeralCollision(ctx context.Context, tenantID, name, which string) (tools.Result, bool) {
	_, err := v.Store.VolumeDefGetByName(ctx, tenantID, name)
	if err == nil {
		return errResult(fmt.Sprintf("create: %q collides with %s dynamic volume — ephemeral names must be unique", name, which)), true
	}
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return tools.Result{}, false
	}
	return errResult(fmt.Sprintf("create: checking %q against dynamic volumes: %s", name, err)), true
}

// ---- get / list ----

func (v *VolumeDef) execGet(ctx context.Context, in volumeDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("get: missing required field: name"), nil
	}
	tenantID := tools.RunIdentity(ctx).TenantID
	row, err := v.Store.VolumeDefGetByName(ctx, tenantID, in.Name)
	if err != nil {
		// VolumeDefGetByName is already tenant-scoped, so a cross-tenant
		// name returns *ErrNotFound here — the opaque-404 is intrinsic. We
		// surface the same not-found message regardless of cause.
		return errResult(fmt.Sprintf("get: volume %q not found", in.Name)), nil
	}
	return okJSON(volumeDefRowResponse(row, ""))
}

func (v *VolumeDef) execList(ctx context.Context, _ volumeDefInput) (tools.Result, error) {
	tenantID := tools.RunIdentity(ctx).TenantID
	rows, err := v.Store.VolumeDefList(ctx, tenantID)
	if err != nil {
		return errResult(fmt.Sprintf("list: %s", err)), nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, volumeDefRowResponse(r, ""))
	}
	return okJSONCount(map[string]any{"volumes": out}, len(out))
}

// ---- delete ----

func (v *VolumeDef) execDelete(ctx context.Context, in volumeDefInput) (tools.Result, error) {
	if err := v.checkScopeForName(ctx, in.Name); err != nil {
		return errResult(err.Error()), nil
	}
	if in.Name == "" {
		return errResult("delete: missing required field: name"), nil
	}
	tenantID := tools.RunIdentity(ctx).TenantID
	// VolumeDefDelete is tenant-scoped, so a cross-tenant name affects no
	// rows (found=false) — a tenant can only delete its own. NON-destructive:
	// the on-disk directory is left intact (that is purge's job).
	found, err := v.Store.VolumeDefDelete(ctx, tenantID, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("delete: %s", err)), nil
	}
	if !found {
		return errResult(fmt.Sprintf("delete: volume %q not found", in.Name)), nil
	}
	return okJSON(map[string]any{"name": in.Name, "deleted": true, "files_removed": false})
}

// ---- purge (the most dangerous op — fenced four ways) ----

// execPurge removes the row AND os.RemoveAll's the directory tree. Because
// it is a model-influenced recursive delete, it is fenced four ways before
// the RemoveAll:
//
//  1. TENANT OWNERSHIP — the caller's tenant must own the row (the
//     tenant-scoped GetByName returns *ErrNotFound otherwise → opaque-404,
//     so a tenant can only purge its own volume).
//  2. RE-DERIVE, DON'T TRUST — the path is RE-DERIVED from
//     (dynamic_root, tenant, name); the stored definition.path is NEVER
//     trusted for the delete (a tampered row can't redirect the RemoveAll).
//  3. EVALSYMLINKS THE REAL PATH — we EvalSymlinks the derived path and
//     delete the RESOLVED real path, so a swapped symlink can't redirect
//     the delete outside the volume.
//  4. ASSERT STRICTLY INSIDE + EXPECTED PREFIX + NOT-THE-ROOT — the
//     resolved path must be strictly inside the dynamic root, carry the
//     expected <dynamic_root>/<tenant-segment>/ prefix, and NOT equal the
//     dynamic root or the tenant-segment dir itself.
//
// Any assertion failure → refuse + log, do NOT delete. The row is removed
// only AFTER a successful RemoveAll.
func (v *VolumeDef) execPurge(ctx context.Context, in volumeDefInput) (tools.Result, error) {
	if err := v.checkScopeForName(ctx, in.Name); err != nil {
		return errResult(err.Error()), nil
	}
	if in.Name == "" {
		return errResult("purge: missing required field: name"), nil
	}
	// Re-validate the name even though create did: a row could in principle
	// carry a name that no longer passes the charset (schema drift), and we
	// must never feed an unvalidated name into path derivation.
	if err := v.validateName(in.Name); err != nil {
		return errResult(fmt.Sprintf("purge: %s", err)), nil
	}
	tenantID := tools.RunIdentity(ctx).TenantID
	// Re-validated here, as Provision does on create: Execute already refused a
	// bad tenant, but this op is the one that DELETES a derived path, so it must
	// not depend on its caller for that.
	if err := dynvol.ValidTenant(tenantID); err != nil {
		return errResult(fmt.Sprintf("purge: %s", err)), nil
	}

	// Fence (1): tenant ownership. The tenant-scoped read returns
	// *ErrNotFound for a name this tenant doesn't own → opaque-404.
	if _, err := v.Store.VolumeDefGetByName(ctx, tenantID, in.Name); err != nil {
		return errResult(fmt.Sprintf("purge: volume %q not found", in.Name)), nil
	}

	dynRoot, err := v.dynamicRoot()
	if err != nil {
		return errResult(fmt.Sprintf("purge: %s", err)), nil
	}

	// Fence (2): RE-DERIVE the path — never trust the stored definition.path.
	derived := dynvol.DerivedPath(dynRoot, tenantID, in.Name)

	// Fences (3)+(4): EvalSymlinks + strictly-inside + expected
	// <root>/<tenant-segment>/ prefix + not-the-root/parent, via the shared
	// fence helper. expectedParent is the tenant-segment dir (resolved under
	// the dynamic root). removed=false means the dir was already gone.
	rootResolved, err := filepath.EvalSymlinks(dynRoot)
	if err != nil {
		return errResult(fmt.Sprintf("purge: dynamic root: %s", err)), nil
	}
	tenantDir := filepath.Join(rootResolved, dynvol.TenantSegment(tenantID))
	who := fmt.Sprintf("VolumeDef purge (tenant=%q name=%q)", tenantID, in.Name)
	removed, err := fencedRemoveAll(dynRoot, tenantDir, derived, who)
	if err != nil {
		return errResult(fmt.Sprintf("purge: %s", err)), nil
	}
	found, err := v.Store.VolumeDefDelete(ctx, tenantID, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("purge: delete row (files already removed=%v): %s", removed, err)), nil
	}
	return okJSON(map[string]any{"name": in.Name, "deleted": found, "files_removed": removed})
}

// ---- helpers ----

// validateName enforces the charset (no path components) + the optional
// MaxNameLen cap.
func (v *VolumeDef) validateName(name string) error {
	if name == "" {
		return fmt.Errorf("missing required field: name")
	}
	if v.MaxNameLen > 0 && len(name) > v.MaxNameLen {
		return fmt.Errorf("name %q exceeds max length %d", name, v.MaxNameLen)
	}
	return dynvol.ValidName(name)
}

// ValidateVolumeDefBody re-runs the create-time checks over a volume a
// snapshot restore is about to provision: the name charset, the access mode
// and the tenant segment (one charset-clean, non-reserved path segment — a
// hand-edited tenant_id like "x/.." would otherwise derive another tenant's
// tree). body is the snapshot's volume entry
// ({tenant_id, name, mode}); it carries no path, which restore derives on
// this host. The restore call sites inject this, as they do the other
// sections' authoring validators, since the snapshot package cannot import
// this one.
func ValidateVolumeDefBody(body json.RawMessage) error {
	var e struct {
		TenantID string `json:"tenant_id"`
		Name     string `json:"name"`
		Mode     string `json:"mode"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return fmt.Errorf("entry does not decode as a volume: %w", err)
	}
	if err := dynvol.ValidTenant(e.TenantID); err != nil {
		return err
	}
	if err := dynvol.ValidName(e.Name); err != nil {
		return err
	}
	return dynvol.ValidMode(e.Mode)
}

// dynamicRoot returns the operator-blessed parent (the static volume marked
// dynamic_root: true). Refuses when none is configured.
func (v *VolumeDef) dynamicRoot() (string, error) {
	if root, ok := DynamicVolumeRoot(v.Cfg); ok {
		return root, nil
	}
	return "", fmt.Errorf("no dynamic volume root configured — mark a static volume `dynamic_root: true`")
}

// DynamicVolumeRoot returns the operator-blessed parent path (the static
// volume marked dynamic_root: true), or ("", false) when none is configured.
// EXPORTED so the HTTP server's inline ephemeral purge + the sweeper backstop
// resolve the SAME root the tool uses — there is one source of truth for the
// dynamic root. nil cfg → ("", false).
func DynamicVolumeRoot(cfg *config.Config) (string, bool) {
	if cfg == nil {
		return "", false
	}
	for _, vol := range cfg.Volumes {
		if vol.DynamicRoot {
			return vol.Path, true
		}
	}
	return "", false
}

// StaticVolumeFence returns the dynamic root when a static volume's root is
// that directory or above it, else "". The result is the binding's Fence:
// the file tools then refuse paths under it through that binding, so a
// static volume cannot reach other tenants' dynamic volumes or write into a
// read-only ephemeral one. The comparison is on real paths, so a symlinked
// layout is caught too.
func StaticVolumeFence(cfg *config.Config, root string) string {
	dyn, ok := DynamicVolumeRoot(cfg)
	if !ok || root == "" {
		return ""
	}
	dynReal, rootReal := realOrClean(dyn), realOrClean(root)
	rel, err := filepath.Rel(rootReal, dynReal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return dynReal
}

// realOrClean is p with symlinks evaluated, or cleaned when it cannot be.
func realOrClean(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// checkScopeForName enforces the agent's volume_def_scopes against the
// proposed name. Default-deny when empty (gates create/delete/purge only).
func (v *VolumeDef) checkScopeForName(ctx context.Context, name string) error {
	policy := tools.PolicyOf(ctx).VolumeDef
	if len(policy.Scopes) == 0 {
		return fmt.Errorf("VolumeDef tool: agent has no volume_def_scopes (default-deny); add `volume_def_scopes: [...]` to the agent yaml")
	}
	for _, sc := range policy.Scopes {
		if sc == "any" {
			return nil
		}
		if strings.HasPrefix(sc, "named:") && strings.TrimPrefix(sc, "named:") == name {
			return nil
		}
	}
	return fmt.Errorf("VolumeDef tool: name %q not in this agent's volume_def_scopes (%v)", name, policy.Scopes)
}

// fencedRemoveAll is the shared os.RemoveAll fence used by EVERY destructive
// volume path (RFC AH §6): VolumeDef `purge` (Phase 2a), the inline
// run-completion ephemeral purge, and the ephemeral sweeper backstop (Phase
// 2b). It NEVER trusts a stored/caller path — `target` is a runtime-RE-DERIVED
// path, and the fence re-checks containment before deleting.
//
// Fences, in order:
//
//  1. EvalSymlinks the dynamic root + the target, and check the RESOLVED
//     real path. The delete then goes through a handle on the dynamic root,
//     walking to the target's parent one checked directory at a time, so a
//     symlink swapped in after the check can't redirect it (RFC EU, Part A).
//     A non-existent target is a no-op success (removed=false): the
//     directory is already gone.
//  2. The resolved path must be STRICTLY inside the dynamic root
//     (relInsideRoot), and NOT equal the dynamic root OR the expectedParent
//     dir itself (you can never RemoveAll an operator-blessed root or a
//     shared parent like the tenant / _ephemeral dir).
//  3. The resolved path must carry the expectedParent/ prefix — belt-and-
//     braces on top of the strict-inside check.
//
// dynRoot is the operator-blessed parent (the dynamic_root). expectedParent
// is the directory the target must sit strictly under (the tenant-segment
// dir for Phase 2a; the per-run _ephemeral/<run_id> dir for Phase 2b — note
// for 2b the target IS that dir, so expectedParent is its parent
// _ephemeral/<run_id>'s PARENT is the dynamic-root subtree; callers pass the
// dir one level up). On any fence failure it logs `who` + the reason and
// returns (false, error) WITHOUT deleting.
func fencedRemoveAll(dynRoot, expectedParent, target, who string) (removed bool, err error) {
	rootResolved, err := filepath.EvalSymlinks(dynRoot)
	if err != nil {
		return false, fmt.Errorf("dynamic root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // already gone — nothing to delete
		}
		return false, fmt.Errorf("resolve path: %w", err)
	}
	if err := relInsideRoot(rootResolved, target, resolved); err != nil {
		log.Printf("%s REFUSED: %s (target=%q)", who, err, target)
		return false, fmt.Errorf("refusing to delete %q — it does not resolve strictly inside the dynamic root", resolved)
	}
	if resolved == rootResolved || resolved == expectedParent {
		log.Printf("%s REFUSED: resolved path %q is the dynamic root or the parent dir itself", who, resolved)
		return false, fmt.Errorf("refusing to delete the dynamic root or the parent directory itself")
	}
	expectedPrefix := expectedParent + string(filepath.Separator)
	if !strings.HasPrefix(resolved+string(filepath.Separator), expectedPrefix) {
		log.Printf("%s REFUSED: resolved %q lacks expected prefix %q", who, resolved, expectedPrefix)
		return false, fmt.Errorf("refusing to delete %q — it is not under the expected parent directory", resolved)
	}
	// Delete through a handle on the dynamic root, never by the checked
	// string: an ancestor of resolved swapped for a symlink after the checks
	// above would carry a path-based delete wherever the link points, into
	// another tenant's directory or out of the root (RFC EU, Part A). openDir
	// walks to the parent one directory handle at a time and refuses a step
	// that is no longer the real directory the path named; RemoveAll inside
	// that handle never follows a symlink.
	vfs, err := openVolumeFS(rootResolved, "")
	if err != nil {
		return false, fmt.Errorf("dynamic root: %w", err)
	}
	defer vfs.Close()
	parentRel, err := vfs.rel(filepath.Dir(resolved))
	if err != nil {
		return false, err
	}
	dir, err := vfs.openDir(parentRel)
	if err != nil {
		log.Printf("%s REFUSED: %v (target=%q)", who, err, target)
		return false, fmt.Errorf("refusing to delete %q: %w", resolved, err)
	}
	defer dir.Close()
	if err := dir.RemoveAll(filepath.Base(resolved)); err != nil {
		return false, fmt.Errorf("remove %q: %w", resolved, err)
	}
	return true, nil
}

// PurgeEphemeralRunTree RemoveAll's the <dynamic_root>/_ephemeral/<root_run_id>
// subtree behind fencedRemoveAll (RFC AH §6). expectedParent is
// <dynamic_root>/_ephemeral — the run dir must sit strictly under it and not
// equal it (so a missing/escaping run id can never widen the delete to the
// whole _ephemeral tree or the dynamic root). Re-derives the path; never
// trusts a stored row. EXPORTED so the HTTP server's inline run-completion
// purge + the cmd/loomcycle ephemeral sweeper backstop reuse the SAME fence.
// who is a log prefix (e.g. "ephemeral inline purge"). removed=false when
// the subtree was already gone (a no-op success).
func PurgeEphemeralRunTree(dynRoot, rootRunID, who string) (removed bool, err error) {
	if rootRunID == "" {
		return false, fmt.Errorf("empty root run id")
	}
	rootResolved, rerr := filepath.EvalSymlinks(dynRoot)
	if rerr != nil {
		return false, fmt.Errorf("dynamic root: %w", rerr)
	}
	expectedParent := filepath.Join(rootResolved, dynvol.EphemeralSegment)
	target := dynvol.EphemeralRunDir(dynRoot, rootRunID)
	return fencedRemoveAll(dynRoot, expectedParent, target, who)
}

// volumeDefRowResponse shapes the tool's reply for one row. mode is the
// caller-supplied/created mode for create; for get/list it is decoded from
// the row's definition (empty → decode here).
func volumeDefRowResponse(row store.VolumeDefRow, mode string) map[string]any {
	var body dynvol.Body
	_ = json.Unmarshal(row.Definition, &body)
	if mode == "" {
		mode = body.Mode
	}
	return map[string]any{
		"name":       row.Name,
		"path":       body.Path,
		"mode":       mode,
		"created_at": row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		"updated_at": row.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
	}
}
