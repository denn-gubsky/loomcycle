package builtin

// document_derive_units.go — the operator pass that generates Document derived
// search units (RFC DM C2): POST /v1/_document/derive_units.
//
// WHICH DOCUMENTS. A document opts in through `index_units` in its root chunk's
// fields — true (all three kinds), false or [] (none), or a list of kinds — and an
// operator can mark a Path subtree in memory.unit_generator.subtrees, which is the
// default for every document under it; the document's own setting wins. The memory
// trees are refused whatever is set: a document named under /facts or /memory, or
// one whose chunks carry memory entity metadata, is memory's own derived data, and
// units written from it would feed memory's output back into its input. An opted-in
// document refused by that rule is REPORTED, not silently skipped.
//
// WHICH CHUNKS. Every chunk with prose to describe. A chunk whose units were written
// from its current body (by hash) and cover the kinds asked for is up to date and
// costs nothing; one whose body changed since, or that has none yet, is generated.
//
// Explicit, bounded and resumable, like the other Document passes: a dry run by
// default, `limit` bounds the model calls one request makes, and a cursor resumes.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// DeriveUnitsOptions steers one call of the pass.
type DeriveUnitsOptions struct {
	// Generator is the operator's memory.unit_generator block — its subtrees are
	// the Path-subtree switch.
	Generator config.UnitGeneratorConfig
	// After resumes from a previous call's NextCursor.
	After string
	// Limit bounds the chunks generated (model calls) in this call. 0 = 25.
	Limit  int
	DryRun bool
}

// DeriveUnitsReport is one call's outcome.
type DeriveUnitsReport struct {
	Scope  string `json:"scope"`
	Model  string `json:"model,omitempty"`
	DryRun bool   `json:"dry_run"`

	DocumentsExamined int `json:"documents_examined"`
	DocumentsOptedIn  int `json:"documents_opted_in"`
	// SkippedByRule lists opted-in documents the pass refused (the memory trees).
	SkippedByRule []SkippedDocument `json:"skipped_by_rule,omitempty"`

	ChunksExamined int `json:"chunks_examined"`
	UpToDate       int `json:"up_to_date"`
	// Generated counts chunks given units for the first time; Rewritten, chunks
	// whose units were stale. In a dry run, what WOULD be.
	Generated    int `json:"generated"`
	Rewritten    int `json:"rewritten"`
	UnitsWritten int `json:"units_written"`
	Failed       int `json:"failed"`

	FailedChunks []string       `json:"failed_chunks,omitempty"`
	FirstFailure string         `json:"first_failure,omitempty"`
	Samples      []DerivedChunk `json:"samples,omitempty"`

	NextCursor string `json:"next_cursor,omitempty"`
	More       bool   `json:"more"`
}

// SkippedDocument is an opted-in document the pass refused, and why.
type SkippedDocument struct {
	DocumentID string `json:"document_id"`
	Title      string `json:"title"`
	Path       string `json:"path,omitempty"`
	Reason     string `json:"reason"`
}

// DerivedChunk samples what the pass wrote (or would write) for one chunk.
type DerivedChunk struct {
	ChunkID string                  `json:"chunk_id"`
	Title   string                  `json:"title"`
	Units   []memrank.GeneratedUnit `json:"units,omitempty"`
}

const (
	deriveDefaultLimit = 25
	deriveMaxLimit     = 500
	deriveSampleCap    = 5
	deriveFailedCap    = 50
	deriveDocPage      = 200
)

