package builtin

// document_node_stats.go — node statistics for the taxonomy's nodes (RFC GM §3, plan
// P2.1): Document op=node_stats and what describe_ontology with_stats reads.
//
// A NODE is a concept of a scheme (kind concept, id its chunk) or a document class
// (kind class, id its name: the seed classes have no chunk). Its MEMBERS are whole
// documents (RFC GM D3, set by P0): a concept's are the documents the counts of
// with_counts count (conceptMembership — the search concept filter's rule, rolled
// up, each document once); a class's are the documents of that type and, in a
// confirmed ontology, of its subclasses, as with_counts counts them.
//
// A member's VECTOR is its document-level annotation's index row
// (doc.annotation:<root>:document), read in-process through MemoryEmbedListVectors.
// Vectors and centroids are returned by no op. The vector stage runs only where the
// store keeps vectors (Postgres with pgvector) and an embedder names the model, as the
// 3D projection does; elsewhere a row keeps the member counts alone and the report
// says why.
//
// STALENESS. A row is recomputed when its inputs hash moves: the node's own
// definition (kind, name, meaning line, tag or parent chain), its sorted member ids,
// each member's annotation inputs_sha256, whether each member has a vector, and the
// embed model. The ontology version is not in it: it moves on every ontology edit
// and would mark every node stale. A
// sibling's change does not mark a node stale either (RFC GM §3.2), so its siblings
// and terms can be one pass behind; the sweeper's next pass after the sibling's own
// recompute does not fix that, a definition or membership change of the node does.
//
// A pass recomputes each stale node from its own member set. A broader node's set is
// already the union of its narrower ones' (conceptMembership), so no running sum is
// kept: at hundreds to low thousands of documents a recompute is milliseconds.
//
// WHO IS COUNTED (RFC GM C8, replacing D8 (i)). A node's members, here and in
// everything built on the plan (the series, the node annotations' requests), are
// only the documents every member of the tenant may read (unrestrictedDocList). A
// restricted document is left out entirely: never counted, never an exemplar, never
// in the terms. A member whose document annotation is metadata-only (RFC GP 10f,
// chunk_annotations.tier = 'metadata', read only where the column exists) is
// counted in members but left out of the centroid, the exemplars and the terms.
//
// READING (describe_ontology with_stats). Every reader of the ontology gets the same
// statistics: nothing is withheld per reader. Only rows of unrestrictedFamily are
// served; one stored before C8 reads as absent until the next pass. A row whose
// stored member set (members_sha256) is not the node's current one — a member
// restricted, filed elsewhere or turned metadata-only since the pass — serves only
// today's member count until the next pass (statsView).
//
// SQLite has one connection per scope: every read is made before the write
// transaction opens.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
	"github.com/denn-gubsky/loomcycle/internal/projection"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	nodeStatsOp = "node_stats"

	nodeKindConcept = "concept"
	nodeKindClass   = "class"

	nodeStatsModeReport  = "report"
	nodeStatsModeCompute = "compute"

	// nodeStatsHashVersion is the version of what the inputs hash covers: a change
	// to the hashed form bumps it, which marks every row stale on purpose.
	// node_stats/3: members are the unrestricted documents (RFC GM C8) and a
	// metadata-only member is marked (C9).
	nodeStatsHashVersion = "node_stats/3"
	// nodeStatsVectorPage is the vectors read per store call (the store's cap).
	nodeStatsVectorPage = 1000
	// nodeStatsDocBatch is the document ids per IN list.
	nodeStatsDocBatch = 200
)

// nodeStatsDDL is built per tier, like chunk_projection_basis: the centroid is
// binary (float32 little-endian, the mean of the members' unit vectors), BLOB on
// sqlite and BYTEA on postgres. exemplars, siblings and terms are JSON arrays.
// members_sha256 is memberSetHash: the sorted member ids, each marked when it is
// metadata-only. A read serves the row's figures only while it still equals the
// node's current members' (statsView), so a document restricted after a pass, or
// turned metadata-only, stops shaping what every reader sees at once.
func nodeStatsDDL(binType string) string {
	return `CREATE TABLE IF NOT EXISTS node_stats (
		node_id TEXT NOT NULL, kind TEXT NOT NULL,
		members INTEGER NOT NULL, members_with_vector INTEGER NOT NULL,
		spread DOUBLE PRECISION, centroid ` + binType + `, embed_model TEXT,
		exemplars TEXT, siblings TEXT, terms TEXT,
		inputs_sha256 TEXT NOT NULL, members_sha256 TEXT NOT NULL,
		computed_at BIGINT NOT NULL,
		PRIMARY KEY (node_id, kind))`
}

// statNode is one node as a pass sees it.
type statNode struct {
	ID, Kind, Name, Tag string
	// group is the sibling group: the nodes under the same parent (a concept's
	// broader concept or its scheme's top level; a class's parent class).
	group string
	// def is the node's own definition, as hashed.
	def     string
	members []string // sorted document ids
}

// statDoc is what a pass knows of one member document.
type statDoc struct {
	root, title           string
	brief, abstract, hash string // the document-level annotation; hash its inputs_sha256
	annotated             bool
	// metadataOnly: the annotation was written from the record without its content
	// (tier metadata, C9): counted, never averaged, an exemplar or a source of terms.
	metadataOnly bool
}

