package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Grep is a Claude-Code-compatible content search tool. Walks the
// sandbox root looking for files whose contents match a regex; returns
// either the matching paths, the matching lines with context, or per-
// file counts.
//
// Pure-Go implementation — no `rg` dependency. Trades some throughput
// on huge trees for a self-contained binary; for the typical agent
// workload (single-digit MB of source) the difference is invisible.
//
// Sandbox model: same Root as Read. A Grep with Root="" rejects every
// call (matching Read's posture). All paths resolved via
// resolveInsideRoot — symlinks evaluated; refusal on root escape.
type Grep struct {
	// Root is the sandbox root. Reuses LOOMCYCLE_READ_ROOT in
	// production (set in cmd/loomcycle/main.go). Empty = refuse.
	Root string
	// MaxOutputBytes caps the assembled response so a model-readable
	// pattern that matches half the repo doesn't blow the context
	// window. 0 = 256 KiB default.
	MaxOutputBytes int
}

const grepDefaultMaxOutputBytes = 256 * 1024

func (g *Grep) Name() string { return "Grep" }

func (g *Grep) Description() string {
	return "Search file contents in the sandbox root with an RE2 regex. " +
		"Returns matching file paths, lines with optional context, or per-file counts. " +
		"Binary files are skipped automatically."
}

func (g *Grep) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern":           {"type": "string", "description": "RE2 regex to match against each file's contents."},
			"path":              {"type": "string", "description": "Optional subpath under the sandbox root. Defaults to the root."},
			"glob":              {"type": "string", "description": "Optional single-segment filename pattern (e.g. *.go). Filenames not matching are skipped."},
			"output_mode":       {"type": "string", "enum": ["files_with_matches", "content", "count"], "description": "files_with_matches (default) returns paths; content returns file:line:text with optional -A/-B/-C context; count returns file:N."},
			"case_insensitive": {"type": "boolean", "description": "If true, applies the (?i) RE2 flag."},
			"multiline":         {"type": "boolean", "description": "If true, applies (?s)(?m) so . matches newlines and ^/$ match line boundaries."},
			"head_limit":        {"type": "integer", "description": "Maximum number of results to return. Default 100."},
			"-A":                {"type": "integer", "description": "Lines AFTER each match (content mode)."},
			"-B":                {"type": "integer", "description": "Lines BEFORE each match (content mode)."},
			"-C":                {"type": "integer", "description": "Lines BEFORE+AFTER each match (content mode); overrides -A/-B when set."}
		},
		"required": ["pattern"]
	}`)
}

type grepInput struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path,omitempty"`
	Glob            string `json:"glob,omitempty"`
	OutputMode      string `json:"output_mode,omitempty"`
	CaseInsensitive bool   `json:"case_insensitive,omitempty"`
	Multiline       bool   `json:"multiline,omitempty"`
	HeadLimit       int    `json:"head_limit,omitempty"`
	After           int    `json:"-A,omitempty"`
	Before           int    `json:"-B,omitempty"`
	Context         int    `json:"-C,omitempty"`
}

func (g *Grep) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args grepInput
	if err := json.Unmarshal(input, &args); err != nil {
		return errResult("invalid input: " + err.Error()), nil
	}
	if args.Pattern == "" {
		return errResult("pattern is required"), nil
	}
	if g.Root == "" {
		return errResult("Grep tool is not configured with a sandbox root; set LOOMCYCLE_READ_ROOT"), nil
	}

	// Compile regex with applied flags. Use bracket prefix so user
	// patterns that already begin with (?...) don't double-stack.
	flags := ""
	if args.CaseInsensitive {
		flags += "i"
	}
	if args.Multiline {
		flags += "sm"
	}
	pat := args.Pattern
	if flags != "" {
		pat = "(?" + flags + ")" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return errResult(fmt.Sprintf("invalid regex: %v", err)), nil
	}

	// Resolve effective search root. Empty path = the sandbox root
	// itself; non-empty = subpath of it (resolved + symlink-checked).
	// Relative paths are joined with the root; absolute paths flow
	// through and get the same escape check.
	searchRoot := g.Root
	if args.Path != "" {
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

	mode := args.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}
	switch mode {
	case "files_with_matches", "content", "count":
	default:
		return errResult(fmt.Sprintf("unknown output_mode %q (want files_with_matches|content|count)", mode)), nil
	}

	headLimit := args.HeadLimit
	if headLimit <= 0 {
		headLimit = 100
	}

	maxBytes := g.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = grepDefaultMaxOutputBytes
	}

	// `-C` overrides `-A`/`-B` when set (matches Claude Code).
	before, after := args.Before, args.After
	if args.Context > 0 {
		before, after = args.Context, args.Context
	}

	res, err := grepWalk(searchRoot, re, args.Glob, mode, headLimit, maxBytes, before, after)
	if err != nil {
		return errResult(err.Error()), nil
	}
	return tools.Result{Text: res}, nil
}

