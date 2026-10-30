package connector

// FileTwinOps are the File-tool ops Connector.File runs on a run's inbox.
// download and upload are not among them: they go out on a run's HTTP host
// policy, which a caller outside the run has none of.
var FileTwinOps = []string{"stat", "ls", "extract", "import", "keep", "rm", "clear"}

// FileTwinFields are the keys a Connector.File call may carry, exactly as
// spelled: run_id, and the File tool's own fields less download's and
// upload's. The implementation refuses any other key, and the MCP tool's
// schema is the File tool's cut to these.
var FileTwinFields = []string{
	"run_id", "op", "path", "from", "to", "overwrite", "recursive", "prefix",
	"images", "max_rows", "excerpt_chars", "title", "doc_path", "scope", "tags", "source", "headings",
	"reimport", "work_key", "edition_date", "amending_document_id",
}
