package builtin

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Edit replaces text in an existing file inside the sandbox root.
// Mirrors the Claude Code Edit semantics: by default the OldString must
// occur EXACTLY once — ambiguity is an error so the model can supply
// more surrounding context. ReplaceAll lifts that requirement.
//
// Sandbox mirrors Write: a rw volume is required. The file must already
// exist; Edit does not create files (callers should use Write for that).
// The whole file is read into memory, mutated, and written back via the
// same atomic tempfile + rename dance Write uses.
type Edit struct {
	// MaxBytes caps the file size Edit will load. Default 1 MiB.
	MaxBytes int64
}

func (e *Edit) Name() string { return "Edit" }
func (e *Edit) Description() string {
	return "Replace an exact string inside an EXISTING file on a read-write volume, leaving the rest untouched. " +
		"Takes `path`, `old_string` (must match the file byte for byte, whitespace included) and `new_string`; " +
		"optional `replace_all` lifts the uniqueness requirement, optional `volume` picks a non-default one. " +
		"By default `old_string` must occur EXACTLY ONCE — an ambiguous match is an error asking you for more surrounding context, not a guess. " +
		"Use this for every change to a file that already exists; it is the safe default because it cannot lose content you did not mention. " +
		"Do NOT use it to create a file — it refuses a missing path; use Write. " +
		"Files are read whole, so it is bounded at 1 MiB."
}

func (e *Edit) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path":        {"type": "string", "description": "Path RELATIVE to the volume root (e.g. \"src/main.go\"). ~ is not expanded; an absolute path is accepted only if it resolves inside the root. Call Context op=self to see your volumes."},
			"old_string":  {"type": "string", "description": "Exact text to replace. Must be unique unless replace_all is true."},
			"new_string":  {"type": "string", "description": "Replacement text. Must differ from old_string."},
			"replace_all": {"type": "boolean", "description": "Replace every occurrence instead of requiring exactly one match."},
			"volume":      {"type": "string", "description": "Optional read-write volume name to edit in. Omit to use your default volume. Read-only volumes are refused. Call Context op=self for the volumes you may access."}
		},
		"required": ["path", "old_string", "new_string"]
	}`)
}

func (e *Edit) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
		Volume     string `json:"volume"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.Result{Text: "invalid input: " + err.Error(), IsError: true}, nil
	}
	if args.Path == "" {
		return tools.Result{Text: "path is required", IsError: true}, nil
	}
	if args.OldString == args.NewString {
		return tools.Result{Text: fsops.ErrEditNoChange.Error(), IsError: true}, nil
	}
	root, fence, err := effectiveRoot(ctx, args.Volume, true)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	out, err := fsops.Edit(ctx, root, fence, fsops.EditRequest{
		Path:       args.Path,
		OldString:  args.OldString,
		NewString:  args.NewString,
		ReplaceAll: args.ReplaceAll,
		MaxBytes:   e.MaxBytes,
	})
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	return tools.Result{Text: out}, nil
}
