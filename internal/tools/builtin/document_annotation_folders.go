package builtin

// document_annotation_folders.go — the annotation of a Path folder.
//
// A folder has no row of its own (directories are implicit), so its annotation
// lives on a real document named <folder>/_annotation, of the operational type
// Annotation: its title is the folder's name, its root fields say which folder
// and which documents it covers and carry the brief, and its root chunk holds
// the (root, path) record. The document's body stays empty on purpose: the
// abstract lives in the record, so the document is never ranked by an ordinary
// search as if it were knowledge. A folder that is itself a document (a document
// with names below it) gets no _annotation document; its path record sits on that
// document's own root, and the document's content is not touched.
//
// A folder's annotation is written from its direct children — each document's
// annotation and each sub-folder's — its classification (how many documents of
// each class, and the topics they are about) and the kinds of relation among the
// documents it covers. A change deep below reaches it only through the
// sub-folder's annotation, as a section's does through its parts.
//
// What a folder's annotation says is drawn from every document it covers, so it
// is withheld from a caller who may not read every one of them: the record, and
// the _annotation document itself, which answers as not found.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	// annotationDocName is the reserved last segment of a folder's annotation
	// document.
	annotationDocName = "_annotation"
	// annotationDocType is its root type, one of the operational document types.
	annotationDocType = "Annotation"
	// maxFolderChildren is how many direct children a folder request names one by
	// one; past it the rest are grouped by class with counts.
	maxFolderChildren = 60
	// folderGroupTitles is how many titles a grouped class names.
	folderGroupTitles = 5
	maxFolderConcepts = 10
)

// migrateAnnotationFolders adds the folder a path record is of and the documents
// it covers, to a scope whose chunk_annotations predates them.
func (d *Document) migrateAnnotationFolders(ctx context.Context, key sqlmem.ScopeKey) error {
	if d.tableHasColumn(ctx, key, "chunk_annotations", "folder") && d.tableHasColumn(ctx, key, "chunk_annotations", "covers") {
		return nil
	}
	if !d.tableHasColumn(ctx, key, "chunk_annotations", "folder") {
		if err := d.exec(ctx, key, `ALTER TABLE chunk_annotations ADD COLUMN folder TEXT`); err != nil {
			return err
		}
	}
	if !d.tableHasColumn(ctx, key, "chunk_annotations", "covers") {
		if err := d.exec(ctx, key, `ALTER TABLE chunk_annotations ADD COLUMN covers TEXT`); err != nil {
			return err
		}
	}
	return nil
}

// annotationExcludedTrees are the Path trees no annotation covers: subject
// documents (entities) and the memory layer's documents.
var annotationExcludedTrees = []string{subjectDocPathPrefix, "/memory/"}

func annotationExcludedTree(full string) bool {
	for _, p := range annotationExcludedTrees {
		if strings.HasPrefix(full+"/", p) {
			return true
		}
	}
	return false
}

// isAnnotationDocName reports a Path name whose last segment is the reserved
// annotation name.
func isAnnotationDocName(canonical string) bool {
	_, name, isRoot := splitPath(canonical)
	return !isRoot && name == annotationDocName
}

// annotationNameErr refuses creating or renaming a tenant name to or from the
// reserved last segment, unless the caller writes annotations.
func annotationNameErr(ctx context.Context, scope string, canonical ...string) error {
	if scope != "tenant" || annotationWriteAllowed(ctx) {
		return nil
	}
	if _, ok := ctx.Value(annotationProvisionKey{}).(bool); ok {
		return nil
	}
	for _, c := range canonical {
		if isAnnotationDocName(c) {
			return &reservedNameError{name: c, why: "the name " + annotationDocName +
				" is reserved for a folder's annotation, which only the annotation pass and the operator write"}
		}
	}
	return nil
}

// annotationProvisionKey marks the runtime's own write of an _annotation
// document. Unexported, so nothing outside this package can set it.
type annotationProvisionKey struct{}

// folderChild is one direct child of a folder as the model sees it.
type folderChild struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"` // document | folder
	DocumentID string `json:"document_id,omitempty"`
	Title      string `json:"title,omitempty"`
	Class      string `json:"class,omitempty"`
	Brief      string `json:"brief,omitempty"`
	Stale      bool   `json:"stale,omitempty"`
}

