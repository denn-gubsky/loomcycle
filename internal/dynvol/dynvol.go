// Package dynvol holds the rules for where a dynamic volume lives on disk:
// which names and tenants may have one, how its path is derived under the
// operator-blessed dynamic root, and the fence every provisioning write goes
// through. The VolumeDef tool and snapshot restore both provision dynamic
// volumes, and a second copy of these rules is how the two would drift, so
// both call this package.
//
// The security crux: a dynamic volume is created by NAME + MODE only. Its path
// is always derived,
//
//	<dynamic_root>/<tenant-segment>/<name>
//
// and never taken from a caller or from a stored row. lookup trusts the stored
// path at run time, so whatever writes a row must derive the path here.
package dynvol

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SharedTenantSegment is the on-disk path segment for the shared tenant ("").
// A real tenant whose id is literally this string is refused (ValidTenant), so
// the segment is unambiguous and two distinct tenants never share a subtree.
const SharedTenantSegment = "_shared"

// EphemeralSegment is the reserved first segment under the dynamic root for
// run-scoped ephemeral volumes: <dynamic_root>/_ephemeral/<root_run_id>/<name>.
// A tenant id equal to it is refused, and the name charset forbids a leading
// "_", so neither a tenant nor a volume can author under it.
const EphemeralSegment = "_ephemeral"

// nameRe constrains a dynamic volume name so it can never inject a path
// component: no "/", no ".", no "..", no leading dot or underscore, lowercase
// alnum + "_" + "-" only, 1–64 chars.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Body is the {path,mode} shape persisted in volume_defs.definition. Path is
// always runtime-derived (DerivedPath); never caller- or snapshot-supplied.
type Body struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

// ValidName enforces the name charset.
func ValidName(name string) error {
	if name == "" {
		return fmt.Errorf("missing required field: name")
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("name %q invalid (must match ^[a-z0-9][a-z0-9_-]{0,63}$ — no slashes, dots, or leading dot)", name)
	}
	return nil
}

// ValidMode accepts the two access modes. An empty mode is the caller's
// default to apply (the tool defaults to rw) before asking.
func ValidMode(mode string) error {
	if mode != "rw" && mode != "ro" {
		return fmt.Errorf("invalid mode %q (want rw or ro)", mode)
	}
	return nil
}

// tenantRe is the tenant-id charset, the same one minted tokens enforce. The
// id is used verbatim as ONE path segment (TenantSegment), so it must never
// carry "/" or be "." / "..": "x/.." would derive tenant acme's tree, and a
// purge by that tenant would delete it. Tenant ids also reach this package
// from config principals and hand-edited snapshot entries, which do not go
// through the mint check, so the charset is enforced here as well.
var tenantRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// ReservedTenant reports whether tenantID collides with a reserved on-disk
// segment: "_shared" would share the shared tenant's tree, "_ephemeral" the
// run-scoped tree, blurring the purge fences either way. The comparison
// ignores case because on a case-insensitive filesystem (the macOS default,
// a ZFS dataset with casesensitivity=insensitive) "_Shared" IS "_shared".
func ReservedTenant(tenantID string) bool {
	return strings.EqualFold(tenantID, SharedTenantSegment) || strings.EqualFold(tenantID, EphemeralSegment)
}

// ValidTenant accepts the shared tenant "" or an id that is exactly one
// charset-clean path segment, and refuses a reserved one.
func ValidTenant(tenantID string) error {
	if tenantID == "" {
		return nil
	}
	if ReservedTenant(tenantID) {
		return fmt.Errorf("tenant id %q is reserved", tenantID)
	}
	if !tenantRe.MatchString(tenantID) {
		return fmt.Errorf("tenant id %q invalid (must match ^[a-zA-Z0-9_-]{1,64}$ — one path segment, no slashes or dots)", tenantID)
	}
	return nil
}

// CaseFoldCollision returns the first id in known that differs from
// candidate but equals it ignoring case. Two such tenants would share one
// volume tree on a case-insensitive filesystem, so whatever registers a new
// tenant id refuses one that has such a twin.
func CaseFoldCollision(candidate string, known []string) (string, bool) {
	for _, k := range known {
		if k != candidate && strings.EqualFold(k, candidate) {
			return k, true
		}
	}
	return "", false
}

// TenantSegment maps a tenant id to its on-disk path segment. The shared
// tenant "" uses SharedTenantSegment; every other tenant uses its id verbatim.
func TenantSegment(tenantID string) string {
	if tenantID == "" {
		return SharedTenantSegment
	}
	return tenantID
}

// DerivedPath builds <dynamic_root>/<tenant-segment>/<name>. The name MUST
// already be charset-validated (no path components) by the caller.
func DerivedPath(dynRoot, tenantID, name string) string {
	return filepath.Join(dynRoot, TenantSegment(tenantID), name)
}

// DerivedEphemeralPath builds <dynamic_root>/_ephemeral/<root_run_id>/<name>.
// The name MUST already be charset-validated; rootRunID is a globally-unique
// run id (charset [A-Za-z0-9_-], validated at the wire boundary), so two runs
// — any tenant — never collide.
func DerivedEphemeralPath(dynRoot, rootRunID, name string) string {
	return filepath.Join(dynRoot, EphemeralSegment, rootRunID, name)
}

// EphemeralRunDir is the per-run ephemeral subtree
// <dynamic_root>/_ephemeral/<root_run_id> — the unit both ephemeral purge
// paths remove. Re-derived, never trusted from a stored row.
func EphemeralRunDir(dynRoot, rootRunID string) string {
	return filepath.Join(dynRoot, EphemeralSegment, rootRunID)
}

