package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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
// Sandbox model: same as Read — the root comes from the run's VolumePolicy
// (RFC AH); an agent bound to no volume is refused. All paths resolved via
// resolveInsideRoot — symlinks evaluated; refusal on root escape.
type Grep struct {
	// MaxOutputBytes caps the assembled response so a model-readable
	// pattern that matches half the repo doesn't blow the context
	// window. 0 = 256 KiB default.
	MaxOutputBytes int
}

const grepDefaultMaxOutputBytes = 256 * 1024

func (g *Grep) Name() string { return "Grep" }

func (g *Grep) Description() string {
	return "Search file CONTENTS on a volume with an RE2 regular expression. " +
		"Takes `pattern` (RE2 — no backreferences or lookaround); optional `path` narrows the subtree, `glob` filters which files are searched, " +
		"`output_mode` selects paths / matching lines / per-file counts, `-i` `-n` `-A` `-B` `-C` mirror the grep flags, " +
		"`head_limit` caps results (default 100) and `volume` picks a non-default one. " +
		"Use this to find WHERE something appears — a symbol, an error string, a config key. " +
		"Prefer it over reading files one by one: locate with Grep, then Read the file it names. " +
		"Do NOT use it to match file names — that is Glob. " +
		"Binary files are skipped, symlinks are not followed out of the volume, and output is capped at 256 KiB."
}

func (g *Grep) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern":           {"type": "string", "description": "RE2 regex to match against each file's contents."},
			"path":              {"type": "string", "description": "Optional subpath under the volume root. Defaults to the root."},
			"volume":            {"type": "string", "description": "Optional volume name to search. Omit to use your default volume. Call Context op=self for the volumes you may access."},
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
	Volume          string `json:"volume,omitempty"`
	Glob            string `json:"glob,omitempty"`
	OutputMode      string `json:"output_mode,omitempty"`
	CaseInsensitive bool   `json:"case_insensitive,omitempty"`
	Multiline       bool   `json:"multiline,omitempty"`
	HeadLimit       int    `json:"head_limit,omitempty"`
	After           int    `json:"-A,omitempty"`
	Before          int    `json:"-B,omitempty"`
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
	root, fence, rootErr := effectiveRoot(ctx, args.Volume, false)
	if rootErr != nil {
		return errResult(rootErr.Error()), nil
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
	// Validate the optional `glob` filename filter up-front. Without
	// this pre-check, a malformed glob (e.g. `[unclosed`) would make
	// every filepath.Match return ErrBadPattern silently and the agent
	// would see "no matches" instead of a clear error.
	if args.Glob != "" {
		if _, gerr := filepath.Match(args.Glob, ""); gerr != nil {
			return errResult(fmt.Sprintf("invalid glob pattern: %v", gerr)), nil
		}
	}

	// Resolve effective search root. Empty path = the sandbox root
	// itself; non-empty = subpath of it (resolved + symlink-checked).
	// Relative paths are joined with the root; absolute paths flow
	// through and get the same escape check.
	// The walk starts at a REAL path: WalkDir does not descend a root that is
	// itself a symlink, so a volume whose root is one searched nothing.
	// Results are relative to the walk root, so the output is unchanged.
	searchRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return errResult("sandbox root: " + err.Error()), nil
	}
	if args.Path != "" {
		target := args.Path
		if !filepath.IsAbs(target) {
			target = filepath.Join(root, target)
		}
		resolved, rerr := resolveInsideRoot(root, target)
		if rerr == nil {
			rerr = outsideFence(fence, resolved)
		}
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

	res, n, err := grepWalk(root, fence, searchRoot, re, args.Glob, mode, headLimit, maxBytes, before, after)
	if err != nil {
		return errResult(err.Error()), nil
	}
	// Count travels as structure so a caller never has to parse prose — and
	// 0 is the value that matters, because it is what distinguishes "searched,
	// found nothing" from "the search did not run".
	return tools.Result{Text: res, Count: &n}, nil
}