// storedNodeStats is one node_stats row, without its centroid.
type storedNodeStats struct {
	members, withVector int
	spread              *float64
	embedModel          string
	exemplars           []nodeStatsScored
	siblings            []nodeStatsScored
	terms               []string
	inputs, memberSHA   string
	computedAt          int64
}

// current reports whether the row was computed under RFC GM C8, from unrestricted
// documents only: only such a row is ever served.
func (r storedNodeStats) current() bool { return strings.HasPrefix(r.inputs, unrestrictedFamily) }

// nodeStatsScored is a stored exemplar (a document) or sibling (a node) with its
// cosine similarity to the node's centroid.
type nodeStatsScored struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

// nodeStatsPlan is the current state of every node, read before anything is written.
type nodeStatsPlan struct {
	nodes  []statNode
	docs   map[string]*statDoc
	stored map[string]storedNodeStats // kind + "\x00" + id
	// model is the embed model a vector must be of, "" when the vector stage cannot
	// run; note says why it cannot.
	model, note string
	// vecs is each member document's vector of model, read once per plan; a
	// metadata-only member has none.
	vecs map[string]nodestats.Vector
	// unrestricted narrows the scope to what every member may read (nil: all of it);
	// tierCol says chunk_annotations has the tier column (C9), probed once per plan.
	unrestricted *docReadList
	tierCol      bool
}

func nodeStatsKey(kind, id string) string { return kind + "\x00" + id }

// nodeHash is the inputs hash of n under plan p (RFC GM §3.2). A member's line
// carries its annotation's inputs_sha256 AND a hash of the annotation's own text:
// inputs_sha256 is the hash of what the annotation was written FROM, so a rewrite
// of the same inputs (a manual edit, a regeneration under the same prompt) keeps
// it while the brief, the abstract and their vector — what terms and centroid are
// computed from — change.
func (p *nodeStatsPlan) nodeHash(n statNode) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s\nmodel %s\n", nodeStatsHashVersion, n.Kind, n.def, p.model)
	for _, id := range n.members {
		fmt.Fprintf(h, "%s\n", p.memberLine(id))
	}
	// Whether a member HAS a vector is an input too: the annotation row is written
	// before its vector, and an embed can fail, so a row computed while a vector was
	// missing must go stale when it arrives.
	for _, id := range n.members {
		if _, ok := p.vecs[id]; ok {
			fmt.Fprintf(h, "vector %s\n", id)
		}
	}
	return unrestrictedFamily + hex.EncodeToString(h.Sum(nil))
}

// memberLine is one member as an inputs hash reads it: its id, its annotation's
// inputs_sha256, a hash of the annotation's own text, and whether it is
// metadata-only (which takes it out of the centroid and the terms).
func (p *nodeStatsPlan) memberLine(id string) string {
	ann, text, meta := "", "", ""
	if d := p.docs[id]; d != nil && d.annotated {
		sum := sha256.Sum256([]byte(d.brief + "\x00" + d.abstract))
		ann, text = d.hash, hex.EncodeToString(sum[:])
		if d.metadataOnly {
			meta = " metadata"
		}
	}
	return id + " " + ann + " " + text + meta
}

// averaged reports whether member id's text and vector go into a node's figures:
// a metadata-only member is counted and nothing more (C9).
func (p *nodeStatsPlan) averaged(id string) bool {
	d := p.docs[id]
	return d == nil || !d.metadataOnly
}

// memberSetHash is a row's or record's stored member set: membersHash of the ids,
// a metadata-only member's id followed by " metadata" (C9). With no metadata-only
// member it equals membersHash(ids).
func (p *nodeStatsPlan) memberSetHash(ids []string) string {
	lines := make([]string, len(ids))
	for i, id := range ids {
		lines[i] = id
		if !p.averaged(id) {
			lines[i] += " metadata"
		}
	}
	return membersHash(lines)
}

