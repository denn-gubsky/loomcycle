package builtin

// document_statements.go — a relation is a STATEMENT (RFC FN P0).
//
// An edge used to be (from_id, to_id, kind, created_at, auto): it could not say why
// it existed, who made it, from what text, since when, or whether anyone checked
// it, so every consumer that needed those answers kept a ledger beside the store
// and materialised edges from it — one fact in two places, nothing keeping them in
// step. Here the edge carries them itself: its own id, a status (asserted |
// proposed | rejected), the method that made it, world-time validity, the actor
// and run, a verdict, declared attributes, and evidence rows — verbatim quotes
// from a chunk of this scope, a chat turn, or another scope by natural key. Every
// reader the runtime owns (get_edges, backlinks, graph_recall, export, judge_fact)
// sees them without a second table to keep in agreement.
//
// No foreign key, like the rest of this schema: deleteEdgesWhere removes an edge's
// evidence with it, and the dead-link sweep collects any evidence row an older
// path left behind.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// chunkEdgesColumns is the statement table's shape. One definition, so a scope
// created fresh and a scope rebuilt by migrateEdgeStatements end in the same table.
//
// confidence is DOUBLE PRECISION for the reason chunk_memory_meta's is: postgres
// REAL is float4 and would read 0.9 back as 0.8999999761581421.
//
// Two confidences, because they are two claims: `confidence` is what the writer
// said of its own relation, `verdict_confidence` what a judge (or an operator)
// found when checking it. Only the verdict withholds — a writer unsure of a link
// is not a refutation of it — and an operator's accept can clear a refuting
// verdict without erasing what the writer said.
//
// operator_ruled_at marks a statement an operator ruled on (accept_statement,
// reject_statement). It is its own column, written only by that ruling and by no
// run path, because judged_by is rewritten by every later judge: a guard on it
// would let a run's judge_fact lift the protection a ruling gives.
const chunkEdgesColumns = `
		id TEXT NOT NULL, from_id TEXT NOT NULL, to_id TEXT NOT NULL, kind TEXT NOT NULL,
		created_at BIGINT NOT NULL, auto INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'asserted', method TEXT NOT NULL DEFAULT 'manual',
		confidence DOUBLE PRECISION, valid_at BIGINT, invalid_at BIGINT,
		expired_at BIGINT, verified_at BIGINT, actor TEXT, run_id TEXT,
		ontology_version INTEGER, discriminator TEXT NOT NULL DEFAULT '', attributes TEXT,
		judged_at BIGINT, judge_reason TEXT, judged_by TEXT, verdict_confidence DOUBLE PRECISION,
		operator_ruled_at BIGINT,
		PRIMARY KEY (id)`

// preStatementEdgeColumns is the whole chunk_edges of a scope created before
// statements. A table holding any other column is not that table.
var preStatementEdgeColumns = map[string]bool{"from_id": true, "to_id": true, "kind": true, "created_at": true, "auto": true}

// chunkEdgeEvidenceColumns: exactly one of the three source forms is set — a chunk
// of this scope, a chat turn (session_id + event_seq), or another scope by
// natural key (resolved by a tolerant reader, never enumerated).
const chunkEdgeEvidenceColumns = `
		id TEXT PRIMARY KEY, edge_id TEXT NOT NULL,
		chunk_id TEXT, session_id TEXT, event_seq BIGINT,
		ref_scope TEXT, ref_natural_key TEXT,
		quote TEXT NOT NULL, char_start INTEGER, char_end INTEGER,
		stale INTEGER NOT NULL DEFAULT 0, created_at BIGINT NOT NULL, run_id TEXT`

// chunkEdgeIndexes name columns a pre-statement chunk_edges does not have, so they
// cannot sit in docSchemaDDL (which runs before the rebuild — the #1106 ordering
// lesson). The unique index is what keeps one assertion per undated, unqualified
// triple: a second link of the same triple adds evidence instead of a second edge,
// while a statement that differs in valid_at or in its discriminating qualifier
// coexists (one edge per modification).
var chunkEdgeIndexes = []string{
	`CREATE UNIQUE INDEX IF NOT EXISTS chunk_edges_statement ON chunk_edges(from_id, to_id, kind, COALESCE(valid_at, 0), discriminator)`,
	`CREATE INDEX IF NOT EXISTS chunk_edges_to_kind ON chunk_edges(to_id, kind)`,
	`CREATE INDEX IF NOT EXISTS chunk_edges_from_kind ON chunk_edges(from_id, kind)`,
	`CREATE INDEX IF NOT EXISTS chunk_edges_status ON chunk_edges(status) WHERE status <> 'asserted'`,
	`CREATE INDEX IF NOT EXISTS chunk_edge_evidence_edge ON chunk_edge_evidence(edge_id)`,
	`CREATE INDEX IF NOT EXISTS chunk_edge_evidence_chunk ON chunk_edge_evidence(chunk_id)`,
}

// Statement statuses. Unknown values read as live (fail open): a status this
// build does not know was written by a newer one, and hiding it would lose data.
const (
	edgeStatusAsserted = "asserted"
	edgeStatusProposed = "proposed"
	edgeStatusRejected = "rejected"
)

// Server-stamped methods. None is accepted from the wire.
const (
	edgeMethodManual   = "manual"
	edgeMethodWikilink = "wikilink"
	edgeMethodImport   = "import"
	edgeMethodSync     = "sync"
	edgeMethodCanvas   = "canvas"
	edgeMethodRuntime  = "runtime"
	// edgeMethodConsolidator: a typed relation the memory pass extracted from a
	// fact (upsert_chunk relation). Never accepted from the wire.
	edgeMethodConsolidator = "consolidator"
)

// edgeOriginMethodAttr names, on a statement a sync carried, the method it was
// made by where it came from. Provenance, not a qualifier: the vocabulary gate
// does not check it.
const edgeOriginMethodAttr = "origin_method"

// madeByAPerson reports whether a person wrote s: the test behind every rule
// that keeps a pass from undoing a person's statement. A pull writes every
// statement it carries with method sync, so a synced statement is judged by its
// origin_method, and judged a person's unless that names a machine method. It
// fails closed: an origin of manual, none at all (a pull from before
// origin_method was carried), or anything this build does not know reads as a
// person's.
func madeByAPerson(s *statementRow) bool {
	switch s.Method {
	case edgeMethodManual:
		return true
	case edgeMethodSync:
		origin, _ := s.Attributes[edgeOriginMethodAttr].(string)
		return !machineMethod(origin)
	}
	return false
}

// machineMethod reports whether m names a method code stamps rather than a
// person: a code agent's rule:<name> / extractor:<name>, or one of the
// runtime's own. sync is not one — it says how a statement arrived, not who
// made it.
func machineMethod(m string) bool {
	if prefix, name, ok := strings.Cut(m, ":"); ok {
		return (prefix == "rule" || prefix == "extractor") && statementMethodName.MatchString(name)
	}
	switch m {
	case edgeMethodImport, edgeMethodCanvas, edgeMethodWikilink, edgeMethodRuntime, edgeMethodConsolidator, edgeMethodOntology:
		return true
	}
	return false
}

// Bounds on what a caller may attach to one statement.
const (
	edgeEvidenceDefaultMax = 32
	edgeEvidenceQuoteMax   = 4000
	edgeAttributesMaxBytes = 4096
	edgeGetEdgesDefault    = 500
	edgeGetEdgesMax        = 5000
	edgeEvidencePreview    = 3
	// edgeProposalConfidenceCap is where an annotated (ontology-invalid) proposal's
	// confidence is lowered to — below a clean proposal, above the withholding floor,
	// so it sorts last in review without disappearing.
	edgeProposalConfidenceCap = 0.3
)

// edgeEvidenceMax is LOOMCYCLE_DOCUMENT_EVIDENCE_MAX_PER_EDGE. An edge with many
// supporting quotes is evidence the relation is real; past a few dozen it is a
// table growing without telling a reader anything new.
func edgeEvidenceMax() int {
	if v, err := strconv.Atoi(os.Getenv("LOOMCYCLE_DOCUMENT_EVIDENCE_MAX_PER_EDGE")); err == nil && v > 0 {
		return v
	}
	return edgeEvidenceDefaultMax
}

