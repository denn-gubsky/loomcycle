package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Glob is a Claude-Code-compatible file pattern matcher. Walks the
// sandbox root and returns paths matching a glob, sorted by mtime
// DESC (newest first — matches the upstream behaviour).
//
// Pattern grammar:
//   - `*`   matches any run of chars (non-separator) inside one segment
//   - `?`   matches one char inside one segment
//   - `[..]`character class (RE2-style brackets via filepath.Match)
//   - `**`  matches zero or more segments — recursive
//
// `**` is the value-add over stdlib `filepath.Match`. Inline matcher
// below (~30 LOC) keeps the binary self-contained — no doublestar dep.
//
// Sandbox model: same Root as Read. Empty Root = refuse every call.
// All paths resolved via resolveInsideRoot — symlinks evaluated;
// refusal on root escape.
type Glob struct {
	// Root is the sandbox root (reuses LOOMCYCLE_READ_ROOT in
	// production; set in cmd/loomcycle/main.go). Empty = refuse.
	Root string
	// MaxResults caps the result list so a model-issued `**` doesn't
	// blow the context window. 0 = 100 default (matches Claude Code).
	MaxResults int
}

const globDefaultMaxResults = 100

func (g *Glob) Name() string { return "Glob" }

func (g *Glob) Description() string {
	return "Find files in the sandbox root by glob pattern (supports ** for recursive match). " +
		"Returns matching paths sorted by modification time, newest first."
}

func (g *Glob) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Glob pattern. Supports *, ?, [..], and ** (recursive)."},
			"path":    {"type": "string", "description": "Optional subpath under the sandbox root to scope the search."}
		},
		"required": ["pattern"]
	}`)
}

type globInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

func (g *Glob) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args globInput
	if err := json.Unmarshal(input, &args); err != nil {
		return errResult("invalid input: " + err.Error()), nil
	}
	if args.Pattern == "" {
		return errResult("pattern is required"), nil
	}
	if g.Root == "" {
		return errResult("Glob tool is not configured with a sandbox root; set LOOMCYCLE_READ_ROOT"), nil
	}
	// Validate every pattern segment up-front. filepath.Match returns
	// ErrBadPattern on malformed brackets etc.; without this pre-check
	// each per-file Match() call would silently return non-match,
	// leaving the agent with "no matches" instead of a clear error.
	// `**` survives as a literal segment for doublestarMatch — Match()
	// would also accept it as a no-op pattern against an empty string.
	for _, seg := range strings.Split(args.Pattern, "/") {
		if seg == "**" {
			continue
		}
		if _, err := filepath.Match(seg, ""); err != nil {
			return errResult(fmt.Sprintf("invalid glob pattern: %v", err)), nil
		}
	}

	searchRoot := g.Root
	if args.Path != "" {
		// Relative `path` is interpreted under the sandbox root; absolute
		// paths flow through unchanged and get the same escape check.
		// This matches Claude Code's "subpath under root" semantics
		// without losing the absolute-path refusal for /etc & friends.
		target := args.Path
		if !filepath.IsAbs(target) {
			target = filepath.Join(g.Root, target)
		}
		resolved, rerr := resolveInsideRoot(g.Root, target)
		if rerr != nil {
			return errResult(rerr.Error()), nil
		}
		searchRoot = resolved
	}

	maxResults := g.MaxResults
	if maxResults <= 0 {
		maxResults = globDefaultMaxResults
	}

	// Pre-compile the pattern into a segment slice so we don't re-split
	// per file. `**` survives as a literal sentinel inside the segment
	// matcher.
	patSegments := strings.Split(args.Pattern, "/")

	type fileInfo struct {
		rel   string
		mtime int64
	}
	var matches []fileInfo

	err := filepath.WalkDir(searchRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(searchRoot, path)
		if err != nil {
			return nil
		}
		// On Windows filepath.Rel uses `\`; normalise so the pattern
		// language is `/`-only regardless of host OS.
		rel = filepath.ToSlash(rel)
		relSegments := strings.Split(rel, "/")
		if !doublestarMatch(patSegments, relSegments) {
			return nil
		}
		info, statErr := d.Info()
		var mt int64
		if statErr == nil {
			mt = info.ModTime().UnixNano()
		}
		matches = append(matches, fileInfo{rel: rel, mtime: mt})
		return nil
	})
	if err != nil {
		return errResult(fmt.Sprintf("walk: %v", err)), nil
	}

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
		return tools.Result{Text: "no matches\n"}, nil
	}
	return tools.Result{Text: out.String()}, nil
}

// doublestarMatch matches a `/`-split pattern against a `/`-split
// path. `**` matches zero or more segments. All other segments fall
// through to stdlib filepath.Match (single-segment glob).
//
// Algorithm: backtracking DP over (pattern-index, path-index). Small
// enough for typical agent workloads — patterns rarely exceed 5
// segments and paths rarely exceed 10. If profiling ever flags this,
// switch to memoised DP.
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