// DeriveUnits runs the pass over one scope. gen may be nil for a dry run.
func (d *Document) DeriveUnits(ctx context.Context, scope string, gen memrank.UnitGenerator, opts DeriveUnitsOptions) (DeriveUnitsReport, error) {
	rep := DeriveUnitsReport{Scope: scope, DryRun: opts.DryRun}
	if d.Store == nil || d.SqlMem == nil {
		return rep, fmt.Errorf("derive units: SQL Memory is not configured")
	}
	if !opts.DryRun && (gen == nil || d.Embedder == nil) {
		return rep, fmt.Errorf("derive units: a unit generator and an embedder are both required to write units")
	}
	if gen != nil {
		rep.Model = gen.ModelID()
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = deriveDefaultLimit
	}
	if limit > deriveMaxLimit {
		limit = deriveMaxLimit
	}
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return rep, err
	}
	if err := d.ensureSchema(ctx, key); err != nil {
		return rep, err
	}
	paths := d.documentPaths(ctx, key)
	afterDoc, afterChunk, _ := strings.Cut(opts.After, "/")

	// The first page starts AT the cursor's document (>=), so the one a previous call
	// stopped inside is resumed rather than skipped; later pages start strictly after
	// the last id seen. (Not "id >= last+NUL": Postgres rejects a NUL in text.)
	op, docCursor := ">=", afterDoc
	for {
		res, err := d.query(ctx, key, `SELECT id, title, root_chunk_id FROM documents WHERE id `+op+` ? ORDER BY id LIMIT ?`,
			docCursor, deriveDocPage)
		if err != nil {
			return rep, err
		}
		for _, row := range res.Rows {
			docID, title, rootID := asStr(row[0]), asStr(row[1]), asStr(row[2])
			fromChunk := ""
			if docID == afterDoc {
				fromChunk = afterChunk
			}
			done, err := d.deriveDocument(ctx, key, mscope, gen, opts, &rep, docID, title, rootID, paths[docID], fromChunk, limit)
			if err != nil {
				return rep, err
			}
			if !done {
				return rep, nil // stopped at the limit; rep carries the cursor
			}
			op, docCursor = ">", docID
		}
		if len(res.Rows) < deriveDocPage {
			return rep, nil
		}
	}
}

// deriveDocument handles one document. done is false when the call stopped inside
// it at the limit (the cursor is then set).
func (d *Document) deriveDocument(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, gen memrank.UnitGenerator,
	opts DeriveUnitsOptions, rep *DeriveUnitsReport, docID, title, rootID string, paths []string, fromChunk string, limit int) (bool, error) {

	if fromChunk == "" {
		rep.DocumentsExamined++
	}
	kinds := d.unitKindsFor(ctx, mscope, key, rootID, paths, opts.Generator)
	if len(kinds) == 0 {
		return true, nil // not opted in: never touched
	}
	if fromChunk == "" {
		rep.DocumentsOptedIn++
	}
	if reason, where := d.unitsRefusedFor(ctx, key, docID, paths); reason != "" {
		if fromChunk == "" {
			rep.SkippedByRule = append(rep.SkippedByRule, SkippedDocument{DocumentID: docID, Title: title, Path: where, Reason: reason})
		}
		return true, nil
	}
	res, err := d.query(ctx, key, `SELECT id, title, type, revision FROM chunks WHERE document_id = ? AND id > ? ORDER BY id`, docID, fromChunk)
	if err != nil {
		return false, err
	}
	for _, row := range res.Rows {
		chunkID, chunkTitle, chunkType, revision := asStr(row[0]), asStr(row[1]), asStr(row[2]), asInt(row[3])
		if rep.Generated+rep.Rewritten+rep.Failed >= limit {
			rep.NextCursor, rep.More = docID+"/"+lastChunkBefore(res.Rows, chunkID), true
			return false, nil
		}
		text, ok := d.unitSourceText(ctx, mscope, key, chunkID, chunkType)
		if !ok {
			continue
		}
		rep.ChunksExamined++
		sum := sha256.Sum256([]byte(text))
		sha := hex.EncodeToString(sum[:])
		existing := d.unitsOf(ctx, direntTenant(ctx), mscope, key.ScopeID, chunkID)
		if unitsCurrent(existing, sha, kinds) {
			rep.UpToDate++
			continue
		}
		stale := len(existing) > 0
		if opts.DryRun {
			if stale {
				rep.Rewritten++
			} else {
				rep.Generated++
			}
			if len(rep.Samples) < deriveSampleCap {
				rep.Samples = append(rep.Samples, DerivedChunk{ChunkID: chunkID, Title: chunkTitle})
			}
			continue
		}
		titles, _, _ := d.chunkLineage(ctx, key, chunkID)
		section := ""
		if len(titles) > 1 {
			section = strings.Join(titles[1:], indexPathSep)
		}
		units, err := gen.Generate(ctx, memrank.UnitRequest{DocumentTitle: title, SectionPath: section, Text: text, Kinds: kinds})
		if err == nil && len(units) == 0 {
			err = fmt.Errorf("the model wrote no units")
		}
		var n int
		if err == nil {
			derived := make([]DerivedUnit, len(units))
			for i, u := range units {
				derived[i] = DerivedUnit{Kind: u.Kind, Text: u.Text}
			}
			n, err = d.ReplaceUnits(ctx, key.Scope, chunkID, derived, UnitSource{Model: gen.ModelID(), BodyRevision: revision, BodySHA256: sha})
		}
		if err != nil {
			// A failed chunk keeps whatever units it had: a transport fault or an
			// unreadable answer is no reason to delete units that still match.
			rep.Failed++
			if len(rep.FailedChunks) < deriveFailedCap {
				rep.FailedChunks = append(rep.FailedChunks, chunkID)
			}
			if rep.FirstFailure == "" {
				rep.FirstFailure = err.Error()
			}
			continue
		}
		if stale {
			rep.Rewritten++
		} else {
			rep.Generated++
		}
		rep.UnitsWritten += n
		if len(rep.Samples) < deriveSampleCap {
			rep.Samples = append(rep.Samples, DerivedChunk{ChunkID: chunkID, Title: chunkTitle, Units: units})
		}
	}
	// A listing the SQL Memory row cap clipped: stop here and resume after the last
	// chunk seen, rather than finish the document with its tail never reached.
	if res.Truncated && len(res.Rows) > 0 {
		rep.NextCursor, rep.More = docID+"/"+asStr(res.Rows[len(res.Rows)-1][0]), true
		return false, nil
	}
	return true, nil
}

