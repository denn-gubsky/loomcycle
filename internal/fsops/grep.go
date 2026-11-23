package fsops

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// The default bounds of a grep, when GrepRequest leaves them unset.
const (
	DefaultGrepMaxOutputBytes = 256 * 1024 // assembled output
	DefaultGrepHeadLimit      = 100        // result items
)

// grepEmpty is the text of a grep that matched nothing. An empty result is a
// SUCCESS, and has to read like one: "no matches" alone is a fragment that an
// agent can just as easily read as a failure.
const grepEmpty = "no matches — the search completed successfully and the pattern was not found\n"

// GrepRequest is one content search. Pattern is RE2. Path optionally narrows
// it to a file or directory under the root; Glob filters file names (one
// segment). OutputMode is files_with_matches (default), content or count.
// HeadLimit <= 0 means DefaultGrepHeadLimit, MaxOutputBytes <= 0
// DefaultGrepMaxOutputBytes. Context > 0 sets both Before and After.
type GrepRequest struct {
	Pattern         string
	Path            string
	Glob            string
	OutputMode      string
	CaseInsensitive bool
	Multiline       bool
	HeadLimit       int
	MaxOutputBytes  int
	Before          int
	After           int
	Context         int
}

// Grep searches the contents of the files under root. It returns the rendered
// result and how many items it holds, in the mode's unit: files for
// files_with_matches and count, matching lines for content.
//
// Bounds: at most HeadLimit items, and output stops growing once it reaches
// MaxOutputBytes (a truncation line says so); a file holding a line longer
// than 1 MiB, or with a NUL byte in its first 8 KiB (binary), yields no
// matches. A symlinked file is followed only when it resolves inside the root
// and outside the fence; a symlinked directory is never descended.
func Grep(root, fence string, req GrepRequest) (string, int, error) {
	if req.Pattern == "" {
		return "", 0, ErrPatternRequired
	}
	// Compile with the flags as a prefix group so a pattern that already
	// begins with (?...) does not double-stack.
	flags := ""
	if req.CaseInsensitive {
		flags += "i"
	}
	if req.Multiline {
		flags += "sm"
	}
	pat := req.Pattern
	if flags != "" {
		pat = "(?" + flags + ")" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return "", 0, fmt.Errorf("invalid regex: %v", err)
	}
	// Validate the optional glob filter up-front: a malformed one would make
	// every filepath.Match fail silently and read as "no matches".
	if req.Glob != "" {
		if _, gerr := filepath.Match(req.Glob, ""); gerr != nil {
			return "", 0, fmt.Errorf("invalid glob pattern: %v", gerr)
		}
	}

	// The walk starts at a REAL path: a walk does not descend a root that is
	// itself a symlink. Results are relative to the walk root, so the output
	// is unchanged.
	rootResolved, err := resolveRoot(root)
	if err != nil {
		return "", 0, err
	}
	searchRoot := rootResolved
	if req.Path != "" {
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
		searchRoot = resolved
	}

	mode := req.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}
	switch mode {
	case "files_with_matches", "content", "count":
	default:
		return "", 0, fmt.Errorf("unknown output_mode %q (want files_with_matches|content|count)", mode)
	}
	headLimit := req.HeadLimit
	if headLimit <= 0 {
		headLimit = DefaultGrepHeadLimit
	}
	maxBytes := req.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = DefaultGrepMaxOutputBytes
	}
	// Context overrides Before/After when set (as Claude Code's Grep does).
	before, after := req.Before, req.After
	if req.Context > 0 {
		before, after = req.Context, req.Context
	}

	vfs, err := openResolved(rootResolved, fence)
	if err != nil {
		return "", 0, err
	}
	defer vfs.Close()
	return grepWalk(vfs, fence, searchRoot, re, req.Glob, mode, headLimit, maxBytes, before, after)
}

// grepWalk walks start (a symlink-free path under the root) and renders the
// matches. Every walked file is opened through vfs; a symlinked entry is
// re-checked against the root and the fence before it is opened, so an
// in-root symlink pointing outside the root can't leak out-of-root file
// contents.
func grepWalk(vfs *VolumeFS, fence, start string, re *regexp.Regexp, glob, mode string, headLimit, maxBytes, before, after int) (string, int, error) {
	startRel, err := vfs.Rel(start)
	if err != nil {
		return "", 0, err
	}

	var (
		out      bytes.Buffer
		results  int
		truncate = false
	)

	type fileMatch struct {
		path    string
		matches []grepMatch
	}
	var perFile []fileMatch

	vfs.Walk(startRel, func(dir *os.Root, path string, d fs.DirEntry) {
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
			// but it still yields a symlinked FILE entry. An in-root symlink
			// to /etc/passwd (plantable via a rw Bash/Bashbox, or pre-existing
			// in a mounted dir) must not leak out-of-root file CONTENTS.
			// Resolve the entry and re-assert it is inside the root: an
			// in-root symlink resolves to its real in-root path (still
			// grepped, opened through the root); one that escapes is skipped,
			// never opened.
			abs := filepath.Join(start, path)
			resolved, rerr := filepath.EvalSymlinks(abs)
			if rerr != nil {
				return // dangling / unreadable symlink → skip
			}
			if RelInsideRoot(vfs.dir, abs, resolved) != nil {
				return // escapes the root → skip, never open
			}
			if OutsideFence(fence, resolved) != nil {
				return // a symlink into the fence → skip, never open
			}
			rel, rerr := vfs.Rel(resolved)
			if rerr != nil {
				return
			}
			f, oerr = vfs.Open(rel)
		} else {
			f, oerr = vfs.OpenIn(dir, d.Name())
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
		return grepEmpty, 0, nil
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
