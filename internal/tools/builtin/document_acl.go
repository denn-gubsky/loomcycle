package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC EZ §1.5 seam 5 — the Document tool's per-call INSTANCE check. The run
// was admitted to the tool by type when it started; which document a call
// touches is only known from the call, so each op asks the caller's set
// (tools.CallerACL: the run's originating principal, or the principal of an
// off-run HTTP / gRPC / MCP call) about `document:<scope>:<path>`, where path
// is the Path-tree name the document is filed under.
//
// A document named at several paths must be allowed at EVERY one of them: a
// deny on one name is not escaped by addressing the document through another.
// A document filed nowhere is governed by the rules on its whole scope.
//
// Ops that reach a scope's documents without naming them first (a fact walk,
// a raw query, a search over facts) ask about every document of the scope at
// once, so a deny on any part of the scope refuses them; the three listing
// ops that return documents by id (query_documents, documents_summary,
// search) instead leave out what the caller may not read (RFC EZ §1.6).

// docACLTarget says how an op names the document(s) it acts on.
type docACLTarget int

const (
	// docACLNew: the op makes a new document; createDocument asks about the
	// path it is about to file it under.
	docACLNew             docACLTarget = iota
	docACLDocument                     // id | document_id | path
	docACLDocumentField                // document_id; absent = the whole scope
	docACLChunk                        // id is a chunk
	docACLChunkOrDocument              // id is a chunk, else document_id, else the whole scope
	docACLCreateChunk                  // document_id, parent_id and after_id
	docACLMoveChunk                    // id and new_parent_id
	docACLLink                         // from_id and to_id
	docACLSetPath                      // the document (id | document_id) and the new path
	docACLImport                       // document_id when given, else a new document
	docACLSupersede                    // supersedes_id and id
	docACLJudge                        // id, else the chunk holding natural_key
	docACLScope                        // every document of the scope
	docACLTenantScope                  // every document of the TENANT scope (ontology proposals file there)
	docACLListing                      // no single target; the answer is filtered
	docACLStatement                    // id is a statement: both endpoint documents
	docACLMaintain                     // id and survivor_id are statements, chunk_id a chunk
)

// docACLFilter names where a listing's answer carries document ids.
type docACLFilter struct {
	key    string   // the result array
	fields []string // the id fields of one entry; every one present must be readable
	// required: an entry with no document id is dropped (a search hit whose
	// chunk is gone cannot be placed, so it cannot be shown to be readable).
	required bool
}

type docACLRule struct {
	verb   string
	target docACLTarget
	filter *docACLFilter
}

var (
	docListDocuments  = &docACLFilter{key: "documents", fields: []string{"document_id"}}
	docListChunks     = &docACLFilter{key: "chunks", fields: []string{"document_id"}, required: true}
	docListEdges      = &docACLFilter{key: "edges", fields: []string{"from_document_id", "to_document_id"}}
	docListStatements = &docACLFilter{key: "statements", fields: []string{"from_document_id", "to_document_id"}}
)

