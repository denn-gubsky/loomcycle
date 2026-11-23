package fsops

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultGlobMaxResults caps a glob's listing when GlobRequest.MaxResults is
// unset, so a `**` cannot blow a context window.
const DefaultGlobMaxResults = 100

// ErrPatternRequired is a glob or grep given no pattern.
var ErrPatternRequired = errors.New("pattern is required")

// GlobRequest is one file-name search. Path optionally narrows it to a
// directory under the root; MaxResults <= 0 means DefaultGlobMaxResults.
type GlobRequest struct {
	Pattern    string
	Path       string
	MaxResults int
}

// GlobEmpty is the text of a glob that matched nothing. An empty list is a
// legitimate ANSWER, not a failure, and the text says so — "no matches" alone
// is a fragment an agent can read either way.
const GlobEmpty = "no matches — the search completed successfully and no file matched the pattern\n"

// Glob lists the files under root whose path, relative to the search
// directory, matches req.Pattern: `*` and `?` within one segment, `[..]` a
// class, `**` zero or more segments. It returns the listing (newest first,
// then by path), and how many entries it holds: at most MaxResults, followed
// by a truncation line when there were more. An absolute pattern is matched
// relative to the search directory; one outside it matches nothing. The walk
// never follows a symlinked directory and skips the fence.
func Glob(root, fence string, req GlobRequest) (string, int, error) {
	if req.Pattern == "" {
		return "", 0, ErrPatternRequired
	}
	// Validate every pattern segment up-front. filepath.Match returns
	// ErrBadPattern on malformed brackets etc.; without this pre-check each
	// per-file Match() call would silently return non-match, leaving the agent
	// with "no matches" instead of a clear error. `**` survives as a literal
	// segment for doublestarMatch.
	for _, seg := range strings.Split(req.Pattern, "/") {
		if seg == "**" {
			continue
		}
		if _, err := filepath.Match(seg, ""); err != nil {
			return "", 0, fmt.Errorf("invalid glob pattern: %v", err)
		}
	}

	// The walk starts at a REAL path: a walk does not descend a root that is
	// itself a symlink. Matches are relative to the walk root, so the output
	// is unchanged; declared is the same directory as the caller names it,
	// for an absolute pattern.
	declared := root
	searchRoot, err := resolveRoot(root)
	if err != nil {
		return "", 0, err
	}
	rootResolved := searchRoot
	if req.Path != "" {
		// Relative `path` is interpreted under the root; absolute paths flow
		// through unchanged and get the same escape check.
		target := req.Path
		if !filepath.IsAbs(target) {
			target = filepath.Join(root, target)
		}
		resolved, rerr := resolveUnderShown(rootResolved, target, req.Path)
		if rerr == nil {
			rerr = OutsideFence(fence, resolved)
		}
		if rerr != nil {
			return "", 0, rerr
		}
		declared, searchRoot = target, resolved
	}

	// The matcher compares pattern segments against walk-root-RELATIVE file
	// paths. An absolute pattern's leading "/" splits to an empty first
	// segment that never matches a relative path, so an absolute in-root
	// pattern silently returned no matches (exp7 R1). Relativize it to the
	// walk root; an absolute pattern pointing outside the root can't match
	// anything in the sandbox.
	pattern := req.Pattern
	if filepath.IsAbs(pattern) {
		rel, inRoot := relativizeToRoot(searchRoot, pattern)
		if !inRoot {
			rel, inRoot = relativizeToRoot(declared, pattern)
		}
		if !inRoot {
			return GlobEmpty, 0, nil
		}
		pattern = rel
	}

	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = DefaultGlobMaxResults
	}
	patSegments := strings.Split(pattern, "/")

	type fileInfo struct {
		rel   string
		mtime int64
	}
	var matches []fileInfo

	vfs, err := openResolved(rootResolved, fence)
	if err != nil {
		return "", 0, err
	}
	defer vfs.Close()
	startRel, err := vfs.Rel(searchRoot)
	if err != nil {
		return "", 0, err
	}
	vfs.Walk(startRel, func(dir *os.Root, path string, d fs.DirEntry) {
		// On Windows paths use `\`; normalise so the pattern language is
		// `/`-only regardless of host OS.
		rel := filepath.ToSlash(path)
		if !doublestarMatch(patSegments, strings.Split(rel, "/")) {
			return
		}
		info, statErr := dir.Lstat(d.Name())
		var mt int64
		if statErr == nil {
			mt = info.ModTime().UnixNano()
		}
		matches = append(matches, fileInfo{rel: rel, mtime: mt})
	})

	// mtime DESC; tie-break on path ASC for determinism.
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].mtime != matches[j].mtime {
			return matches[i].mtime > matches[j].mtime
		}
		return matches[i].rel < matches[j].rel
	})

	var out bytes.Buffer
	truncated := false
	for i, m := range matches {
		if i >= maxResults {
			truncated = true
			break
		}
		out.WriteString(m.rel + "\n")
	}
	if truncated {
		fmt.Fprintf(&out, "\n[truncated at max_results=%d; %d total matches]\n", maxResults, len(matches))
	}
	if out.Len() == 0 {
		return GlobEmpty, 0, nil
	}
	n := len(matches)
	if truncated {
		n = maxResults
	}
	return out.String(), n, nil
}

// relativizeToRoot converts an absolute glob pattern to one relative to root
// (the walk root). Returns (rel, true) when the pattern sits inside root, or
// ("", false) when it is the root itself or points outside it — in which case
// it can match nothing in the sandbox. Glob metacharacters are preserved;
// only the fixed leading directory prefix is stripped. filepath.Clean
// collapses any `..` first, so a traversal-escaping pattern fails the prefix
// check and yields false.
func relativizeToRoot(root, pattern string) (string, bool) {
	cleanRoot := filepath.Clean(root)
	cleanPat := filepath.Clean(pattern)
	if cleanPat == cleanRoot {
		return "", false
	}
	prefix := cleanRoot + string(filepath.Separator)
	if !strings.HasPrefix(cleanPat, prefix) {
		return "", false
	}
	return filepath.ToSlash(cleanPat[len(prefix):]), true
}

// doublestarMatch matches a `/`-split pattern against a `/`-split
// path. `**` matches zero or more segments. All other segments fall
// through to stdlib filepath.Match (single-segment glob).
//
// Algorithm: backtracking over (pattern-index, path-index). Small enough for
// typical agent workloads — patterns rarely exceed 5 segments and paths
// rarely exceed 10. If profiling ever flags this, switch to memoised DP.
func doublestarMatch(pat, path []string) bool {
	// Empty pattern matches empty path only.
	if len(pat) == 0 {
		return len(path) == 0
	}
	if pat[0] == "**" {
		// `**` consumes zero or more path segments. Try each prefix
		// length until one matches the remaining pattern.
		for i := 0; i <= len(path); i++ {
			if doublestarMatch(pat[1:], path[i:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	ok, err := filepath.Match(pat[0], path[0])
	if err != nil || !ok {
		return false
	}
	return doublestarMatch(pat[1:], path[1:])
}
