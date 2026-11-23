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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutsideVolume is a path that resolves outside the root. The refusals
// that carry it name the path as the caller wrote it.
var ErrOutsideVolume = errors.New("the path is outside the volume")

// pathError is a refusal whose text names only what the caller wrote: never
// the root's host path, a symlink's host target or the fence. The model reads
// this text, and so does anyone reading the run's transcript, so a host path
// in it would tell them where the volume lives on the server. err is what
// callers test with errors.Is: the errno (fs.ErrNotExist still matches
// ENOENT) or ErrOutsideVolume, never the *fs.PathError that named the host
// path.
type pathError struct {
	msg string
	err error
}

func (e *pathError) Error() string { return e.msg }
func (e *pathError) Unwrap() error { return e.err }

// osCause is the errno an os call failed with, without the *fs.PathError or
// *os.LinkError around it that names a host path.
func osCause(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}

// unresolved is the refusal for a path whose symlinks could not be
// evaluated: "does not exist" for the common case, the errno's own words
// otherwise.
func unresolved(shown string, cause error) error {
	if errors.Is(cause, fs.ErrNotExist) {
		return &pathError{msg: fmt.Sprintf("path %q does not exist", shown), err: cause}
	}
	return &pathError{msg: fmt.Sprintf("path %q: %v", shown, cause), err: cause}
}

// rootError is a root that cannot be resolved or opened. The root is
// operator configuration, so its host path is not the caller's to see.
func rootError(err error) error {
	cause := osCause(err)
	return &pathError{msg: fmt.Sprintf("the volume's directory cannot be used: %v", cause), err: cause}
}

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
		return "", rootError(err)
	}
	return rootResolved, nil
}

// resolveUnder is ResolveInsideRoot with the root already resolved. The order
// matters: both the root and the target are symlink-free before the Rel
// check, which is what refuses a target that is a symlink out of the root.
func resolveUnder(rootResolved, target string) (string, error) {
	return resolveUnderShown(rootResolved, target, target)
}

// resolveUnderShown is resolveUnder for a target the caller wrote as shown:
// Glob and Grep anchor a relative path to the root themselves before
// resolving it, and the refusal must name what the caller wrote.
func resolveUnderShown(rootResolved, target, shown string) (string, error) {
	abs := absUnderRoot(rootResolved, target)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", unresolved(shown, osCause(err))
	}
	if err := refuseOutside(rootResolved, shown, abs, resolved); err != nil {
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
		cause := osCause(err)
		if errors.Is(cause, fs.ErrNotExist) {
			return "", &pathError{msg: fmt.Sprintf("the folder that would hold %q does not exist", target), err: cause}
		}
		return "", &pathError{msg: fmt.Sprintf("the folder that would hold %q: %v", target, cause), err: cause}
	}
	parentEquivalent := filepath.Join(parentResolved, filepath.Base(abs))
	if err := refuseOutside(rootResolved, target, abs, parentEquivalent); err != nil {
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

// RelInsideRoot returns nil if resolved is inside root (or is root), and an
// error wrapping ErrOutsideVolume otherwise. requested is the path before its
// symlinks were evaluated, and the refusal names it and nothing else: neither
// root nor resolved, which are host paths. When requested is inside root and
// resolved is not, a symlink led it out, and the refusal says so.
func RelInsideRoot(root, requested, resolved string) error {
	return refuseOutside(root, requested, requested, resolved)
}

// refuseOutside is RelInsideRoot with the refusal naming shown, the path as
// the caller wrote it. abs is that path made absolute, before its symlinks
// were evaluated: inside root, it was a symlink that led out; outside, the
// path itself climbed out (`..`, or an absolute path elsewhere).
func refuseOutside(root, shown, abs, resolved string) error {
	if inside(root, resolved) {
		return nil
	}
	if inside(root, abs) {
		return &pathError{msg: fmt.Sprintf("path %q goes through a symbolic link that leads outside the volume", shown), err: ErrOutsideVolume}
	}
	return &pathError{msg: fmt.Sprintf("path %q escapes the volume", shown), err: ErrOutsideVolume}
}

// inside reports whether p is dir or under it. The HasPrefix form uses the
// OS-specific separator so `..\foo` on Windows is caught alongside `../foo`
// on POSIX.
func inside(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// OutsideFence refuses a resolved path that is the fence or under it. The
// fence is a real path (operator configuration) and resolved has had its
// symlinks evaluated, so a symlink inside the volume that points into the
// fence is refused as well. An empty fence refuses nothing. The refusal is
// ErrInFence, which names no path: both fence and resolved are host paths.
func OutsideFence(fence, resolved string) error {
	if fence == "" || !inside(fence, resolved) {
		return nil
	}
	return ErrInFence
}