type folderGroup struct {
	Class  string   `json:"class"`
	Count  int      `json:"count"`
	Titles []string `json:"titles"`
}

type folderCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// folderInputs is everything a folder's annotation is written from.
type folderInputs struct {
	Level          string            `json:"level"`
	Path           string            `json:"path"`
	Title          string            `json:"title"`
	Parent         string            `json:"parent,omitempty"`
	ChunkID        string            `json:"chunk_id,omitempty"`
	DocumentID     string            `json:"document_id,omitempty"`
	Classes        []folderCount     `json:"classes"`
	Concepts       []folderCount     `json:"concepts,omitempty"`
	Children       []folderChild     `json:"children"`
	ChildrenMore   []folderGroup     `json:"children_more,omitempty"`
	Relations      []folderCount     `json:"relations,omitempty"`
	Covers         []string          `json:"covers"`
	Above          *annotationAbove  `json:"above,omitempty"`
	Language       string            `json:"language"`
	PromptVersion  int               `json:"prompt_version"`
	InputsSHA256   string            `json:"inputs_sha256"`
	Record         *annotationRecord `json:"record,omitempty"`
	Stale          bool              `json:"stale"`
	FolderDocument bool              `json:"folder_document,omitempty"`
}

// folderTarget is where a folder's path record lives.
type folderTarget struct {
	canonical string
	// folderDoc is the document named at the folder's own path, when the folder is
	// a document with names below it; its root carries the record.
	folderDoc string
	// annDoc is the folder's _annotation document, when it has one.
	annDoc string
	// root is the chunk that carries (or will carry) the record: folderDoc's
	// root, else annDoc's, else "".
	root string
}

func (d *Document) direntDocument(ctx context.Context, key sqlmem.ScopeKey, parent, name string) (string, error) {
	row, err := d.Store.DirentGet(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), parent, name)
	if err != nil {
		var nf *store.ErrNotFound
		if asNotFound(err, &nf) {
			return "", nil
		}
		return "", err
	}
	if row.Kind != "document" {
		return "", nil
	}
	var ref struct {
		DocumentID string `json:"document_id"`
	}
	_ = json.Unmarshal(row.ResourceRef, &ref)
	return ref.DocumentID, nil
}

