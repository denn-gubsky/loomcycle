package config

import (
	"fmt"
	"slices"
	"strings"
)

// WorkspacePushConfig is `workspace.push` (RFC GW §6.2; the owner's rulings
// of 2026-11-26, P11 plan §7.1): what a git workspace's push may do. A push
// moves only the workspace's own branch; these keys bound what that branch
// may be named, whether a tag may be pushed, and how large a push is.
type WorkspacePushConfig struct {
	// RefPrefixes are where a workspace's own branch may be named: each a
	// prefix under refs/heads/ ending in "/". Listing refs/tags/ also lets
	// a push create a tag under it. Default [refs/heads/].
	RefPrefixes []string `yaml:"ref_prefixes,omitempty"`
	// MaxBytes bounds one push's body: go-git holds a whole push in the
	// worker's memory. Default 64 MiB, at most 1 GiB.
	MaxBytes int64 `yaml:"max_bytes,omitempty"`
	// AdoptExisting lets a workspace be opened on a branch that already
	// exists on the forge. Off, such an open is refused: a forced update of
	// the own branch is accepted (D11), so an agent could otherwise rewrite
	// a branch someone else works on.
	AdoptExisting bool `yaml:"adopt_existing,omitempty"`
}

const (
	defaultWorkspacePushBytes = 64 << 20
	maxWorkspacePushBytes     = 1 << 30
	workspacePushTags         = "refs/tags/"
)

// Resolved fills the defaults.
func (p WorkspacePushConfig) Resolved() WorkspacePushConfig {
	if p.RefPrefixes == nil {
		p.RefPrefixes = []string{"refs/heads/"}
	} else {
		p.RefPrefixes = slices.Clone(p.RefPrefixes)
	}
	if p.MaxBytes == 0 {
		p.MaxBytes = defaultWorkspacePushBytes
	}
	return p
}

// AllowsTags reports whether a push may create a tag.
func (p WorkspacePushConfig) AllowsTags() bool { return slices.Contains(p.RefPrefixes, workspacePushTags) }

// BranchAllowed reports whether a workspace's own branch may be named branch
// (a short name): refs/heads/<branch> lies under a heads prefix listed.
func (p WorkspacePushConfig) BranchAllowed(branch string) bool {
	if branch == "" {
		return false
	}
	ref := "refs/heads/" + branch
	for _, pre := range p.RefPrefixes {
		if strings.HasPrefix(pre, "refs/heads/") && strings.HasPrefix(ref, pre) && len(ref) > len(pre) {
			return true
		}
	}
	return false
}

func validateWorkspacePush(p WorkspacePushConfig) error {
	if p.RefPrefixes != nil && len(p.RefPrefixes) == 0 {
		return fmt.Errorf("workspace.push.ref_prefixes: an empty list allows no branch; leave it out for the default [refs/heads/]")
	}
	seen := map[string]bool{}
	for _, pre := range p.RefPrefixes {
		if seen[pre] {
			return fmt.Errorf("workspace.push.ref_prefixes: %q is listed twice", pre)
		}
		seen[pre] = true
		if pre == workspacePushTags {
			continue
		}
		if !strings.HasPrefix(pre, "refs/heads/") || !strings.HasSuffix(pre, "/") || strings.Contains(pre, "..") || strings.Contains(pre, "//") || strings.ContainsAny(pre, " ~^:?*[\\") {
			return fmt.Errorf("workspace.push.ref_prefixes: %q is not refs/tags/ or a prefix under refs/heads/ ending in \"/\"", pre)
		}
	}
	if p.MaxBytes < 0 || p.MaxBytes > maxWorkspacePushBytes {
		return fmt.Errorf("workspace.push.max_bytes: %d is out of range (1 to %d, 1 GiB)", p.MaxBytes, int64(maxWorkspacePushBytes))
	}
	return nil
}
