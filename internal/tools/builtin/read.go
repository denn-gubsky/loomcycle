// Package builtin holds the built-in tools agent runs ship with.
package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Read returns text from a file path. Bounded by MaxBytes (default 256 KiB)
// so a malicious or confused model can't blow the context window with a huge
// file. Always sandboxed to a volume: the call resolves a root from the run's
// VolumePolicy (RFC AH) — an agent bound to no volume is refused.
type Read struct {
	MaxBytes int64
	// MaxScanBytes bounds how far a line-range read looks into a file to
	// count its lines. Zero means 64 MiB.
	MaxScanBytes int64
	// MaxSeekBytes bounds how far a line-range read goes to reach its first
	// line. Zero means 1 GiB.
	MaxSeekBytes int64
}

func (r *Read) Name() string { return "Read" }
func (r *Read) Description() string {
	return "Read one UTF-8 text file from a volume you are bound to. " +
		"Takes `path` relative to the volume root (an absolute path is accepted only if it resolves inside the root; ~ is not expanded) " +
		"and optional `volume` to pick a non-default one — call Context op=self to see which volumes you have. " +
		"Returns the file's text, truncated at 256 KiB with no notice. " +
		"To read part of a file, pass `offset` (first line, counted from 1), `limit` (number of lines) or `line_numbers: true`: " +
		"the result is then whole lines only and ends with a status line such as `[lines 812-818 of 3044]`, which also names the offset to continue from when the result was cut. " +
		"Use this to read source, config or data files you know the path of. " +
		"Do NOT use it to find files — use Glob to match by name or Grep to search by content. " +
		"Do NOT use it for stored documents or memory: those live in the Document and Memory tools, not on a volume. " +
		"An agent bound to no volume is refused, and a path outside the root is refused rather than clamped."
}

func (r *Read) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path":   {"type": "string", "description": "Path RELATIVE to the volume root (e.g. \"src/main.go\"). ~ is not expanded; an absolute path is accepted only if it resolves inside the root. Call Context op=self to see your volumes."},
			"volume": {"type": "string", "description": "Optional volume name to read from. Omit to use your default volume. Call Context op=self for the volumes you may access."},
			"offset": {"type": "integer", "minimum": 1, "description": "First line to return, counted from 1. Turns on line mode."},
			"limit":  {"type": "integer", "minimum": 1, "description": "How many lines to return. Omit to read to the end of the file (or to the size limit). Turns on line mode."},
			"line_numbers": {"type": "boolean", "description": "true puts each line's number before it (right-aligned, then a tab), as cat -n does. Turns on line mode."}
		},
		"required": ["path"]
	}`)
}

func (r *Read) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args struct {
		Path        string `json:"path"`
		Volume      string `json:"volume"`
		Offset      *int   `json:"offset"`
		Limit       *int   `json:"limit"`
		LineNumbers bool   `json:"line_numbers"`
	}
	// Strict: an argument Read does not have is refused, not dropped. A
	// misspelt `offset` used to return the whole file with no hint that it
	// had been ignored, which is how a caller reads the wrong lines.
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return errValidation("Read: unknown argument "+name+" — nothing was read",
				"Read takes path, volume, offset, limit and line_numbers. For a line range pass offset (first line, from 1) and limit (number of lines)."), nil
		}
		return tools.Result{Text: "invalid input: " + err.Error(), IsError: true}, nil
	}
	if args.Path == "" {
		return tools.Result{Text: "path is required", IsError: true}, nil
	}
	lineMode := args.Offset != nil || args.Limit != nil || args.LineNumbers
	if args.Offset != nil && *args.Offset < 1 {
		return errValidation("Read: offset must be 1 or more (lines are counted from 1)", "Pass offset: 1 for the start of the file."), nil
	}
	if args.Limit != nil && *args.Limit < 1 {
		return errValidation("Read: limit must be 1 or more", "Omit limit to read to the end of the file."), nil
	}
	root, fence, err := effectiveRoot(ctx, args.Volume, false)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}

	req := fsops.ReadRequest{Path: args.Path, MaxBytes: r.MaxBytes}
	if lineMode {
		w := fsops.LineWindow{Offset: 1, Numbers: args.LineNumbers, MaxScan: r.MaxScanBytes, MaxSeek: r.MaxSeekBytes}
		if args.Offset != nil {
			w.Offset = *args.Offset
		}
		if args.Limit != nil {
			w.Limit = *args.Limit
		}
		req.Lines = &w
	}
	return readResult(fsops.Read(ctx, root, fence, req)), nil
}

// readResult renders what fsops.Read returned as the tool's result. A line
// range over a binary file is a business refusal with a way out; any other
// failure is the error's text.
func readResult(text string, err error) tools.Result {
	if errors.Is(err, fsops.ErrBinaryLineRange) {
		return errBusiness("Read: this looks like a binary file; a line range reads text only",
			"Call Read without offset, limit and line_numbers to get the file's bytes.")
	}
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}
	}
	return tools.Result{Text: text}
}