// lastChunkBefore is the id preceding chunkID in rows — the cursor names the last
// chunk the call HANDLED, so a resume starts at chunkID itself.
func lastChunkBefore(rows [][]any, chunkID string) string {
	prev := ""
	for _, r := range rows {
		id := asStr(r[0])
		if id == chunkID {
			return prev
		}
		prev = id
	}
	return prev
}

// unitKindsFor is the kinds a document asks for: its root's own index_units wins,
// else the deepest marked subtree ANY of its names lies under, else none.
func (d *Document) unitKindsFor(ctx context.Context, mscope store.MemoryScope, key sqlmem.ScopeKey, rootID string, paths []string, gen config.UnitGeneratorConfig) []string {
	if cb, err := d.readBody(ctx, mscope, key.ScopeID, rootID); err == nil && len(cb.Fields) > 0 {
		var f struct {
			IndexUnits json.RawMessage `json:"index_units"`
		}
		if json.Unmarshal(cb.Fields, &f) == nil && len(f.IndexUnits) > 0 {
			return parseIndexUnits(f.IndexUnits)
		}
	}
	// The deepest subtree wins across all of a document's names, as it does within
	// one: every document imported by title is ALSO named /documents/<title>, so the
	// name an operator gave it is rarely its only one.
	var kinds []string
	best := -1
	for _, p := range paths {
		k, ok := gen.SubtreeKinds(p)
		if !ok {
			continue
		}
		if depth := deepestSubtree(gen, p); depth > best {
			best, kinds = depth, k
		}
	}
	return kinds
}

// deepestSubtree is the length of the deepest marked subtree covering path.
func deepestSubtree(gen config.UnitGeneratorConfig, path string) int {
	best := -1
	for _, st := range gen.Subtrees {
		root := strings.TrimRight(st.Path, "/")
		if (path == root || strings.HasPrefix(path, root+"/")) && len(root) > best {
			best = len(root)
		}
	}
	return best
}

