package builtin

import (
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// toolmode_ops.go — which operations of the Document and Path tools only read,
// for an agent granted the tool in read mode (tool_modes, RFC GL D6). The
// dispatcher refuses every other op before the tool runs.
//
// Every op of each tool's schema enum is in its table; a test derives the op
// list from the schema and fails on an unclassified op, so a new op is
// classified before it ships. An op missing here anyway is not a read.
//
// The rule: an op that can change the store in any case is a write, unless
// its read case is selected by one argument the dispatcher can check before
// the call (opRead.writes). When in doubt, write.

// opRead is one op's class. A read op with writes set is a read only when
// writes reports false for the call. writes is given the tool's own decoded
// input (docInput), so the dispatcher and the tool cannot read the arguments
// differently (duplicate keys, case-folded names).
type opRead struct {
	read   bool
	writes func(in docInput) bool // nil: always a read
}

var readOp = opRead{read: true}
var writeOp = opRead{}

// documentOpModes classifies every Document op.
//
// Judged with care:
//   - classify: a write unless write is "report" (the default, propose,
//     files proposals and may apply a rule's match).
//   - find_duplicates: a write unless propose is false (true files a
//     duplicate_of proposal per pair).
//   - resolve_anchor: a write when create_placeholder is true (it files a
//     placeholder document), else a lookup.
//   - copy_documents / move_documents: writes, whatever dry_run says: a
//     dry run is the default the model can turn off.
//   - judge_statements: a write even with write:recommend, which still
//     records the verdicts.
//   - lease / release: writes: they change the scope's lease rows.
//   - annotation_inputs: a read; it returns the inputs an annotating driver
//     writes from, and writes nothing (the write is set_annotation).
//   - export_md / export_canvas: reads; they return the text and write no
//     file or row.
//   - diff_remote: a read; it fetches the peer and compares, and only sync
//     writes.
//   - verification_stats / verbatim_answer: reads (aggregate queries).
var documentOpModes = map[string]opRead{
	"create_document":       writeOp,
	"get_document":          readOp,
	"documents_summary":     readOp,
	"query_documents":       readOp,
	"graph_snapshot":        readOp,
	"delete_document":       writeOp,
	"set_path":              writeOp,
	"copy_documents":        writeOp,
	"move_documents":        writeOp,
	"find_duplicates":       {read: true, writes: func(in docInput) bool { return in.Propose }},
	"mark_source":           writeOp,
	"create_chunk":          writeOp,
	"propose_entity":        writeOp,
	"propose_subject":       writeOp,
	"judge_fact":            writeOp,
	"upsert_chunk":          writeOp,
	"get_chunk":             readOp,
	"update_chunk":          writeOp,
	"delete_chunk":          writeOp,
	"supersede_chunk":       writeOp,
	"graph_recall":          readOp,
	"list_facts":            readOp,
	"verbatim_answer":       readOp,
	"verification_stats":    readOp,
	"remember":              writeOp,
	"move_chunk":            writeOp,
	"reorder_chunk":         writeOp,
	"link_chunks":           writeOp,
	"unlink_chunks":         writeOp,
	"get_edges":             readOp,
	"get_edge":              readOp,
	"propose_statement":     writeOp,
	"accept_statement":      writeOp,
	"reject_statement":      writeOp,
	"list_statements":       readOp,
	"judge_statements":      writeOp,
	"maintain_statement":    writeOp,
	"resolve_anchor":        {read: true, writes: func(in docInput) bool { return in.CreatePlaceholder }},
	"lease":                 writeOp,
	"release":               writeOp,
	"set_annotation":        writeOp,
	"get_annotation":        readOp,
	"annotation_inputs":     readOp,
	"list_annotations":      readOp,
	"delete_annotation":     writeOp,
	"describe_ontology":     readOp,
	"effective_policy":      readOp,
	"extract_citations":     readOp,
	"classify":              {read: true, writes: func(in docInput) bool { return in.Write != classifyWriteReport }},
	"accept_classification": writeOp,
	"query_chunks":          readOp,
	"add_tags":              writeOp,
	"remove_tags":           writeOp,
	"list_tags":             readOp,
	"define_type":           writeOp,
	"list_types":            readOp,
	"set_asset":             writeOp,
	"get_asset":             readOp,
	"export_md":             readOp,
	"import_md":             writeOp,
	"export_canvas":         readOp,
	"import_canvas":         writeOp,
	"history":               readOp,
	"get_version":           readOp,
	"diff":                  readOp,
	"backlinks":             readOp,
	"search":                readOp,
	"related":               readOp,
	"unlinked_mentions":     readOp,
	"set_remote":            writeOp,
	"sync":                  writeOp,
	"diff_remote":           readOp,
}

// pathOpModes classifies every Path op: resolve, ls and stat read the tree;
// mkdir, mv and rm change it.
var pathOpModes = map[string]opRead{
	"resolve": readOp,
	"ls":      readOp,
	"stat":    readOp,
	"mkdir":   writeOp,
	"mv":      writeOp,
	"rm":      writeOp,
}

// CallIsRead implements tools.ModalTool for the Document tool. The input is
// decoded as Execute decodes it; an input that does not decode, an unknown op
// or a write is not a read.
func (d *Document) CallIsRead(raw json.RawMessage) (string, bool) {
	var in docInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", false
	}
	m, ok := documentOpModes[in.Op]
	if !ok || !m.read || (m.writes != nil && m.writes(in)) {
		return in.Op, false
	}
	return in.Op, true
}

// CallIsRead implements tools.ModalTool for the Path tool, on Execute's decode.
func (p *Path) CallIsRead(raw json.RawMessage) (string, bool) {
	var in pathInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", false
	}
	m, ok := pathOpModes[in.Op]
	return in.Op, ok && m.read
}

var (
	_ tools.ModalTool = (*Document)(nil)
	_ tools.ModalTool = (*Path)(nil)
)
