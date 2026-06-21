package builtin

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// resolveVolume resolves which sandbox root a tool call targets and whether
// that binding is read-only (RFC AH) — WITHOUT applying the read-write
// requirement. It is the shared core of two callers with different ro
// postures:
//
//   - effectiveRoot (Read/Write/Edit/Glob/Grep/Bash) refuses a read-only
//     binding on a write, because none of them can HONESTLY enforce ro — a
//     shell or an absolute path defeats path-confinement (RFC §6).
//   - the Bashbox tool HONORS ro instead: it mounts the root under gbash's
//     in-RAM write overlay, so writes succeed in-sandbox but never touch the
//     host tree. It needs (root, readOnly) to choose the mount mode.
//
// The returned name is the binding's canonical name for model-facing error
// messages (== volumeName for an ephemeral or explicitly-named binding; the
// resolved default's real name when volumeName is empty).
//
// Resolution rules (RFC §2/§5):
//
//   - RFC AH Phase 2b — ephemeral (run-tree-scoped) volumes resolve FIRST,
//     but ONLY for an explicitly NAMED volume (ephemeral volumes are always
//     named — an omitted `volume` uses the default-binding logic below). A
//     name that is NOT an ephemeral volume falls through to the Phase-1/2a
//     VolumePolicy binding logic — ephemeral resolution is purely additive,
//     it never bypasses confinement.
//
//   - No VolumePolicy on ctx (an UNBOUND agent, or a deployment with no
//     `volumes:` config) → DENY. There is no fallback root; declare a
//     `volumes:` block (with a `default` volume to restore the old
//     single-jail behaviour).
//
//   - volumeName == "" → the binding marked Default, or the SOLE binding
//     when there's exactly one. Multiple bindings + no designated default →
//     error listing the names.
//
//   - volumeName != "" → that binding; the agent isn't bound to it → error
//     listing the volumes it IS bound to.
func resolveVolume(ctx context.Context, volumeName string) (root string, readOnly bool, name string, err error) {
	if volumeName != "" {
		if ref, ok := tools.EphemeralVolumes(ctx).Get(volumeName); ok {
			return ref.Root, ref.ReadOnly, volumeName, nil
		}
	}

	pol := tools.VolumePolicy(ctx)
	if !pol.Active {
		// No volume confinement in force → DENY (RFC AH Phase 3:
		// sandbox-by-default; the legacy fallback root is gone). An agent
		// gets filesystem access only via a `volumes:` binding.
		return "", false, "", fmt.Errorf("no filesystem volume available to this agent — bind one via the agent's `volumes:` list (Read/Write/etc. require a volume)")
	}
	if len(pol.Bindings) == 0 {
		// Active but confined to NOTHING (e.g. a sub-agent whose declared
		// volumes share none of the parent's). Deny — identical posture to
		// the inactive case above; spawn confinement must never leak a root.
		return "", false, "", fmt.Errorf("no filesystem volume is available to this agent; refusing")
	}

	var binding *tools.VolumeBinding
	if volumeName == "" {
		binding = defaultBinding(pol.Bindings)
		if binding == nil {
			return "", false, "", fmt.Errorf("no volume specified and no default binding; specify one of: %s", volumeNames(pol.Bindings))
		}
	} else {
		for i := range pol.Bindings {
			if pol.Bindings[i].Name == volumeName {
				binding = &pol.Bindings[i]
				break
			}
		}
		if binding == nil {
			return "", false, "", fmt.Errorf("not bound to volume %q; available: %s", volumeName, volumeNames(pol.Bindings))
		}
	}

	return binding.Root, binding.ReadOnly, binding.Name, nil
}

// effectiveRoot resolves which sandbox root a file/exec tool call targets
// (RFC AH) and enforces the rw requirement. It is the ONE seam volumes add:
// every file tool calls this to pick a root, then the UNCHANGED
// resolveInsideRoot(root, path) does the TOCTOU-safe containment check
// against it.
//
// needWrite && readOnly → refuse (Write/Edit/NotebookEdit and Bash require
// rw; none can truly enforce ro, so they refuse rather than ship a false
// guarantee — RFC §6). All resolution lives in resolveVolume.
func effectiveRoot(ctx context.Context, volumeName string, needWrite bool) (string, error) {
	root, readOnly, name, err := resolveVolume(ctx, volumeName)
	if err != nil {
		return "", err
	}
	if needWrite && readOnly {
		return "", fmt.Errorf("volume %q is read-only; this operation requires a read-write volume", name)
	}
	return root, nil
}

// defaultBinding returns the binding a call with no explicit `volume`
// targets: the one marked Default, else the sole binding when there's
// exactly one, else nil (the caller errors listing the names). Returns a
// pointer into the input slice — do not retain past the call.
func defaultBinding(bindings []tools.VolumeBinding) *tools.VolumeBinding {
	for i := range bindings {
		if bindings[i].Default {
			return &bindings[i]
		}
	}
	if len(bindings) == 1 {
		return &bindings[0]
	}
	return nil
}

// volumeNames renders the bound volume names for a model-facing error so
// the model can pick a valid one without an extra Context op=self call.
func volumeNames(bindings []tools.VolumeBinding) string {
	names := make([]string, 0, len(bindings))
	for _, b := range bindings {
		names = append(names, b.Name)
	}
	return strings.Join(names, ", ")
}

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
