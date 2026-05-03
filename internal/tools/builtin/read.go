// Package builtin holds the built-in tools agent runs ship with.
package builtin

import (
	"context"
	"encoding/json"
	"os"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Read returns text from a file path. Bounded by MaxBytes (default 256 KiB)
// so a malicious or confused model can't blow the context window with a huge
// file. Always sandboxed: Root must be set to a directory the tool may read
// from. An empty Root is rejected at call time — there is no "open mode".
type Read struct {
	// Root is the sandbox root. All paths must resolve inside Root after
	// full symlink evaluation; otherwise the call returns an error.
	// Required: a Read with empty Root rejects every call.
	Root     string
	MaxBytes int64
}

func (r *Read) Name() string        { return "Read" }
func (r *Read) Description() string { return "Read a UTF-8 text file from disk." }

func (r *Read) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Absolute file path inside the sandbox root."}
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
	if r.Root == "" {
		return tools.Result{Text: "Read tool is not configured with a sandbox root; refusing to read", IsError: true}, nil
	}

	resolved, err := resolveInsideRoot(r.Root, args.Path)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}

	maxBytes := r.MaxBytes
	if maxBytes == 0 {
		maxBytes = 256 * 1024
	}

	// Open the already-resolved path so a symlink swap between EvalSymlinks
	// and Open can't change what we read.
	f, err := os.Open(resolved)
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
