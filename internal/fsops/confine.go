// Package fsops is the file-operation logic of the built-in Read, Write, Edit,
// Glob and Grep tools, taken out of the tools so that something with no run
// context (a workspace worker, RFC GW) can run exactly the same code on a
// directory it is given.
//
// Every function takes the root (and, where it matters, the fence) as an
// argument and reads nothing from a context but its cancellation. The tools
// pick the root from the run's volume policy and call in here; a worker passes
// its workspace directory.
//
// The guarantees, which hold for every exported operation:
//
//   - Confinement. A path, relative or absolute, is accepted only when it
//     resolves inside the root after its symlinks are evaluated, so `..`, an
//     absolute path elsewhere, and a symlink (or a chain of them, or a
//     symlinked parent directory) leading out of the root are refused. The
//     resolved path is a check, not something opened by name: every open,
//     create and rename goes through an os.Root at the root, one directory
//     handle at a time, so a component swapped for a symlink after the check
//     is refused rather than followed (RFC ES #1).
//   - The fence. A fence is a directory inside the root that holds other
//     runs' volumes; a path that is the fence or under it is refused, both at
//     the check and at the open.
//   - The root is resolved once. Only the root (operator configuration) is
//     passed through EvalSymlinks; every other path is derived from the
//     resolved root.
//   - Bounds. Each operation states its own byte and result caps.
//
// The package imports only the standard library.
package fsops

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ResolveInsideRoot resolves target (absolute, or relative to root) and
// returns its symlink-free absolute path, or an error when the target does not
// exist or resolves outside root. Use it where the file must exist (read,
// edit); ResolveParentInsideRoot is the variant for a file about to be made.
//
// The result is a CHECK and the source of the user-facing error text; open
// it through VolumeFS, never by this string.
func ResolveInsideRoot(root, target string) (string, error) {
	rootResolved, err := resolveRoot(root)
	if err != nil {
		return "", err
	}
	return resolveUnder(rootResolved, target)
}

// ResolveParentInsideRoot is ResolveInsideRoot for a file that may not exist
// yet: it resolves only the parent directory, checks IT is inside root, and
// returns the resolved parent joined with the target's base name.
func ResolveParentInsideRoot(root, target string) (string, error) {
	rootResolved, err := resolveRoot(root)
	if err != nil {
		return "", err
	}
	return resolveParentUnder(rootResolved, target)
}

// resolveRoot evaluates the root's symlinks. It is the one EvalSymlinks of
// operator configuration an operation does; every other path is derived from
// its result.
func resolveRoot(root string) (string, error) {
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("sandbox root: %w", err)
	}
	return rootResolved, nil
}

// resolveUnder is ResolveInsideRoot with the root already resolved. The order
// matters: both the root and the target are symlink-free before the Rel
// check, which is what refuses a target that is a symlink out of the root.
func resolveUnder(rootResolved, target string) (string, error) {
	abs := absUnderRoot(rootResolved, target)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if err := RelInsideRoot(rootResolved, abs, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// resolveParentUnder is ResolveParentInsideRoot with the root already
// resolved.
func resolveParentUnder(rootResolved, target string) (string, error) {
	abs := absUnderRoot(rootResolved, target)
	parentResolved, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("parent dir: %w", err)
	}
	// Pass abs as both requested AND the resolved-equivalent so the error
	// message matches the resolveUnder shape: when the user path didn't
	// redirect via symlinks (parent itself was inside root, or wholly
	// outside), the message stays single-path.
	parentEquivalent := filepath.Join(parentResolved, filepath.Base(abs))
	if err := RelInsideRoot(rootResolved, abs, parentEquivalent); err != nil {
		return "", err
	}
	return parentEquivalent, nil
}

// absUnderRoot makes target absolute, anchoring a RELATIVE target to the
// sandbox root — NOT the process's working directory.
//
// Why: a relative tool path ("internal/foo.go") should mean "inside the
// sandbox", the same thing it means to the Bash tool (whose cwd is the jail)
// and the same thing every model assumes. Resolving it with filepath.Abs
// anchored it wherever the server happened to start, so a relative path landed
// OUTSIDE the sandbox and either failed or — if a like-named file sat at the
// server's cwd — produced a baffling error (a code-reviewer agent's
// `Read internal/store/store.go` resolved into a stray `loomcycle` binary at
// the repo root → ENOTDIR).
//
// The result is only a CANDIDATE: every caller still EvalSymlinks it and
// re-checks containment with RelInsideRoot, so a relative `..` that climbs
// out of root is still rejected.
func absUnderRoot(rootResolved, target string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Join(rootResolved, target)
}

// RelInsideRoot returns nil if resolved is inside root (or is root). requested
// is the caller-supplied path (pre-symlink-resolution); resolved is what was
// actually checked. When the two differ (a symlink redirected the target),
// the error names both so the model learns its input was rerouted. The
// HasPrefix form uses the OS-specific separator so `..\foo` on Windows is
// caught alongside `../foo` on POSIX.
func RelInsideRoot(root, requested, resolved string) error {
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		if requested == resolved {
			return fmt.Errorf("path %q escapes sandbox %q", requested, root)
		}
		return fmt.Errorf("path %q (resolved to %q) escapes sandbox %q", requested, resolved, root)
	}
	return nil
}

// OutsideFence refuses a resolved path that is the fence or under it. The
// fence is a real path (operator configuration) and resolved has had its
// symlinks evaluated, so a symlink inside the volume that points into the
// fence is refused as well. An empty fence refuses nothing.
func OutsideFence(fence, resolved string) error {
	if fence == "" {
		return nil
	}
	rel, err := filepath.Rel(fence, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	return fmt.Errorf("path %q is inside the directory that holds dynamic and ephemeral volumes; name that volume in the \"volume\" argument instead", resolved)
}
