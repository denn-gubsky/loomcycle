package builtin

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
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
// `**` is the value-add over stdlib `filepath.Match`. The matcher in
// internal/fsops (~30 LOC) keeps the binary self-contained — no doublestar dep.
//
// Sandbox model: same as Read — the root comes from the run's VolumePolicy
// (RFC AH); an agent bound to no volume is refused. All paths resolved via
// fsops.ResolveInsideRoot — symlinks evaluated; refusal on root escape.
type Glob struct {
	// MaxResults caps the result list so a model-issued `**` doesn't
	// blow the context window. 0 = 100 default (matches Claude Code).
	MaxResults int
}

func (g *Glob) Name() string { return "Glob" }

func (g *Glob) Description() string {
	return "Find files by NAME on a volume, using a glob pattern. " +
		"Takes `pattern` — `*` and `?` match within one path segment, `[abc]` is a character class, and `**` matches across segments recursively " +
		"(e.g. `**/*_test.go`); optional `path` narrows to a subdirectory and `volume` picks a non-default one. " +
		"Returns matching paths sorted by modification time, newest first, so recently-touched files come back on top. " +
		"Use this when you know roughly what a file is CALLED but not where it is. " +
		"Do NOT use it to search file CONTENTS — that is Grep. " +
		"It matches paths only: a pattern that matches nothing returns an empty list rather than an error."
}

func (g *Glob) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Glob pattern. Supports *, ?, [..], and ** (recursive)."},
			"path":    {"type": "string", "description": "Optional subpath under the volume root to scope the search."},
			"volume":  {"type": "string", "description": "Optional volume name to search. Omit to use your default volume. Call Context op=self for the volumes you may access."}
		},
		"required": ["pattern"]
	}`)
}

type globInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Volume  string `json:"volume,omitempty"`
}

func (g *Glob) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args globInput
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
	text, n, err := fsops.Glob(root, fence, fsops.GlobRequest{Pattern: args.Pattern, Path: args.Path, MaxResults: g.MaxResults})
	if err != nil {
		return errResult(err.Error()), nil
	}
	// Count is 0 rather than absent on no match: absent means "this tool does
	// not count", which is a different statement.
	return tools.Result{Text: text, Count: &n}, nil
}