// parseIndexUnits reads a document's `index_units`: true = all three kinds, false
// or [] = none, a list = those kinds (unknown ones ignored). Anything else is none
// — an opt-in has to be unambiguous before it spends a model call per chunk.
func parseIndexUnits(raw json.RawMessage) []string {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		if b {
			return append([]string(nil), config.UnitKinds...)
		}
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	var out []string
	for _, k := range config.UnitKinds { // canonical order, de-duplicated
		for _, want := range list {
			if want == k {
				out = append(out, k)
				break
			}
		}
	}
	return out
}

// unitsRefusedFor says why a document may never get units, or "" — and, for a name
// in a memory tree, which name. ANY such name refuses it: a document is memory's
// own data if it is filed there under one of its names.
func (d *Document) unitsRefusedFor(ctx context.Context, key sqlmem.ScopeKey, docID string, paths []string) (string, string) {
	for _, p := range paths {
		if config.UnitsExcludedPath(p) {
			return "it is named in a memory tree (/facts, /memory)", p
		}
	}
	res, err := d.query(ctx, key, `SELECT 1 FROM chunk_memory_meta m JOIN chunks c ON c.id = m.chunk_id WHERE c.document_id = ? LIMIT 1`, docID)
	if err != nil {
		// Unknowable is refused: the rule protects against a recursion, and a pass
		// that cannot tell must not risk one.
		return "its chunks could not be checked for memory entity metadata: " + err.Error(), ""
	}
	if len(res.Rows) > 0 {
		return "its chunks carry memory entity metadata", ""
	}
	return "", ""
}

// unitSourceText is the text units are written from: the chunk's prose. ok is false
// for a chunk with nothing to describe (no body, a bare image or a diagram).
func (d *Document) unitSourceText(ctx context.Context, mscope store.MemoryScope, key sqlmem.ScopeKey, chunkID, chunkType string) (string, bool) {
	if chunkType == "image" || chunkType == "mermaid" {
		return "", false
	}
	cb, err := d.readBody(ctx, mscope, key.ScopeID, chunkID)
	if err != nil {
		return "", false
	}
	text, skip := d.chunkContentText(ctx, key, chunkID, chunkType, "", cb.Body)
	if skip || strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

// unitsCurrent reports whether a chunk's units were written from this body and cover
// exactly the kinds asked for.
func unitsCurrent(existing []store.MemoryEntry, sha string, kinds []string) bool {
	if len(existing) == 0 {
		return false
	}
	have := map[string]bool{}
	for _, e := range existing {
		var v memrank.UnitValue
		if json.Unmarshal(e.Value, &v) != nil || v.BodySHA256 != sha {
			return false
		}
		have[kindOption(v.Kind)] = true
	}
	want := map[string]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	if len(have) != len(want) {
		return false
	}
	for k := range want {
		if !have[k] {
			return false
		}
	}
	return true
}

// kindOption maps a stored unit kind to the opt-in vocabulary.
func kindOption(kind string) string {
	switch kind {
	case memrank.UnitClaim:
		return "claims"
	case memrank.UnitQuestion:
		return "questions"
	}
	return kind
}

// documentPaths maps each document id to EVERY Path-tree path naming it.
func (d *Document) documentPaths(ctx context.Context, key sqlmem.ScopeKey) map[string][]string {
	out := map[string][]string{}
	rows, err := d.Store.DirentListUnder(ctx, direntTenant(ctx), key.Scope, key.ScopeID, "/")
	if err != nil {
		return out
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].ParentPath+rows[i].Name < rows[j].ParentPath+rows[j].Name
	})
	for _, r := range rows {
		if r.Kind != "document" {
			continue
		}
		var ref struct {
			DocumentID string `json:"document_id"`
		}
		if json.Unmarshal(r.ResourceRef, &ref) != nil || ref.DocumentID == "" {
			continue
		}
		out[ref.DocumentID] = append(out[ref.DocumentID], r.ParentPath+r.Name)
	}
	return out
}
