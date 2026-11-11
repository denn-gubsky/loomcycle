package builtin

import "encoding/json"

// RFC FY Part A: a member reaches another subject's Path tree and documents
// off a run only through a rule that lets it browse that subject, and a
// browse is a read. The off-run routes ask these before such a call, so a
// browse grant never becomes write or delete access.
//
// Both are explicit lists of the ops that only read: every op a tool
// dispatches is on its read-only list or on its write list
// (TestDocumentCallReadsOnly_EveryOpIsClassified,
// TestPathCallReadsOnly_EveryOpIsClassified), so a new op is a write until
// someone decides otherwise.

// documentReadOnlyCall is the subset of a Document call's input a read-only
// condition reads.
type documentReadOnlyCall struct {
	Op                string `json:"op"`
	Write             string `json:"write"`
	CreatePlaceholder bool   `json:"create_placeholder"`
	Propose           bool   `json:"propose"`
}

// documentReadOnlyOps are the Document ops that change nothing, each with the
// condition under which it does not (nil: always).
var documentReadOnlyOps = map[string]func(documentReadOnlyCall) bool{
	"get_document":       nil,
	"documents_summary":  nil,
	"query_documents":    nil,
	"graph_snapshot":     nil,
	"get_chunk":          nil,
	"graph_recall":       nil,
	"verbatim_answer":    nil,
	"verification_stats": nil,
	"list_facts":         nil,
	"get_edges":          nil,
	"get_edge":           nil,
	"list_statements":    nil,
	"describe_ontology":  nil,
	"effective_policy":   nil,
	"extract_citations":  nil,
	"query_chunks":       nil,
	"list_tags":          nil,
	"list_types":         nil,
	"get_asset":          nil,
	"export_md":          nil,
	"export_canvas":      nil,
	"history":            nil,
	"get_annotation":     nil,
	"annotation_inputs":  nil,
	"list_annotations":   nil,
	"get_version":        nil,
	"diff":               nil,
	"backlinks":          nil,
	"search":             nil,
	"related":            nil,
	"unlinked_mentions":  nil,
	"diff_remote":        nil,
	// classify proposes what it finds unless write is "report".
	"classify": func(in documentReadOnlyCall) bool { return in.Write == "report" },
	// resolve_anchor files a placeholder document when asked to.
	"resolve_anchor": func(in documentReadOnlyCall) bool { return !in.CreatePlaceholder },
	// find_duplicates files a proposal per pair when propose is true.
	"find_duplicates": func(in documentReadOnlyCall) bool { return !in.Propose },
}

// documentWriteOps are the Document ops that change something. copy and move
// are asked `read` of the access-control rules but write every target (and
// move removes the source); judge_statements rules on what it judges.
var documentWriteOps = map[string]bool{
	"create_document": true, "delete_document": true, "set_path": true,
	"copy_documents": true, "move_documents": true, "mark_source": true,
	"create_chunk": true, "update_chunk": true, "delete_chunk": true, "move_chunk": true, "reorder_chunk": true,
	"upsert_chunk": true, "supersede_chunk": true, "remember": true, "judge_fact": true,
	"propose_entity": true, "propose_subject": true,
	"link_chunks": true, "unlink_chunks": true,
	"propose_statement": true, "accept_statement": true, "reject_statement": true,
	"judge_statements": true, "maintain_statement": true,
	"lease": true, "release": true, "accept_classification": true,
	"set_annotation": true, "delete_annotation": true,
	"add_tags": true, "remove_tags": true, "define_type": true, "set_asset": true,
	"import_md": true, "import_canvas": true, "set_remote": true, "sync": true,
}

// DocumentCallReadsOnly reports whether a Document call reads and changes
// nothing: its op is on documentReadOnlyOps and the op's condition holds. An
// op on neither list, or input that is not an object, is not a read.
func DocumentCallReadsOnly(input json.RawMessage) bool {
	var in documentReadOnlyCall
	if json.Unmarshal(input, &in) != nil {
		return false
	}
	cond, ok := documentReadOnlyOps[in.Op]
	if !ok {
		return false
	}
	return cond == nil || cond(in)
}

// pathReadOnlyOps / pathWriteOps split the Path tool's ops: resolve, ls and
// stat ask `read` (pathtool.go); mkdir, mv and rm ask `write`.
var (
	pathReadOnlyOps = map[string]bool{"resolve": true, "ls": true, "stat": true}
	pathWriteOps    = map[string]bool{"mkdir": true, "mv": true, "rm": true}
)

// PathCallReadsOnly reports whether a Path call reads and changes nothing.
func PathCallReadsOnly(input json.RawMessage) bool {
	var in struct {
		Op string `json:"op"`
	}
	if json.Unmarshal(input, &in) != nil {
		return false
	}
	return pathReadOnlyOps[in.Op]
}
