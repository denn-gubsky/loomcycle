package builtin

import (
	"fmt"
	"regexp"
	"strings"
)

// Path normalization for the RFC AL Path primitive. Canonical form: slash-
// rooted, no "..", no empty segments, case-sensitive, segment chars
// [a-zA-Z0-9._-] (no spaces, no inner slashes), <=64 segments, <=1024 chars.
// This is the logical-path analog of sandbox.go's relInsideRoot host-path
// confinement — a crafted path can't escape its (tenant, scope) tree because
// ".." is rejected outright rather than resolved.

const (
	maxPathLen      = 1024
	maxPathSegments = 64
	maxSegmentLen   = 64
)

var pathSegmentRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// normalizePath validates and canonicalizes a path. Returns the canonical form
// ("/" for the root, or "/a/b/c" with no trailing slash) or an error the model
// can self-correct from.
func normalizePath(raw string) (string, error) {
	if len(raw) > maxPathLen {
		return "", fmt.Errorf("path too long (%d chars, max %d)", len(raw), maxPathLen)
	}
	if raw == "" || raw == "/" {
		return "/", nil
	}
	if !strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("path must be absolute (start with %q): %q", "/", raw)
	}
	segs := make([]string, 0, 8)
	for _, p := range strings.Split(raw, "/") {
		if p == "" {
			continue // collapse "//" and a trailing "/"
		}
		if p == "." || p == ".." {
			return "", fmt.Errorf("path may not contain %q segments: %q", p, raw)
		}
		if len(p) > maxSegmentLen {
			return "", fmt.Errorf("path segment %q too long (max %d chars)", p, maxSegmentLen)
		}
		if !pathSegmentRe.MatchString(p) {
			return "", fmt.Errorf("invalid path segment %q (allowed: letters, digits, . _ -): %q", p, raw)
		}
		segs = append(segs, p)
	}
	if len(segs) == 0 {
		return "/", nil
	}
	if len(segs) > maxPathSegments {
		return "", fmt.Errorf("too many path segments (%d, max %d)", len(segs), maxPathSegments)
	}
	return "/" + strings.Join(segs, "/"), nil
}

// splitPath splits a canonical full path into the stored parent_path (always
// trailing-slashed) and the leaf name. isRoot is true for "/" (no name).
//
//	"/docs/x" -> ("/docs/", "x", false)
//	"/x"      -> ("/", "x", false)
//	"/"       -> ("", "", true)
func splitPath(canonical string) (parentPath, name string, isRoot bool) {
	if canonical == "/" {
		return "", "", true
	}
	i := strings.LastIndex(canonical, "/")
	return canonical[:i+1], canonical[i+1:], false
}

// dirPrefix returns the stored parent_path of the children of a directory at
// the given canonical path: the root's children sit at "/", and "/docs"'s
// children sit at "/docs/".
func dirPrefix(canonical string) string {
	if canonical == "/" {
		return "/"
	}
	return canonical + "/"
}
