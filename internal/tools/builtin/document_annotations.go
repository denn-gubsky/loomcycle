package builtin

// document_annotations.go — the annotation layer of a knowledge base.
//
// After import, classification and linking, nothing above a section says what it
// is about: an agent learns what a document holds by reading it. An annotation is
// two short texts a model writes ABOUT a part of the tree — a brief (one sentence,
// for choosing) and an abstract (one paragraph, standing in for reading) — at the
// level of a section branch or of a whole document (Path folders come later).
//
// An annotation is NOT content. It lives in its own table beside chunks, outside
// the body, the content hash, history, diff, sync and export, so writing one never
// changes what the document says. Each record carries the hash of the inputs it
// was written from, and the RUNTIME owns that hash (annotationInputsFor): a
// driver asks for the inputs, sends them to a model, and writes back with the
// hash it was given; set_annotation refuses the write when the inputs moved under
// it. A record whose inputs changed since is stale — still served, marked, never
// withheld — until the scheduled walk rewrites it. A record an operator wrote by
// hand (manual) is never overwritten by the walk.
//
// The hash covers what the text was written from and nothing above it: a
// branch's own body, title, class, language and prompt version, and for each
// child either the child's own record or, when the child has none, its body. A
// change below a branch therefore changes the branch's hash only through its
// children, so staleness climbs exactly as far as the text changed.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/lang"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// chunkAnnotationsDDL is one record per (chunk, level). Per-scope like every
// document table; no foreign key, the cascade is explicit in Go with the other
// chunk sidecars.
const chunkAnnotationsDDL = `CREATE TABLE IF NOT EXISTS chunk_annotations (
		chunk_id TEXT NOT NULL, level TEXT NOT NULL,
		brief TEXT NOT NULL, abstract TEXT NOT NULL, language TEXT NOT NULL,
		class TEXT, ontology_version INTEGER,
		inputs_sha256 TEXT NOT NULL, prompt_version INTEGER NOT NULL, model TEXT,
		manual INTEGER NOT NULL DEFAULT 0,
		written_at BIGINT NOT NULL, written_by TEXT NOT NULL,
		PRIMARY KEY (chunk_id, level))`

const (
	annotationLevelBranch   = "branch"
	annotationLevelDocument = "document"
	annotationLevelPath     = "path"

	// annotationGrant is the statement_methods value that lets a code-js driver
	// write annotations. It stamps nothing on any statement.
	annotationGrant = "annotate"
	// annotatorLeaseName is the one lease name the grant may hold.
	annotatorLeaseName = "annotator"

	maxAnnotationBriefChars    = 240
	maxAnnotationAbstractChars = 2000
	maxAnnotationModelChars    = 128

	defaultAnnotationBodyChars = 6000
	maxAnnotationBodyChars     = 20000
	// A child with no record of its own is shown to the model by its text, so
	// the parent's annotation covers it; these bound what all of them add.
	annotationChildBodyChars     = 1500
	annotationChildrenBodyBudget = 12000
	maxAnnotationRelations       = 20
	// annotationLanguageSample is how much of a document the language guess reads.
	annotationLanguageSample = 4000
	// annotationFallbackLanguage is the language of a document whose text does
	// not say which one it is in.
	annotationFallbackLanguage = "en"
	// annotationCanonVersion is the version of the hashed form itself: a change
	// to what the hash covers bumps it, which makes every record stale on purpose.
	annotationCanonVersion = 1

	defaultListAnnotations = 50
	maxListAnnotations     = 500
	// maxSummarizeStaleChecks bounds how many records list_annotations
	// summarize recomputes the inputs of.
	maxSummarizeStaleChecks = 2000
)

var annotationLanguageRE = regexp.MustCompile(`^[a-z]{2,3}$`)

// annotationRecord is one stored annotation as the ops return it.
type annotationRecord struct {
	ChunkID         string `json:"chunk_id"`
	DocumentID      string `json:"document_id"`
	Level           string `json:"level"`
	Brief           string `json:"brief"`
	Abstract        string `json:"abstract"`
	Language        string `json:"language"`
	Class           string `json:"class,omitempty"`
	OntologyVersion *int64 `json:"ontology_version,omitempty"`
	InputsSHA256    string `json:"inputs_sha256"`
	PromptVersion   int    `json:"prompt_version"`
	Model           string `json:"model,omitempty"`
	Manual          bool   `json:"manual"`
	WrittenAt       string `json:"written_at"`
	WrittenBy       string `json:"written_by"`
	// Folder is the Path folder a path record annotates.
	Folder string `json:"folder,omitempty"`
	// Stale is computed on read: the current inputs hash differs from the stored
	// one. Absent where the read did not compute it.
	Stale *bool `json:"stale,omitempty"`

	writtenAtNs int64
	// covers is the documents a path record was written from: the caller must be
	// able to read every one of them to be shown it.
	covers []string
}

const annotationSelectCols = `a.chunk_id, c.document_id, a.level, a.brief, a.abstract, a.language, a.class,
	a.ontology_version, a.inputs_sha256, a.prompt_version, a.model, a.manual, a.written_at, a.written_by,
	a.folder, a.covers`

// annotationFrom is the FROM clause every record read uses: a record whose chunk
// is gone is an orphan and is never returned.
const annotationFrom = ` FROM chunk_annotations a JOIN chunks c ON c.id = a.chunk_id`