func (d *Document) rootChunkOf(ctx context.Context, key sqlmem.ScopeKey, docID string) (string, error) {
	res, err := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ?`, docID)
	if err != nil || len(res.Rows) == 0 {
		return "", err
	}
	return asStr(res.Rows[0][0]), nil
}

// folderTargetOf resolves where the folder canonical's record lives.
func (d *Document) folderTargetOf(ctx context.Context, key sqlmem.ScopeKey, canonical string) (folderTarget, error) {
	t := folderTarget{canonical: canonical}
	if parent, name, isRoot := splitPath(canonical); !isRoot {
		id, err := d.direntDocument(ctx, key, parent, name)
		if err != nil {
			return t, err
		}
		t.folderDoc = id
	}
	id, err := d.direntDocument(ctx, key, dirPrefix(canonical), annotationDocName)
	if err != nil {
		return t, err
	}
	t.annDoc = id
	switch {
	case t.folderDoc != "":
		t.root, err = d.rootChunkOf(ctx, key, t.folderDoc)
	case t.annDoc != "":
		t.root, err = d.rootChunkOf(ctx, key, t.annDoc)
	}
	return t, err
}

// folderRecord is the path record of a folder, if it has one.
func (d *Document) folderRecord(ctx context.Context, key sqlmem.ScopeKey, t folderTarget) (*annotationRecord, error) {
	if t.root == "" {
		return nil, nil
	}
	recs, err := d.annotationRecordsFor(ctx, key, []string{t.root}, annotationLevelPath)
	if err != nil {
		return nil, err
	}
	if rec, ok := recs[t.root][annotationLevelPath]; ok {
		return &rec, nil
	}
	return nil, nil
}

// folderInputsFor computes what the folder canonical's annotation is written
// from, and its hash. acl filters only what is shown (sub-folder briefs whose
// covers the caller may not read); the hash covers everything.
func (s *annotationScope) folderInputsFor(ctx context.Context, canonical string, promptVersion int, acl *docACL) (folderInputs, error) {
	d, key := s.d, s.key
	parent, name, isRoot := splitPath(canonical)
	in := folderInputs{Level: annotationLevelPath, Path: canonical, Title: name, PromptVersion: promptVersion,
		Classes: []folderCount{}, Children: []folderChild{}, Covers: []string{}}
	if isRoot {
		in.Title = "/"
	} else {
		in.Parent = strings.TrimSuffix(parent, "/")
		if in.Parent == "" {
			in.Parent = "/"
		}
	}
	t, err := d.folderTargetOf(ctx, key, canonical)
	if err != nil {
		return in, err
	}
	in.ChunkID, in.FolderDocument = t.root, t.folderDoc != ""
	if t.folderDoc != "" {
		in.DocumentID = t.folderDoc
	} else {
		in.DocumentID = t.annDoc
	}
	prefix := dirPrefix(canonical)
	rows, err := d.Store.DirentListUnder(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), prefix)
	if err != nil {
		return in, err
	}
	// Every document below, and the direct children: documents and sub-folders.
	type docRef struct{ name, id string }
	var direct []docRef
	subFolders := map[string]bool{}
	coverSet := map[string]bool{}
	for _, r := range rows {
		// The subject and memory trees hold entities and the memory layer's own
		// documents, never annotated, so a folder above them does not cover them.
		if annotationExcludedTree(r.ParentPath + r.Name) {
			continue
		}
		if r.Kind == "document" && r.Name != annotationDocName {
			var ref struct {
				DocumentID string `json:"document_id"`
			}
			_ = json.Unmarshal(r.ResourceRef, &ref)
			if ref.DocumentID != "" {
				coverSet[ref.DocumentID] = true
				if r.ParentPath == prefix {
					direct = append(direct, docRef{name: r.Name, id: ref.DocumentID})
				}
			}
		}
		if r.ParentPath == prefix && r.Kind == "directory" {
			subFolders[r.Name] = true
		} else if r.ParentPath != prefix {
			rest := strings.TrimPrefix(r.ParentPath, prefix)
			if i := strings.IndexByte(rest, '/'); i > 0 {
				subFolders[rest[:i]] = true
			}
		}
	}
	if t.folderDoc != "" {
		delete(coverSet, t.folderDoc)
	}
	// Annotation documents are never covered: they say nothing of their own.
	ids := make([]string, 0, len(coverSet))
	for id := range coverSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	types := map[string]string{}
	titles := map[string]string{}
	roots := map[string]string{}
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		ph, args := inPlaceholders(ids[start:end])
		res, err := d.query(ctx, key, `SELECT id, title, type, root_chunk_id FROM documents WHERE id IN (`+ph+`)`, args...)
		if err != nil {
			return in, err
		}
		for _, r := range res.Rows {
			id := asStr(r[0])
			titles[id], types[id], roots[id] = asStr(r[1]), asStr(r[2]), asStr(r[3])
		}
	}
	classCount := map[string]int{}
	for _, id := range ids {
		if _, ok := titles[id]; !ok || memrank.IsOperationalDocumentType(types[id]) {
			continue
		}
		in.Covers = append(in.Covers, id)
		cls := types[id]
		if cls == "" {
			cls = "(untyped)"
		}
		classCount[cls]++
	}
	in.Classes = sortedCounts(classCount, 0)

	// The direct children's own annotations.
	var rootIDs []string
	for _, c := range direct {
		if r := roots[c.id]; r != "" {
			rootIDs = append(rootIDs, r)
		}
	}
	docRecs, err := d.annotationRecordsFor(ctx, key, rootIDs, annotationLevelDocument)
	if err != nil {
		return in, err
	}
	sort.Slice(direct, func(i, j int) bool { return direct[i].name < direct[j].name })
	var terms []string
	langs := map[string]int{}
	var children []folderChild
	for _, c := range direct {
		if _, ok := titles[c.id]; !ok || memrank.IsOperationalDocumentType(types[c.id]) {
			continue
		}
		ch := folderChild{Name: c.name, Kind: "document", DocumentID: c.id, Title: titles[c.id], Class: types[c.id]}
		term := "d:" + c.name + ":" + titles[c.id] + ":none"
		if rec, ok := docRecs[roots[c.id]][annotationLevelDocument]; ok {
			ch.Brief = rec.Brief
			term = "d:" + c.name + ":" + sha256Hex(titles[c.id]+"\x00"+rec.Brief+"\x00"+rec.Abstract)
			langs[rec.Language]++
		}
		terms = append(terms, term)
		children = append(children, ch)
	}
	var folderNames []string
	for n := range subFolders {
		if n != "" {
			folderNames = append(folderNames, n)
		}
	}
	sort.Strings(folderNames)
	for _, n := range folderNames {
		sub := prefix + n
		st, err := d.folderTargetOf(ctx, key, sub)
		if err != nil {
			return in, err
		}
		rec, err := d.folderRecord(ctx, key, st)
		if err != nil {
			return in, err
		}
		ch := folderChild{Name: n, Kind: "folder"}
		term := "f:" + n + ":none"
		if rec != nil {
			term = "f:" + n + ":" + sha256Hex(rec.Brief+"\x00"+rec.Abstract)
			if ok, err := coversReadable(ctx, acl, rec.covers); err != nil {
				return in, err
			} else if ok {
				ch.Brief = rec.Brief
			}
		}
		terms = append(terms, term)
		children = append(children, ch)
	}
	if len(children) > maxFolderChildren {
		in.Children = children[:maxFolderChildren]
		groups := map[string]*folderGroup{}
		var order []string
		for _, ch := range children[maxFolderChildren:] {
			cls := ch.Class
			if ch.Kind == "folder" {
				cls = "folder"
			} else if cls == "" {
				cls = "(untyped)"
			}
			g := groups[cls]
			if g == nil {
				g = &folderGroup{Class: cls, Titles: []string{}}
				groups[cls] = g
				order = append(order, cls)
			}
			g.Count++
			if len(g.Titles) < folderGroupTitles {
				t := ch.Title
				if t == "" {
					t = ch.Name
				}
				g.Titles = append(g.Titles, t)
			}
		}
		for _, cls := range order {
			in.ChildrenMore = append(in.ChildrenMore, *groups[cls])
		}
	} else if children != nil {
		in.Children = children
	}

	// The topics the covered documents are about, and how they relate.
	if len(in.Covers) > 0 {
		if in.Concepts, err = s.coveredCounts(ctx, `SELECT ct.title FROM chunk_edges e
			JOIN chunks cf ON cf.id = e.from_id JOIN chunks ct ON ct.id = e.to_id
			WHERE e.kind = 'subject' AND e.status = 'asserted' AND e.expired_at IS NULL AND cf.document_id IN (%s)`,
			in.Covers, maxFolderConcepts); err != nil {
			return in, err
		}
		if in.Relations, err = s.coveredCounts(ctx, `SELECT e.kind FROM chunk_edges e
			JOIN chunks cf ON cf.id = e.from_id JOIN chunks ct ON ct.id = e.to_id
			WHERE e.status = 'asserted' AND e.expired_at IS NULL AND e.kind <> 'subject'
			  AND cf.document_id <> ct.document_id AND cf.document_id IN (%s) AND ct.document_id IN (%[1]s)`,
			in.Covers, 0); err != nil {
			return in, err
		}
	}

	// The language: the one most of its documents' annotations are written in.
	in.Language = annotationFallbackLanguage
	best := 0
	for l, n := range langs {
		if n > best || (n == best && l < in.Language) {
			in.Language, best = l, n
		}
	}
	if !isRoot {
		pt, err := d.folderTargetOf(ctx, key, in.Parent)
		if err == nil {
			if rec, err := d.folderRecord(ctx, key, pt); err == nil && rec != nil {
				if ok, _ := coversReadable(ctx, acl, rec.covers); ok {
					in.Above = &annotationAbove{Level: annotationLevelPath, Abstract: rec.Abstract}
				}
			}
		}
	}
	canon := struct {
		V             int      `json:"v"`
		Level         string   `json:"level"`
		Path          string   `json:"path"`
		Language      string   `json:"language"`
		PromptVersion int      `json:"prompt_version"`
		Children      []string `json:"children"`
		Classes       []string `json:"classes"`
		Concepts      []string `json:"concepts"`
		Relations     []string `json:"relations"`
	}{V: annotationCanonVersion, Level: annotationLevelPath, Path: canonical, Language: in.Language,
		PromptVersion: promptVersion, Children: terms}
	for _, c := range in.Classes {
		canon.Classes = append(canon.Classes, c.Name+"="+strconv.Itoa(c.Count))
	}
	for _, c := range in.Concepts {
		canon.Concepts = append(canon.Concepts, c.Name+"="+strconv.Itoa(c.Count))
	}
	for _, c := range in.Relations {
		canon.Relations = append(canon.Relations, c.Name+"="+strconv.Itoa(c.Count))
	}
	raw, _ := json.Marshal(canon)
	in.InputsSHA256 = sha256Hex(string(raw))
	return in, nil
}

// coveredCounts runs a one-column query over the covered documents (the %s is
// their id list) and counts its values, largest first.
func (s *annotationScope) coveredCounts(ctx context.Context, stmt string, covers []string, limit int) ([]folderCount, error) {
	ph, args := inPlaceholders(covers)
	q := fmt.Sprintf(stmt, ph)
	if strings.Count(q, ph) == 2 {
		args = append(args, args...)
	}
	res, err := s.d.query(ctx, s.key, q, args...)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, r := range res.Rows {
		if v := strings.TrimSpace(asStr(r[0])); v != "" {
			counts[v]++
		}
	}
	out := sortedCounts(counts, limit)
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func sortedCounts(m map[string]int, limit int) []folderCount {
	out := make([]folderCount, 0, len(m))
	for k, v := range m {
		out = append(out, folderCount{Name: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// coversReadable reports whether the caller may read every covered document. A
// caller whose set limits nothing may.
func coversReadable(ctx context.Context, acl *docACL, covers []string) (bool, error) {
	if acl == nil {
		return true, nil
	}
	limited, err := acl.limitsReading(ctx)
	if err != nil || !limited {
		return !limited, err
	}
	for _, id := range covers {
		ok, err := acl.readable(ctx, id)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// folderPathOf normalises a folder path: "/" stays "/", anything else loses its
// trailing slash.
func folderPathOf(raw string) (string, error) {
	if strings.TrimSpace(raw) == "/" {
		return "/", nil
	}
	return normalizePath(raw)
}

// --- the ops at level path ---

func (d *Document) folderAnnotationInputs(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	const op = "annotation_inputs"
	canonical, err := folderPathOf(in.Path)
	if err != nil {
		return errValidation(op+": "+err.Error(), "Pass the folder's path, e.g. /kb/policies."), nil
	}
	s := d.newAnnotationScope(key, mscope)
	acl := d.newDocACL(ctx, key)
	got, err := s.folderInputsFor(ctx, canonical, in.PromptVersion, acl)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	// A folder's inputs name and describe every document below it: they are read
	// only by a caller who may read all of them, as the annotation itself is.
	if refused := folderCoversRefusal(ctx, op, acl, got.Covers); refused != nil {
		return *refused, nil
	}
	got.Stale = true
	t, err := d.folderTargetOf(ctx, key, canonical)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	rec, err := d.folderRecord(ctx, key, t)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	if rec != nil {
		stale := rec.InputsSHA256 != got.InputsSHA256
		rec.Stale = &stale
		got.Record, got.Stale = rec, stale
	}
	return jsonResult(got)
}

func (d *Document) setFolderAnnotation(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput, brief, abstract string) (tools.Result, error) {
	const op = "set_annotation"
	canonical, err := folderPathOf(in.Path)
	if err != nil {
		return errValidation(op+": "+err.Error(), "Pass the folder's path, e.g. /kb/policies."), nil
	}
	s := d.newAnnotationScope(key, mscope)
	cur, err := s.folderInputsFor(ctx, canonical, in.PromptVersion, nil)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	if len(cur.Covers) == 0 {
		return errValidation(op+": "+canonical+" holds no document to annotate", "Annotate a folder that holds documents."), nil
	}
	if refused := folderCoversRefusal(ctx, op, d.newDocACL(ctx, key), cur.Covers); refused != nil {
		return *refused, nil
	}
	if in.InputsSHA256 != "" && in.InputsSHA256 != cur.InputsSHA256 {
		return errBusiness(op+": annotation_inputs_changed: the folder's inputs changed since they were read",
			"Read them again with annotation_inputs and write an annotation of the current folder."), nil
	}
	t, err := d.folderTargetOf(ctx, key, canonical)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	prev, err := d.folderRecord(ctx, key, t)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	if prev != nil && prev.Manual && !in.Manual {
		return errBusiness(op+": manual_record: an operator wrote this annotation; it is not overwritten",
			"Leave it, or ask an operator to delete it first."), nil
	}
	language := in.Language
	if language == "" {
		language = cur.Language
	}
	coversJSON, _ := json.Marshal(cur.Covers)
	// The record's home: the folder's own document, else its _annotation
	// document, made now if it is not there yet.
	if t.root == "" {
		if t.root, err = d.createAnnotationDocument(ctx, key, mscope, canonical); err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
		t.annDoc = "made"
	}
	if t.folderDoc == "" {
		fields, _ := json.Marshal(map[string]any{"level": annotationLevelPath, "path": canonical, "covers": cur.Covers, "brief": brief})
		if err := d.setAnnotationRootFields(ctx, key, mscope, t.root, fields); err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
	}
	manual := 0
	if in.Manual {
		manual = 1
	}
	var model any
	if in.Model != "" {
		model = in.Model
	}
	if err := d.exec(ctx, key, `INSERT INTO chunk_annotations (chunk_id, level, brief, abstract, language, class,
		ontology_version, inputs_sha256, prompt_version, model, manual, written_at, written_by, folder, covers)
		VALUES (?, ?, ?, ?, ?, NULL, NULL, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (chunk_id, level) DO UPDATE SET brief = excluded.brief, abstract = excluded.abstract,
		language = excluded.language, inputs_sha256 = excluded.inputs_sha256, prompt_version = excluded.prompt_version,
		model = excluded.model, manual = excluded.manual, written_at = excluded.written_at, written_by = excluded.written_by,
		folder = excluded.folder, covers = excluded.covers`,
		t.root, annotationLevelPath, brief, abstract, language, cur.InputsSHA256, in.PromptVersion, model, manual,
		time.Now().UnixNano(), annotationWriter(ctx), canonical, string(coversJSON)); err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	rec, err := d.folderRecord(ctx, key, folderTarget{root: t.root})
	if err != nil || rec == nil {
		return errFrom(op+": the record was not read back", err), nil
	}
	d.indexAnnotation(ctx, key, mscope, *rec)
	notStale := false
	rec.Stale = &notStale
	return jsonResult(map[string]any{"annotation": rec})
}

// createAnnotationDocument makes the folder's _annotation document and returns
// its root chunk.
func (d *Document) createAnnotationDocument(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, canonical string) (string, error) {
	_, name, isRoot := splitPath(canonical)
	if isRoot {
		name = "/"
	}
	pctx := context.WithValue(ctx, annotationProvisionKey{}, true)
	res, err := d.createDocument(pctx, key, mscope, docInput{Op: "create_document", Title: name, Type: annotationDocType,
		Path: dirPrefix(canonical) + annotationDocName})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("%s", res.Text)
	}
	var out struct {
		Root string `json:"root_chunk_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || out.Root == "" {
		return "", fmt.Errorf("the annotation document was made without a root")
	}
	return out.Root, nil
}

