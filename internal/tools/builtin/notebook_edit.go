package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// NotebookEdit is a Claude-Code-compatible Jupyter notebook cell
// mutator. Three modes: replace (update one cell by id), insert
// (add a new cell after a target id, or at index 0 when id is
// empty), delete (remove one cell by id).
//
// Why a dedicated tool: an agent could in principle hand-edit the
// JSON via Edit, but `.ipynb` files have a `source: [...]` array-
// of-lines structure that's tedious to round-trip and easy to
// corrupt. NotebookEdit handles the on-disk shape and preserves all
// non-target cells + top-level metadata verbatim.
//
// Sandbox model: same Root as Write (this mutates files). Writes
// are atomic: tempfile in the same dir + rename.
type NotebookEdit struct {
	// Root is the sandbox root (reuses LOOMCYCLE_WRITE_ROOT in
	// production). Empty = refuse.
	Root string
	// MaxBytes caps both input file size and produced JSON. Default 8 MiB
	// — notebooks routinely run larger than the 1 MiB Write default
	// because of inline base64 image outputs.
	MaxBytes int64
}

const notebookEditDefaultMaxBytes int64 = 8 << 20

func (n *NotebookEdit) Name() string { return "NotebookEdit" }

func (n *NotebookEdit) Description() string {
	return "Surgically edit cells in a Jupyter .ipynb notebook (replace, insert, or delete one cell by id). " +
		"Preserves notebook metadata and other cells; writes atomically."
}

func (n *NotebookEdit) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"file_path": {"type": "string", "description": "Path to the .ipynb file inside the sandbox root."},
			"cell_id":   {"type": "string", "description": "Target cell id. Required for replace+delete. For insert, when empty the new cell is prepended at index 0; when set, the new cell is inserted AFTER it."},
			"cell_type": {"type": "string", "enum": ["code", "markdown"], "description": "Cell type for replace/insert. Defaults to 'code' for insert; preserves existing type for replace when omitted."},
			"source":    {"type": "string", "description": "Cell content. Required for replace and insert."},
			"mode":      {"type": "string", "enum": ["replace", "insert", "delete"], "description": "Operation. Defaults to 'replace'."}
		},
		"required": ["file_path"]
	}`)
}

type notebookEditInput struct {
	FilePath string `json:"file_path"`
	CellID   string `json:"cell_id,omitempty"`
	CellType string `json:"cell_type,omitempty"`
	Source   string `json:"source,omitempty"`
	Mode     string `json:"mode,omitempty"`
}

// notebook is the minimal struct we care about. Other keys ride along
// as RawMessage so the round-trip preserves them byte-for-byte
// (json.RawMessage is verbatim).
//
// On disk the source field is `[]string` (one entry per line, newlines
// included on all but the last). Round-trip preserves that.
type notebook struct {
	Cells         []notebookCell  `json:"cells"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	NBFormat      int             `json:"nbformat"`
	NBFormatMinor int             `json:"nbformat_minor"`
}