// AssertInsideRoot verifies path lies strictly inside dynRoot. The parent
// (tenant-segment dir) may not exist yet at create, so the check is on the
// cleaned path, not on a resolved one; the purge-time check additionally
// EvalSymlinks the real path.
func AssertInsideRoot(dynRoot, path string) error {
	_, _, err := locateBelowRoot(dynRoot, path)
	return err
}

// locateBelowRoot resolves the dynamic root once and returns it with path's
// location below it (rel: never "." and never a ".." step).
//
// A root reached through a symlink (/tmp on macOS, a symlinked NAS mount) is
// the operator's own choice and is trusted, so path may be spelled under the
// root as configured — which is how DerivedPath builds it — or under the
// resolved root. Each spelling is compared with the root in the SAME form:
// comparing the configured spelling against the resolved root refused every
// path under a symlinked root. Nothing below the root is trusted by this; the
// provisioning write walks those components (refuseSymlinkBelowRoot).
func locateBelowRoot(dynRoot, path string) (rootResolved, rel string, err error) {
	rootResolved, err = filepath.EvalSymlinks(dynRoot)
	if err != nil {
		return "", "", fmt.Errorf("dynamic root: %w", err)
	}
	clean := filepath.Clean(path)
	for _, root := range []string{filepath.Clean(dynRoot), rootResolved} {
		if r, ok := strictlyBelow(root, clean); ok {
			return rootResolved, r, nil
		}
	}
	return "", "", fmt.Errorf("path %q escapes dynamic root %q", clean, rootResolved)
}

// UnderResolvedRoot respells a path inside dynRoot under the resolved root,
// following the root's own symlink and nothing below it — the one form in
// which two spellings of a volume path compare equal.
func UnderResolvedRoot(dynRoot, path string) (string, error) {
	rootResolved, rel, err := locateBelowRoot(dynRoot, path)
	if err != nil {
		return "", err
	}
	return filepath.Join(rootResolved, rel), nil
}

// strictlyBelow returns path relative to root when it lies strictly inside
// it: not the root itself, and no ".." step out of it.
func strictlyBelow(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// Provision derives a persistent dynamic volume's path, fences it inside the
// dynamic root and creates the directory (0700: the volume tree is the
// tenant's own, not group- or world-readable). It returns the derived path,
// which is what a volume_defs row may name, and whether this call created the
// leaf directory (false: it already existed).
//
// The name and tenant are re-checked here even though every caller validates
// them first: a derivation fed an unvalidated name ("a/b" stays inside the
// root, so the fence alone would not refuse it) must never reach MkdirAll.
func Provision(dynRoot, tenantID, name string) (path string, created bool, err error) {
	if err := ValidName(name); err != nil {
		return "", false, err
	}
	if err := ValidTenant(tenantID); err != nil {
		return "", false, err
	}
	path = DerivedPath(dynRoot, tenantID, name)
	created, err = mkdirFenced(dynRoot, path)
	if err != nil {
		return "", false, err
	}
	return path, created, nil
}

// ProvisionEphemeral derives a run-scoped ephemeral volume's path, fences it
// inside the dynamic root and creates the directory, as Provision does.
func ProvisionEphemeral(dynRoot, rootRunID, name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	path := DerivedEphemeralPath(dynRoot, rootRunID, name)
	if _, err := mkdirFenced(dynRoot, path); err != nil {
		return "", err
	}
	return path, nil
}

// mkdirFenced is the one provisioning write: even though path is
// runtime-derived, it is verified to lie strictly inside the dynamic root
// before MkdirAll, which catches a future bug in the derivation rather than
// trusting the construction.
//
// The lexical fence alone is not enough: MkdirAll follows a symlink it finds
// on the way, so a symlink already sitting at the tenant segment or the leaf
// would create — and the row would then name — a directory outside the root.
// Every existing component below the root is therefore refused if it is a
// symlink, and the created path must resolve to itself afterwards, which
// catches one swapped in between the check and the MkdirAll.
//
// Every filesystem call below works on the path under the RESOLVED root: the
// root's own (trusted) symlink is followed once, when it is resolved, and so
// "resolves to itself" means exactly "no symlink below the root". The caller
// still stores path as derived, under the root as configured.
func mkdirFenced(dynRoot, path string) (created bool, err error) {
	rootResolved, rel, err := locateBelowRoot(dynRoot, path)
	if err != nil {
		return false, fmt.Errorf("refusing to provision outside the dynamic root: %s", err)
	}
	if err := refuseSymlinkBelowRoot(rootResolved, rel); err != nil {
		return false, fmt.Errorf("refusing to provision outside the dynamic root: %s", err)
	}
	onDisk := filepath.Join(rootResolved, rel)
	if _, statErr := os.Lstat(onDisk); os.IsNotExist(statErr) {
		created = true
	}
	if err := os.MkdirAll(onDisk, 0o700); err != nil {
		return false, fmt.Errorf("mkdir %q: %s", onDisk, err)
	}
	if resolved, err := filepath.EvalSymlinks(onDisk); err != nil || resolved != onDisk {
		return false, fmt.Errorf("refusing to provision outside the dynamic root: %q does not resolve to itself", onDisk)
	}
	return created, nil
}

// refuseSymlinkBelowRoot walks rel's components below the resolved root and
// refuses any that already exists as a symlink. It stops at the first
// component that does not exist yet: MkdirAll creates the rest as real
// directories. locateBelowRoot has already placed rel strictly inside.
func refuseSymlinkBelowRoot(rootResolved, rel string) error {
	cur := rootResolved
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%q is a symlink", cur)
		}
	}
	return nil
}