// grepWalk does the file iteration. Pulled out so tests can drive
// it with a synthetic root. `root` is the sandbox volume root; the walk and
// every file open go through an os.Root opened at it (RFC ES #1), and a
// symlinked entry is re-checked against it (symlink-resolved) before being
// opened, so an in-volume symlink pointing outside the volume can't leak
// out-of-volume file contents.
// grepWalk renders the matches and reports HOW MANY result items it wrote, in
// whatever unit the mode counts in: files for files_with_matches and count,
// matching lines for content. The caller surfaces it so a reader never has to
// infer "zero" by noticing the absence of output.
func grepWalk(root, fence, searchRoot string, re *regexp.Regexp, glob, mode string, headLimit, maxBytes, before, after int) (string, int, error) {
	// Open the sandbox root ONCE; each walked file is checked against it
	// below. A root that can't be resolved is a misconfiguration, not a
	// per-file skip, so fail the walk.
	vfs, err := openVolumeFS(root, fence)
	if err != nil {
		return "", 0, err
	}
	defer vfs.Close()
	start, err := filepath.EvalSymlinks(searchRoot)
	if err != nil {
		return "", 0, fmt.Errorf("walk: %w", err)
	}
	startRel, err := vfs.rel(start)
	if err != nil {
		return "", 0, err
	}

	var (
		out      bytes.Buffer
		results  int
		truncate = false
	)

	// Collect candidate files first so the iteration order is
	// deterministic (lexicographic). The walk visits in sorted order so we
	// just track paths as they come.
	type fileMatch struct {
		path    string
		matches []grepMatch
	}
	var perFile []fileMatch

	vfs.walk(startRel, func(dir *os.Root, path string, d fs.DirEntry) {
		if glob != "" {
			matched, gerr := filepath.Match(glob, d.Name())
			if gerr != nil || !matched {
				return
			}
		}
		var (
			f    *os.File
			oerr error
		)
		if d.Type()&fs.ModeSymlink != 0 {
			// Symlink containment: the walk never descends a symlinked DIR,
			// but it still yields a symlinked FILE entry. An in-volume
			// symlink to /etc/passwd (plantable via a rw Bash/Bashbox, or
			// pre-existing in a mounted dir) must not leak out-of-volume file
			// CONTENTS. Resolve the entry and re-assert it is inside the
			// volume: an in-volume symlink resolves to its real in-volume path
			// (still grepped, opened through the root); one that escapes is
			// skipped, never opened.
			abs := filepath.Join(start, path)
			resolved, rerr := filepath.EvalSymlinks(abs)
			if rerr != nil {
				return // dangling / unreadable symlink → skip
			}
			if relInsideRoot(vfs.dir, abs, resolved) != nil {
				return // escapes the volume → skip, never open
			}
			if outsideFence(fence, resolved) != nil {
				return // a symlink into the dynamic root → skip, never open
			}
			rel, rerr := vfs.rel(resolved)
			if rerr != nil {
				return
			}
			f, oerr = vfs.open(rel)
		} else {
			f, oerr = vfs.openIn(dir, d.Name())
		}
		if oerr != nil {
			return // skip silently — permission / disappeared / swapped
		}
		matches := grepFile(f, re, mode == "content", before, after)
		f.Close()
		if len(matches) == 0 {
			return
		}
		perFile = append(perFile, fileMatch{path: path, matches: matches})
	})

	// Deterministic ordering on path. The walk already sorts but be
	// explicit so the contract is testable.
	sort.Slice(perFile, func(i, j int) bool { return perFile[i].path < perFile[j].path })

	// Render per mode. The headLimit/maxBytes checks fire BEFORE
	// writing each entry so a result set that exactly fills the cap
	// doesn't get a spurious truncation marker.
	for _, fm := range perFile {
		rel := fm.path
		switch mode {
		case "files_with_matches":
			if results >= headLimit {
				truncate = true
				break
			}
			out.WriteString(rel + "\n")
			results++
		case "count":
			if results >= headLimit {
				truncate = true
				break
			}
			fmt.Fprintf(&out, "%s:%d\n", rel, len(fm.matches))
			results++
		case "content":
			for _, m := range fm.matches {
				if results >= headLimit {
					truncate = true
					break
				}
				if out.Len() >= maxBytes {
					truncate = true
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
			}
		}
		if truncate {
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
		// An empty result is a SUCCESS, and has to read like one. "no matches"
		// alone is a fragment that an agent can just as easily read as a failure;
		// saying the search completed is what separates "nothing is there" from
		// "the search did not run".
		return "no matches — the search completed successfully and the pattern was not found\n", 0, nil
	}
	return out.String(), results, nil
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

// grepFile matches one opened file. wantContent=true → return full match
// objects with optional before/after context. wantContent=false →
// return at least one match (we only need to know "any match"). A file it
// cannot read is skipped silently (no matches).
func grepFile(f io.ReadSeeker, re *regexp.Regexp, wantContent bool, before, after int) []grepMatch {
	// Binary detection: peek the first 8 KiB; refuse if NUL byte
	// present. Mirrors ripgrep's heuristic.
	peek := make([]byte, 8*1024)
	n, _ := f.Read(peek)
	if bytes.IndexByte(peek[:n], 0) >= 0 {
		return nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil
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
		return nil
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
	return out
}