func scanAnnotationRecord(r []any) annotationRecord {
	rec := annotationRecord{
		ChunkID: asStr(r[0]), DocumentID: asStr(r[1]), Level: asStr(r[2]),
		Brief: asStr(r[3]), Abstract: asStr(r[4]), Language: asStr(r[5]), Class: asStr(r[6]),
		InputsSHA256: asStr(r[8]), PromptVersion: asInt(r[9]), Model: asStr(r[10]),
		Manual: asInt(r[11]) != 0, writtenAtNs: int64(asInt(r[12])), WrittenBy: asStr(r[13]),
	}
	if r[7] != nil {
		v := int64(asInt(r[7]))
		rec.OntologyVersion = &v
	}
	rec.WrittenAt = time.Unix(0, rec.writtenAtNs).UTC().Format(time.RFC3339)
	rec.Folder = asStr(r[14])
	if c := asStr(r[15]); c != "" {
		rec.covers = parseCovers(c)
	}
	return rec
}

// annotationRecordsFor loads the records of chunkIDs at the given levels (all
// levels when none), keyed chunk_id → level.
func (d *Document) annotationRecordsFor(ctx context.Context, key sqlmem.ScopeKey, chunkIDs []string, levels ...string) (map[string]map[string]annotationRecord, error) {
	out := map[string]map[string]annotationRecord{}
	for start := 0; start < len(chunkIDs); start += 500 {
		end := min(start+500, len(chunkIDs))
		ph, args := inPlaceholders(chunkIDs[start:end])
		q := `SELECT ` + annotationSelectCols + annotationFrom + ` WHERE a.chunk_id IN (` + ph + `)`
		if len(levels) > 0 {
			lph, largs := inPlaceholders(levels)
			q += ` AND a.level IN (` + lph + `)`
			args = append(args, largs...)
		}
		res, err := d.query(ctx, key, q, args...)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Rows {
			rec := scanAnnotationRecord(r)
			if out[rec.ChunkID] == nil {
				out[rec.ChunkID] = map[string]annotationRecord{}
			}
			out[rec.ChunkID][rec.Level] = rec
		}
	}
	return out, nil
}

// --- the inputs ---

// annotationChild is one child of a target as the model sees it: its own record
// when it has one, else its text.
type annotationChild struct {
	ChunkID     string `json:"chunk_id"`
	Title       string `json:"title"`
	Type        string `json:"type,omitempty"`
	HasChildren bool   `json:"has_children,omitempty"`
	BodyChars   int    `json:"body_chars"`
	Brief       string `json:"brief,omitempty"`
	Abstract    string `json:"abstract,omitempty"`
	// Body is a record-less child's text, capped; BodyTruncated says the cap cut it.
	Body          string `json:"body,omitempty"`
	BodyTruncated bool   `json:"body_truncated,omitempty"`
	Stale         bool   `json:"stale,omitempty"`
}

// annotationRelation is one kind of asserted statement from the document to
// other documents, by the titles of those documents.
type annotationRelation struct {
	Kind   string   `json:"kind"`
	Count  int      `json:"count"`
	Titles []string `json:"titles"`
}

// annotationAbove is the annotation one level up, so the model knows what a
// reader already knows. Context only: it is not part of the hash, or a parent
// and its children would each go stale whenever the other was rewritten.
type annotationAbove struct {
	Level    string `json:"level"`
	Brief    string `json:"brief,omitempty"`
	Abstract string `json:"abstract,omitempty"`
}

// annotationInputs is everything a target's annotation is written from, and the
// hash of it.
type annotationInputs struct {
	ChunkID       string               `json:"chunk_id"`
	DocumentID    string               `json:"document_id"`
	Level         string               `json:"level"`
	Class         string               `json:"class,omitempty"`
	Title         string               `json:"title"`
	DocumentTitle string               `json:"document_title"`
	Place         []string             `json:"place"`
	Path          string               `json:"path,omitempty"`
	Identity      map[string]string    `json:"identity,omitempty"`
	Body          string               `json:"body"`
	BodyChars     int                  `json:"body_chars"`
	BodyTruncated bool                 `json:"body_truncated,omitempty"`
	Children      []annotationChild    `json:"children"`
	Relations     []annotationRelation `json:"relations,omitempty"`
	Above         *annotationAbove     `json:"above,omitempty"`
	Language      string               `json:"language"`
	PromptVersion int                  `json:"prompt_version"`
	InputsSHA256  string               `json:"inputs_sha256"`
	Record        *annotationRecord    `json:"record,omitempty"`
	Stale         bool                 `json:"stale"`
}

