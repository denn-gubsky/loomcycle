package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	return "Replace text in an existing file inside the sandbox root."
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
		return tools.Result{Text: "old_string and new_string must differ", IsError: true}, nil
	}
	root, err := effectiveRoot(ctx, args.Volume, true)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}

	maxBytes := e.MaxBytes
	if maxBytes == 0 {
		maxBytes = 1 << 20
	}

	// Edit requires the file to exist, so we use resolveInsideRoot
	// (Write would resolve the parent because the file may be new).
	resolved, err := resolveInsideRoot(root, args.Path)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	if info.Size() > maxBytes {
		return tools.Result{Text: fmt.Sprintf("file size %d exceeds limit %d", info.Size(), maxBytes), IsError: true}, nil
	}

	body, err := os.ReadFile(resolved)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}

	count := strings.Count(string(body), args.OldString)
	if count == 0 {
		return tools.Result{Text: "old_string not found in file", IsError: true}, nil
	}
	if count > 1 && !args.ReplaceAll {
		return tools.Result{Text: fmt.Sprintf("old_string occurs %d times; supply more context or set replace_all=true", count), IsError: true}, nil
	}

	var replaced string
	if args.ReplaceAll {
		replaced = strings.ReplaceAll(string(body), args.OldString, args.NewString)
	} else {
		replaced = strings.Replace(string(body), args.OldString, args.NewString, 1)
	}

	parent := filepath.Dir(resolved)
	tmp, err := os.CreateTemp(parent, ".loomcycle-edit-*")
	if err != nil {
		return tools.Result{Text: "create tempfile: " + err.Error(), IsError: true}, nil
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.WriteString(replaced); err != nil {
		tmp.Close()
		return tools.Result{Text: "write tempfile: " + err.Error(), IsError: true}, nil
	}
	if err := tmp.Close(); err != nil {
		return tools.Result{Text: "close tempfile: " + err.Error(), IsError: true}, nil
	}
	if err := ctx.Err(); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	if err := os.Rename(tmpName, resolved); err != nil {
		return tools.Result{Text: "rename: " + err.Error(), IsError: true}, nil
	}
	return tools.Result{Text: fmt.Sprintf("edited %s (%d replacement%s)", resolved, count, plural(count))}, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
