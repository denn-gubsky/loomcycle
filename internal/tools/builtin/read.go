// Package builtin holds the built-in tools agent runs ship with.
package builtin

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Read returns text from a file path. Bounded by MaxBytes (default 256 KiB)
// so a malicious or confused model can't blow the context window with a huge
// file. Always sandboxed to a volume: the call resolves a root from the run's
// VolumePolicy (RFC AH) — an agent bound to no volume is refused.
type Read struct {
	MaxBytes int64
}

func (r *Read) Name() string        { return "Read" }
func (r *Read) Description() string { return "Read a UTF-8 text file from disk." }

func (r *Read) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path":   {"type": "string", "description": "Path RELATIVE to the volume root (e.g. \"src/main.go\"). ~ is not expanded; an absolute path is accepted only if it resolves inside the root. Call Context op=self to see your volumes."},
			"volume": {"type": "string", "description": "Optional volume name to read from. Omit to use your default volume. Call Context op=self for the volumes you may access."}
		},
		"required": ["path"]
	}`)
}

func (r *Read) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args struct {
		Path   string `json:"path"`
		Volume string `json:"volume"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.Result{Text: "invalid input: " + err.Error(), IsError: true}, nil
	}
	if args.Path == "" {
		return tools.Result{Text: "path is required", IsError: true}, nil
	}
	root, err := effectiveRoot(ctx, args.Volume, false)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}

	resolved, err := resolveInsideRoot(root, args.Path)
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

	// Read up to maxBytes. io.ReadAll over a LimitReader handles a short read
	// (a single os.File.Read may legally return fewer bytes than requested —
	// e.g. on a FIFO/device, a network FS, or a >1 GiB read) and treats EOF as
	// success — replacing the prior single f.Read + the fragile
	// err.Error() == "EOF" string compare (which breaks on a wrapped EOF). A
	// file larger than maxBytes is bounded to exactly maxBytes.
	data, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	return tools.Result{Text: string(data)}, nil
}