// docACLOps is every op's question. TestDocumentACL_EveryOpIsMapped holds it
// to the input schema's op enum; an op missing here is asked every verb on
// the whole scope (the strongest question), so a forgotten entry fails closed.
var docACLOps = map[string]docACLRule{
	"create_document":    {verb: "write", target: docACLNew},
	"get_document":       {verb: "read", target: docACLDocument},
	"documents_summary":  {verb: "read", target: docACLListing, filter: docListDocuments},
	"query_documents":    {verb: "read", target: docACLListing, filter: docListDocuments},
	"delete_document":    {verb: "delete", target: docACLDocument},
	"set_path":           {verb: "write", target: docACLSetPath},
	"judge_fact":         {verb: "write", target: docACLJudge},
	"propose_entity":     {verb: "write", target: docACLTenantScope},
	"propose_subject":    {verb: "write", target: docACLTenantScope},
	"create_chunk":       {verb: "write", target: docACLCreateChunk},
	"get_chunk":          {verb: "read", target: docACLChunk},
	"update_chunk":       {verb: "write", target: docACLChunk},
	"delete_chunk":       {verb: "delete", target: docACLChunk},
	"move_chunk":         {verb: "write", target: docACLMoveChunk},
	"reorder_chunk":      {verb: "write", target: docACLChunk},
	"upsert_chunk":       {verb: "write", target: docACLScope},
	"supersede_chunk":    {verb: "write", target: docACLSupersede},
	"graph_recall":       {verb: "read", target: docACLScope},
	"remember":           {verb: "write", target: docACLScope},
	"verbatim_answer":    {verb: "read", target: docACLScope},
	"verification_stats": {verb: "read", target: docACLScope},
	"list_facts":         {verb: "read", target: docACLScope},
	"link_chunks":        {verb: "link", target: docACLLink},
	"unlink_chunks":      {verb: "link", target: docACLLink},
	"get_edges":          {verb: "read", target: docACLDocumentField, filter: docListEdges},
	"get_edge":           {verb: "read", target: docACLStatement},
	"propose_statement":  {verb: "propose", target: docACLStatement},
	"accept_statement":   {verb: "link", target: docACLStatement},
	"reject_statement":   {verb: "link", target: docACLStatement},
	"list_statements":    {verb: "read", target: docACLListing, filter: docListStatements},
	// maintain_statement: the statement's ends, a merge survivor's, and the
	// chunk an evidence row is relocated to.
	"maintain_statement": {verb: "propose", target: docACLMaintain},
	// resolve_anchor names its document by work key, which only the op can
	// resolve: it asks read on the document it finds (and write on the path of a
	// placeholder it files) itself.
	"resolve_anchor": {verb: "read", target: docACLListing},
	// describe_ontology reads the tenant's ontology document whatever the call's
	// scope: it asks read on that document itself.
	"describe_ontology": {verb: "read", target: docACLListing},
	// extract_citations reads the document or chunk it names; resolving a citation
	// asks read on the cited work through resolve_anchor.
	"extract_citations": {verb: "read", target: docACLChunkOrDocument},
	// classify reads the document; with write:propose it asks write on it, and
	// propose / link on each statement's ends, itself.
	"classify": {verb: "read", target: docACLDocument},
	// accept_classification changes the document's type and accepts statements:
	// write on the document here; each statement's ruling asks its own ends.
	"accept_classification": {verb: "write", target: docACLDocument},
	// A lease guards a pass over the whole scope and changes no content: it asks
	// propose, the verb a statement-maintaining driver already needs there.
	"lease":             {verb: "propose", target: docACLScope},
	"release":           {verb: "propose", target: docACLScope},
	"query_chunks":      {verb: "read", target: docACLScope},
	"add_tags":          {verb: "write", target: docACLChunkOrDocument},
	"remove_tags":       {verb: "write", target: docACLChunkOrDocument},
	"list_tags":         {verb: "read", target: docACLChunkOrDocument},
	"define_type":       {verb: "write", target: docACLDocumentField},
	"list_types":        {verb: "read", target: docACLDocumentField},
	"set_asset":         {verb: "write", target: docACLChunk},
	"get_asset":         {verb: "read", target: docACLChunk},
	"export_md":         {verb: "read", target: docACLDocument},
	"import_md":         {verb: "write", target: docACLImport},
	"export_canvas":     {verb: "read", target: docACLDocument},
	"import_canvas":     {verb: "write", target: docACLNew},
	"history":           {verb: "read", target: docACLChunk},
	"get_version":       {verb: "read", target: docACLChunk},
	"diff":              {verb: "read", target: docACLChunk},
	"backlinks":         {verb: "read", target: docACLScope},
	"search":            {verb: "read", target: docACLListing, filter: docListChunks},
	"related":           {verb: "read", target: docACLScope},
	"unlinked_mentions": {verb: "read", target: docACLScope},
	"set_remote":        {verb: "write", target: docACLDocument},
	"sync":              {verb: "write", target: docACLDocument},
	"diff_remote":       {verb: "read", target: docACLDocument},
}

