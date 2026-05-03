package builtin

import (
	"fmt"
	"path/filepath"
	"strings"
)

// resolveInsideRoot resolves an absolute target path and verifies it sits
// inside root after symlink evaluation. Returns the resolved absolute
// path on success.
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
	abs, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return "", fmt.Errorf("abs path: %w", err)
	}
	resolved, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if err := relInsideRoot(rootResolved, resolved); err != nil {
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
	abs, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return "", fmt.Errorf("abs path: %w", err)
	}
	parentResolved, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("parent dir: %w", err)
	}
	if err := relInsideRoot(rootResolved, parentResolved); err != nil {
		// Reuse the absolute path in the message so the model sees what
		// it asked for, not the resolved-parent location.
		return "", fmt.Errorf("path %q escapes sandbox %q", abs, rootResolved)
	}
	return filepath.Join(parentResolved, filepath.Base(abs)), nil
}

// relInsideRoot returns nil if resolved is inside root. The HasPrefix
// form uses the OS-specific separator so `..\foo` on Windows is caught
// alongside `../foo` on POSIX.
func relInsideRoot(root, resolved string) error {
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes sandbox %q", resolved, root)
	}
	return nil
}
