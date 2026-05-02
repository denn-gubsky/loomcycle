// Package builtin holds the built-in tools agent runs ship with.
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Read returns text from a file path. Bounded by maxBytes (default 256 KiB)
// so a malicious or confused model can't blow the context window with a huge
// file.
type Read struct {
	// Root is the optional sandbox root. When set, all paths must resolve
	// inside Root after symlink evaluation; otherwise the call returns an error.
	Root     string
	MaxBytes int64
}

func (r *Read) Name() string        { return "Read" }
func (r *Read) Description() string { return "Read a UTF-8 text file from disk." }

func (r *Read) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Absolute file path."}
		},
		"required": ["path"]
	}`)
}

func (r *Read) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.Result{Text: "invalid input: " + err.Error(), IsError: true}, nil
	}
	if args.Path == "" {
		return tools.Result{Text: "path is required", IsError: true}, nil
	}

	clean := filepath.Clean(args.Path)
	if r.Root != "" {
		// Resolve symlinks under root to prevent escape.
		root, err := filepath.EvalSymlinks(r.Root)
		if err != nil {
			return tools.Result{Text: "sandbox root: " + err.Error(), IsError: true}, nil
		}
		abs, err := filepath.Abs(clean)
		if err != nil {
			return tools.Result{Text: "abs path: " + err.Error(), IsError: true}, nil
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || (len(rel) >= 3 && rel[:3] == "../") {
			return tools.Result{Text: fmt.Sprintf("path %q escapes sandbox %q", abs, root), IsError: true}, nil
		}
	}

	maxBytes := r.MaxBytes
	if maxBytes == 0 {
		maxBytes = 256 * 1024
	}

	f, err := os.Open(clean)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	defer f.Close()

	buf := make([]byte, maxBytes)
	n, err := f.Read(buf)
	if err != nil && err.Error() != "EOF" {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	return tools.Result{Text: string(buf[:n])}, nil
}
