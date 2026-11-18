package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
)

// Source names where the data is. Nothing here is a credential: DSNs come from
// the environment or flags and are never written to the output.
type Source struct {
	Tenant string
	// DocSchema is the scope's document schema; "" finds it in sqlmem_meta.
	DocSchema string
	// SchemeRoot is the scheme's root chunk id.
	SchemeRoot string
	// SubjectKinds are the statement kinds that place a document on a concept.
	SubjectKinds []string
	// BodyScopeID is the memory scope_id holding the scope's chunk bodies.
	BodyScopeID string
	// VectorScopeID is the memory scope_id holding annotation vectors.
	VectorScopeID string
	Model         string
}

// The runtime's statement visibility (internal/tools/builtin, statementVisibility
// with no options): not proposed, not rejected, not withheld by a verdict below
// the floor, not retired. Restated, not imported: tools/builtin is the runtime,
// and this program reads a restored dump with no runtime around it.
const (
	withholdBelowConfidence = 0.25 // document_verdict.go withholdBelowConfidence
	chunkBodyKeyPrefix      = "doc.chunk:"
	annotationKeyPrefix     = "doc.annotation:"
)

// Load reads one scope's documents, the scheme, its statements, annotations and
// vectors. main is the runtime database (memory, memory_embeddings); sqlmem the
// SQL Memory database.
func Load(ctx context.Context, mainDB, sqlDB *pgx.Conn, src Source) (*Data, error) {
	d := &Data{Docs: map[string]*Doc{}, Counts: map[string]int{}, NotLiveDocs: map[string]bool{}}
	schema := src.DocSchema
	if schema == "" {
		var err error
		if schema, err = findDocSchema(ctx, sqlDB, src.Tenant); err != nil {
			return nil, err
		}
	}
	t := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }

	chunkDoc, err := loadDocs(ctx, sqlDB, t, d)
	if err != nil {
		return nil, err
	}
	if err := loadConcepts(ctx, mainDB, sqlDB, t, src, d); err != nil {
		return nil, err
	}
	if err := loadStatements(ctx, sqlDB, t, src, chunkDoc, d); err != nil {
		return nil, err
	}
	if err := loadAnnotations(ctx, sqlDB, t, chunkDoc, d); err != nil {
		return nil, err
	}
	if err := loadVectors(ctx, mainDB, src, chunkDoc, d); err != nil {
		return nil, err
	}
	if err := loadProvenance(ctx, mainDB, src, d); err != nil {
		return nil, err
	}
	return d, nil
}

