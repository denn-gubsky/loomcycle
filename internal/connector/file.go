package connector

// FileTwinOps are the File-tool ops Connector.File runs on a run's inbox,
// and render, which needs no run: it writes a Document of the caller's as a
// file into a volume of the caller's tenant (RFC GR D8). download and upload
// are not among them: they go out on a run's HTTP host policy, which a
// caller outside the run has none of.
var FileTwinOps = []string{"stat", "ls", "extract", "import", "keep", "rm", "clear", "render"}

// FileTwinFields are the keys a Connector.File call may carry, exactly as
// spelled: run_id, and the File tool's own fields less download's and
// upload's. The implementation refuses any other key, and the MCP tool's
// schema is the File tool's cut to these.
var FileTwinFields = []string{
	"run_id", "op", "path", "from", "to", "overwrite", "recursive", "prefix",
	"images", "max_rows", "excerpt_chars", "structure", "title", "doc_path", "scope", "tags", "source", "headings",
	"reimport", "work_key", "edition_date", "amending_document_id",
	"document", "chunk", "format", "toc", "title_page", "provenance", "page_size", "language", "metadata",
}
