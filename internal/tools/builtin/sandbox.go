package builtin

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// effectiveRoot resolves which sandbox root a file/exec tool call targets
// (RFC AH Phase 1). It is the ONE seam volumes add: every file tool calls
// this to pick a root, then the UNCHANGED resolveInsideRoot(root, path)
// does the TOCTOU-safe containment check against it.
//
// Two paths:
//
//   - No VolumePolicy on ctx (an UNBOUND agent, or a deployment with no
//     `volumes:` config) → return fallbackRoot, the tool's
//     construction-time Root (the legacy jail). This is the
//     backward-compat path: behaviour is byte-identical to pre-feature.
//     The `volume` argument is ignored in this mode — there are no named
//     volumes to address, so naming one is meaningless rather than an
//     error (the legacy single-jail has exactly one root).
//
//   - A VolumePolicy exists (a BOUND agent) → resolve the named binding,
//     or the designated default when volumeName is empty, and enforce the
//     ro/rw axis. Returns a model-facing error (surfaced as is_error so
//     the model can self-correct) listing the available volumes on a
//     miss. Resolution rules (RFC §2/§5):
//
//   - volumeName == "" → the binding marked Default, or the SOLE
//     binding when there's exactly one. Multiple bindings + no
//     designated default → error listing the names.
//
//   - volumeName != "" → that binding; the agent isn't bound to it →
//     error listing the volumes it IS bound to.
//
//   - needWrite && binding.ReadOnly → refuse (Write/Edit/NotebookEdit
//     and Bash require rw; Bash cannot truly enforce ro, so it refuses
//     rather than ship a false guarantee — RFC §6).
func effectiveRoot(ctx context.Context, fallbackRoot, volumeName string, needWrite bool) (string, error) {
	// RFC AH Phase 2b — ephemeral (run-tree-scoped) volumes resolve FIRST,
	// but ONLY for an explicitly NAMED volume (ephemeral volumes are always
	// named — an omitted `volume` uses the existing default-binding logic
	// below). The run-scoped set is the resolution source; the ro/rw axis is
	// enforced here and the file tools' UNCHANGED resolveInsideRoot still
	// applies containment per-volume. A name that is NOT an ephemeral volume
	// falls through to the Phase-1/2a VolumePolicy binding logic — ephemeral
	// resolution is purely additive, it never bypasses confinement.
	if volumeName != "" {
		if ref, ok := tools.EphemeralVolumes(ctx).Get(volumeName); ok {
			if needWrite && ref.ReadOnly {
				return "", fmt.Errorf("volume %q is read-only; this operation requires a read-write volume", volumeName)
			}
			return ref.Root, nil
		}
	}

	pol := tools.VolumePolicy(ctx)
	if !pol.Active {
		// No volume confinement in force → legacy construction-time root.
		// fallbackRoot may be "" (an unset root), in which case the caller's
		// existing empty-Root guard refuses the call exactly as before.
		return fallbackRoot, nil
	}
	if len(pol.Bindings) == 0 {
		// Active but confined to NOTHING (e.g. a sub-agent whose declared
		// volumes share none of the parent's). Deny — do NOT fall back to the
		// legacy jail, or spawn confinement would be a no-op.
		return "", fmt.Errorf("no filesystem volume is available to this agent; refusing")
	}

	var binding *tools.VolumeBinding
	if volumeName == "" {
		binding = defaultBinding(pol.Bindings)
		if binding == nil {
			return "", fmt.Errorf("no volume specified and no default binding; specify one of: %s", volumeNames(pol.Bindings))
		}
	} else {
		for i := range pol.Bindings {
			if pol.Bindings[i].Name == volumeName {
				binding = &pol.Bindings[i]
				break
			}
		}
		if binding == nil {
			return "", fmt.Errorf("not bound to volume %q; available: %s", volumeName, volumeNames(pol.Bindings))
		}
	}

	if needWrite && binding.ReadOnly {
		return "", fmt.Errorf("volume %q is read-only; this operation requires a read-write volume", binding.Name)
	}
	return binding.Root, nil
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
