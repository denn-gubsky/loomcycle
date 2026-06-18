package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Write creates or overwrites a UTF-8 text file inside the sandbox root.
// Writes are atomic-ish: the content lands in a tempfile in the target's
// directory, then a single rename into place. Partial writes are never
// observed by readers (POSIX rename is atomic on the same filesystem).
//
// Sandbox semantics mirror Read: an agent bound to no rw volume is refused.
// Path resolution checks the PARENT directory (the file may not exist yet),
// then writes the new file beside the resolved parent. A symlink at the
// target path is silently replaced by the rename, which is fine — we
// never follow it.
type Write struct {
	// MaxBytes caps the content size. Default 1 MiB.
	MaxBytes int64
}

func (w *Write) Name() string { return "Write" }
func (w *Write) Description() string {
	return "Create or overwrite a text file inside the sandbox root."
}

func (w *Write) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path":    {"type": "string", "description": "Path RELATIVE to the volume root (e.g. \"src/main.go\"). ~ is not expanded; an absolute path is accepted only if it resolves inside the root. Call Context op=self to see your volumes."},
			"content": {"type": "string", "description": "UTF-8 text to write. Replaces any existing content."},
			"volume":  {"type": "string", "description": "Optional read-write volume name to write to. Omit to use your default volume. Read-only volumes are refused. Call Context op=self for the volumes you may access."}
		},
		"required": ["path", "content"]
	}`)
}

func (w *Write) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Volume  string `json:"volume"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.Result{Text: "invalid input: " + err.Error(), IsError: true}, nil
	}
	if args.Path == "" {
		return tools.Result{Text: "path is required", IsError: true}, nil
	}
	root, err := effectiveRoot(ctx, args.Volume, true)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}

	maxBytes := w.MaxBytes
	if maxBytes == 0 {
		maxBytes = 1 << 20
	}
	if int64(len(args.Content)) > maxBytes {
		return tools.Result{Text: fmt.Sprintf("content exceeds %d bytes (got %d)", maxBytes, len(args.Content)), IsError: true}, nil
	}

	target, err := resolveParentInsideRoot(root, args.Path)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	resolvedParent := filepath.Dir(target)

	// Tempfile in the same directory so the rename is intra-filesystem
	// (cross-filesystem rename is not atomic).
	tmp, err := os.CreateTemp(resolvedParent, ".loomcycle-write-*")
	if err != nil {
		return tools.Result{Text: "create tempfile: " + err.Error(), IsError: true}, nil
	}
	tmpName := tmp.Name()
	// On any failure path, remove the temp file. After successful rename
	// the temp name no longer exists, so the Remove is a no-op.
	defer os.Remove(tmpName)

	if _, err := tmp.WriteString(args.Content); err != nil {
		tmp.Close()
		return tools.Result{Text: "write tempfile: " + err.Error(), IsError: true}, nil
	}
	if err := tmp.Close(); err != nil {
		return tools.Result{Text: "close tempfile: " + err.Error(), IsError: true}, nil
	}
	// Honour ctx cancellation BEFORE the visible rename so a cancelled
	// call leaves no trace at the target.
	if err := ctx.Err(); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	if err := os.Rename(tmpName, target); err != nil {
		return tools.Result{Text: "rename: " + err.Error(), IsError: true}, nil
	}
	return tools.Result{Text: fmt.Sprintf("wrote %d bytes to %s", len(args.Content), target)}, nil
}