// grepWalk does the file iteration. Pulled out so tests can drive
// it with a synthetic root.
func grepWalk(searchRoot string, re *regexp.Regexp, glob, mode string, headLimit, maxBytes, before, after int) (string, error) {
	var (
		out      bytes.Buffer
		results  int
		truncate = false
	)

	// Collect candidate files first so the iteration order is
	// deterministic (lexicographic). filepath.WalkDir already visits
	// in sorted order so we just track paths as they come.
	type fileMatch struct {
		path    string
		matches []grepMatch
	}
	var perFile []fileMatch

	err := filepath.WalkDir(searchRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Skip dirs we can't enter; don't fail the whole walk.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if glob != "" {
			matched, gerr := filepath.Match(glob, d.Name())
			if gerr != nil || !matched {
				return nil
			}
		}
		matches, perr := grepFile(path, re, mode == "content", before, after)
		if perr != nil || len(matches) == 0 {
			return nil
		}
		perFile = append(perFile, fileMatch{path: path, matches: matches})
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk: %w", err)
	}

	// Deterministic ordering on path. WalkDir already sorts but be
	// explicit so the contract is testable.
	sort.Slice(perFile, func(i, j int) bool { return perFile[i].path < perFile[j].path })

	// Render per mode.
	for _, fm := range perFile {
		rel, _ := filepath.Rel(searchRoot, fm.path)
		if rel == "" {
			rel = fm.path
		}
		switch mode {
		case "files_with_matches":
			out.WriteString(rel + "\n")
			results++
		case "count":
			fmt.Fprintf(&out, "%s:%d\n", rel, len(fm.matches))
			results++
		case "content":
			for _, m := range fm.matches {
				if results >= headLimit {
					break
				}
				for _, line := range m.contextBefore {
					fmt.Fprintf(&out, "%s-%d-%s\n", rel, line.num, line.text)
				}
				fmt.Fprintf(&out, "%s:%d:%s\n", rel, m.lineNum, m.line)
				for _, line := range m.contextAfter {
					fmt.Fprintf(&out, "%s-%d-%s\n", rel, line.num, line.text)
				}
				results++
				if out.Len() >= maxBytes {
					truncate = true
					break
				}
			}
		}
		if results >= headLimit {
			truncate = true
			break
		}
		if out.Len() >= maxBytes {
			truncate = true
			break
		}
	}

	if truncate {
		out.WriteString(fmt.Sprintf("\n[truncated at head_limit=%d or max_bytes=%d]\n", headLimit, maxBytes))
	}
	if out.Len() == 0 {
		return "no matches\n", nil
	}
	return out.String(), nil
}

type grepMatch struct {
	lineNum       int
	line          string
	contextBefore []grepContextLine
	contextAfter  []grepContextLine
}

type grepContextLine struct {
	num  int
	text string
}

// grepFile matches one file. wantContent=true → return full match
// objects with optional before/after context. wantContent=false →
// return at least one match (we only need to know "any match").
func grepFile(path string, re *regexp.Regexp, wantContent bool, before, after int) ([]grepMatch, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil // skip silently — permission / disappeared
	}
	defer f.Close()

	// Binary detection: peek the first 8 KiB; refuse if NUL byte
	// present. Mirrors ripgrep's heuristic.
	peek := make([]byte, 8*1024)
	n, _ := f.Read(peek)
	if bytes.IndexByte(peek[:n], 0) >= 0 {
		return nil, nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, nil
	}

	// Read all lines into a slice so we can render -A/-B context.
	// For large files we'd want a sliding buffer; the head_limit +
	// max_bytes caps above keep total memory bounded.
	scanner := bufio.NewScanner(f)
	// Allow long lines up to 1 MiB; default 64 KiB chokes on
	// minified JS.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, nil
	}

	var out []grepMatch
	for i, line := range lines {
		if !re.MatchString(line) {
			continue
		}
		m := grepMatch{lineNum: i + 1, line: line}
		if wantContent {
			if before > 0 {
				start := i - before
				if start < 0 {
					start = 0
				}
				for j := start; j < i; j++ {
					m.contextBefore = append(m.contextBefore, grepContextLine{num: j + 1, text: lines[j]})
				}
			}
			if after > 0 {
				end := i + after + 1
				if end > len(lines) {
					end = len(lines)
				}
				for j := i + 1; j < end; j++ {
					m.contextAfter = append(m.contextAfter, grepContextLine{num: j + 1, text: lines[j]})
				}
			}
		}
		out = append(out, m)
	}
	return out, nil
}