// setAnnotationRootFields writes an _annotation document's root fields, through
// update_chunk as any write of them would go.
func (d *Document) setAnnotationRootFields(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, root string, fields json.RawMessage) error {
	row, ok, err := d.getChunkRow(ctx, key, root)
	if err != nil || !ok {
		return fmt.Errorf("the annotation document's root is gone")
	}
	raw, _ := json.Marshal(map[string]any{"op": "update_chunk", "id": root, "revision": row.Revision, "fields": fields})
	var in docInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return err
	}
	res, err := d.dispatch(context.WithValue(ctx, annotationProvisionKey{}, true), key, mscope, in, raw)
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("%s", res.Text)
	}
	return nil
}

// annotationDocumentWithheld reports whether docID is a folder's annotation
// document whose covered documents the caller may not all read.
func (a *docACL) annotationDocumentWithheld(ctx context.Context, docID string) (bool, error) {
	if a == nil || !a.pathSensitive || docID == "" {
		return false, nil
	}
	res, err := a.d.query(ctx, a.key, `SELECT a.covers FROM documents d
		JOIN chunk_annotations a ON a.chunk_id = d.root_chunk_id AND a.level = ?
		WHERE d.id = ? AND d.type = ?`, annotationLevelPath, docID, annotationDocType)
	if err != nil || len(res.Rows) == 0 {
		return false, err
	}
	ok, err := coversReadable(ctx, a, parseCovers(asStr(res.Rows[0][0])))
	return !ok, err
}

