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

// ReservedTenant reports whether tenantID collides with a reserved on-disk
// segment: "_shared" would share the shared tenant's tree, "_ephemeral" the
// run-scoped tree, blurring the purge fences either way.
func ReservedTenant(tenantID string) bool {
	return tenantID == SharedTenantSegment || tenantID == EphemeralSegment
}

// ValidTenant refuses a reserved tenant id.
func ValidTenant(tenantID string) error {
	if ReservedTenant(tenantID) {
		return fmt.Errorf("tenant id %q is reserved", tenantID)
	}
	return nil
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

// AssertInsideRoot verifies path resolves strictly inside dynRoot. The parent
// (tenant-segment dir) may not exist yet at create, so it resolves the dynamic
// root and checks the lexical containment of the cleaned path; the purge-time
// check additionally EvalSymlinks the real path.
func AssertInsideRoot(dynRoot, path string) error {
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
// runtime-derived, it is verified to resolve strictly inside the dynamic root
// before MkdirAll, which catches a future bug in the derivation (or a
// symlinked dynamic root that escapes) rather than trusting the construction.
func mkdirFenced(dynRoot, path string) (created bool, err error) {
	if err := AssertInsideRoot(dynRoot, path); err != nil {
		return false, fmt.Errorf("refusing to provision outside the dynamic root: %s", err)
	}
	if _, statErr := os.Lstat(path); os.IsNotExist(statErr) {
		created = true
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return false, fmt.Errorf("mkdir %q: %s", path, err)
	}
	return created, nil
}