// annotationCanon is the hashed form. JSON of a struct is deterministic, and
// every list in it is in a fixed order.
type annotationCanon struct {
	V             int      `json:"v"`
	Level         string   `json:"level"`
	Title         string   `json:"title"`
	Class         string   `json:"class"`
	Language      string   `json:"language"`
	PromptVersion int      `json:"prompt_version"`
	BodySHA       string   `json:"body_sha"`
	Identity      []string `json:"identity,omitempty"`
	Children      []string `json:"children"`
	Relations     []string `json:"relations,omitempty"`
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// annotationTarget is a resolved (chunk, level).
type annotationTarget struct {
	chunk    chunkRow
	level    string
	docTitle string
	docType  string
	rootID   string
}

// annotationScope reads the records, documents and languages one call needs
// once, however many targets the call touches.
type annotationScope struct {
	d      *Document
	key    sqlmem.ScopeKey
	mscope store.MemoryScope
	docs   map[string]annDoc
	langs  map[string]string
	bodies map[string]chunkBody
}

type annDoc struct {
	title, root, typ string
}

func (d *Document) newAnnotationScope(key sqlmem.ScopeKey, mscope store.MemoryScope) *annotationScope {
	return &annotationScope{d: d, key: key, mscope: mscope, docs: map[string]annDoc{},
		langs: map[string]string{}, bodies: map[string]chunkBody{}}
}

func (s *annotationScope) doc(ctx context.Context, id string) (annDoc, bool, error) {
	if m, ok := s.docs[id]; ok {
		return m, true, nil
	}
	res, err := s.d.query(ctx, s.key, `SELECT title, root_chunk_id, type FROM documents WHERE id = ?`, id)
	if err != nil {
		return annDoc{}, false, err
	}
	if len(res.Rows) == 0 {
		return annDoc{}, false, nil
	}
	m := annDoc{title: asStr(res.Rows[0][0]), root: asStr(res.Rows[0][1]), typ: asStr(res.Rows[0][2])}
	s.docs[id] = m
	return m, true, nil
}

func (s *annotationScope) body(ctx context.Context, chunkID string) (chunkBody, error) {
	if b, ok := s.bodies[chunkID]; ok {
		return b, nil
	}
	b, err := s.d.readBody(ctx, s.mscope, s.key, chunkID)
	if err != nil {
		return chunkBody{}, err
	}
	s.bodies[chunkID] = b
	return b, nil
}

// language is a document's language: guessed from its root body and the text of
// its first top-level sections.
func (s *annotationScope) language(ctx context.Context, docID, rootID string) (string, error) {
	if l, ok := s.langs[docID]; ok {
		return l, nil
	}
	var sample strings.Builder
	add := func(chunkID string) error {
		b, err := s.body(ctx, chunkID)
		if err != nil {
			return err
		}
		sample.WriteString(b.Body)
		sample.WriteString("\n")
		return nil
	}
	if err := add(rootID); err != nil {
		return "", err
	}
	if sample.Len() < annotationLanguageSample {
		res, err := s.d.query(ctx, s.key, `SELECT id FROM chunks WHERE parent_id = ? ORDER BY position, id LIMIT 5`, rootID)
		if err != nil {
			return "", err
		}
		for _, r := range res.Rows {
			if sample.Len() >= annotationLanguageSample {
				break
			}
			if err := add(asStr(r[0])); err != nil {
				return "", err
			}
		}
	}
	l := lang.DetectOr(truncateRunes(sample.String(), annotationLanguageSample), annotationFallbackLanguage)
	s.langs[docID] = l
	return l, nil
}

// inputs computes what target's annotation is written from, and its hash under
// promptVersion. bodyChars caps the target's own body in the result (never the
// hash, which covers the whole body).
//
// full=false computes only what the hash needs (a staleness check): no place,
// no path and no annotation from above, none of which the hash covers.
func (s *annotationScope) inputs(ctx context.Context, t annotationTarget, promptVersion, bodyChars int, acl *docACL, full bool) (annotationInputs, error) {
	d, key := s.d, s.key
	in := annotationInputs{ChunkID: t.chunk.ID, DocumentID: t.chunk.DocumentID, Level: t.level,
		Class: t.docType, Title: t.chunk.Title, DocumentTitle: t.docTitle, PromptVersion: promptVersion,
		Children: []annotationChild{}}
	in.Place = []string{}
	if titles, _, ok := d.chunkLineage(ctx, key, t.chunk.ID); full && ok && len(titles) > 1 {
		in.Place = titles[:len(titles)-1]
	}
	if full {
		in.Path = d.annotationDocumentPath(ctx, key, t.chunk.DocumentID)
	}
	own, err := s.body(ctx, t.chunk.ID)
	if err != nil {
		return in, err
	}
	in.BodyChars = utf8.RuneCountInString(own.Body)
	in.Body = truncateRunes(own.Body, bodyChars)
	in.BodyTruncated = in.BodyChars > bodyChars
	if in.Language, err = s.language(ctx, t.chunk.DocumentID, t.rootID); err != nil {
		return in, err
	}
	canon := annotationCanon{V: annotationCanonVersion, Level: t.level, Title: t.chunk.Title, Class: t.docType,
		Language: in.Language, PromptVersion: promptVersion, BodySHA: sha256Hex(own.Body), Children: []string{}}

	// The children, in order: each one's own branch record, else its text.
	cres, err := d.query(ctx, key, `SELECT c.id, c.title, c.type,
		(SELECT COUNT(*) FROM chunks g WHERE g.parent_id = c.id) AS kids
		FROM chunks c WHERE c.parent_id = ? ORDER BY c.position, c.id`, t.chunk.ID)
	if err != nil {
		return in, err
	}
	var childIDs []string
	for _, r := range cres.Rows {
		childIDs = append(childIDs, asStr(r[0]))
	}
	childRecs, err := d.annotationRecordsFor(ctx, key, childIDs, annotationLevelBranch)
	if err != nil {
		return in, err
	}
	budget := annotationChildrenBodyBudget
	for _, r := range cres.Rows {
		ch := annotationChild{ChunkID: asStr(r[0]), Title: asStr(r[1]), Type: asStr(r[2]), HasChildren: asInt(r[3]) > 0}
		b, err := s.body(ctx, ch.ChunkID)
		if err != nil {
			return in, err
		}
		ch.BodyChars = utf8.RuneCountInString(b.Body)
		if rec, ok := childRecs[ch.ChunkID][annotationLevelBranch]; ok {
			ch.Brief, ch.Abstract = rec.Brief, rec.Abstract
			canon.Children = append(canon.Children, "r:"+sha256Hex(ch.Title+"\x00"+rec.Brief+"\x00"+rec.Abstract))
		} else {
			take := min(annotationChildBodyChars, max(budget, 0))
			ch.Body = truncateRunes(b.Body, take)
			ch.BodyTruncated = ch.BodyChars > take
			budget -= utf8.RuneCountInString(ch.Body)
			canon.Children = append(canon.Children, "b:"+sha256Hex(ch.Title+"\x00"+b.Body))
		}
		in.Children = append(in.Children, ch)
	}

	switch t.level {
	case annotationLevelDocument:
		in.Identity = d.annotationIdentity(ctx, key, t.chunk.ID, own)
		for _, k := range sortedAnnotationKeys(in.Identity) {
			canon.Identity = append(canon.Identity, k+"="+in.Identity[k])
		}
		if in.Relations, canon.Relations, err = s.relations(ctx, t.chunk.DocumentID, acl); err != nil {
			return in, err
		}
	case annotationLevelBranch:
		if !full {
			break
		}
		// What the level above already says, nearest first: the parent's branch
		// record, else the document's.
		if recs, err := d.annotationRecordsFor(ctx, key, []string{t.chunk.ParentID, t.rootID}); err == nil {
			if rec, ok := recs[t.chunk.ParentID][annotationLevelBranch]; ok {
				in.Above = &annotationAbove{Level: annotationLevelBranch, Abstract: rec.Abstract}
			} else if rec, ok := recs[t.rootID][annotationLevelDocument]; ok {
				in.Above = &annotationAbove{Level: annotationLevelDocument, Brief: rec.Brief}
			}
		}
	}
	raw, _ := json.Marshal(canon)
	in.InputsSHA256 = sha256Hex(string(raw))
	return in, nil
}

// annotationIdentityFields are the root fields that say WHICH work a document
// is, shown to the model as identity: never a field that changes on its own
// (a timestamp, a provenance block, a colour).
var annotationIdentityFields = []string{"identity_key", "work_key", "letter", "code", "issuer", "edition_date"}

func (d *Document) annotationIdentity(ctx context.Context, key sqlmem.ScopeKey, rootID string, root chunkBody) map[string]string {
	out := map[string]string{}
	if nk := d.naturalKeyOf(ctx, key, rootID); nk != "" {
		out["natural_key"] = nk
	}
	var fields map[string]any
	if len(root.Fields) > 0 && json.Unmarshal(root.Fields, &fields) == nil {
		for _, f := range annotationIdentityFields {
			if v, ok := fields[f].(string); ok && strings.TrimSpace(v) != "" {
				out[f] = truncateRunes(strings.TrimSpace(v), 120)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// relations is the document's asserted, live statements to OTHER documents,
// grouped by kind, each kind with the titles of the documents it reaches.
//
// It returns two views. canon covers every statement whoever asks, because the
// hash must not depend on the caller; shown leaves out each document the caller
// may not read.
func (s *annotationScope) relations(ctx context.Context, docID string, acl *docACL) (shown []annotationRelation, canon []string, err error) {
	res, err := s.d.query(ctx, s.key, `SELECT e.kind, dt.id, dt.title FROM chunk_edges e
		JOIN chunks cf ON cf.id = e.from_id
		JOIN chunks ct ON ct.id = e.to_id
		JOIN documents dt ON dt.id = ct.document_id
		WHERE cf.document_id = ? AND ct.document_id <> ? AND e.status = 'asserted' AND e.expired_at IS NULL
		ORDER BY e.kind, dt.title, dt.id`, docID, docID)
	if err != nil {
		return nil, nil, err
	}
	limited := false
	if acl != nil {
		if limited, err = acl.limitsReading(ctx); err != nil {
			return nil, nil, err
		}
	}
	type rel struct{ kind, id, title string }
	var all []rel
	seen := map[string]bool{}
	for _, r := range res.Rows {
		x := rel{asStr(r[0]), asStr(r[1]), asStr(r[2])}
		if !seen[x.kind+"\x00"+x.id] {
			seen[x.kind+"\x00"+x.id] = true
			all = append(all, x)
		}
	}
	group := func(keep func(rel) (bool, error)) ([]annotationRelation, error) {
		byKind := map[string]*annotationRelation{}
		var kinds []string
		titles := 0
		for _, x := range all {
			ok, err := keep(x)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			g := byKind[x.kind]
			if g == nil {
				g = &annotationRelation{Kind: x.kind, Titles: []string{}}
				byKind[x.kind] = g
				kinds = append(kinds, x.kind)
			}
			g.Count++
			if titles < maxAnnotationRelations {
				g.Titles = append(g.Titles, x.title)
				titles++
			}
		}
		out := make([]annotationRelation, 0, len(kinds))
		for _, k := range kinds {
			out = append(out, *byKind[k])
		}
		return out, nil
	}
	every, _ := group(func(rel) (bool, error) { return true, nil })
	for _, g := range every {
		canon = append(canon, g.Kind+":"+strconv.Itoa(g.Count)+":"+strings.Join(g.Titles, "\x00"))
	}
	if !limited {
		shown = every
	} else if shown, err = group(func(x rel) (bool, error) { return acl.readable(ctx, x.id) }); err != nil {
		return nil, nil, err
	}
	if len(shown) == 0 {
		shown = nil
	}
	return shown, canon, nil
}

// stale reports whether rec's inputs changed since it was written, computed
// under the prompt version it was written with (a newer prompt is the driver's
// to know about, through annotation_inputs).
func (s *annotationScope) stale(ctx context.Context, rec annotationRecord) (bool, error) {
	if rec.Level == annotationLevelPath {
		if rec.Folder == "" {
			return true, nil
		}
		in, err := s.folderInputsFor(ctx, rec.Folder, rec.PromptVersion, nil)
		if err != nil {
			return false, err
		}
		return in.InputsSHA256 != rec.InputsSHA256, nil
	}
	t, ok, err := s.targetOf(ctx, rec.ChunkID, rec.Level)
	if err != nil || !ok {
		return false, err
	}
	in, err := s.inputs(ctx, t, rec.PromptVersion, 0, nil, false)
	if err != nil {
		return false, err
	}
	return in.InputsSHA256 != rec.InputsSHA256, nil
}

// targetOf resolves (chunk, level) without validating the level against the
// chunk's place; callers that accept input validate first.
func (s *annotationScope) targetOf(ctx context.Context, chunkID, level string) (annotationTarget, bool, error) {
	row, ok, err := s.d.getChunkRow(ctx, s.key, chunkID)
	if err != nil || !ok {
		return annotationTarget{}, ok, err
	}
	doc, ok, err := s.doc(ctx, row.DocumentID)
	if err != nil || !ok {
		return annotationTarget{}, ok, err
	}
	return annotationTarget{chunk: row, level: level, docTitle: doc.title, docType: doc.typ, rootID: doc.root}, true, nil
}

// resolveAnnotationTarget reads the target an op names: `id` as a chunk (a
// branch, or a document's root), else the document (document_id | path) as its
// root. level defaults to what the chunk is.
func (d *Document) resolveAnnotationTarget(ctx context.Context, s *annotationScope, op string, in docInput, needLevel bool) (annotationTarget, *tools.Result) {
	fail := func(r tools.Result) (annotationTarget, *tools.Result) { return annotationTarget{}, &r }
	chunkID := in.ID
	if chunkID == "" {
		if in.DocumentID == "" && in.Path == "" {
			return fail(errValidation(op+": missing required field: id (a chunk) or document_id (or path)",
				"Pass the chunk id of a section, or the document_id of a document."))
		}
		docID, err := d.docIDFromInput(ctx, s.key, docInput{DocumentID: in.DocumentID, Path: in.Path})
		if err != nil {
			return fail(docFail(op+": ", err))
		}
		doc, ok, err := s.doc(ctx, docID)
		if err != nil {
			return fail(errFrom(op+": "+err.Error(), err))
		}
		if !ok {
			return fail(errNotFound(op+": no such document: "+docID, "Find the document with op=query_documents in the same scope."))
		}
		chunkID = doc.root
	}
	level := in.Level
	switch level {
	case "", annotationLevelBranch, annotationLevelDocument:
	case annotationLevelPath:
		if in.ID == "" {
			return fail(errValidation(op+": a folder's annotation is addressed by its path with level path", ""))
		}
	default:
		return fail(errValidation(op+": level must be branch, document or path", ""))
	}
	t, ok, err := s.targetOf(ctx, chunkID, level)
	if err != nil {
		return fail(errFrom(op+": "+err.Error(), err))
	}
	if !ok {
		return fail(errNotFound(op+": no such chunk: "+chunkID, "Pass a chunk id from get_chunk or search in the same scope."))
	}
	isRoot := t.chunk.ID == t.rootID
	if t.level == "" {
		if !needLevel {
			return t, nil
		}
		t.level = annotationLevelBranch
		if isRoot {
			t.level = annotationLevelDocument
		}
	}
	if t.level == annotationLevelPath && !isRoot {
		return fail(errValidation(op+": level path is kept on a document's root chunk; this chunk is a section", ""))
	}
	if t.level == annotationLevelDocument && !isRoot {
		return fail(errValidation(op+": level document is written on a document's root chunk; this chunk is a section",
			"Pass level branch for a section, or the document_id for the document."))
	}
	if t.level == annotationLevelBranch && isRoot {
		return fail(errValidation(op+": this chunk is the document's root; its annotation has level document", "Pass level document."))
	}
	return t, nil
}

// --- permission ---

// annotationWriteAllowed: the operator plane, or a run holding the annotate grant.
func annotationWriteAllowed(ctx context.Context) bool {
	return tools.IsOperatorPlane(ctx) || hasAnnotateGrant(ctx)
}

func hasAnnotateGrant(ctx context.Context) bool {
	return contains(tools.PolicyOf(ctx).StatementMethods, annotationGrant)
}

// annotationTenantOnly refuses an annotation write outside the tenant scope:
// the knowledge base is the tenant's, and a user's or an agent's notes are not
// annotated.
func annotationTenantOnly(op string, key sqlmem.ScopeKey) *tools.Result {
	if key.Scope == string(store.MemoryScopeTenant) {
		return nil
	}
	r := errValidation(op+": annotations are kept for the tenant knowledge base only", `Pass scope "tenant".`)
	return &r
}

func annotationWriter(ctx context.Context) string {
	if id := tools.RunID(ctx); id != "" {
		return "run:" + id
	}
	if p, ok := auth.PrincipalFromContext(ctx); ok && p.Subject != "" {
		return "principal:" + p.Subject
	}
	return "operator"
}

// --- ops ---

func (d *Document) annotationInputsOp(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	const op = "annotation_inputs"
	if !annotationWriteAllowed(ctx) {
		return errPermission(op+": only the operator or an annotating driver reads annotation inputs",
			"Run it from the console, or from a code-js agent granted statement_methods: [annotate]."), nil
	}
	if r := annotationTenantOnly(op, key); r != nil {
		return *r, nil
	}
	if in.PromptVersion < 0 {
		return errValidation(op+": prompt_version must be 0 or more", ""), nil
	}
	if in.Level == annotationLevelPath {
		return d.folderAnnotationInputs(ctx, key, mscope, in)
	}
	bodyChars := 0
	if in.BodyChars != nil {
		bodyChars = *in.BodyChars
	}
	switch {
	case bodyChars < 0:
		return errValidation(op+": body_chars must be 0 or more", ""), nil
	case bodyChars == 0:
		bodyChars = defaultAnnotationBodyChars
	case bodyChars > maxAnnotationBodyChars:
		bodyChars = maxAnnotationBodyChars
	}
	s := d.newAnnotationScope(key, mscope)
	t, refused := d.resolveAnnotationTarget(ctx, s, op, in, true)
	if refused != nil {
		return *refused, nil
	}
	got, err := s.inputs(ctx, t, in.PromptVersion, bodyChars, d.newDocACL(ctx, key), true)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	recs, err := d.annotationRecordsFor(ctx, key, []string{t.chunk.ID}, t.level)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	got.Stale = true
	if rec, ok := recs[t.chunk.ID][t.level]; ok {
		stale := rec.InputsSHA256 != got.InputsSHA256
		rec.Stale = &stale
		got.Record, got.Stale = &rec, stale
	}
	return jsonResult(got)
}

func (d *Document) setAnnotation(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	const op = "set_annotation"
	if !annotationWriteAllowed(ctx) {
		return errPermission(op+": only the operator or an annotating driver writes annotations",
			"Write it from the console, or from a code-js agent granted statement_methods: [annotate]."), nil
	}
	if r := annotationTenantOnly(op, key); r != nil {
		return *r, nil
	}
	brief, abstract := strings.TrimSpace(in.Brief), strings.TrimSpace(in.Abstract)
	switch {
	case brief == "" || abstract == "":
		return errValidation(op+": brief and abstract are both required", ""), nil
	case utf8.RuneCountInString(brief) > maxAnnotationBriefChars:
		return errValidation(fmt.Sprintf("%s: brief is %d characters, at most %d", op, utf8.RuneCountInString(brief), maxAnnotationBriefChars),
			"Shorten the brief to one sentence."), nil
	case utf8.RuneCountInString(abstract) > maxAnnotationAbstractChars:
		return errValidation(fmt.Sprintf("%s: abstract is %d characters, at most %d", op, utf8.RuneCountInString(abstract), maxAnnotationAbstractChars),
			"Shorten the abstract to one paragraph."), nil
	case in.Language != "" && !annotationLanguageRE.MatchString(in.Language):
		return errValidation(op+": language must be a two- or three-letter lower-case code, e.g. en", ""), nil
	case utf8.RuneCountInString(in.Model) > maxAnnotationModelChars:
		return errValidation(fmt.Sprintf("%s: model is longer than %d characters", op, maxAnnotationModelChars), ""), nil
	case in.PromptVersion < 0:
		return errValidation(op+": prompt_version must be 0 or more", ""), nil
	case in.InputsSHA256 == "" && !in.Manual:
		return errValidation(op+": missing required field: inputs_sha256",
			"Pass the inputs_sha256 annotation_inputs returned for this target, or manual: true for an operator's own text."), nil
	case in.Manual && !tools.IsOperatorPlane(ctx):
		return errPermission(op+": manual annotations are written by the operator", "Omit manual."), nil
	}
	if in.Level == annotationLevelPath {
		return d.setFolderAnnotation(ctx, key, mscope, in, brief, abstract)
	}
	s := d.newAnnotationScope(key, mscope)
	t, refused := d.resolveAnnotationTarget(ctx, s, op, in, true)
	if refused != nil {
		return *refused, nil
	}
	if t.level == annotationLevelPath {
		return errValidation(op+": a folder's annotation is written by its path with level path", ""), nil
	}
	cur, err := s.inputs(ctx, t, in.PromptVersion, 0, nil, false)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	if in.InputsSHA256 != "" && in.InputsSHA256 != cur.InputsSHA256 {
		return errBusiness(op+": annotation_inputs_changed: the target's inputs changed since they were read",
			"Read them again with annotation_inputs and write an annotation of the current text."), nil
	}
	existing, err := d.annotationRecordsFor(ctx, key, []string{t.chunk.ID}, t.level)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	if prev, ok := existing[t.chunk.ID][t.level]; ok && prev.Manual && !in.Manual {
		return errBusiness(op+": manual_record: an operator wrote this annotation; it is not overwritten",
			"Leave it, or ask an operator to delete it first."), nil
	}
	language := in.Language
	if language == "" {
		language = cur.Language
	}
	manual := 0
	if in.Manual {
		manual = 1
	}
	now := time.Now().UnixNano()
	var class any
	if t.docType != "" {
		class = t.docType
	}
	var model any
	if in.Model != "" {
		model = in.Model
	}
	if err := d.exec(ctx, key, `INSERT INTO chunk_annotations (chunk_id, level, brief, abstract, language, class,
		ontology_version, inputs_sha256, prompt_version, model, manual, written_at, written_by)
		VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (chunk_id, level) DO UPDATE SET brief = excluded.brief, abstract = excluded.abstract,
		language = excluded.language, class = excluded.class, ontology_version = excluded.ontology_version,
		inputs_sha256 = excluded.inputs_sha256, prompt_version = excluded.prompt_version, model = excluded.model,
		manual = excluded.manual, written_at = excluded.written_at, written_by = excluded.written_by`,
		t.chunk.ID, t.level, brief, abstract, language, class, cur.InputsSHA256, in.PromptVersion, model, manual, now, annotationWriter(ctx)); err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	recs, err := d.annotationRecordsFor(ctx, key, []string{t.chunk.ID}, t.level)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	rec := recs[t.chunk.ID][t.level]
	notStale := false
	rec.Stale = &notStale
	return jsonResult(map[string]any{"annotation": rec})
}

func (d *Document) getAnnotation(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	const op = "get_annotation"
	if in.Level == annotationLevelPath && in.ID == "" {
		root, refused := d.folderRecordChunk(ctx, key, op, in.Path)
		if refused != nil {
			return *refused, nil
		}
		if root == "" {
			return jsonResult(map[string]any{"annotations": []any{}})
		}
		in.ID, in.Path = root, ""
	}
	s := d.newAnnotationScope(key, mscope)
	acl := d.newDocACL(ctx, key)
	t, refused := d.resolveAnnotationTarget(ctx, s, op, in, false)
	if refused != nil {
		return *refused, nil
	}
	var levels []string
	if t.level != "" {
		levels = []string{t.level}
	}
	recs, err := d.annotationRecordsFor(ctx, key, []string{t.chunk.ID}, levels...)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	out := []annotationRecord{}
	for _, lvl := range []string{annotationLevelDocument, annotationLevelPath, annotationLevelBranch} {
		rec, ok := recs[t.chunk.ID][lvl]
		if !ok {
			continue
		}
		if lvl == annotationLevelPath {
			if ok, err := coversReadable(ctx, acl, rec.covers); err != nil {
				return errFrom(op+": "+err.Error(), err), nil
			} else if !ok {
				continue
			}
		}
		stale, err := s.stale(ctx, rec)
		if err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
		rec.Stale = &stale
		out = append(out, rec)
	}
	return jsonResult(map[string]any{"annotations": out})
}

func (d *Document) deleteAnnotation(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	const op = "delete_annotation"
	if !annotationWriteAllowed(ctx) {
		return errPermission(op+": only the operator or an annotating driver deletes annotations",
			"Delete it from the console."), nil
	}
	if in.Level == "" {
		return errValidation(op+": missing required field: level (branch, document or path)", ""), nil
	}
	if in.Level == annotationLevelPath && in.ID == "" {
		root, refused := d.folderRecordChunk(ctx, key, op, in.Path)
		if refused != nil {
			return *refused, nil
		}
		if root == "" {
			return jsonResult(map[string]any{"deleted": false})
		}
		in.ID, in.Path = root, ""
	}
	s := d.newAnnotationScope(key, mscope)
	t, refused := d.resolveAnnotationTarget(ctx, s, op, in, true)
	if refused != nil {
		return *refused, nil
	}
	recs, err := d.annotationRecordsFor(ctx, key, []string{t.chunk.ID}, t.level)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	rec, ok := recs[t.chunk.ID][t.level]
	if !ok {
		return jsonResult(map[string]any{"deleted": false})
	}
	if rec.Manual && !tools.IsOperatorPlane(ctx) {
		return errPermission(op+": manual_record: an operator wrote this annotation; only an operator deletes it", ""), nil
	}
	if err := d.exec(ctx, key, `DELETE FROM chunk_annotations WHERE chunk_id = ? AND level = ?`, t.chunk.ID, t.level); err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	// A folder's _annotation document exists only to carry its record.
	if t.level == annotationLevelPath && strings.EqualFold(t.docType, annotationDocType) {
		raw, _ := json.Marshal(map[string]any{"op": "delete_document", "id": t.chunk.DocumentID})
		var del docInput
		_ = json.Unmarshal(raw, &del)
		if res, err := d.dispatch(ctx, key, mscope, del, raw); err != nil || res.IsError {
			return jsonResult(map[string]any{"deleted": true, "document_kept": true})
		}
	}
	return jsonResult(map[string]any{"deleted": true})
}

func (d *Document) listAnnotations(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	const op = "list_annotations"
	var where []string
	var args []any
	switch in.Level {
	case "":
	case annotationLevelBranch, annotationLevelDocument, annotationLevelPath:
		where = append(where, "a.level = ?")
		args = append(args, in.Level)
	default:
		return errValidation(op+": level must be branch, document or path", ""), nil
	}
	if in.DocumentID != "" {
		where = append(where, "c.document_id = ?")
		args = append(args, in.DocumentID)
	}
	if in.UnderPath != "" {
		ids, err := d.documentsUnderPath(ctx, key, in.UnderPath)
		if err != nil {
			return docFail(op+": ", err), nil
		}
		if len(ids) == 0 {
			return d.emptyAnnotationList(in)
		}
		ph, a := inPlaceholders(ids)
		where = append(where, "c.document_id IN ("+ph+")")
		args = append(args, a...)
	}
	if in.Manual {
		where = append(where, "a.manual = 1")
	}
	if in.WrittenBefore != "" {
		at, err := time.Parse(time.RFC3339, in.WrittenBefore)
		if err != nil {
			return errValidation(op+": written_before must be an RFC 3339 time, e.g. 2026-11-01T00:00:00Z", ""), nil
		}
		where = append(where, "a.written_at < ?")
		args = append(args, at.UnixNano())
	}
	acl := d.newDocACL(ctx, key)
	limited, err := acl.limitsReading(ctx)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	s := d.newAnnotationScope(key, mscope)
	if in.Summarize {
		q := `SELECT ` + annotationSelectCols + annotationFrom
		if len(where) > 0 {
			q += ` WHERE ` + strings.Join(where, " AND ")
		}
		res, err := d.query(ctx, key, q+` ORDER BY a.chunk_id, a.level`, args...)
		if err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
		counts := map[string]map[string]int{}
		checked, truncated := 0, false
		for _, r := range res.Rows {
			rec := scanAnnotationRecord(r)
			if limited {
				if ok, err := acl.readable(ctx, rec.DocumentID); err != nil {
					return errFrom(op+": "+err.Error(), err), nil
				} else if !ok {
					continue
				}
			}
			c := counts[rec.Level]
			if c == nil {
				c = map[string]int{"total": 0, "stale": 0, "manual": 0}
				counts[rec.Level] = c
			}
			c["total"]++
			if rec.Manual {
				c["manual"]++
			}
			if checked >= maxSummarizeStaleChecks {
				truncated = true
				continue
			}
			checked++
			if stale, err := s.stale(ctx, rec); err != nil {
				return errFrom(op+": "+err.Error(), err), nil
			} else if stale {
				c["stale"]++
			}
		}
		out := map[string]any{"counts": counts}
		if truncated {
			out["stale_checked"] = checked
		}
		return jsonResult(out)
	}
	if in.After != "" {
		chunk, level, _ := strings.Cut(in.After, "/")
		where = append(where, "(a.chunk_id > ? OR (a.chunk_id = ? AND a.level > ?))")
		args = append(args, chunk, chunk, level)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultListAnnotations
	}
	limit = min(limit, maxListAnnotations)
	q := `SELECT ` + annotationSelectCols + annotationFrom
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	res, err := d.query(ctx, key, q+` ORDER BY a.chunk_id, a.level LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	rows := res.Rows
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	out := []annotationRecord{}
	for _, r := range rows {
		rec := scanAnnotationRecord(r)
		stale, err := s.stale(ctx, rec)
		if err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
		if in.Stale && !stale {
			continue
		}
		rec.Stale = &stale
		out = append(out, rec)
	}
	resp := map[string]any{"annotations": out}
	if more {
		last := scanAnnotationRecord(rows[len(rows)-1])
		resp["next_cursor"] = last.ChunkID + "/" + last.Level
	}
	return jsonResult(resp)
}

func (d *Document) emptyAnnotationList(in docInput) (tools.Result, error) {
	if in.Summarize {
		return jsonResult(map[string]any{"counts": map[string]any{}})
	}
	return jsonResult(map[string]any{"annotations": []any{}})
}

func sortedAnnotationKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// annotationDocumentPath is the first Path-tree name of one document, or "".
func (d *Document) annotationDocumentPath(ctx context.Context, key sqlmem.ScopeKey, docID string) string {
	rows, err := d.Store.DirentListByDocuments(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), []string{docID})
	if err != nil || len(rows) == 0 {
		return ""
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.ParentPath+r.Name)
	}
	sort.Strings(names)
	return names[0]
}

// folderRecordChunk is the chunk a folder's path record lives on, "" when the
// folder has none.
func (d *Document) folderRecordChunk(ctx context.Context, key sqlmem.ScopeKey, op, path string) (string, *tools.Result) {
	if path == "" {
		r := errValidation(op+": missing required field: path (the folder)", "")
		return "", &r
	}
	canonical, err := folderPathOf(path)
	if err != nil {
		r := errValidation(op+": "+err.Error(), "Pass the folder's path, e.g. /kb/policies.")
		return "", &r
	}
	t, err := d.folderTargetOf(ctx, key, canonical)
	if err != nil {
		r := errFrom(op+": "+err.Error(), err)
		return "", &r
	}
	return t.root, nil
}
