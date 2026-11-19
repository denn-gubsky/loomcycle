package builtin

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
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
// fsops.ResolveInsideRoot — symlinks evaluated; refusal on root escape.
type Grep struct {
	// MaxOutputBytes caps the assembled response so a model-readable
	// pattern that matches half the repo doesn't blow the context
	// window. 0 = 256 KiB default.
	MaxOutputBytes int
}

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
	res, n, err := fsops.Grep(root, fence, fsops.GrepRequest{
		Pattern:         args.Pattern,
		Path:            args.Path,
		Glob:            args.Glob,
		OutputMode:      args.OutputMode,
		CaseInsensitive: args.CaseInsensitive,
		Multiline:       args.Multiline,
		HeadLimit:       args.HeadLimit,
		MaxOutputBytes:  g.MaxOutputBytes,
		Before:          args.Before,
		After:           args.After,
		Context:         args.Context,
	})
	if err != nil {
		return errResult(err.Error()), nil
	}
	// Count travels as structure so a caller never has to parse prose — and
	// 0 is the value that matters, because it is what distinguishes "searched,
	// found nothing" from "the search did not run".
	return tools.Result{Text: res, Count: &n}, nil
}