func membersHash(ids []string) string {
	h := sha256.New()
	for _, id := range ids {
		fmt.Fprintf(h, "%s\n", id)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// nodeStatsVectorsNote is why a pass keeps counts only, "" when it need not.
func (d *Document) nodeStatsVectorsNote() string {
	switch {
	case d.Store == nil || !d.Store.SupportsVectors():
		return "the store keeps no vectors (that needs Postgres with pgvector): only member counts are kept"
	case d.Embedder == nil:
		return "no embedder is configured, so no vector has a model to match: only member counts are kept"
	}
	return ""
}

// buildNodeStatsPlan reads the nodes of read (only scheme's concepts when scheme is
// set), their full membership, their member documents and the stored rows.
func (d *Document) buildNodeStatsPlan(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, scheme string) (*nodeStatsPlan, error) {
	if err := d.ensureSchema(ctx, key); err != nil {
		return nil, err
	}
	p := &nodeStatsPlan{docs: map[string]*statDoc{}, stored: map[string]storedNodeStats{}}
	var err error
	if p.note = d.nodeStatsVectorsNote(); p.note == "" {
		p.model = d.projectionModel()
	}
	var schemes []memrank.OntologyScheme
	if read.Confirmed {
		schemes = read.Schemes
	}
	if p.unrestricted, err = d.unrestrictedDocList(ctx, key); err != nil {
		return nil, err
	}
	p.tierCol = d.tableHasColumn(ctx, key, "chunk_annotations", "tier")
	membership, err := d.conceptMembership(ctx, key, schemes, p.unrestricted)
	if err != nil {
		return nil, err
	}
	for _, s := range schemes {
		if scheme != "" && s.Name != scheme {
			continue
		}
		for _, c := range s.Concepts {
			if c.ChunkID == "" || c.Tag == "" {
				continue
			}
			parent := s.Name
			if i := strings.LastIndex(c.Tag, "/"); i >= 0 {
				parent = c.Tag[:i]
			}
			n := statNode{ID: c.ChunkID, Kind: nodeKindConcept, Name: c.Name, Tag: c.Tag, group: "concept\x00" + parent,
				def: fmt.Sprintf("tag %s\nname %s\nmeaning %s\nrank %d", c.Tag, c.Name, c.Meaning, c.Rank)}
			n.members = sortedSet(membership.members[c.ChunkID])
			p.nodes = append(p.nodes, n)
		}
	}
	if scheme == "" {
		classes, err := d.classStatNodes(ctx, key, read, p.unrestricted)
		if err != nil {
			return nil, err
		}
		p.nodes = append(p.nodes, classes...)
	}
	all := map[string]bool{}
	for _, n := range p.nodes {
		for _, id := range n.members {
			all[id] = true
		}
	}
	if err := d.loadStatDocs(ctx, key, sortedSet(all), p.docs, p.tierCol); err != nil {
		return nil, err
	}
	// The vectors are read here, not only when a pass computes: which members have
	// one is part of every node's inputs hash. The memory tenant is the run
	// identity's, as NodeStatsPass's tenant is (NodeStatsScopeKey stamps it).
	if p.model != "" {
		if _, err := d.loadNodeStatsVectors(ctx, key, direntTenant(ctx), p); errors.Is(err, store.ErrVectorUnsupported) {
			p.model, p.note = "", "the store keeps no vectors (that needs Postgres with pgvector): only member counts are kept"
		} else if err != nil {
			return nil, err
		}
	}
	if err := d.loadStoredNodeStats(ctx, key, p.stored); err != nil {
		return nil, err
	}
	return p, nil
}

// classStatNodes is every document class in force, its members the documents of its
// type and — in a confirmed ontology, where search type: widens — its subclasses'.
func (d *Document) classStatNodes(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, unrestricted *docReadList) ([]statNode, error) {
	described := describeOntologyRead(read)
	parentOf := map[string]string{}
	kids := map[string][]string{}
	var classes []describedClass
	for _, c := range described.Classes {
		parentOf[c.Name] = c.Parent
		if c.Kind != "document" {
			continue
		}
		classes = append(classes, c)
		if read.Confirmed && c.Parent != "" {
			kids[c.Parent] = append(kids[c.Parent], c.Name)
		}
	}
	if len(classes) == 0 {
		return nil, nil
	}
	byType, err := d.documentsByType(ctx, key)
	if err != nil {
		return nil, err
	}
	out := make([]statNode, 0, len(classes))
	for _, c := range classes {
		var chain []string
		for p, depth := c.Parent, 0; p != "" && depth <= memrank.OntologyMaxDepth; p, depth = parentOf[p], depth+1 {
			chain = append(chain, p)
		}
		members := map[string]bool{}
		seen := map[string]bool{c.Name: true}
		frontier := []string{c.Name}
		for depth := 0; depth <= memrank.OntologyMaxDepth && len(frontier) > 0; depth++ {
			var next []string
			for _, name := range frontier {
				for _, id := range byType[name] {
					if unrestricted.readable(id) {
						members[id] = true
					}
				}
				for _, k := range kids[name] {
					if !seen[k] {
						seen[k] = true
						next = append(next, k)
					}
				}
			}
			frontier = next
		}
		out = append(out, statNode{ID: c.Name, Kind: nodeKindClass, Name: c.Name, group: "class\x00" + c.Parent,
			def:     fmt.Sprintf("class %s\nmeaning %s\nparents %s", c.Name, c.Meaning, strings.Join(chain, " < ")),
			members: sortedSet(members)})
	}
	return out, nil
}

// documentsByType is every typed document of the scope by its type, keyset-paged so
// the row cap never cuts it.
func (d *Document) documentsByType(ctx context.Context, key sqlmem.ScopeKey) (map[string][]string, error) {
	out := map[string][]string{}
	after := ""
	for {
		res, err := d.query(ctx, key, `SELECT id, type FROM documents WHERE type IS NOT NULL AND type <> '' AND id > ?
			ORDER BY id LIMIT ?`, after, searchFilterPage)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Rows {
			out[asStr(r[1])] = append(out[asStr(r[1])], asStr(r[0]))
		}
		if len(res.Rows) == 0 || (!res.Truncated && len(res.Rows) < searchFilterPage) {
			return out, nil
		}
		after = asStr(res.Rows[len(res.Rows)-1][0])
	}
}

// loadStatDocs fills docs with each document's root, title and document-level
// annotation. A document that is gone has no entry. tierCol: chunk_annotations has
// RFC GP's tier column, whose 'metadata' marks a metadata-only member (C9).
func (d *Document) loadStatDocs(ctx context.Context, key sqlmem.ScopeKey, ids []string, docs map[string]*statDoc, tierCol bool) error {
	byRoot := map[string]*statDoc{}
	for start := 0; start < len(ids); start += nodeStatsDocBatch {
		ph, args := inPlaceholders(ids[start:min(start+nodeStatsDocBatch, len(ids))])
		res, err := d.query(ctx, key, `SELECT id, root_chunk_id, title FROM documents WHERE id IN (`+ph+`)`, args...)
		if err != nil {
			return err
		}
		if res.Truncated {
			return fmt.Errorf("node_stats: a document batch was cut short")
		}
		for _, r := range res.Rows {
			sd := &statDoc{root: asStr(r[1]), title: asStr(r[2])}
			docs[asStr(r[0])] = sd
			byRoot[sd.root] = sd
		}
	}
	roots := make([]string, 0, len(byRoot))
	for r := range byRoot {
		roots = append(roots, r)
	}
	sort.Strings(roots)
	tier := `''`
	if tierCol {
		tier = `coalesce(tier, '')`
	}
	for start := 0; start < len(roots); start += nodeStatsDocBatch {
		ph, args := inPlaceholders(roots[start:min(start+nodeStatsDocBatch, len(roots))])
		res, err := d.query(ctx, key, `SELECT chunk_id, brief, abstract, inputs_sha256, `+tier+` FROM chunk_annotations
			WHERE level = '`+annotationLevelDocument+`' AND chunk_id IN (`+ph+`)`, args...)
		if err != nil {
			return err
		}
		if res.Truncated {
			return fmt.Errorf("node_stats: an annotation batch was cut short")
		}
		for _, r := range res.Rows {
			if sd := byRoot[asStr(r[0])]; sd != nil {
				sd.brief, sd.abstract, sd.hash, sd.annotated = asStr(r[1]), asStr(r[2]), asStr(r[3]), true
				sd.metadataOnly = asStr(r[4]) == "metadata"
			}
		}
	}
	return nil
}

// loadStoredNodeStats reads every node_stats row of the scope, without centroids.
func (d *Document) loadStoredNodeStats(ctx context.Context, key sqlmem.ScopeKey, out map[string]storedNodeStats) error {
	res, err := d.query(ctx, key, `SELECT node_id, kind, members, members_with_vector, spread, embed_model,
		exemplars, siblings, terms, inputs_sha256, members_sha256, computed_at FROM node_stats`)
	if err != nil {
		return err
	}
	if res.Truncated {
		return fmt.Errorf("node_stats: the stored rows were cut short")
	}
	for _, r := range res.Rows {
		row := storedNodeStats{members: asInt(r[2]), withVector: asInt(r[3]), spread: asFloat64Ptr(r[4]),
			embedModel: asStr(r[5]), inputs: asStr(r[9]), memberSHA: asStr(r[10]), computedAt: asInt64Val(r[11])}
		// A malformed JSON column (a row from a later version) reads as empty.
		_ = json.Unmarshal([]byte(asStr(r[6])), &row.exemplars)
		_ = json.Unmarshal([]byte(asStr(r[7])), &row.siblings)
		_ = json.Unmarshal([]byte(asStr(r[8])), &row.terms)
		out[nodeStatsKey(asStr(r[1]), asStr(r[0]))] = row
	}
	return nil
}

// loadNodeStatsVectors reads the document-level annotation vector of each member
// document's root, of model only. A vector of another model is no vector. Read
// once per plan: a second call returns the first one's vectors.
func (d *Document) loadNodeStatsVectors(ctx context.Context, key sqlmem.ScopeKey, tenant string, p *nodeStatsPlan) (map[string]nodestats.Vector, error) {
	if p.vecs != nil {
		return p.vecs, nil
	}
	docOfRoot := map[string]string{}
	for id, sd := range p.docs {
		docOfRoot[sd.root] = id
	}
	out := map[string]nodestats.Vector{}
	mscope := store.MemoryScopeTenant
	scopeID := annotationMemScopeID(mscope, key)
	suffix := ":" + annotationLevelDocument
	after := ""
	for {
		rows, err := d.Store.MemoryEmbedListVectors(ctx, tenant, mscope, scopeID, annotationKeyPrefix, after, nodeStatsVectorPage)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			root, ok := strings.CutSuffix(strings.TrimPrefix(r.Key, annotationKeyPrefix), suffix)
			if !ok || rowModel(r) != p.model {
				continue
			}
			if doc, ok := docOfRoot[root]; ok && p.averaged(doc) {
				out[doc] = nodestats.Vector(r.Vector)
			}
		}
		if len(rows) < nodeStatsVectorPage {
			p.vecs = out
			return out, nil
		}
		after = rows[len(rows)-1].Key
	}
}

// nodeStatsNode is one node in node_stats' answer.
type nodeStatsNode struct {
	NodeID string `json:"node_id"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Tag    string `json:"tag,omitempty"`
	// Members is the node's current member count over the whole scope.
	Members int `json:"members"`
	// MembersWithVector: compute, the members a vector was found for; report, the
	// stored row's figure (absent without a row).
	MembersWithVector *int `json:"members_with_vector,omitempty"`
	// Status: fresh (the row's inputs are current), stale or missing (no row).
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	// Computed: compute wrote the row in this call.
	Computed bool `json:"computed,omitempty"`
	// Error: the statistics of this node could not be computed (counts were kept).
	Error string `json:"error,omitempty"`
}

// NodeStatsReport is node_stats' answer, and a sweeper pass's.
type NodeStatsReport struct {
	Mode   string `json:"mode"`
	Scheme string `json:"scheme,omitempty"`
	// Vectors: the vector stage runs here; Note says why it does not.
	Vectors    bool            `json:"vectors"`
	Note       string          `json:"note,omitempty"`
	EmbedModel string          `json:"embed_model,omitempty"`
	MinMembers int             `json:"min_members"`
	Nodes      []nodeStatsNode `json:"nodes"`
	// Orphans are rows whose node is no longer in force; compute removes them
	// (only without scheme, which sees every node).
	Orphans  []string `json:"orphans,omitempty"`
	Fresh    int      `json:"fresh"`
	Stale    int      `json:"stale"`
	Missing  int      `json:"missing"`
	Computed int      `json:"computed"`
	Removed  int      `json:"removed"`
	// Series is the series stage's part (RFC GM 6A, document_measure.go).
	Series *SeriesReport `json:"series,omitempty"`
}

// nodeStats is Document op=node_stats: report what is stale and why, or compute
// the stale nodes. Always the tenant scope: the taxonomy is the tenant's.
func (d *Document) nodeStats(ctx context.Context, in docInput) (tools.Result, error) {
	mode := in.Mode
	if mode == "" {
		mode = nodeStatsModeReport
	}
	if mode != nodeStatsModeReport && mode != nodeStatsModeCompute {
		return errValidation(fmt.Sprintf("%s: mode %q is not report or compute", nodeStatsOp, mode),
			"Pass mode: report to see what is stale, compute to recompute it."), nil
	}
	key, _, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return docFail(nodeStatsOp, err), nil
	}
	if _, _, err := d.resolveScope(ctx, "tenant"); err != nil {
		return errPermission(nodeStatsOp+": permission: node statistics are kept in the tenant scope, which this caller is not granted",
			"Ask an operator to grant the tenant scope in memory_scopes and sql_scopes."), nil
	}
	if !callerDecidesIn(ctx, key) && !HasStatementMethodGrant(ctx) {
		return errPermission(nodeStatsOp+": permission: only an operator, or a code agent granted statement_methods, runs node statistics",
			"Ask an operator to run it, or to grant this agent statement_methods. describe_ontology with_stats reads what is stored."), nil
	}
	// Every member's text and vector goes into a node's figures, so the pass must be
	// able to read every document of the scope.
	list, err := d.readableDocList(ctx, d.newDocACL(ctx, key))
	if err != nil {
		return errFrom(nodeStatsOp+": "+err.Error(), err), nil
	}
	if !readsEverything(list) {
		return errPermission(nodeStatsOp+": permission: the pass reads every document of the tenant scope, and this caller's access-control rules hide some",
			"Run it as an operator, or from an agent whose rules let it read the whole tenant scope."), nil
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || read.partial {
		// A partial read can lose a whole scheme section: its nodes would read as
		// removed and their rows be deleted as orphans.
		return errBusiness(nodeStatsOp+": the tenant ontology could not be read completely", "Retry the call; a store fault is usually transient."), nil
	}
	if in.Scheme != "" && !schemeInForce(read, in.Scheme) {
		return errValidation(fmt.Sprintf("%s: no scheme %q is in force", nodeStatsOp, in.Scheme),
			"Name a scheme from describe_ontology's schemes, or omit scheme for every node."), nil
	}
	ans, err := d.NodeStatsPass(ctx, key, direntTenant(ctx), read, in.Scheme, mode == nodeStatsModeCompute)
	if err != nil {
		return errFrom(nodeStatsOp+": "+err.Error(), err), nil
	}
	return jsonResult(ans)
}

// readsEverything reports whether a reader's list hides no document.
func readsEverything(l *docReadList) bool {
	return l == nil || (l.unnamedReadable && len(l.differs) == 0)
}

func schemeInForce(read OntologyRead, name string) bool {
	if !read.Confirmed {
		return false
	}
	for _, s := range read.Schemes {
		if s.Name == name {
			return true
		}
	}
	return false
}

// NodeStatsPass reports, and with compute recomputes, the node statistics of the
// tenant scope key names. tenant is the memory tenant the annotation vectors are kept
// under. The caller has checked who may run it.
func (d *Document) NodeStatsPass(ctx context.Context, key sqlmem.ScopeKey, tenant string, read OntologyRead, scheme string, compute bool) (NodeStatsReport, error) {
	ans := NodeStatsReport{Mode: nodeStatsModeReport, Scheme: scheme, MinMembers: nodestats.MinMembers, Nodes: []nodeStatsNode{}}
	if compute {
		ans.Mode = nodeStatsModeCompute
	}
	p, err := d.buildNodeStatsPlan(ctx, key, read, scheme)
	if err != nil {
		return ans, err
	}
	ans.Vectors, ans.Note, ans.EmbedModel = p.model != "", p.note, p.model

	var stale []nodeStatsPending
	inForce := map[string]bool{}
	for _, n := range p.nodes {
		inForce[nodeStatsKey(n.Kind, n.ID)] = true
		node := nodeStatsNode{NodeID: n.ID, Kind: n.Kind, Name: n.Name, Tag: n.Tag, Members: len(n.members), Status: "fresh"}
		row, ok := p.stored[nodeStatsKey(n.Kind, n.ID)]
		switch {
		case !ok:
			node.Status, node.Reason = "missing", "never computed"
		case row.inputs != p.nodeHash(n):
			node.Status, node.Reason = "stale", p.staleReason(row, n)
		}
		if ok {
			wv := row.withVector
			node.MembersWithVector = &wv
		}
		ans.Nodes = append(ans.Nodes, node)
	}
	for i := range ans.Nodes {
		switch ans.Nodes[i].Status {
		case "fresh":
			ans.Fresh++
		case "stale":
			ans.Stale++
		case "missing":
			ans.Missing++
		}
		if ans.Nodes[i].Status != "fresh" {
			stale = append(stale, nodeStatsPending{n: p.nodes[i], node: &ans.Nodes[i]})
		}
	}
	if scheme == "" {
		for k := range p.stored {
			if !inForce[k] {
				ans.Orphans = append(ans.Orphans, strings.Replace(k, "\x00", ":", 1))
			}
		}
		sort.Strings(ans.Orphans)
	}
	// The series stage (RFC GM 6A, document_measure.go): its reads come before any
	// write, as every read of the pass does.
	sp, err := d.buildSeriesPlan(ctx, key, read, scheme, p)
	if err != nil {
		return ans, err
	}
	storedPoints, err := d.loadMeasurePoints(ctx, key, 0, nil, nil)
	if err != nil {
		return ans, err
	}
	series, points := sp.seriesDiff(storedPoints)
	ans.Series = &series
	nodesDue := len(stale) > 0 || len(ans.Orphans) > 0
	if !compute || (!nodesDue && len(points) == 0) {
		return ans, nil
	}

	var vecs map[string]nodestats.Vector
	if p.model != "" && (len(stale) > 0 || hasShift(points)) {
		vecs, err = d.loadNodeStatsVectors(ctx, key, tenant, p)
		if errors.Is(err, store.ErrVectorUnsupported) {
			ans.Vectors, ans.Note, ans.EmbedModel, p.model = false, "the store keeps no vectors (that needs Postgres with pgvector): only member counts are kept", "", ""
			// No vector was read, so no shift is known: leave the stored ones as they are.
			points = withoutShift(points)
		} else if err != nil {
			return ans, err
		}
	}
	now := time.Now().UnixNano()
	if nodesDue {
		if err := d.computeNodeStats(ctx, key, p, stale, vecs, ans.Orphans, now); err != nil {
			return ans, err
		}
		ans.Computed, ans.Removed = len(stale), len(ans.Orphans)
	}
	points = fillShift(points, vecs)
	if err := d.writeMeasurePoints(ctx, key, points, now); err != nil {
		return ans, err
	}
	series.Computed = len(points)
	return ans, nil
}

// nodeStatsPending is a node to recompute, with its place in the answer.
type nodeStatsPending struct {
	n    statNode
	node *nodeStatsNode
}

// computeNodeStats recomputes the stale nodes and writes them with the orphan
// removals, filling each node's answer.
func (d *Document) computeNodeStats(ctx context.Context, key sqlmem.ScopeKey, p *nodeStatsPlan, stale []nodeStatsPending,
	vecs map[string]nodestats.Vector, orphans []string, now int64) error {
	rows := make([]nodeStatsRow, 0, len(stale))
	figures := p.figures(vecs)
	for _, s := range stale {
		row, nerr := p.computeRow(s.n, vecs, figures)
		if nerr != nil {
			s.node.Error = nerr.Error()
		}
		row.computedAt = now
		wv := row.withVector
		s.node.MembersWithVector, s.node.Computed = &wv, true
		rows = append(rows, row)
	}
	return d.writeNodeStats(ctx, key, rows, orphans)
}

// staleReason says which input of a stale row moved, as far as the row tells.
func (p *nodeStatsPlan) staleReason(row storedNodeStats, n statNode) string {
	switch {
	case !row.current():
		return "computed before statistics left out restricted documents; not served until recomputed"
	case row.embedModel != p.model:
		return "embed model changed"
	case row.memberSHA != p.memberSetHash(n.members):
		return "members changed"
	}
	return "the node's definition or a member's annotation changed"
}

// nodeFigures is a node's centroid and the texts its terms are drawn from, for
// every node of the plan: a stale node's siblings need theirs whether or not they
// are stale.
type nodeFigures struct {
	centroid *nodestats.Centroid
	members  []nodestats.Member
	texts    []string
	err      error
}

func (p *nodeStatsPlan) figures(vecs map[string]nodestats.Vector) map[string]*nodeFigures {
	out := make(map[string]*nodeFigures, len(p.nodes))
	for _, n := range p.nodes {
		f := &nodeFigures{}
		for _, id := range n.members {
			if v, ok := vecs[id]; ok {
				f.members = append(f.members, nodestats.Member{ID: id, Vec: v})
			}
			if sd := p.docs[id]; sd != nil && sd.annotated && !sd.metadataOnly {
				f.texts = append(f.texts, sd.brief, sd.abstract)
			}
		}
		if len(f.members) >= nodestats.MinMembers {
			vs := make([]nodestats.Vector, len(f.members))
			for i, m := range f.members {
				vs[i] = m.Vec
			}
			c, err := nodestats.CentroidOf(vs)
			if err != nil {
				f.err = err
			} else {
				f.centroid = &c
			}
		}
		out[nodeStatsKey(n.Kind, n.ID)] = f
	}
	return out
}

// nodeStatsRow is one row to write.
type nodeStatsRow struct {
	id, kind            string
	members, withVector int
	spread              *float64
	centroid            []float32
	embedModel          string
	exemplars, siblings []nodeStatsScored
	terms               []string
	inputs, memberSHA   string
	computedAt          int64
	countsOnly          bool
}

// computeRow is n's row. Counts are always kept; the statistics only with a vector
// stage and at least MinMembers members with a vector. A failure of the statistics
// keeps the counts and is returned beside the row.
func (p *nodeStatsPlan) computeRow(n statNode, vecs map[string]nodestats.Vector, figures map[string]*nodeFigures) (nodeStatsRow, error) {
	f := figures[nodeStatsKey(n.Kind, n.ID)]
	row := nodeStatsRow{id: n.ID, kind: n.Kind, members: len(n.members), withVector: len(f.members),
		embedModel: p.model, inputs: p.nodeHash(n), memberSHA: p.memberSetHash(n.members), countsOnly: true}
	if p.model == "" || f.centroid == nil {
		return row, f.err
	}
	c := *f.centroid
	ex, err := nodestats.Exemplars(c, f.members, nodestats.KExemplars)
	if err != nil {
		return row, err
	}
	var sibs []nodestats.NodeCentroid
	var sibTexts [][]string
	for _, o := range p.nodes {
		if o.group != n.group || (o.Kind == n.Kind && o.ID == n.ID) {
			continue
		}
		of := figures[nodeStatsKey(o.Kind, o.ID)]
		sibTexts = append(sibTexts, of.texts)
		if of.centroid != nil {
			sibs = append(sibs, nodestats.NodeCentroid{ID: o.ID, Centroid: *of.centroid})
		}
	}
	near := []nodestats.Scored{}
	if len(sibs) > 0 {
		if near, err = nodestats.NearestSiblings(nodestats.NodeCentroid{ID: n.ID, Centroid: c}, sibs, nodestats.KSiblings); err != nil {
			return row, err
		}
	}
	terms, err := nodestats.DistinctiveTerms(f.texts, sibTexts, nodestats.TopTerms)
	if err != nil {
		return row, err
	}
	spread := c.Spread()
	row.spread, row.countsOnly = &spread, false
	row.centroid = make([]float32, len(c.Mean))
	for i, x := range c.Mean {
		row.centroid[i] = float32(x)
	}
	for _, e := range ex {
		row.exemplars = append(row.exemplars, nodeStatsScored{ID: e.ID, Score: e.Score})
	}
	for _, s := range near {
		row.siblings = append(row.siblings, nodeStatsScored{ID: s.ID, Score: s.Score})
	}
	for _, t := range terms {
		row.terms = append(row.terms, t.Term)
	}
	return row, nil
}

// writeNodeStats upserts rows and removes the orphan rows ("kind:id") in one
// transaction. An upsert, not delete-then-insert: two passes over one scope (the
// sweeper on another replica and an operator call) would otherwise race on the key.
func (d *Document) writeNodeStats(ctx context.Context, key sqlmem.ScopeKey, rows []nodeStatsRow, orphans []string) error {
	jsonText := func(v any, empty bool) any {
		if empty {
			return nil
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	return d.withSqlTxn(ctx, key, func(txnID string) error {
		for _, r := range rows {
			var centroid any
			if !r.countsOnly {
				centroid = projection.EncodeFloat32s(r.centroid)
			}
			var spread any
			if r.spread != nil {
				spread = *r.spread
			}
			model := any(nil)
			if r.embedModel != "" {
				model = r.embedModel
			}
			if err := d.execTxn(ctx, txnID, `INSERT INTO node_stats (node_id, kind, members, members_with_vector, spread,
				centroid, embed_model, exemplars, siblings, terms, inputs_sha256, members_sha256, computed_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (node_id, kind) DO UPDATE SET members = excluded.members,
				members_with_vector = excluded.members_with_vector, spread = excluded.spread, centroid = excluded.centroid,
				embed_model = excluded.embed_model, exemplars = excluded.exemplars, siblings = excluded.siblings,
				terms = excluded.terms, inputs_sha256 = excluded.inputs_sha256, members_sha256 = excluded.members_sha256,
				computed_at = excluded.computed_at`,
				r.id, r.kind, r.members, r.withVector, spread, centroid, model,
				jsonText(r.exemplars, r.countsOnly), jsonText(r.siblings, r.countsOnly), jsonText(r.terms, r.countsOnly),
				r.inputs, r.memberSHA, r.computedAt); err != nil {
				return fmt.Errorf("node_stats %s %s: %w", r.kind, r.id, err)
			}
		}
		for _, o := range orphans {
			kind, id, _ := strings.Cut(o, ":")
			if err := d.execTxn(ctx, txnID, `DELETE FROM node_stats WHERE node_id = ? AND kind = ?`, id, kind); err != nil {
				return err
			}
		}
		return nil
	})
}

// nodeStatsView is a node's statistics as describe_ontology with_stats shows them,
// the same to every reader of the ontology (RFC GM C8).
type nodeStatsView struct {
	Members           *int              `json:"members,omitempty"`
	MembersWithVector *int              `json:"members_with_vector,omitempty"`
	Spread            *float64          `json:"spread,omitempty"`
	Exemplars         []nodeStatsTitled `json:"exemplars,omitempty"`
	Siblings          []string          `json:"siblings,omitempty"`
	Terms             []string          `json:"terms,omitempty"`
	ComputedAt        int64             `json:"computed_at,omitempty"`
	// Stale: an input of the row moved since it was computed.
	Stale bool `json:"stale,omitempty"`
	// Recomputing: the node's members are no longer the ones the row was computed
	// from, so only today's member count is served until the next pass.
	Recomputing string `json:"recomputing,omitempty"`
}

// nodeStatsRecomputing is Recomputing's text. One text for every cause (a member
// restricted, filed elsewhere, turned metadata-only, or simply added), so the
// answer does not say which.
const nodeStatsRecomputing = "the node's members changed since these statistics were computed; spread, terms, exemplars and siblings return after the next node_stats pass"

// nodeStatsTitled is an exemplar document.
type nodeStatsTitled struct {
	DocumentID string `json:"document_id"`
	Title      string `json:"title"`
}

// describeStats adds with_stats' statistics to a description describeCounts has
// already counted: per concept and per document class, the stored row when one of
// unrestrictedFamily is stored. Every reader gets the same view: the figures come
// from unrestricted documents only. A store fault marks the answer partial, as the
// counts do. It returns the plan, which describeSeries checks the series against;
// nil when there is none.
func (d *Document) describeStats(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, out *ontologyDescription) *nodeStatsPlan {
	if _, _, err := d.resolveScope(ctx, "tenant"); err != nil {
		return nil // describeCounts has said why
	}
	p, err := d.buildNodeStatsPlan(ctx, key, read, "")
	if err != nil {
		out.Partial = true
		out.Issues = append(out.Issues, "with_stats: the node statistics could not be read; read again")
		return nil
	}
	names := map[string]string{}
	for _, n := range p.nodes {
		names[nodeStatsKey(n.Kind, n.ID)] = n.Name
	}
	view := map[string]*nodeStatsView{}
	for _, n := range p.nodes {
		k := nodeStatsKey(n.Kind, n.ID)
		if row, ok := p.stored[k]; ok && row.current() {
			view[k] = p.statsView(n, row, names)
		}
	}
	for i := range out.Schemes {
		for j := range out.Schemes[i].Concepts {
			c := &out.Schemes[i].Concepts[j]
			c.Stats = view[nodeStatsKey(nodeKindConcept, c.ChunkID)]
		}
	}
	for i := range out.Classes {
		if out.Classes[i].Kind == "document" {
			out.Classes[i].Stats = view[nodeStatsKey(nodeKindClass, out.Classes[i].Name)]
		}
	}
	return p
}

// statsView is n's stored row as every reader sees it. The row's figures are served
// only while its stored member set is the node's current one (memberSetHash): every
// member then is still unrestricted, and still not metadata-only. Otherwise a
// document restricted after the pass (a new deny, a set_path under a denied name)
// would keep shaping the terms, spread, exemplars and siblings every reader sees
// until the next pass, which the sweeper, off by default, may never run; so only
// today's member count is served. This also hides the figures of a node that merely
// gained a member: the safe direction, for one pass.
func (p *nodeStatsPlan) statsView(n statNode, row storedNodeStats, names map[string]string) *nodeStatsView {
	if row.memberSHA != p.memberSetHash(n.members) {
		members := len(n.members)
		return &nodeStatsView{Members: &members, ComputedAt: row.computedAt, Stale: true, Recomputing: nodeStatsRecomputing}
	}
	v := &nodeStatsView{Members: &row.members, MembersWithVector: &row.withVector, Spread: row.spread,
		Terms: row.terms, ComputedAt: row.computedAt, Stale: row.inputs != p.nodeHash(n)}
	for _, e := range row.exemplars {
		if sd := p.docs[e.ID]; sd != nil {
			v.Exemplars = append(v.Exemplars, nodeStatsTitled{DocumentID: e.ID, Title: sd.title})
		}
	}
	for _, s := range row.siblings {
		if name, ok := names[nodeStatsKey(n.Kind, s.ID)]; ok {
			v.Siblings = append(v.Siblings, name)
		}
	}
	return v
}

// NodeStatsScopeKey runs one compute pass over a tenant scope the sweeper found.
// ran is false, and nothing is read or provisioned, for any other scope, for a
// tenant scope whose document tables do not exist yet, and for a tenant with no
// ontology document: there is no taxonomy to keep statistics of.
//
// The memory tenant is whichever spelling of the scope's tenant holds its
// annotation vectors (bodyTenantsFor); the run identity carries it, so the
// ontology read and the pass resolve the same tenant the op would.
func (d *Document) NodeStatsScopeKey(ctx context.Context, key sqlmem.ScopeKey) (rep NodeStatsReport, ran bool, err error) {
	if d.Store == nil || d.SqlMem == nil || key.Scope != "tenant" || key.ScopeID != key.Tenant {
		return rep, false, nil
	}
	// A probe that never provisions: d.query would create the tables on a scope
	// that holds only Memory SQL.
	if !d.tableHasColumn(ctx, key, "documents", "id") {
		return rep, false, nil
	}
	tenants := bodyTenantsFor(key.Tenant)
	memTenant := tenants[len(tenants)-1]
	if d.nodeStatsVectorsNote() == "" && len(tenants) > 1 {
		scopeID := annotationMemScopeID(store.MemoryScopeTenant, key)
		for _, t := range tenants {
			page, lerr := d.Store.MemoryEmbedListVectors(ctx, t, store.MemoryScopeTenant, scopeID, annotationKeyPrefix, "", 1)
			if lerr == nil && len(page) > 0 {
				memTenant = t
				break
			}
		}
	}
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: memTenant})
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || read.partial {
		return rep, false, fmt.Errorf("node_stats: the tenant ontology could not be read completely")
	}
	if read.DocumentID == "" {
		return rep, false, nil
	}
	rep, err = d.NodeStatsPass(ctx, key, memTenant, read, "", true)
	return rep, err == nil, err
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