// docACL is the caller's set for one Document call.
type docACL struct {
	d   *Document
	eff *authz.Effective
	key sqlmem.ScopeKey
	// pathSensitive: some rule names part of this scope, so a document's
	// answer depends on its path. Without one, the bare scope answers for
	// every document and no name is looked up.
	pathSensitive bool
	paths         map[string][]string // document id → its Path-tree names, loaded once
}

// newDocACL returns the caller's set for this scope, or nil when there is none.
func (d *Document) newDocACL(ctx context.Context, key sqlmem.ScopeKey) *docACL {
	eff := tools.CallerACL(ctx)
	if eff == nil {
		return nil
	}
	return &docACL{d: d, eff: eff, key: key, pathSensitive: eff.HasInstanceRules("document", key.Scope)}
}

func (a *docACL) resource(path string) authz.Resource {
	name := a.key.Scope
	if path != "" && path != "/" {
		name += ":" + path
	}
	r := authz.Resource{Type: "document", Name: name}
	if a.key.Scope == "user" {
		r.Owner = a.key.ScopeID
	}
	return r
}

// docDenied is the refusal: the op, the verb and the name that was refused.
func docDenied(op, verb string, res authz.Resource) *tools.Result {
	r := errPermission(fmt.Sprintf("Document %s: the caller's access-control rules do not allow %s on %s:%s", op, verb, res.Type, res.Name),
		"This document is outside what the caller may "+verb+"; do not retry it. Work with a document the caller may reach.")
	return &r
}

// path asks verb about one Path-tree name.
func (a *docACL) path(ctx context.Context, op, verb, path string) *tools.Result {
	res := a.resource(path)
	if !tools.ACLAllowsInstance(ctx, a.eff, verb, res) {
		return docDenied(op, verb, res)
	}
	return nil
}

// scope asks verb about every document of scope (the caller's own when "").
func (a *docACL) scope(ctx context.Context, op, verb, scope string) *tools.Result {
	res := a.resource("")
	if scope != "" {
		res = authz.Resource{Type: "document", Name: scope}
	}
	if !tools.ACLAllowsScope(ctx, a.eff, verb, res) {
		return docDenied(op, verb, res)
	}
	return nil
}

// namesOf returns the Path-tree names of a document in this scope.
func (a *docACL) namesOf(ctx context.Context, docID string) ([]string, error) {
	if a.paths == nil {
		rows, err := a.d.Store.DirentListUnder(ctx, direntTenant(ctx), a.key.Scope, direntScopeID(a.key), "/")
		if err != nil {
			return nil, err
		}
		a.paths = map[string][]string{}
		for _, r := range rows {
			if r.Kind != "document" {
				continue
			}
			var ref struct {
				DocumentID string `json:"document_id"`
			}
			_ = json.Unmarshal(r.ResourceRef, &ref)
			if ref.DocumentID != "" {
				a.paths[ref.DocumentID] = append(a.paths[ref.DocumentID], r.ParentPath+r.Name)
			}
		}
	}
	return a.paths[docID], nil
}

// document asks verb about a document at every name it is filed under.
func (a *docACL) document(ctx context.Context, op, verb, docID string) (*tools.Result, error) {
	if !a.pathSensitive {
		return a.path(ctx, op, verb, ""), nil
	}
	names, err := a.namesOf(ctx, docID)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return a.path(ctx, op, verb, ""), nil
	}
	for _, p := range names {
		if r := a.path(ctx, op, verb, p); r != nil {
			return r, nil
		}
	}
	return nil, nil
}

