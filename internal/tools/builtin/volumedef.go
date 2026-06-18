package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
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
}

// volumeNameRe constrains a dynamic volume name so it can NEVER inject a
// path component: no "/", no ".", no "..", no leading dot, lowercase
// alnum + "_" + "-" only, 1–64 chars. This is the first line of the
// no-caller-controlled-path defence.
var volumeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// volumeDefBody is the {path,mode} shape persisted in volume_defs.definition.
// Path is runtime-derived; never caller-supplied.
type volumeDefBody struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

const volumeDefDescription = `Provision, inspect, and remove CONFINED dynamic filesystem volumes at runtime (RFC AH). ` +
	`A volume is created by NAME + MODE only — the runtime derives the path inside an operator-blessed ` +
	`parent (<dynamic_root>/<tenant>/<name>); you never supply a host path. Static yaml volumes: are the ` +
	`operator's ground truth and cannot be created over. Operations: create, get, list, delete, purge. ` +
	`delete removes the mapping but LEAVES files on disk; purge removes the mapping AND deletes the ` +
	`directory tree (fenced: it can only ever delete a runtime-derived path strictly inside the dynamic root).`

const volumeDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":   {"type": "string", "enum": ["create","get","list","delete","purge"]},
    "name": {"type": "string", "description": "Dynamic volume name (required for create/get/delete/purge). Charset ^[a-z0-9][a-z0-9_-]{0,63}$ — no slashes or dots."},
    "mode": {"type": "string", "enum": ["rw","ro"], "description": "Access mode for create (default rw). rw allows Write/Edit/Bash; ro is read-only."}
  },
  "required": ["op"]
}`

type volumeDefInput struct {
	Op   string `json:"op"`
	Name string `json:"name,omitempty"`
	Mode string `json:"mode,omitempty"`
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
	// sharedTenantSegment. Refuse a real tenant whose minted id is literally
	// that string, so the reserved segment can never be shared by two distinct
	// tenants (which would let one purge the other's volume tree). The tenant
	// is authoritative from the principal, never the wire.
	if tools.RunIdentity(ctx).TenantID == sharedTenantSegment {
		return errResult(fmt.Sprintf("VolumeDef tool: tenant id %q is reserved", sharedTenantSegment)), nil
	}
	var in volumeDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
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
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, get, list, delete, purge)", in.Op)), nil
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
	mode := in.Mode
	if mode == "" {
		mode = "rw"
	}
	if mode != "rw" && mode != "ro" {
		return errResult(fmt.Sprintf("create: invalid mode %q (want rw or ro)", in.Mode)), nil
	}
	// Static cfg.Volumes is ground truth — refuse a name that collides with
	// an operator-declared static volume (mirrors MCPServerDef refusing a
	// static-name collision). The resolver also puts static first, so this
	// is belt-and-braces.
	if _, ok := v.Cfg.Volumes[in.Name]; ok {
		return errResult(fmt.Sprintf("create: name %q matches a static volumes: entry — yaml is ground truth; use a different name", in.Name)), nil
	}

	dynRoot, err := v.dynamicRoot()
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	tenantID := tools.RunIdentity(ctx).TenantID
	path := derivedVolumePath(dynRoot, tenantID, in.Name)

	// Defence-in-depth: even though the path is runtime-derived, verify it
	// resolves STRICTLY inside the dynamic root before we MkdirAll. This
	// catches a future bug in the derivation (or a symlinked dynamic root
	// that escapes) rather than trusting the construction blindly.
	if err := assertInsideDynamicRoot(dynRoot, path); err != nil {
		return errResult(fmt.Sprintf("create: refusing to provision outside the dynamic root: %s", err)), nil
	}
	// 0o700: the volume tree is the tenant's own; not group/world readable.
	if err := os.MkdirAll(path, 0o700); err != nil {
		return errResult(fmt.Sprintf("create: mkdir %q: %s", path, err)), nil
	}

	body, err := json.Marshal(volumeDefBody{Path: path, Mode: mode})
	if err != nil {
		return errResult(fmt.Sprintf("create: marshal: %s", err)), nil
	}
	row, err := v.Store.VolumeDefCreate(ctx, store.VolumeDefRow{
		TenantID:   tenantID,
		Name:       in.Name,
		Definition: body,
	})
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	return okJSON(volumeDefRowResponse(row, mode))
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
	return okJSON(map[string]any{"volumes": out})
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
	derived := derivedVolumePath(dynRoot, tenantID, in.Name)

	// Resolve the dynamic root's real path once for all containment checks.
	rootResolved, err := filepath.EvalSymlinks(dynRoot)
	if err != nil {
		return errResult(fmt.Sprintf("purge: dynamic root: %s", err)), nil
	}
	tenantDir := filepath.Join(rootResolved, tenantSegment(tenantID))

	// Fence (3): EvalSymlinks the derived path; delete the RESOLVED real
	// path. If the directory doesn't exist (already gone), there is nothing
	// to delete — drop the row and report files_removed=false.
	resolved, err := filepath.EvalSymlinks(derived)
	if err != nil {
		if os.IsNotExist(err) {
			found, derr := v.Store.VolumeDefDelete(ctx, tenantID, in.Name)
			if derr != nil {
				return errResult(fmt.Sprintf("purge: delete row: %s", derr)), nil
			}
			return okJSON(map[string]any{"name": in.Name, "deleted": found, "files_removed": false})
		}
		return errResult(fmt.Sprintf("purge: resolve path: %s", err)), nil
	}

	// Fence (4): the resolved path must be STRICTLY inside the dynamic root,
	// carry the expected <root>/<tenant-segment>/ prefix, and NOT be the
	// root or the tenant-segment dir itself.
	if err := relInsideRoot(rootResolved, derived, resolved); err != nil {
		log.Printf("VolumeDef purge REFUSED: %s (tenant=%q name=%q)", err, tenantID, in.Name)
		return errResult(fmt.Sprintf("purge: refusing to delete %q — it does not resolve strictly inside the dynamic root", resolved)), nil
	}
	if resolved == rootResolved || resolved == tenantDir {
		log.Printf("VolumeDef purge REFUSED: resolved path %q is the dynamic root or tenant dir (tenant=%q name=%q)", resolved, tenantID, in.Name)
		return errResult("purge: refusing to delete the dynamic root or the tenant directory itself"), nil
	}
	expectedPrefix := tenantDir + string(filepath.Separator)
	if !strings.HasPrefix(resolved+string(filepath.Separator), expectedPrefix) {
		log.Printf("VolumeDef purge REFUSED: resolved %q lacks expected tenant prefix %q (tenant=%q name=%q)", resolved, expectedPrefix, tenantID, in.Name)
		return errResult(fmt.Sprintf("purge: refusing to delete %q — it is not under this tenant's volume directory", resolved)), nil
	}

	// All four fences passed: delete the RESOLVED real path, then the row.
	if err := os.RemoveAll(resolved); err != nil {
		return errResult(fmt.Sprintf("purge: remove %q: %s", resolved, err)), nil
	}
	found, err := v.Store.VolumeDefDelete(ctx, tenantID, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("purge: delete row (files already removed): %s", err)), nil
	}
	return okJSON(map[string]any{"name": in.Name, "deleted": found, "files_removed": true})
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
	if !volumeNameRe.MatchString(name) {
		return fmt.Errorf("name %q invalid (must match ^[a-z0-9][a-z0-9_-]{0,63}$ — no slashes, dots, or leading dot)", name)
	}
	return nil
}

// dynamicRoot returns the operator-blessed parent (the static volume marked
// dynamic_root: true). Refuses when none is configured.
func (v *VolumeDef) dynamicRoot() (string, error) {
	for _, vol := range v.Cfg.Volumes {
		if vol.DynamicRoot {
			return vol.Path, nil
		}
	}
	return "", fmt.Errorf("no dynamic volume root configured — mark a static volume `dynamic_root: true`")
}

// checkScopeForName enforces the agent's volume_def_scopes against the
// proposed name. Default-deny when empty (gates create/delete/purge only).
func (v *VolumeDef) checkScopeForName(ctx context.Context, name string) error {
	policy := tools.VolumeDefPolicy(ctx)
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

// sharedTenantSegment is the on-disk path segment for the shared tenant ("").
// A real tenant whose minted id is literally this string is REJECTED at the op
// boundary (see Execute), so the segment is unambiguous and two distinct
// tenants can never share a directory subtree.
const sharedTenantSegment = "_shared"

// tenantSegment maps a tenant id to its on-disk path segment. The shared
// tenant "" uses sharedTenantSegment; every other tenant uses its id verbatim.
// The Execute guard rejects a tenant id equal to sharedTenantSegment, so the
// "" → sharedTenantSegment mapping can never collide with a real tenant.
func tenantSegment(tenantID string) string {
	if tenantID == "" {
		return sharedTenantSegment
	}
	return tenantID
}

// derivedVolumePath builds <dynamic_root>/<tenant-segment>/<name>. The name
// MUST already be charset-validated (no path components) by the caller.
func derivedVolumePath(dynRoot, tenantID, name string) string {
	return filepath.Join(dynRoot, tenantSegment(tenantID), name)
}

// assertInsideDynamicRoot verifies path resolves strictly inside dynRoot.
// Used at create-time (defence-in-depth on the derivation). The parent
// (tenant-segment dir) may not exist yet at create, so we resolve the
// dynamic root and check the lexical containment of the (cleaned) path —
// the purge-time check additionally EvalSymlinks the real path.
func assertInsideDynamicRoot(dynRoot, path string) error {
	rootResolved, err := filepath.EvalSymlinks(dynRoot)
	if err != nil {
		return fmt.Errorf("dynamic root: %w", err)
	}
	clean := filepath.Clean(path)
	// rel against the resolved root; reject "." (equals root) and any "..".
	rel, err := filepath.Rel(rootResolved, clean)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes dynamic root %q", clean, rootResolved)
	}
	return nil
}

// volumeDefRowResponse shapes the tool's reply for one row. mode is the
// caller-supplied/created mode for create; for get/list it is decoded from
// the row's definition (empty → decode here).
func volumeDefRowResponse(row store.VolumeDefRow, mode string) map[string]any {
	var body volumeDefBody
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
