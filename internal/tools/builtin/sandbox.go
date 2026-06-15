package builtin

import (
	"fmt"
	"path/filepath"
	"strings"
)

// absUnderRoot makes target absolute, anchoring a RELATIVE target to the
// sandbox root — NOT the loomcycle process's working directory.
//
// Why: a relative tool path ("internal/foo.go") should mean "inside the
// sandbox", the same thing it means to the Bash tool (whose cwd is the jail)
// and the same thing every model assumes. The old behaviour
// (filepath.Abs → process cwd) resolved it against wherever the server
// happened to start, so a relative path landed OUTSIDE the sandbox and either
// failed or — if a like-named file sat at the server's cwd — produced a
// baffling error (a code-reviewer agent's `Read internal/store/store.go`
// resolved into a stray `loomcycle` binary at the repo root → ENOTDIR).
//
// The result is only a CANDIDATE: every caller still EvalSymlinks it and
// re-checks containment with relInsideRoot, so a relative `..` that climbs
// out of root is still rejected.
func absUnderRoot(rootResolved, target string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Join(rootResolved, target)
}

// resolveInsideRoot resolves a target path (absolute, or relative to the
// sandbox root — see absUnderRoot) and verifies it sits inside root after
// symlink evaluation. Returns the resolved absolute path on success.
//
// Used by Read and Edit, where the file MUST exist (otherwise EvalSymlinks
// fails). Write uses resolveParentInsideRoot below because the file may
// not exist yet.
//
// The order matters: EvalSymlinks BOTH root and target before the Rel
// check. This blocks the TOCTOU where target was a symlink at check time
// but a different file at open time, and the case where target is a
// symlink to outside root.
func resolveInsideRoot(root, target string) (resolved string, err error) {
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("sandbox root: %w", err)
	}
	abs := absUnderRoot(rootResolved, target)
	resolved, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if err := relInsideRoot(rootResolved, abs, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// resolveParentInsideRoot is the variant for Write: the file may not
// exist yet, so we resolve only the parent directory and verify IT is
// inside root, then return parent + base(target).
func resolveParentInsideRoot(root, target string) (joined string, err error) {
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("sandbox root: %w", err)
	}
	abs := absUnderRoot(rootResolved, target)
	parentResolved, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("parent dir: %w", err)
	}
	// Pass abs as both requested AND the resolved-equivalent so the
	// error message matches the resolveInsideRoot shape: when the user
	// path didn't redirect via symlinks (parent itself was inside root,
	// or wholly outside), the message stays single-path.
	parentEquivalent := filepath.Join(parentResolved, filepath.Base(abs))
	if err := relInsideRoot(rootResolved, abs, parentEquivalent); err != nil {
		return "", err
	}
	return parentEquivalent, nil
}

// relInsideRoot returns nil if resolved is inside root. requested is the
// caller-supplied path (pre-symlink-resolution); resolved is what we
// actually checked. When the two differ (a symlink redirected the
// target), the error message includes both so the model learns its
// input was rerouted. The HasPrefix form uses the OS-specific separator
// so `..\foo` on Windows is caught alongside `../foo` on POSIX.
func relInsideRoot(root, requested, resolved string) error {
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		if requested == resolved {
			return fmt.Errorf("path %q escapes sandbox %q", requested, root)
		}
		return fmt.Errorf("path %q (resolved to %q) escapes sandbox %q", requested, resolved, root)
	}
	return nil
}