// readable is document's read question for a listing: no log line per row.
func (a *docACL) readable(ctx context.Context, docID string) (bool, error) {
	if !a.pathSensitive {
		return tools.ACLFilterAllows(ctx, a.eff, "read", a.resource("")), nil
	}
	names, err := a.namesOf(ctx, docID)
	if err != nil {
		return false, err
	}
	if len(names) == 0 {
		names = []string{""}
	}
	for _, p := range names {
		if !tools.ACLFilterAllows(ctx, a.eff, "read", a.resource(p)) {
			return false, nil
		}
	}
	return true, nil
}

// limitsReading reports whether the set can deny reading some document of this
// scope: a rule naming part of it, or a deny on the whole scope. Nil (no set)
// limits nothing.
func (a *docACL) limitsReading(ctx context.Context) (bool, error) {
	if a == nil {
		return false, nil
	}
	if a.pathSensitive {
		return true, nil
	}
	ok, err := a.readable(ctx, "")
	return !ok, err
}

// chunk asks verb about the document holding a chunk. A chunk that does not
// exist asks nothing: the op reports it missing, as it would to anyone.
func (a *docACL) chunk(ctx context.Context, op, verb, chunkID string) (*tools.Result, error) {
	if chunkID == "" {
		return nil, nil
	}
	row, ok, err := a.d.getChunkRow(ctx, a.key, chunkID)
	if err != nil || !ok {
		return nil, err
	}
	return a.document(ctx, op, verb, row.DocumentID)
}

// guardDocumentACL asks the op's question before it runs. It returns the
// caller's set (nil: none) so the op's answer can be filtered, or a refusal.
func (d *Document) guardDocumentACL(ctx context.Context, key sqlmem.ScopeKey, in docInput) (*docACL, *tools.Result, error) {
	a := d.newDocACL(ctx, key)
	if a == nil {
		return nil, nil, nil
	}
	rule, ok := docACLOps[in.Op]
	if !ok {
		spec, _ := authz.TypeSpecOf("document")
		for _, verb := range spec.Verbs {
			if r := a.scope(ctx, in.Op, verb, ""); r != nil {
				return a, r, nil
			}
		}
		return a, nil, nil
	}
	op, verb := in.Op, rule.verb
	// across_scopes reads the subject in the caller's other scopes too.
	if in.Across {
		for _, sc := range []string{"agent", "user", "tenant"} {
			if sc == key.Scope {
				continue
			}
			if r := a.scope(ctx, op, "read", sc); r != nil {
				return a, r, nil
			}
		}
	}
	var refusal *tools.Result
	var err error
	check := func(f func() (*tools.Result, error)) {
		if refusal == nil && err == nil {
			refusal, err = f()
		}
	}
	doc := func(id string) func() (*tools.Result, error) {
		return func() (*tools.Result, error) {
			if id == "" {
				return nil, nil
			}
			return a.document(ctx, op, verb, id)
		}
	}
	chunk := func(id string) func() (*tools.Result, error) {
		return func() (*tools.Result, error) { return a.chunk(ctx, op, verb, id) }
	}
	wholeScope := func() (*tools.Result, error) { return a.scope(ctx, op, verb, ""), nil }
	// statementEnds asks about both endpoint documents of a statement (RFC FN). An
	// id that is not a statement asks nothing, like a missing chunk.
	statementEnds := func(id string) func() (*tools.Result, error) {
		return func() (*tools.Result, error) {
			if id == "" {
				return nil, nil
			}
			s, err := d.statementByID(ctx, key, id)
			if err != nil || s == nil {
				return nil, err
			}
			if r, err := a.chunk(ctx, op, verb, s.FromID); r != nil || err != nil {
				return r, err
			}
			return a.chunk(ctx, op, verb, s.ToID)
		}
	}

	switch rule.target {
	case docACLNew, docACLListing:
		// createDocument asks about the new name; a listing is filtered.
	case docACLDocument:
		// Every id the call names: an op that prefers document_id over id must
		// not be steered past the check by naming a different one in each.
		check(doc(in.ID))
		check(doc(in.DocumentID))
		if in.ID == "" && in.DocumentID == "" && in.Path != "" {
			if id, perr := d.docIDFromInput(ctx, key, in); perr == nil {
				check(doc(id))
			}
		}
	case docACLDocumentField:
		if in.DocumentID == "" {
			check(wholeScope)
		} else {
			check(doc(in.DocumentID))
		}
	case docACLChunk:
		check(chunk(in.ID))
	case docACLChunkOrDocument:
		switch {
		case in.ID != "":
			check(chunk(in.ID))
		case in.DocumentID != "":
			check(doc(in.DocumentID))
		default:
			check(wholeScope)
		}
	case docACLCreateChunk:
		check(doc(in.DocumentID))
		check(chunk(in.ParentID))
		check(chunk(in.AfterID))
	case docACLMoveChunk:
		check(chunk(in.ID))
		check(chunk(in.NewParentID))
	case docACLLink:
		check(chunk(in.FromID))
		check(chunk(in.ToID))
		// unlink_chunks by statement id names no endpoint itself.
		if in.ID != "" {
			check(statementEnds(in.ID))
		}
	case docACLStatement:
		check(statementEnds(in.ID))
		check(chunk(in.FromID))
		check(chunk(in.ToID))
	case docACLMaintain:
		check(statementEnds(in.ID))
		check(statementEnds(in.SurvivorID))
		check(chunk(in.ChunkID))
	case docACLSetPath:
		check(doc(in.ID))
		check(doc(in.DocumentID))
		if canonical, perr := normalizePath(in.Path); perr == nil {
			check(func() (*tools.Result, error) { return a.path(ctx, op, verb, canonical), nil })
		}
	case docACLImport:
		check(doc(in.DocumentID))
	case docACLSupersede:
		check(chunk(in.SupersedesID))
		check(chunk(in.ID))
	case docACLJudge:
		if in.ID != "" {
			check(chunk(in.ID))
			check(statementEnds(in.ID))
		} else if in.NaturalKey != "" {
			check(func() (*tools.Result, error) {
				id, kerr := d.chunkIDByNaturalKey(ctx, key, in.NaturalKey)
				if kerr != nil {
					return nil, kerr
				}
				return a.chunk(ctx, op, verb, id)
			})
		}
	case docACLScope:
		check(wholeScope)
	case docACLTenantScope:
		check(func() (*tools.Result, error) { return a.scope(ctx, op, verb, "tenant"), nil })
	}
	return a, refusal, err
}