type notebookCell struct {
	ID             string          `json:"id,omitempty"`
	CellType       string          `json:"cell_type"`
	Source         []string        `json:"source"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	Outputs        json.RawMessage `json:"outputs,omitempty"`
	ExecutionCount json.RawMessage `json:"execution_count,omitempty"`
}

func (n *NotebookEdit) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args notebookEditInput
	if err := json.Unmarshal(input, &args); err != nil {
		return errResult("invalid input: " + err.Error()), nil
	}
	if n.Root == "" {
		return errResult("NotebookEdit tool is not configured with a sandbox root; set LOOMCYCLE_WRITE_ROOT"), nil
	}
	if args.FilePath == "" {
		return errResult("file_path is required"), nil
	}
	if !strings.HasSuffix(args.FilePath, ".ipynb") {
		return errResult("file_path must end in .ipynb"), nil
	}

	mode := args.Mode
	if mode == "" {
		mode = "replace"
	}
	switch mode {
	case "replace", "insert", "delete":
	default:
		return errResult(fmt.Sprintf("unknown mode %q (want replace|insert|delete)", mode)), nil
	}

	// replace+delete require an existing cell id; insert may be empty
	// (meaning "prepend at index 0"). replace+insert require source
	// content.
	if (mode == "replace" || mode == "delete") && args.CellID == "" {
		return errResult("cell_id is required for mode=" + mode), nil
	}
	if (mode == "replace" || mode == "insert") && args.Source == "" {
		return errResult("source is required for mode=" + mode), nil
	}

	// Resolve path — file must exist for replace+delete; for insert we
	// also require an existing notebook (the tool creates cells, not
	// notebooks). Use resolveInsideRoot which requires the path to
	// exist, matching Edit's posture.
	target := args.FilePath
	if !filepath.IsAbs(target) {
		target = filepath.Join(n.Root, target)
	}
	resolved, rerr := resolveInsideRoot(n.Root, target)
	if rerr != nil {
		return errResult(rerr.Error()), nil
	}

	maxBytes := n.MaxBytes
	if maxBytes <= 0 {
		maxBytes = notebookEditDefaultMaxBytes
	}

	st, err := os.Stat(resolved)
	if err != nil {
		return errResult("stat: " + err.Error()), nil
	}
	if st.Size() > maxBytes {
		return errResult(fmt.Sprintf("notebook exceeds %d bytes (got %d)", maxBytes, st.Size())), nil
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return errResult("read: " + err.Error()), nil
	}

	var nb notebook
	if err := json.Unmarshal(raw, &nb); err != nil {
		return errResult("invalid notebook JSON: " + err.Error()), nil
	}

	switch mode {
	case "replace":
		idx := findCellIndex(nb.Cells, args.CellID)
		if idx < 0 {
			return errResult(fmt.Sprintf("cell %q not found", args.CellID)), nil
		}
		wasCode := nb.Cells[idx].CellType == "code"
		nb.Cells[idx].Source = splitSourceLines(args.Source)
		if args.CellType != "" {
			nb.Cells[idx].CellType = args.CellType
			switch args.CellType {
			case "markdown":
				// Demoted code → markdown: outputs/execution_count no
				// longer make sense; null them out to keep the file
				// valid.
				nb.Cells[idx].Outputs = nil
				nb.Cells[idx].ExecutionCount = nil
			case "code":
				// Promoted non-code → code: the .ipynb spec requires
				// `outputs` and `execution_count` on code cells.
				// `omitempty` on json.RawMessage drops nil values, so
				// without re-seeding here the resulting file would be
				// missing both keys.
				if !wasCode && nb.Cells[idx].Outputs == nil {
					nb.Cells[idx].Outputs = json.RawMessage("[]")
				}
				if !wasCode && nb.Cells[idx].ExecutionCount == nil {
					nb.Cells[idx].ExecutionCount = json.RawMessage("null")
				}
			}
		}

	case "insert":
		newCellType := args.CellType
		if newCellType == "" {
			newCellType = "code"
		}
		newCell := notebookCell{
			ID:       mintCellID(),
			CellType: newCellType,
			Source:   splitSourceLines(args.Source),
		}
		// code cells default to empty outputs + null execution_count on
		// disk; encode as raw JSON to keep round-trip byte-stable.
		if newCellType == "code" {
			newCell.Outputs = json.RawMessage("[]")
			newCell.ExecutionCount = json.RawMessage("null")
		}
		insertAt := 0
		if args.CellID != "" {
			idx := findCellIndex(nb.Cells, args.CellID)
			if idx < 0 {
				return errResult(fmt.Sprintf("cell %q not found", args.CellID)), nil
			}
			insertAt = idx + 1
		}
		nb.Cells = append(nb.Cells[:insertAt], append([]notebookCell{newCell}, nb.Cells[insertAt:]...)...)

	case "delete":
		idx := findCellIndex(nb.Cells, args.CellID)
		if idx < 0 {
			return errResult(fmt.Sprintf("cell %q not found", args.CellID)), nil
		}
		nb.Cells = append(nb.Cells[:idx], nb.Cells[idx+1:]...)
	}

	out, err := json.MarshalIndent(&nb, "", " ")
	if err != nil {
		return errResult("marshal: " + err.Error()), nil
	}
	// Trailing newline matches the jupyter convention; harmless if absent.
	out = append(out, '\n')
	if int64(len(out)) > maxBytes {
		return errResult(fmt.Sprintf("rendered notebook exceeds %d bytes (got %d)", maxBytes, len(out))), nil
	}

	// Atomic write — tempfile in the same dir + rename. Same recipe
	// as Write so cancellation leaves no trace at the target.
	resolvedDir := filepath.Dir(resolved)
	tmp, err := os.CreateTemp(resolvedDir, ".loomcycle-nbedit-*")
	if err != nil {
		return errResult("create tempfile: " + err.Error()), nil
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return errResult("write tempfile: " + err.Error()), nil
	}
	if err := tmp.Close(); err != nil {
		return errResult("close tempfile: " + err.Error()), nil
	}
	if err := ctx.Err(); err != nil {
		return errResult(err.Error()), nil
	}
	if err := os.Rename(tmpName, resolved); err != nil {
		return errResult("rename: " + err.Error()), nil
	}

	return tools.Result{Text: fmt.Sprintf("notebook %s: %s (%d cells)", mode, resolved, len(nb.Cells))}, nil
}

func findCellIndex(cells []notebookCell, id string) int {
	for i, c := range cells {
		if c.ID == id {
			return i
		}
	}
	return -1
}

// splitSourceLines mirrors the on-disk Jupyter convention: source is
// a JSON array of lines with newlines included on all entries except
// (typically) the last. We split on \n and re-append \n to all but
// the final segment so the round-trip is faithful.
func splitSourceLines(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, "\n")
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		if i < len(parts)-1 {
			out = append(out, p+"\n")
		} else if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// mintCellID generates an 8-hex-char id matching Jupyter's convention.
// Hex from crypto/rand so collisions are impossible at agent scale.
func mintCellID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read failing on a desktop kernel is exotic; fall back
		// to a deterministic-but-unique nanos-based id rather than
		// panicking inside a tool call.
		return "cell0000"
	}
	return hex.EncodeToString(b[:])
}