func parseCovers(raw string) []string {
	var out []string
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// PathEntrySummaries implements PathAnnotations: ls with_summary. A document
// gets its own annotation's brief; a folder (and a document with names below
// it, as folder_summary) its path record's brief and abstract, within
// summaryChars; an _annotation entry the brief of the record it carries. A
// folder's record is withheld when the caller may not read everything it covers.
func (d *Document) PathEntrySummaries(ctx context.Context, scope string, entries []PathSummaryEntry, summaryChars int) map[string]PathEntrySummary {
	out := map[string]PathEntrySummary{}
	if d == nil || d.Store == nil || d.SqlMem == nil || scope != "tenant" || len(entries) == 0 {
		return out
	}
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil || d.ensureSchema(ctx, key) != nil {
		return out
	}
	budget := summaryChars
	if budget <= 0 {
		budget = defaultSummaryChars
	}
	budget = min(budget, maxSummaryChars)
	acl := d.newDocACL(ctx, key)
	s := d.newAnnotationScope(key, mscope)
	checks := 0
	mark := func(sm map[string]any, rec annotationRecord) {
		if checks >= maxSummaryStaleChecks {
			return
		}
		checks++
		if stale, err := s.stale(ctx, rec); err == nil && stale {
			sm["stale"] = true
		}
	}
	// Documents: one batch of roots and records.
	var docIDs []string
	for _, e := range entries {
		if e.DocumentID != "" {
			docIDs = append(docIDs, e.DocumentID)
		}
	}
	roots := map[string]string{}
	if len(docIDs) > 0 {
		ph, args := inPlaceholders(docIDs)
		if res, err := d.query(ctx, key, `SELECT id, root_chunk_id FROM documents WHERE id IN (`+ph+`)`, args...); err == nil {
			for _, r := range res.Rows {
				roots[asStr(r[0])] = asStr(r[1])
			}
		}
	}
	var rootIDs []string
	for _, r := range roots {
		rootIDs = append(rootIDs, r)
	}
	recs, _ := d.annotationRecordsFor(ctx, key, rootIDs, annotationLevelDocument, annotationLevelPath)
	folder := func(full string) map[string]any {
		t, err := d.folderTargetOf(ctx, key, full)
		if err != nil {
			return nil
		}
		rec, err := d.folderRecord(ctx, key, t)
		if err != nil || rec == nil {
			return nil
		}
		if ok, err := coversReadable(ctx, acl, rec.covers); err != nil || !ok {
			return nil
		}
		sm := map[string]any{"level": annotationLevelPath, "brief": rec.Brief}
		if n := utf8.RuneCountInString(rec.Abstract); n <= budget {
			sm["abstract"] = rec.Abstract
			budget -= n
		}
		mark(sm, *rec)
		return sm
	}
	for _, e := range entries {
		var got PathEntrySummary
		root := roots[e.DocumentID]
		_, name, _ := splitPath(e.FullPath)
		switch {
		case e.Kind == "directory":
			got.Summary = folder(e.FullPath)
		case root != "" && name == annotationDocName:
			if rec, ok := recs[root][annotationLevelPath]; ok {
				if ok, err := coversReadable(ctx, acl, rec.covers); err == nil && ok {
					got.Summary = map[string]any{"level": annotationLevelPath, "brief": rec.Brief}
				}
			}
		case root != "":
			if rec, ok := recs[root][annotationLevelDocument]; ok {
				got.Summary = map[string]any{"level": annotationLevelDocument, "brief": rec.Brief}
				mark(got.Summary, rec)
			}
			if e.HasChildren {
				got.FolderSummary = folder(e.FullPath)
			}
		}
		if got.Summary != nil || got.FolderSummary != nil {
			out[e.FullPath] = got
		}
	}
	return out
}

// folderCoversRefusal refuses a folder's inputs or write to a caller who may not
// read every document the folder covers.
func folderCoversRefusal(ctx context.Context, op string, acl *docACL, covers []string) *tools.Result {
	ok, err := coversReadable(ctx, acl, covers)
	if err != nil {
		r := errFrom(op+": "+err.Error(), err)
		return &r
	}
	if ok {
		return nil
	}
	r := errPermission(op+": the folder holds documents you may not read, so you may not read or write its annotation", "")
	return &r
}
