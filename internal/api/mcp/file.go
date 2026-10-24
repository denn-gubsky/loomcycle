package mcp

import (
	"encoding/json"
	"slices"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The `file` meta-tool is the File tool outside a run (RFC FF D16): it acts on
// the inbox of a run the session may read, named by run_id. connector.File
// owns every rule (which run, which ops, whose scopes and volumes); this file
// only advertises the shape.

// fileMCPOverrides are the properties whose builtin wording speaks of the
// agent's own volumes and grants, which a session does not have. Every other
// property is the builtin's, byte for byte.
var fileMCPOverrides = map[string]string{
	"run_id": `{"type": "string", "description": "The run whose inbox to use, as get_run and list_runs report it. A sub-run's id reaches its tree's inbox."}`,
	"from":   `{"type": "string", "description": "keep: the inbox file to copy, e.g. volume:inbox:documents/a.pdf."}`,
	"to":     `{"type": "string", "description": "keep: where to copy it, volume:<name>:<path>, in a read-write dynamic volume of your tenant (volumedef list shows them). A path ending in / (or an existing folder) keeps the file's name. Its folder must exist."}`,
}

// fileMCPSchema is the File builtin's input schema cut to the fields
// connector.File takes (connector.FileTwinFields), its op enum cut to
// connector.FileTwinOps, run_id added and required. Derived rather than
// restated so the shared fields keep tracking the tool's validation; a shape it
// cannot edit falls back to a minimal schema, which
// TestFileMCP_SchemaIsTheToolsCutToTheTwin catches.
func fileMCPSchema() json.RawMessage {
	fallback := json.RawMessage(`{"type": "object", "properties": {"run_id": {"type": "string"}, "op": {"type": "string"}}, "required": ["run_id", "op"]}`)
	var s map[string]json.RawMessage
	if err := json.Unmarshal((&builtin.File{}).InputSchema(), &s); err != nil {
		return fallback
	}
	var props map[string]json.RawMessage
	if err := json.Unmarshal(s["properties"], &props); err != nil || props == nil {
		return fallback
	}
	out := map[string]json.RawMessage{}
	for _, name := range connector.FileTwinFields {
		if o, ok := fileMCPOverrides[name]; ok {
			out[name] = json.RawMessage(o)
			continue
		}
		p, ok := props[name]
		if !ok {
			return fallback
		}
		out[name] = p
	}
	op, err := marshalNoHTMLEscape(map[string]any{"type": "string", "enum": slices.Clone(connector.FileTwinOps)})
	if err != nil {
		return fallback
	}
	out["op"] = op
	encoded, err := marshalNoHTMLEscape(out)
	if err != nil {
		return fallback
	}
	s["properties"] = encoded
	s["required"] = json.RawMessage(`["run_id", "op"]`)
	res, err := marshalNoHTMLEscape(s)
	if err != nil {
		return fallback
	}
	return res
}

// fileMCPDescription is what an MCP client reads. Keep the op list in step with
// connector.FileTwinOps: TestFileMCP_DescriptionListsEveryOp checks it.
const fileMCPDescription = "Inspect and manage the files a RUN's tools returned, kept in that run's inbox volume, from outside the run, without their bytes passing through you. " +
	"Name the run with run_id (get_run and list_runs report it); the inbox belongs to the whole run tree, so a sub-run's id reaches its tree's inbox. " +
	"Ops: stat (size, type, sha256 and origin of one file), ls (list the inbox or a folder of it), " +
	"extract (turn a DOCX, XLSX, PPTX, HTML, CSV, text or image file into Markdown in the inbox, its pictures kept as files, and return an excerpt), " +
	"import (build a Document in YOUR own scopes from such a Markdown file, its pictures as image chunks), " +
	"keep (copy a file into a read-write dynamic volume of your tenant: volume:<name>:<path>), rm (delete one file), clear (empty the inbox, or one folder of it). " +
	"Paths are volume:inbox:<path>, or a bare path in the inbox; no other volume is reachable here except keep's target. " +
	"Works on a run you can read with get_run; another tenant's run is not found. extract, keep, rm and clear write, so only the run's owner or an admin may make them. " +
	"The inbox exists only while the run tree does: it is deleted when the run ends, so keep what must outlive it. Every call is strict: a field the tool does not take is refused. " +
	"Do NOT use it to download or upload a URL: that needs a run's HTTP allowlist, so give the agent the File and HTTP tools instead. " +
	"Do NOT use it to read a document's text whole: extract returns an excerpt, and import makes a document you read with the document tool."