// findDocSchema resolves the tenant scope's schema; a split scope keeps its
// documents in the sqlmem_d_ twin of its sqlmem_s_ schema.
func findDocSchema(ctx context.Context, db *pgx.Conn, tenant string) (string, error) {
	var s string
	err := db.QueryRow(ctx, `SELECT schema_name FROM sqlmem_meta.scope_registry
		WHERE tenant = $1 AND scope = 'tenant'`, tenant).Scan(&s)
	if err != nil {
		return "", fmt.Errorf("find tenant scope schema: %w", err)
	}
	var split bool
	err = db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sqlmem_meta.scope_split WHERE schema_name = $1)`, s).Scan(&split)
	if err != nil {
		return "", fmt.Errorf("read scope_split: %w", err)
	}
	if split {
		s = "sqlmem_d_" + strings.TrimPrefix(s, "sqlmem_s_")
	}
	return s, nil
}

type chunkRef struct {
	doc  string
	root bool
}

func loadDocs(ctx context.Context, db *pgx.Conn, t func(string) string, d *Data) (map[string]chunkRef, error) {
	rows, err := db.Query(ctx, `SELECT d.id, d.title, d.root_chunk_id, d.created_at, c.valid_at
		FROM `+t("documents")+` d LEFT JOIN `+t("chunks")+` c ON c.id = d.root_chunk_id`)
	if err != nil {
		return nil, fmt.Errorf("documents: %w", err)
	}
	for rows.Next() {
		doc := &Doc{}
		if err := rows.Scan(&doc.ID, &doc.Title, &doc.RootChunk, &doc.CreatedAt, &doc.RootValidAt); err != nil {
			rows.Close()
			return nil, err
		}
		d.Docs[doc.ID] = doc
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	d.Counts["documents"] = len(d.Docs)

	// A scope older than document roles has no such table; then nothing is
	// excluded as the ontology and the report says so.
	var roleDocs []string
	r2, err := db.Query(ctx, `SELECT document_id FROM `+t("document_roles")+` WHERE role = 'ontology'`)
	if err == nil {
		roleDocs, err = pgx.CollectRows(r2, pgx.RowTo[string])
	}
	if err != nil {
		d.Notes = append(d.Notes, "document_roles not readable, no ontology document excluded: "+err.Error())
	}
	for _, id := range roleDocs {
		if doc := d.Docs[id]; doc != nil {
			doc.Ontology = true
		}
	}

	chunkDoc := map[string]chunkRef{}
	rows, err = db.Query(ctx, `SELECT id, document_id, parent_id IS NULL FROM `+t("chunks"))
	if err != nil {
		return nil, fmt.Errorf("chunks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var ref chunkRef
		if err := rows.Scan(&id, &ref.doc, &ref.root); err != nil {
			return nil, err
		}
		chunkDoc[id] = ref
	}
	d.Counts["chunks"] = len(chunkDoc)
	return chunkDoc, rows.Err()
}

// loadConcepts reads the tree under the scheme root; a concept's name is its
// chunk title and its meaning and alt labels come from its body, parsed as the
// runtime parses an ontology concept.
func loadConcepts(ctx context.Context, mainDB, db *pgx.Conn, t func(string) string, src Source, d *Data) error {
	rows, err := db.Query(ctx, `WITH RECURSIVE tree AS (
			SELECT id, parent_id, position, title FROM `+t("chunks")+` WHERE parent_id = $1
			UNION ALL
			SELECT c.id, c.parent_id, c.position, c.title FROM `+t("chunks")+` c JOIN tree ON c.parent_id = tree.id)
		SELECT id, parent_id, position, title FROM tree`, src.SchemeRoot)
	if err != nil {
		return fmt.Errorf("concepts: %w", err)
	}
	var keys []string
	for rows.Next() {
		var c Concept
		if err := rows.Scan(&c.ID, &c.Parent, &c.Position, &c.Name); err != nil {
			rows.Close()
			return err
		}
		if c.Parent == src.SchemeRoot {
			c.Parent = ""
		}
		d.Concepts = append(d.Concepts, c)
		keys = append(keys, chunkBodyKeyPrefix+c.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(d.Concepts) == 0 {
		return fmt.Errorf("scheme root %s has no concepts", src.SchemeRoot)
	}
	bodies, err := readBodies(ctx, mainDB, src, keys)
	if err != nil {
		return err
	}
	for i := range d.Concepts {
		c := &d.Concepts[i]
		pc := memory.ParseConceptBody("", c.Name, []string{c.Name}, bodies[chunkBodyKeyPrefix+c.ID].body)
		c.Meaning, c.Alt = pc.Meaning, pc.Alt
	}
	d.Counts["concepts"] = len(d.Concepts)
	return nil
}

type bodyRow struct {
	body            string
	sourceUpdatedAt string
	hasDateField    bool
}

// readBodies reads live chunk-body rows by key, with the import provenance's
// source_updated_at when the body carries one.
func readBodies(ctx context.Context, db *pgx.Conn, src Source, keys []string) (map[string]bodyRow, error) {
	rows, err := db.Query(ctx, `SELECT key, COALESCE(value->>'body', ''),
			COALESCE(value->'fields'->'provenance'->>'source_updated_at', ''),
			COALESCE(jsonb_typeof(value->'fields') = 'object' AND value->'fields' ? 'date', false)
		FROM memory WHERE tenant_id = $1 AND scope = 'tenant' AND scope_id = $2 AND key = ANY($3)
		  AND (expires_at IS NULL OR expires_at > now()) AND superseded_at IS NULL`,
		src.Tenant, src.BodyScopeID, keys)
	if err != nil {
		return nil, fmt.Errorf("chunk bodies: %w", err)
	}
	defer rows.Close()
	out := map[string]bodyRow{}
	for rows.Next() {
		var k string
		var b bodyRow
		if err := rows.Scan(&k, &b.body, &b.sourceUpdatedAt, &b.hasDateField); err != nil {
			return nil, err
		}
		out[k] = b
	}
	return out, rows.Err()
}

func loadStatements(ctx context.Context, db *pgx.Conn, t func(string) string, src Source, chunkDoc map[string]chunkRef, d *Data) error {
	ids := make([]string, len(d.Concepts))
	for i, c := range d.Concepts {
		ids[i] = c.ID
	}
	rows, err := db.Query(ctx, `SELECT e.id, e.from_id, e.to_id, e.kind, e.method, e.status, COALESCE(e.attributes, ''),
			(e.status <> 'proposed' AND e.status <> 'rejected' AND e.expired_at IS NULL
			 AND (e.verdict_confidence IS NULL OR e.verdict_confidence >= $2)) AS live
		FROM `+t("chunk_edges")+` e WHERE e.to_id = ANY($1)`, ids, withholdBelowConfidence)
	if err != nil {
		return fmt.Errorf("statements: %w", err)
	}
	defer rows.Close()
	kinds := map[string]bool{}
	for _, k := range src.SubjectKinds {
		kinds[k] = true
	}
	for rows.Next() {
		var s Statement
		var from, method, status, attrs string
		var live bool
		if err := rows.Scan(&s.ID, &from, &s.ConceptID, &s.Kind, &method, &status, &attrs, &live); err != nil {
			return err
		}
		if !kinds[s.Kind] {
			d.Counts["statements_other_kind:"+s.Kind]++
			continue
		}
		ref, ok := chunkDoc[from]
		if !ok {
			d.Counts["statements_from_unknown_chunk"]++
			continue
		}
		if !live {
			d.Counts["statements_not_live:"+status]++
			d.NotLiveDocs[ref.doc] = true
			continue
		}
		if !ref.root {
			d.Counts["statements_from_non_root_chunk"]++
		}
		s.DocID = ref.doc
		s.Provenance = provenance(method, attrs)
		d.Statements = append(d.Statements, s)
	}
	d.Counts["statements_live"] = len(d.Statements)
	return rows.Err()
}

// provenance names how a statement was made, from its method column and the
// attributes the classifier and judge leave: method/classified_by[+fallback]/
// judge verdict ("-" when unjudged).
func provenance(method, attrs string) string {
	var a struct {
		ClassifiedBy    string `json:"classified_by"`
		ConceptFallback bool   `json:"concept_fallback"`
		Judge           *struct {
			Verdict string `json:"verdict"`
		} `json:"judge"`
	}
	_ = json.Unmarshal([]byte(attrs), &a) // absent or malformed attributes read as none
	by := a.ClassifiedBy
	if by == "" {
		by = "-"
	}
	if a.ConceptFallback {
		by += "+fallback"
	}
	v := "-"
	if a.Judge != nil && a.Judge.Verdict != "" {
		v = a.Judge.Verdict
	}
	return method + "/" + by + "/judge:" + v
}

func loadAnnotations(ctx context.Context, db *pgx.Conn, t func(string) string, chunkDoc map[string]chunkRef, d *Data) error {
	rows, err := db.Query(ctx, `SELECT chunk_id, prompt_version, brief, abstract FROM `+t("chunk_annotations")+
		` WHERE level = 'document'`)
	if err != nil {
		return fmt.Errorf("annotations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var chunk, brief, abstract string
		var pv int
		if err := rows.Scan(&chunk, &pv, &brief, &abstract); err != nil {
			return err
		}
		ref, ok := chunkDoc[chunk]
		doc := d.Docs[ref.doc]
		if !ok || !ref.root || doc == nil {
			d.Counts["annotations_document_level_not_on_a_root"]++
			continue
		}
		doc.HasAnnotation, doc.PromptVersion, doc.Brief, doc.Abstract = true, pv, brief, abstract
		d.Counts["annotations_document_level:v"+strconv.Itoa(pv)]++
	}
	return rows.Err()
}

// loadVectors reads the live annotation vectors, joined to memory for the same
// liveness rule the runtime's MemoryEmbedListVectors applies.
func loadVectors(ctx context.Context, db *pgx.Conn, src Source, chunkDoc map[string]chunkRef, d *Data) error {
	rows, err := db.Query(ctx, `SELECT me.key, me.model, me.embedding::text
		FROM memory_embeddings me
		JOIN memory m ON m.tenant_id = me.tenant_id AND m.scope = me.scope
		 AND m.scope_id = me.scope_id AND m.key = me.key
		WHERE me.tenant_id = $1 AND me.scope = 'tenant' AND me.scope_id = $2 AND me.key LIKE $3
		  AND (m.expires_at IS NULL OR m.expires_at > now()) AND m.superseded_at IS NULL
		ORDER BY me.key`, src.Tenant, src.VectorScopeID, annotationKeyPrefix+"%")
	if err != nil {
		return fmt.Errorf("vectors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, model, text string
		if err := rows.Scan(&key, &model, &text); err != nil {
			return err
		}
		if model != src.Model {
			d.Counts["vectors_other_model"]++
			continue
		}
		chunk, level, ok := splitAnnotationKey(key)
		ref, known := chunkDoc[chunk]
		doc := d.Docs[ref.doc]
		if !ok || !known || doc == nil {
			d.Counts["vectors_unknown_chunk"]++
			continue
		}
		vec, err := parsePgvector(text)
		if err != nil {
			return fmt.Errorf("vector %s: %w", key, err)
		}
		u, err := unitVec(vec)
		if err != nil {
			d.Counts["vectors_invalid"]++
			continue
		}
		switch {
		case level == "document" && ref.root:
			doc.DocVec, doc.DocUnit = vec, u
			d.Counts["vectors_document"]++
		case level == "branch" && !ref.root:
			doc.Sections = append(doc.Sections, vec)
			doc.SecUnits = append(doc.SecUnits, u)
			d.Counts["vectors_branch"]++
		default:
			d.Counts["vectors_skipped:"+level]++
		}
	}
	return rows.Err()
}

// splitAnnotationKey reads doc.annotation:<chunk_id>:<level>.
func splitAnnotationKey(key string) (chunk, level string, ok bool) {
	rest, found := strings.CutPrefix(key, annotationKeyPrefix)
	if !found {
		return "", "", false
	}
	i := strings.LastIndexByte(rest, ':')
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// parsePgvector reads pgvector's text form, "[1,2.5,-3e-05]".
func parsePgvector(s string) (nodestats.Vector, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("not a vector literal")
	}
	parts := strings.Split(s[1:len(s)-1], ",")
	out := make(nodestats.Vector, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("component %d: %w", i, err)
		}
		out[i] = float32(f)
	}
	return out, nil
}

func loadProvenance(ctx context.Context, db *pgx.Conn, src Source, d *Data) error {
	keys := make([]string, 0, len(d.Docs))
	for _, doc := range d.Docs {
		keys = append(keys, chunkBodyKeyPrefix+doc.RootChunk)
	}
	bodies, err := readBodies(ctx, db, src, keys)
	if err != nil {
		return err
	}
	for _, doc := range d.Docs {
		b, ok := bodies[chunkBodyKeyPrefix+doc.RootChunk]
		if !ok {
			d.Counts["root_bodies_missing"]++
			continue
		}
		doc.SourceUpdatedAt = b.sourceUpdatedAt
		// A root field named date is not part of the RFC's dating rule; it is
		// counted so the report can say how many documents carry one anyway.
		if b.hasDateField {
			d.Counts["root_chunks_with_a_date_field"]++
		}
	}
	return nil
}