// filterListing drops from a listing's answer every entry whose document the
// caller may not read. The listing is a search: what is not readable is
// absent, never refused (RFC EZ §1.6).
func (a *docACL) filterListing(ctx context.Context, op string, res tools.Result) tools.Result {
	rule := docACLOps[op]
	if a == nil || rule.filter == nil || res.IsError {
		return res
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(res.Text), &body); err != nil {
		return errFrom("Document "+op+": filter by access control: "+err.Error(), err)
	}
	entries, _ := body[rule.filter.key].([]any)
	kept := make([]any, 0, len(entries))
	for _, e := range entries {
		m, _ := e.(map[string]any)
		keep := true
		for _, f := range rule.filter.fields {
			id, _ := m[f].(string)
			if id == "" {
				keep = keep && !rule.filter.required
				continue
			}
			ok, err := a.readable(ctx, id)
			if err != nil {
				return errFrom("Document "+op+": filter by access control: "+err.Error(), err)
			}
			if !ok {
				keep = false
				break
			}
		}
		if keep {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(entries) {
		return res
	}
	body[rule.filter.key] = kept
	out, err := json.Marshal(body)
	if err != nil {
		return errFrom("Document "+op+": filter by access control: "+err.Error(), err)
	}
	res.Text = string(out)
	return res
}

// aclRefuseNewName is createDocument's question: may the caller file a new
// document at canonical.
func (d *Document) aclRefuseNewName(ctx context.Context, key sqlmem.ScopeKey, op, canonical string) *tools.Result {
	a := d.newDocACL(ctx, key)
	if a == nil {
		return nil
	}
	return a.path(ctx, op, "write", canonical)
}