// migrateEdgeStatements rebuilds a chunk_edges created before statements into the
// statement shape, then ensures the indexes.
//
// A rebuild rather than ALTERs: the primary key moves from the triple to `id`,
// which sqlite cannot ALTER at all and which postgres could, but two code paths
// for one shape is how the tiers drift. Every existing edge keeps its endpoints,
// kind, created_at and auto flag, gains a fresh id, and reads back as an asserted
// statement with no evidence — `wikilink` for an inline [[name]] edge, `manual`
// otherwise — which is byte-identical for every reader that ignores the new fields.
//
// One transaction, so a crash leaves either the old table or the new one.
//
// The rebuild drops every statement column, so it runs only on a POSITIVE reading
// of the table's columns: a probe that failed (a lock, a dropped connection) is an
// error here, never "the column is missing" — which is how a transient fault would
// otherwise rewrite every statement as a fresh, evidence-less assertion.
func (d *Document) migrateEdgeStatements(ctx context.Context, key sqlmem.ScopeKey) error {
	cols, err := d.tableColumns(ctx, key, "chunk_edges")
	if err != nil {
		return fmt.Errorf("probe chunk_edges: %w", err)
	}
	rebuild, err := edgeTableNeedsRebuild(cols)
	if err != nil {
		return err
	}
	if rebuild {
		idExpr := `lower(hex(randomblob(16)))`
		if d.SqlMem.Tier() == "postgres" {
			idExpr = `md5(random()::text || clock_timestamp()::text || from_id || to_id || kind)`
		}
		stmts := []string{
			`CREATE TABLE chunk_edges_statements (` + chunkEdgesColumns + `)`,
			`INSERT INTO chunk_edges_statements (id, from_id, to_id, kind, created_at, auto, method)
			 SELECT ` + idExpr + `, from_id, to_id, kind, created_at, auto,
			        CASE WHEN auto = 1 THEN '` + edgeMethodWikilink + `' ELSE '` + edgeMethodManual + `' END
			   FROM chunk_edges`,
			`DROP TABLE chunk_edges`,
			`ALTER TABLE chunk_edges_statements RENAME TO chunk_edges`,
		}
		if d.SqlMem.Tier() == "postgres" {
			// The renamed table keeps its constraint's old name; a fresh scope's is
			// chunk_edges_pkey. Same name in both, so a dump of one restores as the other.
			stmts = append(stmts, `ALTER TABLE chunk_edges RENAME CONSTRAINT chunk_edges_statements_pkey TO chunk_edges_pkey`)
		}
		if err := d.withSqlTxn(ctx, key, func(txnID string) error {
			for _, s := range stmts {
				if err := d.execTxn(ctx, txnID, s); err != nil {
					return fmt.Errorf("migrate chunk_edges to statements: %w", err)
				}
			}
			return nil
		}); err != nil {
			return err
		}
	} else if !cols["verdict_confidence"] {
		// A scope already in the statement shape from before the two confidences
		// were split. A verdict was the last writer of `confidence` exactly when
		// verified_at is set (only judge_fact stamps it), so that is the value moved.
		if err := d.withSqlTxn(ctx, key, func(txnID string) error {
			for _, s := range []string{
				`ALTER TABLE chunk_edges ADD COLUMN verdict_confidence DOUBLE PRECISION`,
				`UPDATE chunk_edges SET verdict_confidence = confidence WHERE verified_at IS NOT NULL`,
			} {
				if err := d.execTxn(ctx, txnID, s); err != nil {
					return fmt.Errorf("add chunk_edges.verdict_confidence: %w", err)
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if !rebuild && !cols["operator_ruled_at"] {
		// Before the mark, a ruling was recognisable only by judged_by — and only
		// until a later judge rewrote it, so what still says operator is what is
		// marked. A rebuilt table has no rulings and is created with the column.
		if err := d.withSqlTxn(ctx, key, func(txnID string) error {
			for _, s := range []string{
				`ALTER TABLE chunk_edges ADD COLUMN operator_ruled_at BIGINT`,
				`UPDATE chunk_edges SET operator_ruled_at = COALESCE(judged_at, created_at) WHERE judged_by = 'operator'`,
			} {
				if err := d.execTxn(ctx, txnID, s); err != nil {
					return fmt.Errorf("add chunk_edges.operator_ruled_at: %w", err)
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	for _, s := range chunkEdgeIndexes {
		if _, err := d.SqlMem.Exec(ctx, key, s, nil, 0); err != nil {
			return err
		}
	}
	return nil
}

// tableColumns lists a table's columns from the catalog, so a success is a
// complete answer and a failure is only a failure. Not `SELECT * … WHERE 1=0`:
// postgres caches that statement's plan, and once the rebuild changes the table
// the cached plan's result type no longer matches — the probe itself would fail.
// Aggregated into one row, so the operator's row cap cannot clip the listing.
func (d *Document) tableColumns(ctx context.Context, key sqlmem.ScopeKey, table string) (map[string]bool, error) {
	stmt := `SELECT group_concat(name, ',') FROM pragma_table_info(?)`
	if d.SqlMem.Tier() == "postgres" {
		stmt = `SELECT string_agg(column_name, ',') FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ?`
	}
	res, err := d.query(ctx, key, stmt, table)
	if err != nil {
		return nil, err
	}
	if len(res.Rows) != 1 || res.Truncated {
		return nil, fmt.Errorf("column listing of %s came back as %d rows", table, len(res.Rows))
	}
	cols := map[string]bool{}
	for _, c := range strings.Split(asStr(res.Rows[0][0]), ",") {
		if c = strings.ToLower(strings.TrimSpace(c)); c != "" {
			cols[c] = true
		}
	}
	return cols, nil
}

// edgeTableNeedsRebuild reads a chunk_edges column listing: true for the
// pre-statement table, false for one already in the statement shape, and an error
// for anything between — a listing it cannot place is not a table to rebuild.
func edgeTableNeedsRebuild(cols map[string]bool) (bool, error) {
	if cols["discriminator"] {
		return false, nil
	}
	for c := range preStatementEdgeColumns {
		if !cols[c] {
			return false, fmt.Errorf("chunk_edges has no %s column; refusing to rebuild a table this build does not recognise", c)
		}
	}
	for c := range cols {
		if !preStatementEdgeColumns[c] {
			return false, fmt.Errorf("chunk_edges has statement column %s but no discriminator; refusing to rebuild over a partly migrated table", c)
		}
	}
	return true, nil
}

// deleteEdgesWhere removes the edges matching cond and their evidence. Every path
// that deletes edges goes through here so no evidence row outlives its edge.
// txnID "" runs outside a transaction.
func (d *Document) deleteEdgesWhere(ctx context.Context, key sqlmem.ScopeKey, txnID, cond string, args ...any) error {
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM chunk_edge_evidence WHERE edge_id IN (SELECT id FROM chunk_edges WHERE ` + cond + `)`, args},
		{`DELETE FROM chunk_edges WHERE ` + cond, args},
	}
	for _, s := range stmts {
		var err error
		if txnID != "" {
			err = d.execTxn(ctx, txnID, s.sql, s.args...)
		} else {
			err = d.exec(ctx, key, s.sql, s.args...)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// edgeEvidenceIn is one evidence row as a caller supplies it.
type edgeEvidenceIn struct {
	ChunkID    string `json:"chunk_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	EventSeq   *int64 `json:"event_seq,omitempty"`
	Scope      string `json:"scope,omitempty"`
	NaturalKey string `json:"natural_key,omitempty"`
	Quote      string `json:"quote"`
	CharStart  *int   `json:"char_start,omitempty"`
	CharEnd    *int   `json:"char_end,omitempty"`
}

// edgeWrite is one statement write, from any path.
type edgeWrite struct {
	from, to, kind string
	method         string
	status         string // asserted | proposed
	authored       bool   // a principal's own link: refused on a vocabulary violation
	confidence     *float64
	validAt        *int64
	invalidAt      *int64
	attributes     map[string]any
	evidence       []edgeEvidenceIn
	// overrideRejected: an operator may re-assert what was rejected; nobody else.
	overrideRejected bool
	// revisionOf attaches a competing proposal to an existing statement.
	revisionOf string
	// proposeIfInvalid writes the statement proposed rather than asserted when the
	// vocabulary does not accept it (annotate mode for an extracted relation).
	proposeIfInvalid bool
}

// gateAuthored reports a principal's own link_chunks: the one write the
// vocabulary refuses rather than annotates, and the one that asserts a statement
// awaiting review.
func (w edgeWrite) gateAuthored() bool {
	return w.authored && w.method == edgeMethodManual
}

// edgeWriteResult is what a write reports.
type edgeWriteResult struct {
	ID              string   `json:"id"`
	Status          string   `json:"status"`
	Created         bool     `json:"created"`
	EvidenceWritten int      `json:"evidence_written"`
	EvidenceCapped  bool     `json:"evidence_capped,omitempty"`
	OntologyValid   bool     `json:"ontology_valid"`
	Warnings        []string `json:"warnings,omitempty"`
}

// writeStatement is the one write path for a statement: link_chunks,
// propose_statement, import, canvas and sync all end here. A refusal comes back as
// *tools.Result; err is a store fault.
func (d *Document) writeStatement(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, w edgeWrite) (edgeWriteResult, *tools.Result, error) {
	var out edgeWriteResult
	if w.from == "" || w.to == "" || strings.TrimSpace(w.kind) == "" {
		r := errValidation("from_id, to_id and kind are required", "Pass all three: from_id and to_id (chunk ids) and a kind label.")
		return out, &r, nil
	}
	if w.method != edgeMethodRuntime {
		if writer, reserved := reservedEdgeKinds[w.kind]; reserved && !(w.kind == aboutEdgeKind && w.authored && (w.method == edgeMethodManual || w.method == edgeMethodImport)) {
			r := errValidation(fmt.Sprintf("kind_reserved: %q edges are written only by %s", w.kind, writer),
				"Use a kind of your own (cites, based_on, defines, implements, related, …).")
			return out, &r, nil
		}
	}
	// A caller-made `about` must run from a FACT (a chunk with entity metadata):
	// an `about` edge is what makes its target an identity node, so one from
	// ordinary prose would make a passage look like an entity. The runtime's own
	// path (upsert_chunk about_ids) writes it with method runtime; this keeps a
	// consolidator that still calls link_chunks working. An import is exempt: it
	// copies a document's own graph, whose fact metadata import_md does not carry.
	if w.kind == aboutEdgeKind && w.method != edgeMethodRuntime && w.method != edgeMethodImport {
		res, err := d.query(ctx, key, `SELECT 1 FROM chunk_memory_meta WHERE chunk_id = ? LIMIT 1`, w.from)
		if err != nil {
			return out, nil, err
		}
		if len(res.Rows) == 0 {
			r := errValidation("kind_reserved: an `about` edge joins a fact to the subject it is about, and from_id is not a fact",
				"Use a kind of your own (cites, related, …); a fact gets its `about` edges from upsert_chunk about_ids.")
			return out, &r, nil
		}
	}
	for _, id := range []struct{ name, id string }{{"from_id", w.from}, {"to_id", w.to}} {
		if _, ok, err := d.getChunkRow(ctx, key, id.id); err != nil {
			return out, nil, err
		} else if !ok {
			r := errNotFound(id.name+": no such chunk: "+id.id, "Pass a chunk id from this scope (find one with op=query_chunks or op=search).")
			return out, &r, nil
		}
	}
	env, refuse := d.checkEnvelope(ctx, key, &w)
	if refuse != nil {
		return out, refuse, nil
	}
	gate, attrs, confidence := env.gate, env.attrs, env.confidence
	out.OntologyValid, out.Warnings = gate.Valid, gate.Warnings

	evidence, refuse, err := d.checkEvidence(ctx, key, mscope, w.evidence)
	if err != nil || refuse != nil {
		return out, refuse, err
	}

	// discriminator: the value of the kind's declared discriminating qualifier, ""
	// when none is declared — then one assertion per (triple, valid_at).
	discriminator := d.discriminatorFor(ctx, w.kind, w.attributes)
	existing, err := d.statementByKey(ctx, key, w.from, w.to, w.kind, w.validAt, discriminator)
	if err != nil {
		return out, nil, err
	}
	now := time.Now().UnixNano()
	ident := tools.RunIdentity(ctx)
	actor := ident.UserID
	if name := tools.AgentName(ctx); name != "" && tools.RunID(ctx) != "" {
		actor = name
	}
	status := w.status
	if status == "" {
		status = edgeStatusAsserted
	}
	if w.proposeIfInvalid && !gate.Valid {
		status = edgeStatusProposed
	}

	if existing != nil && w.revisionOf == "" {
		if existing.Status == edgeStatusRejected && !w.overrideRejected {
			r := errBusiness("statement_rejected: this relation was rejected by an operator ("+existing.JudgeReason+")",
				"Do not re-propose it. An operator can accept it from the review queue if it was rejected in error.")
			return out, &r, nil
		}
		// A proposal is filed for review; it may not add to a relation that already
		// holds. Evidence on an asserted statement is live data, and the propose verb
		// is narrower than link (a principal holding only read + propose).
		if status == edgeStatusProposed && existing.Status == edgeStatusAsserted {
			r := errBusiness("statement_asserted: this relation is already asserted (id "+existing.ID+"), so a proposal cannot add to it",
				"Leave it, or propose a correction beside it by passing id.")
			return out, &r, nil
		}
		out.ID, out.Status = existing.ID, existing.Status
		// Only an authored link_chunks asserts a statement that awaits review (or, by
		// an operator, one rejected). A machine write — a fact's relation, sync,
		// import, canvas — carries someone else's judgement and adds its evidence to
		// the proposal; clearing the review queue is not something holding write
		// should be able to do.
		if status == edgeStatusAsserted && existing.Status != edgeStatusAsserted && w.gateAuthored() {
			if err := d.exec(ctx, key, `UPDATE chunk_edges SET status = ?, actor = ? WHERE id = ?`, edgeStatusAsserted, nullIfEmpty(actor), existing.ID); err != nil {
				return out, nil, err
			}
			out.Status = edgeStatusAsserted
		}
	} else {
		id := newDocID()
		var attrsArg any
		if attrs != nil {
			attrsArg = string(attrs)
		}
		if w.revisionOf != "" {
			// A competing proposal sits beside the statement it revises; the
			// discriminator keeps it off that statement's unique key.
			if w.attributes == nil {
				w.attributes = map[string]any{}
			}
			w.attributes["revision_of"] = w.revisionOf
			b, _ := json.Marshal(w.attributes)
			attrsArg = string(b)
		}
		disc := discriminator
		if w.revisionOf != "" {
			disc = "revision:" + id
		}
		var ov any
		if v := d.ontologyVersion(ctx); v > 0 {
			ov = v
		}
		auto := 0
		if w.method == edgeMethodWikilink {
			auto = 1
		}
		err := d.exec(ctx, key, `INSERT INTO chunk_edges
			(id, from_id, to_id, kind, created_at, auto, status, method, confidence, valid_at, invalid_at,
			 actor, run_id, ontology_version, discriminator, attributes)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, w.from, w.to, w.kind, now, auto, status, w.method, confidence, w.validAt, w.invalidAt,
			nullIfEmpty(actor), nullIfEmpty(tools.RunID(ctx)), ov, disc, attrsArg)
		if err != nil {
			// A concurrent writer may have made the same statement; its row is ours.
			again, gerr := d.statementByKey(ctx, key, w.from, w.to, w.kind, w.validAt, discriminator)
			if gerr != nil || again == nil || w.revisionOf != "" {
				return out, nil, err
			}
			id = again.ID
			status = again.Status
		} else {
			out.Created = true
		}
		out.ID, out.Status = id, status
	}

	written, capped, err := d.addEvidence(ctx, key, out.ID, evidence)
	if err != nil {
		return out, nil, err
	}
	out.EvidenceWritten, out.EvidenceCapped = written, capped
	return out, nil, nil
}

// edgeEnvelope is a statement's envelope as a write stores it: the attributes as
// JSON (nil for none), the confidence after any annotation cap, and the
// vocabulary gate's verdict.
type edgeEnvelope struct {
	attrs      []byte
	confidence *float64
	gate       relationGate
}

// checkEnvelope validates w's validity, confidence and attributes and runs the
// vocabulary gate over the relation. A refusal comes back as *tools.Result. It
// may add the ontology_valid annotation to w.attributes.
func (d *Document) checkEnvelope(ctx context.Context, key sqlmem.ScopeKey, w *edgeWrite) (edgeEnvelope, *tools.Result) {
	var env edgeEnvelope
	if w.validAt != nil && w.invalidAt != nil && *w.invalidAt <= *w.validAt {
		r := errValidation("invalid_at must be after valid_at", "Leave invalid_at unset for a relation that still holds.")
		return env, &r
	}
	if w.confidence != nil && (*w.confidence < 0 || *w.confidence > 1) {
		r := errValidation("confidence must be between 0 and 1", "Leave confidence unset when you have no estimate.")
		return env, &r
	}
	if len(w.attributes) > 0 {
		b, err := json.Marshal(w.attributes)
		if err != nil || len(b) > edgeAttributesMaxBytes {
			r := errValidation(fmt.Sprintf("attributes must be a JSON object of at most %d bytes", edgeAttributesMaxBytes), "Keep attributes to the relation's qualifiers.")
			return env, &r
		}
		env.attrs = b
	}

	// Only a principal's own link_chunks is refused on a vocabulary violation. A
	// bulk path — import_md, import_canvas, sync — carries relations someone else
	// wrote, and refusing one would drop it from an otherwise faithful copy; it is
	// kept, annotated ontology_valid=false (RFC FO decision, 2026-10-29). w.authored
	// still means "the caller's own authority" for the reserved-kind rule.
	gateAuthored := w.gateAuthored()
	gate := d.gateRelation(ctx, key, w.kind, w.from, w.to, gateAuthored)
	if gateAuthored && gate.Refuse != nil {
		return env, gate.Refuse
	}
	agate := d.gateAttributes(ctx, w.kind, w.attributes, gateAuthored)
	if gateAuthored && agate.Refuse != nil {
		return env, agate.Refuse
	}
	gate.Valid = gate.Valid && agate.Valid
	gate.Warnings = append(gate.Warnings, agate.Warnings...)
	env.gate, env.confidence = gate, w.confidence
	if !gate.Valid {
		// annotate mode (D8): kept, marked, and sorted behind a clean proposal.
		if env.attrs == nil {
			w.attributes = map[string]any{}
		}
		w.attributes["ontology_valid"] = false
		env.attrs, _ = json.Marshal(w.attributes)
		capped := edgeProposalConfidenceCap
		if env.confidence == nil || *env.confidence > capped {
			env.confidence = &capped
		}
	}
	return env, nil
}

// resyncStatement brings a statement a pull wrote to the envelope w carries: the
// checks and the vocabulary annotation a fresh write would apply, then one UPDATE
// of confidence, invalid_at and attributes when any differs. Only an asserted
// statement with method sync is touched — one written here keeps its writer's
// claim — and valid_at and the discriminator are its key, so they never change.
// Evidence is not touched.
func (d *Document) resyncStatement(ctx context.Context, key sqlmem.ScopeKey, id string, w edgeWrite) (bool, *tools.Result, error) {
	env, refuse := d.checkEnvelope(ctx, key, &w)
	if refuse != nil {
		return false, refuse, nil
	}
	s, err := d.statementByID(ctx, key, id)
	if err != nil || s == nil || s.Method != edgeMethodSync || s.Status != edgeStatusAsserted {
		return false, nil, err
	}
	var have []byte
	if len(s.Attributes) > 0 {
		have, _ = json.Marshal(s.Attributes)
	}
	if sameConfidence(s.Confidence, env.confidence) && sameInstant(s.InvalidAt, w.invalidAt) && string(have) == string(env.attrs) {
		return false, nil, nil
	}
	var attrs any
	if env.attrs != nil {
		attrs = string(env.attrs)
	}
	if err := d.exec(ctx, key, `UPDATE chunk_edges SET confidence = ?, invalid_at = ?, attributes = ? WHERE id = ? AND method = ?`,
		env.confidence, w.invalidAt, attrs, id, edgeMethodSync); err != nil {
		return false, nil, err
	}
	return true, nil, nil
}

// checkedEvidence is an evidence row that passed checkEvidence, with its span
// resolved.
type checkedEvidence struct {
	edgeEvidenceIn
	start, end *int
}

// checkEvidence verifies each row before anything is written. A quote that does
// not occur in its source is refused, never stored: a statement's evidence is what
// a judge checks it against and what an operator reads to trust it, so an
// unverified quote would be a forged citation.
func (d *Document) checkEvidence(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in []edgeEvidenceIn) ([]checkedEvidence, *tools.Result, error) {
	out := make([]checkedEvidence, 0, len(in))
	for i, ev := range in {
		where := fmt.Sprintf("evidence[%d]", i)
		quote := ev.Quote
		if strings.TrimSpace(quote) == "" {
			r := errValidation(where+": quote is required — the verbatim text that supports the relation", "Copy the supporting sentence exactly as it appears in the source.")
			return nil, &r, nil
		}
		if utf8.RuneCountInString(quote) > edgeEvidenceQuoteMax {
			r := errValidation(fmt.Sprintf("%s: quote is longer than %d characters", where, edgeEvidenceQuoteMax), "Quote the sentence that carries the relation, not the whole passage.")
			return nil, &r, nil
		}
		forms := 0
		if ev.ChunkID != "" {
			forms++
		}
		if ev.SessionID != "" || ev.EventSeq != nil {
			forms++
		}
		if ev.Scope != "" || ev.NaturalKey != "" {
			forms++
		}
		if forms != 1 {
			r := errValidation(where+": name exactly one source — chunk_id, session_id + event_seq, or scope + natural_key", "")
			return nil, &r, nil
		}
		c := checkedEvidence{edgeEvidenceIn: ev}
		switch {
		case ev.ChunkID != "":
			// A chunk the caller may not read answers exactly like a missing one:
			// verifying a quote against it would otherwise tell the caller whether
			// that text is in a document it cannot open.
			readable, err := d.chunkReadable(ctx, key, ev.ChunkID)
			if err != nil {
				return nil, nil, err
			}
			if !readable {
				r := errNotFound(where+": no such chunk: "+ev.ChunkID, "Evidence must cite a chunk of this scope; cite another scope by scope + natural_key.")
				return nil, &r, nil
			}
			cb, err := d.readBody(ctx, mscope, key, ev.ChunkID)
			if err != nil {
				return nil, nil, err
			}
			start, end, found := quoteSpan(cb.Body, quote, ev.CharStart, ev.CharEnd)
			if !found {
				r := errValidation(where+": evidence_quote_not_found — the quote does not occur in chunk "+ev.ChunkID,
					"Copy the text exactly from the chunk body (op=get_chunk); a paraphrase is not evidence.")
				return nil, &r, nil
			}
			c.start, c.end = &start, &end
		case ev.SessionID != "":
			if ev.EventSeq == nil {
				r := errValidation(where+": a chat-turn citation needs both session_id and event_seq", "")
				return nil, &r, nil
			}
			text, ok, err := d.sessionTurnText(ctx, key, ev.SessionID, *ev.EventSeq)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				r := errNotFound(where+": no such chat turn in a chat this scope may cite", "Cite a turn of a chat the caller can read, by its session_id and event_seq.")
				return nil, &r, nil
			}
			start, end, found := quoteSpan(text, quote, ev.CharStart, ev.CharEnd)
			if !found {
				r := errValidation(where+": evidence_quote_not_found — the quote does not occur in that turn", "Copy the text exactly from the turn.")
				return nil, &r, nil
			}
			c.start, c.end = &start, &end
		default:
			if ev.Scope == "" || ev.NaturalKey == "" {
				r := errValidation(where+": a cross-scope citation needs both scope and natural_key", "")
				return nil, &r, nil
			}
			if ev.Scope != "agent" && ev.Scope != "user" && ev.Scope != "tenant" {
				r := errValidation(where+": scope must be agent, user or tenant", "")
				return nil, &r, nil
			}
			// A reference by name into a scope this one cannot enumerate: kept as
			// written, resolved later by a tolerant reader (RFC CV decision 11).
			c.CharStart, c.CharEnd = nil, nil
		}
		out = append(out, c)
	}
	return out, nil, nil
}

// quoteSpan locates quote in text, in characters. A caller-given span must be
// exactly where the quote is; without one, the first occurrence is used.
func quoteSpan(text, quote string, wantStart, wantEnd *int) (int, int, bool) {
	runes := []rune(text)
	q := []rune(quote)
	if wantStart != nil {
		s := *wantStart
		e := s + len(q)
		if wantEnd != nil && *wantEnd != e {
			return 0, 0, false
		}
		if s < 0 || e > len(runes) || string(runes[s:e]) != quote {
			return 0, 0, false
		}
		return s, e, true
	}
	i := strings.Index(text, quote)
	if i < 0 {
		return 0, 0, false
	}
	s := utf8.RuneCountInString(text[:i])
	return s, s + len(q), true
}

// sessionTurnText returns one turn's text from a chat the caller may cite: one of
// the caller's own chats (same tenant, same user), and for a user scope the
// scope's own user. Verifying a quote against a turn answers "does this text
// occur there", so a wider rule would be an oracle over other people's chats.
// ok=false covers both "no such turn" and "not yours" — one answer, so a citation
// cannot probe for another user's chats.
func (d *Document) sessionTurnText(ctx context.Context, key sqlmem.ScopeKey, sessionID string, seq int64) (string, bool, error) {
	if d.Store == nil {
		return "", false, nil
	}
	sess, err := d.Store.GetSession(ctx, sessionID)
	if err != nil {
		var nf *store.ErrNotFound
		if asNotFound(err, &nf) {
			return "", false, nil
		}
		return "", false, err
	}
	ident := tools.RunIdentity(ctx)
	if sess.TenantID != ident.TenantID || sess.UserID != ident.UserID {
		return "", false, nil
	}
	if key.Scope == "user" && sess.UserID != key.ScopeID {
		return "", false, nil
	}
	events, err := d.Store.GetTranscript(ctx, sess.ID)
	if err != nil {
		return "", false, err
	}
	for _, ev := range events {
		if ev.Seq == seq {
			return eventText(ev.Payload), true, nil
		}
	}
	return "", false, nil
}

// addEvidence appends rows to an edge, skipping one already recorded (same source,
// same quote) and stopping at the cap. Oldest rows are kept: the first support is
// as good as the hundredth, and a cap that rotated would rewrite history.
func (d *Document) addEvidence(ctx context.Context, key sqlmem.ScopeKey, edgeID string, rows []checkedEvidence) (int, bool, error) {
	if len(rows) == 0 {
		return 0, false, nil
	}
	res, err := d.query(ctx, key, `SELECT COALESCE(chunk_id, ''), COALESCE(session_id, ''), COALESCE(event_seq, -1),
		COALESCE(ref_scope, ''), COALESCE(ref_natural_key, ''), quote FROM chunk_edge_evidence WHERE edge_id = ?`, edgeID)
	if err != nil {
		return 0, false, err
	}
	have := map[string]bool{}
	for _, r := range res.Rows {
		have[strings.Join([]string{asStr(r[0]), asStr(r[1]), strconv.Itoa(asInt(r[2])), asStr(r[3]), asStr(r[4]), asStr(r[5])}, "\x00")] = true
	}
	n, max := len(res.Rows), edgeEvidenceMax()
	written, capped := 0, false
	now := time.Now().UnixNano()
	for _, ev := range rows {
		seq := -1
		if ev.EventSeq != nil {
			seq = int(*ev.EventSeq)
		}
		k := strings.Join([]string{ev.ChunkID, ev.SessionID, strconv.Itoa(seq), ev.Scope, ev.NaturalKey, ev.Quote}, "\x00")
		if have[k] {
			continue
		}
		if n >= max {
			capped = true
			break
		}
		var eventSeq any
		if ev.EventSeq != nil {
			eventSeq = *ev.EventSeq
		}
		var start, end any
		if ev.start != nil {
			start, end = *ev.start, *ev.end
		}
		if err := d.exec(ctx, key, `INSERT INTO chunk_edge_evidence
			(id, edge_id, chunk_id, session_id, event_seq, ref_scope, ref_natural_key, quote, char_start, char_end, created_at, run_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			newDocID(), edgeID, nullIfEmpty(ev.ChunkID), nullIfEmpty(ev.SessionID), eventSeq,
			nullIfEmpty(ev.Scope), nullIfEmpty(ev.NaturalKey), ev.Quote, start, end, now, nullIfEmpty(tools.RunID(ctx))); err != nil {
			return written, capped, err
		}
		have[k] = true
		n++
		written++
	}
	return written, capped, nil
}

// statementRow is a stored statement.
type statementRow struct {
	ID              string         `json:"id"`
	FromID          string         `json:"from_id"`
	ToID            string         `json:"to_id"`
	Kind            string         `json:"kind"`
	Auto            bool           `json:"auto"`
	Status          string         `json:"status"`
	Method          string         `json:"method"`
	Confidence      *float64       `json:"confidence,omitempty"`
	ValidAt         *int64         `json:"valid_at,omitempty"`
	InvalidAt       *int64         `json:"invalid_at,omitempty"`
	ExpiredAt       *int64         `json:"expired_at,omitempty"`
	VerifiedAt      *int64         `json:"verified_at,omitempty"`
	Actor           string         `json:"actor,omitempty"`
	RunID           string         `json:"run_id,omitempty"`
	OntologyVersion *int64         `json:"ontology_version,omitempty"`
	Discriminator   string         `json:"discriminator,omitempty"`
	Attributes      map[string]any `json:"attributes,omitempty"`
	JudgedAt        *int64         `json:"judged_at,omitempty"`
	JudgeReason     string         `json:"judge_reason,omitempty"`
	JudgedBy        string         `json:"judged_by,omitempty"`
	// VerdictConfidence is the judge's (or operator's) finding; Confidence the
	// writer's own. Only the verdict withholds.
	VerdictConfidence *float64 `json:"verdict_confidence,omitempty"`
	CreatedAt         int64    `json:"created_at"`
	OperatorRuledAt   *int64   `json:"operator_ruled_at,omitempty"`
}

// statementCols is the SELECT list scanStatement reads, aliased on `e`.
const statementCols = `e.id, e.from_id, e.to_id, e.kind, e.auto, e.status, e.method, e.confidence,
	e.valid_at, e.invalid_at, e.expired_at, e.verified_at, e.actor, e.run_id, e.ontology_version,
	e.discriminator, e.attributes, e.judged_at, e.judge_reason, e.judged_by, e.created_at,
	e.verdict_confidence, e.operator_ruled_at`

const statementColCount = 23

func scanStatement(r []any) statementRow {
	s := statementRow{
		ID: asStr(r[0]), FromID: asStr(r[1]), ToID: asStr(r[2]), Kind: asStr(r[3]),
		Auto: asInt(r[4]) == 1, Status: asStr(r[5]), Method: asStr(r[6]),
		Confidence: asFloat64Ptr(r[7]), ValidAt: asInt64Ptr(r[8]), InvalidAt: asInt64Ptr(r[9]),
		ExpiredAt: asInt64Ptr(r[10]), VerifiedAt: asInt64Ptr(r[11]), Actor: asStr(r[12]), RunID: asStr(r[13]),
		OntologyVersion: asInt64Ptr(r[14]), Discriminator: asStr(r[15]),
		JudgedAt: asInt64Ptr(r[17]), JudgeReason: asStr(r[18]), JudgedBy: asStr(r[19]),
	}
	if a := asStr(r[16]); a != "" {
		_ = json.Unmarshal([]byte(a), &s.Attributes)
	}
	s.CreatedAt, _ = asInt64(r[20])
	s.VerdictConfidence = asFloat64Ptr(r[21])
	s.OperatorRuledAt = asInt64Ptr(r[22])
	if s.Status == "" {
		s.Status = edgeStatusAsserted
	}
	return s
}

// statementByKey finds the statement on the unique key, or nil.
func (d *Document) statementByKey(ctx context.Context, key sqlmem.ScopeKey, from, to, kind string, validAt *int64, disc string) (*statementRow, error) {
	var va int64
	if validAt != nil {
		va = *validAt
	}
	res, err := d.query(ctx, key, `SELECT `+statementCols+` FROM chunk_edges e
		WHERE e.from_id = ? AND e.to_id = ? AND e.kind = ? AND COALESCE(e.valid_at, 0) = ? AND e.discriminator = ?`,
		from, to, kind, va, disc)
	if err != nil || len(res.Rows) == 0 {
		return nil, err
	}
	s := scanStatement(res.Rows[0])
	return &s, nil
}

// statementByID finds one statement, or nil.
func (d *Document) statementByID(ctx context.Context, key sqlmem.ScopeKey, id string) (*statementRow, error) {
	res, err := d.query(ctx, key, `SELECT `+statementCols+` FROM chunk_edges e WHERE e.id = ?`, id)
	if err != nil || len(res.Rows) == 0 {
		return nil, err
	}
	s := scanStatement(res.Rows[0])
	return &s, nil
}

// edgeEvidenceRow is a stored evidence row.
type edgeEvidenceRow struct {
	ID         string `json:"id"`
	ChunkID    string `json:"chunk_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	EventSeq   *int64 `json:"event_seq,omitempty"`
	Scope      string `json:"scope,omitempty"`
	NaturalKey string `json:"natural_key,omitempty"`
	Quote      string `json:"quote"`
	CharStart  *int64 `json:"char_start,omitempty"`
	CharEnd    *int64 `json:"char_end,omitempty"`
	Stale      bool   `json:"stale,omitempty"`
	CreatedAt  int64  `json:"created_at"`
}

// evidenceFor returns up to limit evidence rows per edge (0 = all), oldest first,
// plus each edge's total.
func (d *Document) evidenceFor(ctx context.Context, key sqlmem.ScopeKey, edgeIDs []string, limit int) (map[string][]edgeEvidenceRow, map[string]int, error) {
	rows := map[string][]edgeEvidenceRow{}
	counts := map[string]int{}
	if len(edgeIDs) == 0 {
		return rows, counts, nil
	}
	args := make([]any, len(edgeIDs))
	for i, id := range edgeIDs {
		args[i] = id
	}
	res, err := d.query(ctx, key, `SELECT edge_id, id, chunk_id, session_id, event_seq, ref_scope, ref_natural_key,
		quote, char_start, char_end, stale, created_at
		FROM chunk_edge_evidence WHERE edge_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)
		ORDER BY edge_id, created_at, id`, args...)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range res.Rows {
		eid := asStr(r[0])
		counts[eid]++
		if limit > 0 && len(rows[eid]) >= limit {
			continue
		}
		ev := edgeEvidenceRow{
			ID: asStr(r[1]), ChunkID: asStr(r[2]), SessionID: asStr(r[3]), EventSeq: asInt64Ptr(r[4]),
			Scope: asStr(r[5]), NaturalKey: asStr(r[6]), Quote: asStr(r[7]),
			CharStart: asInt64Ptr(r[8]), CharEnd: asInt64Ptr(r[9]), Stale: asInt(r[10]) == 1,
		}
		ev.CreatedAt, _ = asInt64(r[11])
		rows[eid] = append(rows[eid], ev)
	}
	return rows, counts, nil
}

// readableEvidenceFor is evidenceFor as the caller may read it: rows citing a
// document its access-control set cannot read are withheld, and each count is of
// the rows left — a count that included them would tell the caller a source it
// cannot see exists. Without a reading limit it is evidenceFor unchanged.
func (d *Document) readableEvidenceFor(ctx context.Context, key sqlmem.ScopeKey, edgeIDs []string, limit int) (map[string][]edgeEvidenceRow, map[string]int, error) {
	limits, err := d.newDocACL(ctx, key).limitsReading(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !limits {
		return d.evidenceFor(ctx, key, edgeIDs, limit)
	}
	ev, _, err := d.evidenceFor(ctx, key, edgeIDs, 0)
	if err == nil {
		ev, err = d.withholdUnreadableEvidence(ctx, key, ev)
	}
	if err != nil {
		return nil, nil, err
	}
	counts := make(map[string]int, len(ev))
	for id, rows := range ev {
		counts[id] = len(rows)
		if limit > 0 && len(rows) > limit {
			ev[id] = rows[:limit]
		}
	}
	return ev, counts, nil
}

// statementVisibility is the withholding a reader applies to statements, the edge
// analogue of the fact surfaces' rule: a rejected statement, an unsupported
// verdict (verdict_confidence below the floor) and a retired one are withheld
// unless asked for — never deleted. NULL (never judged) stays visible, and the
// writer's own confidence never withholds.
type statementVisibility struct {
	includeProposed bool
	includeRefuted  bool
	includeRetired  bool
	asOf            *int64
}

// clause renders the visibility as SQL on alias e, with its args.
func (v statementVisibility) clause() (string, []any) {
	var parts []string
	var args []any
	if !v.includeProposed {
		parts = append(parts, "e.status <> '"+edgeStatusProposed+"'")
	}
	if !v.includeRefuted {
		parts = append(parts, "e.status <> '"+edgeStatusRejected+"'")
		parts = append(parts, withholdClause("e.verdict_confidence", false))
	}
	if !v.includeRetired {
		parts = append(parts, "e.expired_at IS NULL")
	}
	if v.asOf != nil {
		parts = append(parts, "(e.valid_at IS NULL OR e.valid_at <= ?) AND (e.invalid_at IS NULL OR e.invalid_at > ?)")
		args = append(args, *v.asOf, *v.asOf)
	}
	if len(parts) == 0 {
		return "1=1", nil
	}
	return strings.Join(parts, " AND "), args
}

// edgeLiveClause is the walk's visibility: asserted (or a status this build does
// not know, which reads as live), not rejected, not refuted, not retired.
var edgeLiveClause = func() string {
	c, _ := statementVisibility{}.clause()
	return c
}()

// edgeCursor is get_edges' paging position: the last row's (kind, created_at, id).
type edgeCursor struct {
	Kind      string `json:"k"`
	CreatedAt int64  `json:"c"`
	ID        string `json:"i"`
}

func encodeEdgeCursor(c edgeCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeEdgeCursor(s string) (edgeCursor, bool) {
	var c edgeCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(b, &c) != nil {
		return c, false
	}
	return c, true
}

// kindFilter widens kinds through the vocabulary and renders the IN clause.
func (d *Document) kindFilter(ctx context.Context, in docInput) (string, []any, []string) {
	kinds := append([]string{}, in.Kinds...)
	if in.Kind != "" {
		kinds = append(kinds, in.Kind)
	}
	if len(kinds) == 0 {
		return "", nil, nil
	}
	expanded, expandedTo := d.expandKindFilter(ctx, kinds)
	sort.Strings(expanded)
	args := make([]any, len(expanded))
	for i, k := range expanded {
		args[i] = k
	}
	return "e.kind IN (" + strings.TrimSuffix(strings.Repeat("?,", len(expanded)), ",") + ")", args, expandedTo
}

// statementJSON renders one statement for a listing: the envelope, the endpoint
// display fields the listing joined in, the inverse name as seen from the queried
// end, and an evidence preview.
func (d *Document) statementJSON(ctx context.Context, s statementRow, ends map[string]string, evidence []edgeEvidenceRow, evidenceCount int, inverseFrom bool) map[string]any {
	e := map[string]any{
		"id": s.ID, "from_id": s.FromID, "to_id": s.ToID, "kind": s.Kind, "auto": s.Auto,
		"status": s.Status, "method": s.Method, "created_at": s.CreatedAt,
	}
	if s.Confidence != nil {
		e["confidence"] = *s.Confidence
	}
	if s.VerdictConfidence != nil {
		e["verdict_confidence"] = *s.VerdictConfidence
	}
	for k, v := range map[string]*int64{"valid_at": s.ValidAt, "invalid_at": s.InvalidAt, "expired_at": s.ExpiredAt, "verified_at": s.VerifiedAt, "judged_at": s.JudgedAt, "ontology_version": s.OntologyVersion, "operator_ruled_at": s.OperatorRuledAt} {
		if v != nil {
			e[k] = *v
		}
	}
	for k, v := range map[string]string{"actor": s.Actor, "run_id": s.RunID, "discriminator": s.Discriminator, "judge_reason": s.JudgeReason, "judged_by": s.JudgedBy} {
		if v != "" {
			e[k] = v
		}
	}
	if len(s.Attributes) > 0 {
		e["attributes"] = s.Attributes
	}
	for k, v := range ends {
		if v != "" {
			e[k] = v
		}
	}
	if inverseFrom {
		if inv := d.inverseKindName(ctx, s.Kind); inv != "" {
			e["inverse_kind"] = inv
		}
	}
	if evidenceCount > 0 {
		e["evidence"] = evidence
		e["evidence_count"] = evidenceCount
	}
	return e
}

// --- ops ---

// linkChunks asserts a relation: a statement with status asserted, method manual
// (or the internal path's own), and any evidence the caller supplies.
func (d *Document) linkChunks(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	method := in.edgeMethod
	if method == "" {
		method = edgeMethodManual
	}
	res, refuse, err := d.writeStatement(ctx, key, mscope, edgeWrite{
		from: in.FromID, to: in.ToID, kind: in.Kind, method: method, status: edgeStatusAsserted,
		authored: true, confidence: in.Confidence, validAt: in.ValidAt, invalidAt: in.InvalidAt,
		attributes: in.Attributes, evidence: in.Evidence,
		// An operator's own link may re-assert a rejection; sync and canvas carry
		// another document's relations and never override one, whoever runs them.
		overrideRejected: method == edgeMethodManual && authorIsOperatorPlane(ctx),
	})
	if err != nil {
		return errFrom("link_chunks: "+err.Error(), err), nil
	}
	if refuse != nil {
		refuse.Text = "link_chunks: " + refuse.Text
		return *refuse, nil
	}
	d.publishEdgeChange(ctx, key, mscope, "link_chunks", res.ID, in.FromID, in.ToID, in.Kind, res.Status)
	out := map[string]any{"ok": true, "from_id": in.FromID, "to_id": in.ToID, "kind": in.Kind,
		"id": res.ID, "status": res.Status, "evidence_written": res.EvidenceWritten}
	if res.EvidenceCapped {
		out["evidence_capped"] = true
	}
	return jsonResult(out)
}

// unlinkChunks deletes a statement and its evidence, by id or by the exact
// triple (every statement of that triple). A hard delete stays available to a
// principal holding `link`: an operator must be able to remove a wrong link
// outright.
func (d *Document) unlinkChunks(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	var cond string
	var args []any
	var from, to, kind string
	switch {
	case in.ID != "":
		s, err := d.statementByID(ctx, key, in.ID)
		if err != nil {
			return errFrom("unlink_chunks: "+err.Error(), err), nil
		}
		if s == nil {
			return errNotFound("unlink_chunks: no such statement: "+in.ID, "List statements with op=get_edges."), nil
		}
		cond, args = "id = ?", []any{in.ID}
		from, to, kind = s.FromID, s.ToID, s.Kind
	case in.FromID != "" && in.ToID != "" && in.Kind != "":
		cond, args = "from_id = ? AND to_id = ? AND kind = ?", []any{in.FromID, in.ToID, in.Kind}
		from, to, kind = in.FromID, in.ToID, in.Kind
	default:
		return errValidation("unlink_chunks: pass id, or from_id, to_id and kind", "Pass the statement's id from op=get_edges, or the exact triple the link was made with."), nil
	}
	if err := d.deleteEdgesWhere(ctx, key, "", cond, args...); err != nil {
		return errFrom("unlink_chunks: "+err.Error(), err), nil
	}
	d.publishEdgeChange(ctx, key, mscope, "unlink_chunks", in.ID, from, to, kind, "")
	return jsonResult(map[string]any{"removed": true})
}

// proposeStatement files a statement for review: status proposed, its evidence,
// and nothing live changes. It cannot clear a status, edit or delete. Re-proposing
// an existing statement adds evidence to it; with id it files a competing proposal
// beside that statement (attributes.revision_of), so a re-verification never
// writes a duplicate.
//
// Auto-assertion is the server's decision, never the caller's: a kind the
// vocabulary marks @auto_assert, made by a rule, with every quote resolved to an
// existing chunk, is written asserted.
func (d *Document) proposeStatement(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	w := edgeWrite{
		from: in.FromID, to: in.ToID, kind: in.Kind, method: edgeMethodManual, status: edgeStatusProposed,
		confidence: in.Confidence, validAt: in.ValidAt, invalidAt: in.InvalidAt,
		attributes: in.Attributes, evidence: in.Evidence,
	}
	if in.ID != "" {
		base, err := d.statementByID(ctx, key, in.ID)
		if err != nil {
			return errFrom("propose_statement: "+err.Error(), err), nil
		}
		if base == nil {
			return errNotFound("propose_statement: no such statement: "+in.ID, "Omit id to propose a new statement."), nil
		}
		if base.Status == edgeStatusRejected {
			return errBusiness("propose_statement: statement_rejected: that relation was rejected by an operator ("+base.JudgeReason+"), so it is not revised",
				"Do not re-propose it. An operator can accept it from the review queue if it was rejected in error."), nil
		}
		w.from, w.to, w.kind, w.revisionOf = base.FromID, base.ToID, base.Kind, base.ID
		if in.Kind != "" {
			w.kind = in.Kind
		}
	}
	// A rejection is a tombstone for the relation, not for one valid_at of it: a
	// proposal that differs only in its date (or rides in as a revision) would
	// otherwise re-file what an operator already refused, as often as asked.
	if reason, rejected, err := d.rejectedRelation(ctx, key, w.from, w.to, w.kind, d.discriminatorFor(ctx, w.kind, w.attributes)); err != nil {
		return errFrom("propose_statement: "+err.Error(), err), nil
	} else if rejected {
		return errBusiness("propose_statement: statement_rejected: this relation was rejected by an operator ("+reason+")",
			"Do not re-propose it. An operator can accept it from the review queue if it was rejected in error."), nil
	}
	// Not for a competing proposal (id): asserting it would set a second live
	// statement beside the one it revises.
	if len(w.evidence) > 0 && w.revisionOf == "" && d.autoAssertKind(ctx, w.kind) && strings.HasPrefix(in.edgeMethod, "rule:") {
		allChunks := true
		for _, ev := range w.evidence {
			if ev.ChunkID == "" {
				allChunks = false
			}
		}
		if allChunks {
			w.status = edgeStatusAsserted
		}
	}
	if in.edgeMethod != "" {
		w.method = in.edgeMethod
	}
	res, refuse, err := d.writeStatement(ctx, key, mscope, w)
	if err != nil {
		return errFrom("propose_statement: "+err.Error(), err), nil
	}
	if refuse != nil {
		refuse.Text = "propose_statement: " + refuse.Text
		return *refuse, nil
	}
	d.publishEdgeChange(ctx, key, mscope, "propose_statement", res.ID, w.from, w.to, w.kind, res.Status)
	return jsonResult(res)
}

// rejectedRelation reports a rejected statement of (from, to, kind,
// discriminator) at any valid_at, with its reason.
func (d *Document) rejectedRelation(ctx context.Context, key sqlmem.ScopeKey, from, to, kind, disc string) (string, bool, error) {
	res, err := d.query(ctx, key, `SELECT COALESCE(judge_reason, '') FROM chunk_edges
		WHERE from_id = ? AND to_id = ? AND kind = ? AND discriminator = ? AND status = ? LIMIT 1`,
		from, to, kind, disc, edgeStatusRejected)
	if err != nil || len(res.Rows) == 0 {
		return "", false, err
	}
	return asStr(res.Rows[0][0]), true, nil
}

// reviewStatement is accept_statement / reject_statement: the operator's ruling on
// a proposal. Not reachable from inside a run — an agent's Document grant must not
// let a model accept its own proposals (the RFC CA posture for ontology proposals).
//
// A ruling is on a proposal (accept or reject) or on a rejection (accept — it was
// rejected in error); an asserted statement is not a ruling's to change. The
// UPDATE is conditional on the status read, so of two operators ruling at once
// the second is refused rather than silently replacing the first one's ruling.
func (d *Document) reviewStatement(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput, accept bool) (tools.Result, error) {
	op := "reject_statement"
	status := edgeStatusRejected
	if accept {
		op, status = "accept_statement", edgeStatusAsserted
	}
	if !authorIsOperatorPlane(ctx) {
		return errPermission(op+": only an operator rules on a proposed statement, from the review queue — not an agent inside a run",
			"Leave it proposed; an operator reviews it in the console."), nil
	}
	if in.ID == "" {
		return errValidation(op+": missing required field: id", "Pass the statement id from op=list_statements."), nil
	}
	if !accept && strings.TrimSpace(in.Reason) == "" {
		return errValidation(op+": reason is required", "Say why, so the proposer never re-proposes it and a later reader knows."), nil
	}
	s, err := d.statementByID(ctx, key, in.ID)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	if s == nil {
		return errNotFound(op+": no such statement: "+in.ID, "List proposals with op=list_statements."), nil
	}
	if s.Status != edgeStatusProposed && !(accept && s.Status == edgeStatusRejected) {
		msg := op + ": not_a_proposal: the statement is " + s.Status + ", so there is nothing to rule on"
		hint := "Rule on proposals from op=list_statements. To withdraw a relation that holds, judge it unsupported (judge_fact) or remove it (unlink_chunks)."
		if s.Auto {
			// Its body re-creates it on every write, so the body is where it goes.
			msg += " — it comes from a [[name]] link in its source chunk's body"
			hint = "Edit the [[name]] link out of the body (update_chunk) to remove it."
		}
		return errBusiness(msg, hint), nil
	}
	now := time.Now().UnixNano()
	reason := nullIfEmpty(strings.TrimSpace(in.Reason))
	// operator_ruled_at is set by every ruling, whatever else it records: it is
	// what tells a run's maintenance this statement is a human decision.
	set, args := `status = ?, operator_ruled_at = ?, judged_at = ?, judge_reason = ?, judged_by = ?`, []any{status, now, now, reason, "operator"}
	if accept {
		refuting := s.VerdictConfidence != nil && *s.VerdictConfidence < withholdBelowConfidence
		switch {
		case refuting:
			// The operator overrules the judge: an accepted statement is visible.
			set += `, verdict_confidence = NULL`
		case s.Status == edgeStatusRejected || s.JudgedAt == nil:
			// The triplet held the rejection being reversed, or nothing.
		default:
			// A judge's standing verdict on the proposal is kept: accepting it adds
			// nothing the verdict does not already say.
			set, args = `status = ?, operator_ruled_at = ?`, []any{status, now}
		}
	}
	args = append(args, in.ID, s.Status)
	ruled, err := d.SqlMem.Exec(ctx, key, d.SqlMem.Rebind(`UPDATE chunk_edges SET `+set+` WHERE id = ? AND status = ?`), args, 0)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	if ruled.RowsAffected == 0 {
		return errBusiness(op+": statement_changed: the statement was ruled on or changed while this ruling was made",
			"Reload it (op=get_edge) and rule again if it still needs it."), nil
	}
	d.publishEdgeChange(ctx, key, mscope, op, in.ID, s.FromID, s.ToID, s.Kind, status)
	return jsonResult(map[string]any{"id": in.ID, "status": status})
}

// getEdge returns one statement with all its evidence.
func (d *Document) getEdge(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errValidation("get_edge: missing required field: id", "Pass a statement id from op=get_edges or op=backlinks."), nil
	}
	res, err := d.query(ctx, key, `SELECT `+statementCols+`,
		cf.title, cf.document_id, ct.title, ct.document_id
		FROM chunk_edges e LEFT JOIN chunks cf ON cf.id = e.from_id LEFT JOIN chunks ct ON ct.id = e.to_id
		WHERE e.id = ?`, in.ID)
	if err != nil {
		return errFrom("get_edge: "+err.Error(), err), nil
	}
	if len(res.Rows) == 0 {
		return errNotFound("get_edge: no such statement: "+in.ID, "List statements with op=get_edges."), nil
	}
	r := res.Rows[0]
	s := scanStatement(r)
	ev, counts, err := d.readableEvidenceFor(ctx, key, []string{s.ID}, 0)
	if err != nil {
		return errFrom("get_edge: "+err.Error(), err), nil
	}
	ends := map[string]string{
		"from_title": asStr(r[statementColCount]), "from_document_id": asStr(r[statementColCount+1]),
		"to_title": asStr(r[statementColCount+2]), "to_document_id": asStr(r[statementColCount+3]),
	}
	out := d.statementJSON(ctx, s, ends, ev[s.ID], counts[s.ID], false)
	if _, ok := out["evidence"]; !ok {
		out["evidence"] = []edgeEvidenceRow{}
		out["evidence_count"] = 0
	}
	return jsonResult(map[string]any{"statement": out})
}

// listStatements is the review queue and a maintainer's selection: statements of
// a status (default proposed) across the scope, narrowed by the selection
// filters, paged newest first (order created) or least recently verified first
// (order verified). A retired statement — expired, or already ended — is left
// out unless include_retired asks for it.
func (d *Document) listStatements(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	status := in.Status
	if status == "" {
		status = edgeStatusProposed
	}
	limit := edgeGetEdgesDefault
	if in.Limit > 0 {
		limit = min(in.Limit, edgeGetEdgesMax)
	}
	order := in.Order
	if order == "" {
		order = statementOrderCreated
	}
	if order != statementOrderCreated && order != statementOrderVerified {
		return errValidation("list_statements: order must be created or verified", "Omit order for newest first."), nil
	}
	where := []string{"e.status = ?"}
	args := []any{status}
	if kc, ka, _ := d.kindFilter(ctx, in); kc != "" {
		where = append(where, kc)
		args = append(args, ka...)
	}
	if !in.IncludeRetired {
		// One-sided on purpose: a statement dated to take effect later is not
		// retired, and a proposal for a coming edition belongs in the queue.
		where = append(where, "e.expired_at IS NULL AND (e.invalid_at IS NULL OR e.invalid_at > ?)")
		args = append(args, time.Now().UnixNano())
	}
	sel, selArgs, none, refusal, err := d.statementSelection(ctx, key, in)
	if err != nil {
		return errFrom("list_statements: "+err.Error(), err), nil
	}
	if refusal != nil {
		return *refusal, nil
	}
	if none {
		return jsonResult(map[string]any{"statements": []any{}})
	}
	where = append(where, sel...)
	args = append(args, selArgs...)
	if in.After != "" {
		c, ok := decodeStatementCursor(in.After)
		if !ok || c.Order != order {
			return errValidation("list_statements: after is not a cursor this op returned for order "+order,
				"Pass next_cursor from the previous page, with the same order."), nil
		}
		cond, cargs := c.after()
		where = append(where, cond)
		args = append(args, cargs...)
	}
	orderBy := "e.created_at DESC, e.id DESC"
	if order == statementOrderVerified {
		// Spelled out rather than NULLS FIRST: the tiers sort NULL at opposite ends.
		orderBy = "CASE WHEN e.verified_at IS NULL THEN 0 ELSE 1 END, e.verified_at, e.id"
	}
	args = append(args, limit+1)
	res, err := d.query(ctx, key, `SELECT `+statementCols+`,
		cf.title, cf.document_id, ct.title, ct.document_id
		FROM chunk_edges e LEFT JOIN chunks cf ON cf.id = e.from_id LEFT JOIN chunks ct ON ct.id = e.to_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY `+orderBy+` LIMIT ?`, args...)
	if err != nil {
		return errFrom("list_statements: "+err.Error(), err), nil
	}
	return d.renderStatementPage(ctx, key, res, limit, "statements", func(s statementRow) string {
		c := statementCursor{Order: order, ID: s.ID}
		if order == statementOrderVerified {
			c.VerifiedAt = s.VerifiedAt
		} else {
			c.CreatedAt = s.CreatedAt
		}
		return c.encode()
	})
}

const (
	statementOrderCreated  = "created"
	statementOrderVerified = "verified"
	// listStatementsMaxDocs bounds each document list list_statements binds: the
	// documents an under_path names, and a confined reader's readable endpoint
	// documents. Each list is bound twice (one per end), and SQLite refuses a
	// statement with more than 32766 variables: 4 x 7500 leaves room for the rest.
	listStatementsMaxDocs = 7500
	// statementMethodPrefixMax bounds method_prefix; a method is a short label.
	statementMethodPrefixMax = 256
)

// statementSelection renders list_statements' selection filters as conditions
// on alias e (cf and ct are its endpoint chunks). none reports a filter that can
// match nothing the caller may read: a document it may not read is a document it
// selects nothing by, so neither the rows nor a page marker can tell it
// statements there exist. The listing's own access-control filter still runs on
// the answer.
func (d *Document) statementSelection(ctx context.Context, key sqlmem.ScopeKey, in docInput) (where []string, args []any, none bool, refusal *tools.Result, err error) {
	acl := d.newDocACL(ctx, key)
	limited, err := acl.limitsReading(ctx)
	if err != nil {
		return nil, nil, false, nil, err
	}
	readable := func(docID string) (bool, error) {
		if !limited {
			return true, nil
		}
		return acl.readable(ctx, docID)
	}
	if limited {
		// Withhold in the query, before LIMIT: rows dropped after it would shrink
		// the page and leave truncated / next_cursor standing on rows the reader
		// cannot see — telling it hidden statements match its filter. The list is
		// an allowlist of readable documents, as search sends, so no id of a denied
		// document reaches the store and no refusal counts one. An end whose chunk
		// is gone has no document and stays visible, as the listing filter has it.
		docs, err := d.readableEndpointDocuments(ctx, key, acl)
		if err != nil {
			return nil, nil, false, nil, err
		}
		if len(docs) > listStatementsMaxDocs {
			r := errBusiness(fmt.Sprintf("list_statements: relations here touch %d documents you may read, more than %d", len(docs), listStatementsMaxDocs),
				"Select by document_id or under_path.")
			return nil, nil, false, &r, nil
		}
		ph := placeholders(len(docs))
		where = append(where, "(cf.document_id IS NULL OR cf.document_id IN ("+ph+")) AND (ct.document_id IS NULL OR ct.document_id IN ("+ph+"))")
		args = append(append(args, docs...), docs...)
	}
	if in.DocumentID != "" {
		ok, err := readable(in.DocumentID)
		if err != nil || !ok {
			return nil, nil, !ok, nil, err
		}
		where = append(where, "(cf.document_id = ? OR ct.document_id = ?)")
		args = append(args, in.DocumentID, in.DocumentID)
	}
	if in.EvidenceDocumentID != "" {
		ok, err := readable(in.EvidenceDocumentID)
		if err != nil || !ok {
			return nil, nil, !ok, nil, err
		}
		where = append(where, `EXISTS (SELECT 1 FROM chunk_edge_evidence v JOIN chunks vc ON vc.id = v.chunk_id
			WHERE v.edge_id = e.id AND vc.document_id = ?)`)
		args = append(args, in.EvidenceDocumentID)
	}
	if in.MethodPrefix != "" {
		if len(in.MethodPrefix) > statementMethodPrefixMax {
			r := errValidation(fmt.Sprintf("list_statements: method_prefix is longer than %d bytes", statementMethodPrefixMax), "")
			return nil, nil, false, &r, nil
		}
		// substr, not LIKE: a prefix holding _ or % must match only itself, and
		// SQLite's LIKE ignores ASCII case. Both tiers count characters.
		where = append(where, "substr(e.method, 1, ?) = ?")
		args = append(args, utf8.RuneCountInString(in.MethodPrefix), in.MethodPrefix)
	}
	if in.UnderPath != "" {
		canonical, perr := normalizePath(in.UnderPath)
		if perr != nil {
			r := errValidation("list_statements: "+perr.Error(), docPathFix)
			return nil, nil, false, &r, nil
		}
		ids, err := d.documentsAtOrUnder(ctx, key, canonical)
		if err != nil {
			return nil, nil, false, nil, err
		}
		docs := make([]any, 0, len(ids))
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			ok, err := readable(id)
			if err != nil {
				return nil, nil, false, nil, err
			}
			if ok {
				docs = append(docs, id)
			}
		}
		if len(docs) == 0 {
			return nil, nil, true, nil, nil
		}
		if len(docs) > listStatementsMaxDocs {
			r := errValidation(fmt.Sprintf("list_statements: under_path names %d documents, more than %d", len(docs), listStatementsMaxDocs),
				"Narrow under_path, or select by document_id.")
			return nil, nil, false, &r, nil
		}
		ph := placeholders(len(docs))
		where = append(where, "(cf.document_id IN ("+ph+") OR ct.document_id IN ("+ph+"))")
		args = append(append(args, docs...), docs...)
	}
	return where, args, false, nil, nil
}

// readableEndpointDocuments lists, in pages under the store's row cap, the
// documents holding an end of some statement that the caller may read.
func (d *Document) readableEndpointDocuments(ctx context.Context, key sqlmem.ScopeKey, acl *docACL) ([]any, error) {
	var docs []any
	after := ""
	for {
		res, err := d.query(ctx, key, `SELECT DISTINCT c.document_id FROM chunks c
			WHERE c.document_id > ? AND (c.id IN (SELECT from_id FROM chunk_edges) OR c.id IN (SELECT to_id FROM chunk_edges))
			ORDER BY c.document_id LIMIT ?`, after, searchFilterPage)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Rows {
			id := asStr(r[0])
			ok, err := acl.readable(ctx, id)
			if err != nil {
				return nil, err
			}
			if ok {
				docs = append(docs, id)
			}
			after = id
		}
		// Truncated: the store's row cap cut the page short of searchFilterPage.
		if len(res.Rows) == 0 || (!res.Truncated && len(res.Rows) < searchFilterPage) {
			return docs, nil
		}
	}
}

// statementCursor is list_statements' paging position. It names the order that
// issued it, so a cursor is never read as a position in the other sort.
type statementCursor struct {
	Order      string `json:"o"`
	CreatedAt  int64  `json:"c,omitempty"`
	VerifiedAt *int64 `json:"v,omitempty"`
	ID         string `json:"i"`
}

func (c statementCursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeStatementCursor(s string) (statementCursor, bool) {
	var c statementCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(b, &c) != nil || c.ID == "" {
		return c, false
	}
	return c, true
}

// after is the keyset condition for the rows past c in c's order.
func (c statementCursor) after() (string, []any) {
	if c.Order == statementOrderCreated {
		return "(e.created_at < ? OR (e.created_at = ? AND e.id < ?))", []any{c.CreatedAt, c.CreatedAt, c.ID}
	}
	if c.VerifiedAt == nil {
		return "((e.verified_at IS NULL AND e.id > ?) OR e.verified_at IS NOT NULL)", []any{c.ID}
	}
	return "(e.verified_at IS NOT NULL AND (e.verified_at > ? OR (e.verified_at = ? AND e.id > ?)))",
		[]any{*c.VerifiedAt, *c.VerifiedAt, c.ID}
}

// renderStatementPage turns a statement query (statementCols + the four endpoint
// display columns, fetched limit+1) into a page with an evidence preview.
func (d *Document) renderStatementPage(ctx context.Context, key sqlmem.ScopeKey, res *sqlmem.QueryResult, limit int, field string, cursorOf func(statementRow) string) (tools.Result, error) {
	rows := res.Rows
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	stmts := make([]statementRow, len(rows))
	ids := make([]string, len(rows))
	for i, r := range rows {
		stmts[i] = scanStatement(r)
		ids[i] = stmts[i].ID
	}
	ev, counts, err := d.readableEvidenceFor(ctx, key, ids, edgeEvidencePreview)
	if err != nil {
		return errFrom(err.Error(), err), nil
	}
	list := make([]map[string]any, 0, len(rows))
	for i, r := range rows {
		ends := map[string]string{
			"from_title": asStr(r[statementColCount]), "from_document_id": asStr(r[statementColCount+1]),
			"to_title": asStr(r[statementColCount+2]), "to_document_id": asStr(r[statementColCount+3]),
		}
		list = append(list, d.statementJSON(ctx, stmts[i], ends, ev[stmts[i].ID], counts[stmts[i].ID], false))
	}
	out := map[string]any{field: list}
	if truncated && len(stmts) > 0 {
		out["truncated"] = true
		out["next_cursor"] = cursorOf(stmts[len(stmts)-1])
	}
	return jsonResult(out)
}

// judgeEdge is judge_fact on a statement (D3): the fact tier's verdict vocabulary
// and withholding rule. The evidence is what the judge checks; a statement with
// none can be vouched for by an operator, never by a run — the fact tier's rule.
func (d *Document) judgeEdge(ctx context.Context, key sqlmem.ScopeKey, s *statementRow, verdict string, confidence float64, reason string) (tools.Result, error) {
	judgedBy := judgedByForVerdict(ctx)
	if verdict != "unclear" && !(judgedBy == "operator" && verdict != "mistyped") {
		// Only evidence this scope verified counts: a reference into another scope
		// is kept as written, never checked, so it is no ground for a verdict.
		res, err := d.query(ctx, key, `SELECT COUNT(*) FROM chunk_edge_evidence
			WHERE edge_id = ? AND (chunk_id IS NOT NULL OR session_id IS NOT NULL)`, s.ID)
		if err != nil {
			return errFrom("judge_fact: "+err.Error(), err), nil
		}
		if len(res.Rows) == 0 || asInt(res.Rows[0][0]) == 0 {
			return errBusiness("judge_fact: that statement records no evidence verified in this scope, so there is nothing to check it against. "+
				"An operator may still vouch for it from the console — a run may not", ""), nil
		}
	}
	now := time.Now().UnixNano()
	if err := d.exec(ctx, key, `UPDATE chunk_edges SET verdict_confidence = ?, judged_at = ?, judge_reason = ?, judged_by = ?, verified_at = ? WHERE id = ?`,
		confidence, now, reason, judgedBy, now, s.ID); err != nil {
		return errFrom("judge_fact: "+err.Error(), err), nil
	}
	return jsonResult(map[string]any{
		"edge_id": s.ID, "verdict": verdict, "confidence": confidence,
		"judged_at": now, "withheld": confidence < withholdBelowConfidence,
	})
}

// publishEdgeChange tells both endpoint documents' change channels.
func (d *Document) publishEdgeChange(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, op, edgeID, from, to, kind, status string) {
	if d.Bus == nil {
		return
	}
	docs := map[string]bool{}
	for _, id := range []string{from, to} {
		if row, ok, err := d.getChunkRow(ctx, key, id); err == nil && ok {
			docs[row.DocumentID] = true
		}
	}
	scopeID := docMemScopeID(mscope, key)
	for docID := range docs {
		d.publishEdgeEvent(ctx, mscope, scopeID, docID, map[string]any{
			"op": op, "edge_id": edgeID, "kind": kind, "status": status, "actor": tools.RunIdentity(ctx).UserID,
		})
	}
}

// --- export / import carry ---

// mdEdgeEnvelope is a statement's envelope in the export_md trailer. Present only
// when it carries something beyond a plain asserted link, so a document whose
// edges have none exports byte-identically to before statements existed.
type mdEdgeEnvelope struct {
	Status     string           `json:"status,omitempty"`
	Confidence *float64         `json:"confidence,omitempty"`
	ValidAt    *int64           `json:"valid_at,omitempty"`
	InvalidAt  *int64           `json:"invalid_at,omitempty"`
	Attributes map[string]any   `json:"attributes,omitempty"`
	Evidence   []edgeEvidenceIn `json:"evidence,omitempty"`
}

// exportEdgeLines renders the trailer: the statements leaving this document's
// chunks, then the ones arriving from outside it whose source carries a natural
// key — written as `@<natural_key>` so an import can reconnect them, which is what
// keeps an inbound citation through an export round trip. Rejected statements are
// a tombstone for this scope and are not carried.
func (d *Document) exportEdgeLines(ctx context.Context, key sqlmem.ScopeKey, docID string) ([]string, error) {
	res, err := d.query(ctx, key, `SELECT `+statementCols+`, '' AS nk FROM chunk_edges e
		WHERE e.from_id IN (SELECT id FROM chunks WHERE document_id = ?) AND e.status <> ?
		ORDER BY e.from_id, e.to_id, e.kind, e.id`, docID, edgeStatusRejected)
	if err != nil {
		return nil, err
	}
	inb, err := d.query(ctx, key, `SELECT `+statementCols+`, m.natural_key FROM chunk_edges e
		JOIN chunk_memory_meta m ON m.chunk_id = e.from_id
		WHERE e.to_id IN (SELECT id FROM chunks WHERE document_id = ?)
		  AND e.from_id NOT IN (SELECT id FROM chunks WHERE document_id = ?)
		  AND m.natural_key IS NOT NULL AND m.natural_key <> '' AND e.status <> ?
		ORDER BY m.natural_key, e.to_id, e.kind, e.id`, docID, docID, edgeStatusRejected)
	if err != nil {
		return nil, err
	}
	rows := append(append([][]any{}, res.Rows...), inb.Rows...)
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = asStr(r[0])
	}
	ev, _, err := d.evidenceFor(ctx, key, ids, 0)
	if err == nil {
		ev, err = d.withholdUnreadableEvidence(ctx, key, ev)
	}
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		s := scanStatement(r)
		from := s.FromID
		if nk := asStr(r[statementColCount]); nk != "" {
			// An inbound source in a document the caller may not read is left out:
			// its natural key and kind would describe that document.
			if ok, err := d.chunkReadable(ctx, key, s.FromID); err != nil {
				return nil, err
			} else if !ok {
				continue
			}
			from = "@" + nk
		}
		line := from + " -> " + s.ToID + " [" + s.Kind + "]"
		env := mdEdgeEnvelope{Confidence: s.Confidence, ValidAt: s.ValidAt, InvalidAt: s.InvalidAt, Attributes: s.Attributes}
		if s.Status != edgeStatusAsserted {
			env.Status = s.Status
		}
		for _, e := range ev[s.ID] {
			env.Evidence = append(env.Evidence, edgeEvidenceIn{
				ChunkID: e.ChunkID, SessionID: e.SessionID, EventSeq: e.EventSeq,
				Scope: e.Scope, NaturalKey: e.NaturalKey, Quote: e.Quote,
			})
		}
		if env.Status != "" || env.Confidence != nil || env.ValidAt != nil || env.InvalidAt != nil || len(env.Attributes) > 0 || len(env.Evidence) > 0 {
			b, _ := json.Marshal(env)
			line += " " + string(b)
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// importEdges writes the trailer's statements into the imported document and
// returns how many it could not place. Endpoints are remapped from the exported
// ids; an evidence row is kept only when its quote still verifies (its chunk
// remapped too, when it was part of the document). A statement keeps `proposed`
// when it was exported proposed and is otherwise asserted: an import is made with
// the importer's own link authority, and can lower a status but never raise one.
func (d *Document) importEdges(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, edges []mdEdge, oldToNew map[string]string) int {
	skipped := 0
	for _, e := range edges {
		nt, okTo := oldToNew[e.to]
		var nf string
		okFrom := false
		if strings.HasPrefix(e.from, "@") {
			// The source is an existing chunk the trailer names by key: linking from
			// it needs the caller's `link` on its document, as link_chunks would ask.
			if id, err := d.chunkIDByNaturalKey(ctx, key, strings.TrimPrefix(e.from, "@")); err == nil && id != "" {
				if acl := d.newDocACL(ctx, key); acl != nil {
					if r, err := acl.chunk(ctx, "import_md", "link", id); err != nil || r != nil {
						skipped++
						continue
					}
				}
				nf, okFrom = id, true
			}
		} else {
			nf, okFrom = oldToNew[e.from]
		}
		if !okFrom || !okTo || (isReservedEdgeKind(e.kind) && e.kind != aboutEdgeKind) {
			skipped++
			continue
		}
		w := edgeWrite{from: nf, to: nt, kind: e.kind, method: edgeMethodImport, status: edgeStatusAsserted, authored: true}
		if e.env != nil {
			if e.env.Status == edgeStatusRejected {
				skipped++
				continue
			}
			if e.env.Status == edgeStatusProposed {
				w.status, w.authored = edgeStatusProposed, false
			}
			w.confidence, w.validAt, w.invalidAt, w.attributes = e.env.Confidence, e.env.ValidAt, e.env.InvalidAt, e.env.Attributes
			for _, ev := range e.env.Evidence {
				if ev.ChunkID != "" {
					if mapped, ok := oldToNew[ev.ChunkID]; ok {
						ev.ChunkID = mapped
					}
				}
				if _, refuse, err := d.checkEvidence(ctx, key, mscope, []edgeEvidenceIn{ev}); err == nil && refuse == nil {
					w.evidence = append(w.evidence, ev)
				}
			}
		}
		if _, refuse, err := d.writeStatement(ctx, key, mscope, w); err != nil || refuse != nil {
			skipped++
		}
	}
	return skipped
}

// chunkReadable reports whether the caller's access-control set lets it read the
// document holding chunkID. A missing chunk is not readable.
func (d *Document) chunkReadable(ctx context.Context, key sqlmem.ScopeKey, chunkID string) (bool, error) {
	row, ok, err := d.getChunkRow(ctx, key, chunkID)
	if err != nil || !ok {
		return false, err
	}
	acl := d.newDocACL(ctx, key)
	if acl == nil {
		return true, nil
	}
	return acl.readable(ctx, row.DocumentID)
}
