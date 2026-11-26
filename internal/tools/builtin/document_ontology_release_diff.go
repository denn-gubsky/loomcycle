package builtin

// Reading ontology releases (RFC GQ P3.4): list them, read one, and diff two — or one
// against the ontology in force — entry by entry.
//
// A diff parses BOTH sides with the reader every other caller uses (parseOntology): a
// release's copied chunks are an ontologySource just as the stored document is, so a
// release reads exactly as the document it copied did, and the entries compared are the
// ones the content version counts (ontologyContentItems). Entries are matched by chunk
// id, so a rename is one changed entry, never an add and a remove.
//
// Every read here runs under noWrites: a tenant scope from before the release tables
// reads as having no release rather than being migrated by a GET. Who may read releases
// is the connector's question (operators only: a release keeps text an edit or an
// erasure removed from the ontology).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// releaseReadKey resolves the tenant scope a release read works in, under noWrites.
func (d *Document) releaseReadKey(ctx context.Context) (context.Context, sqlmem.ScopeKey, store.MemoryScope, error) {
	if d.Store == nil || d.SqlMem == nil {
		return ctx, sqlmem.ScopeKey{}, "", connector.WithMessage(connector.ErrOntologyReleaseUnavailable,
			"ontology releases need SQL Memory; none is configured")
	}
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return ctx, sqlmem.ScopeKey{}, "", connector.WithMessage(connector.ErrOntologyReleaseUnavailable, err.Error())
	}
	return noWrites(ctx), key, mscope, nil
}

// noReleaseTables reports a scope that has never had the release tables (or no
// document plane at all): it has no release, which is not a fault.
func noReleaseTables(err error) bool {
	return schemaMissing(err) || errors.Is(err, sqlmem.ErrPlaneAbsent)
}

func connectorRelease(r OntologyRelease) connector.OntologyRelease {
	return connector.OntologyRelease{Seq: r.Seq, ContentVersion: r.ContentVersion, Fingerprint: r.Fingerprint,
		Reason: r.Reason, Actor: r.Actor, CreatedAt: r.CreatedAt, Entries: r.Entries}
}

// ListOntologyReleases lists the complete releases of ctx's tenant, newest first.
func (d *Document) ListOntologyReleases(ctx context.Context) ([]connector.OntologyRelease, error) {
	ctx, key, _, err := d.releaseReadKey(ctx)
	if err != nil {
		return []connector.OntologyRelease{}, err
	}
	rels, err := d.ontologyReleases(ctx, key, 0)
	if noReleaseTables(err) {
		return []connector.OntologyRelease{}, nil
	}
	if err != nil {
		return []connector.OntologyRelease{}, fmt.Errorf("list ontology releases: %w", err)
	}
	out := make([]connector.OntologyRelease, 0, len(rels))
	for _, r := range rels {
		out = append(out, connectorRelease(r))
	}
	return out, nil
}

// GetOntologyRelease reads one complete release: its header, its plan summary and the
// ontology it copied as Markdown.
func (d *Document) GetOntologyRelease(ctx context.Context, seq int64) (connector.GetOntologyReleaseResult, error) {
	if seq <= 0 {
		return connector.GetOntologyReleaseResult{}, connector.WithMessage(connector.ErrOntologyReleaseInvalid,
			"seq must be a release's sequence number, a positive integer")
	}
	ctx, key, _, err := d.releaseReadKey(ctx)
	if err != nil {
		return connector.GetOntologyReleaseResult{}, err
	}
	res, err := d.query(ctx, key, `SELECT seq, content_version, fingerprint, reason, coalesce(actor, ''), created_at, entries,
		coalesce(plan_summary, ''), coalesce(export_md, '')
		FROM ontology_releases WHERE seq = ? AND complete = 1`, seq)
	if err != nil && !noReleaseTables(err) {
		return connector.GetOntologyReleaseResult{}, fmt.Errorf("read ontology release %d: %w", seq, err)
	}
	if err != nil || len(res.Rows) == 0 || len(res.Rows[0]) < 9 {
		return connector.GetOntologyReleaseResult{}, releaseNotFound(seq)
	}
	r := res.Rows[0]
	out := connector.GetOntologyReleaseResult{
		Release: connector.OntologyRelease{Seq: asInt64Val(r[0]), ContentVersion: asStr(r[1]), Fingerprint: asStr(r[2]),
			Reason: asStr(r[3]), Actor: asStr(r[4]), CreatedAt: asInt64Val(r[5]), Entries: asInt(r[6])},
		ExportMD: asStr(r[8]),
	}
	if s := asStr(r[7]); s != "" && json.Valid([]byte(s)) {
		out.PlanSummary = json.RawMessage(s)
	}
	return out, nil
}

func releaseNotFound(seq int64) error {
	return connector.WithMessage(connector.ErrOntologyReleaseNotFound,
		"no complete ontology release has seq "+strconv.FormatInt(seq, 10))
}

// DiffOntologyReleases compares release from with release to, or with the ontology in
// force when to is 0.
func (d *Document) DiffOntologyReleases(ctx context.Context, from, to int64) (connector.DiffOntologyReleasesResult, error) {
	out := connector.DiffOntologyReleasesResult{Changes: []connector.OntologyEntryDiff{}, Issues: []string{}}
	if from <= 0 || to < 0 {
		return out, connector.WithMessage(connector.ErrOntologyReleaseInvalid,
			"from must be a release's sequence number, and to another's or current")
	}
	ctx, key, mscope, err := d.releaseReadKey(ctx)
	if err != nil {
		return out, err
	}
	before, beforeSide, err := d.releaseSide(ctx, key, from, &out.Issues)
	if err != nil {
		return out, err
	}
	var after map[string]diffItem
	var afterSide connector.OntologyDiffSide
	if to == 0 {
		read, treeParent, err := d.ontologyTreeForKey(ctx, key, mscope, memrank.OntologyPath, nil)
		if noReleaseTables(err) || (err == nil && read.DocumentID == "") {
			return out, connector.WithMessage(connector.ErrOntologyReleaseNotFound, "this tenant has no ontology document")
		}
		if err != nil {
			return out, fmt.Errorf("read the ontology: %w", err)
		}
		if read.partial {
			out.Issues = append(out.Issues, "the ontology in force could not be read whole; entries whose text was not read may show as changed")
		}
		after = diffItems(read, treeParent)
		afterSide = connector.OntologyDiffSide{Current: true, ContentVersion: read.ContentVersion}
	} else if after, afterSide, err = d.releaseSide(ctx, key, to, &out.Issues); err != nil {
		return out, err
	}
	out.From, out.To = beforeSide, afterSide
	out.Changes = diffEntries(before, after)
	return out, nil
}

// releaseSide reads one release as the reader reads the document, and its entries.
func (d *Document) releaseSide(ctx context.Context, key sqlmem.ScopeKey, seq int64, issues *[]string) (map[string]diffItem, connector.OntologyDiffSide, error) {
	side := connector.OntologyDiffSide{Seq: seq}
	rel, err := d.loadRelease(ctx, key, seq)
	if err != nil {
		return nil, side, err
	}
	side.ContentVersion = rel.contentVersion
	if rel.orphans > 0 {
		*issues = append(*issues, fmt.Sprintf("release %d: %d entries sit under a proposal the release did not copy and are not compared", seq, rel.orphans))
	}
	if rel.partial {
		*issues = append(*issues, fmt.Sprintf("release %d could not be read whole; entries whose text was not read may show as changed", seq))
	}
	return rel.items, side, nil
}

// loadedRelease is one complete release as stored and as the reader reads it.
type loadedRelease struct {
	contentVersion string
	// rows are the release's chunks by chunk id, as copied.
	rows map[string]releaseRow
	// items are its in-force entries, as diffItems lists them; partial says some
	// body could not be read.
	items   map[string]diffItem
	partial bool
	// orphans counts chunks under a proposal the release did not copy.
	orphans int
	// body reads one chunk's body and fields from the release.
	body func(id string) (chunkBody, error)
}

// releaseRow is one chunk of a release.
type releaseRow struct {
	parentID, title, status, typ string
	position                     int
}

// loadRelease reads release seq: its header, its chunks, and its entries parsed with
// the reader every other caller uses. releaseNotFound for a release that is not
// complete or does not exist.
func (d *Document) loadRelease(ctx context.Context, key sqlmem.ScopeKey, seq int64) (*loadedRelease, error) {
	hdr, err := d.query(ctx, key, `SELECT content_version FROM ontology_releases WHERE seq = ? AND complete = 1`, seq)
	if err != nil && !noReleaseTables(err) {
		return nil, fmt.Errorf("read ontology release %d: %w", seq, err)
	}
	if err != nil || len(hdr.Rows) == 0 || len(hdr.Rows[0]) < 1 {
		return nil, releaseNotFound(seq)
	}
	rel := &loadedRelease{contentVersion: asStr(hdr.Rows[0][0])}

	res, err := d.query(ctx, key, `SELECT chunk_id, coalesce(parent_id, ''), title, position, coalesce(status, ''), body_sha,
		coalesce(type, '')
		FROM ontology_release_entries WHERE release_seq = ? ORDER BY position, chunk_id`, seq)
	if err != nil {
		return nil, fmt.Errorf("read ontology release %d's entries: %w", seq, err)
	}
	if res.Truncated {
		return nil, fmt.Errorf("ontology release %d has more entries than one read returns", seq)
	}
	src := ontologySource{}
	shaOf := make(map[string]string, len(res.Rows))
	rel.rows = make(map[string]releaseRow, len(res.Rows))
	for _, r := range res.Rows {
		if len(r) < 7 {
			continue
		}
		c := ontologyChunk{id: asStr(r[0]), parentID: asStr(r[1]), title: asStr(r[2]), position: asInt(r[3]), status: asStr(r[4])}
		shaOf[c.id] = asStr(r[5])
		rel.rows[c.id] = releaseRow{parentID: c.parentID, title: c.title, status: c.status, typ: asStr(r[6]), position: c.position}
		if c.parentID == "" && src.rootID == "" {
			src.rootID = c.id
		}
		src.rows = append(src.rows, c)
	}
	// A chunk under a proposal is copied with its stored parent, the proposal, which a
	// release does not copy (P3.3); the reader cannot place it, so it is not compared.
	for _, c := range src.rows {
		if _, ok := shaOf[c.parentID]; c.parentID != "" && !ok {
			rel.orphans++
		}
	}
	// Bodies are read one by one as the parser asks, each its own materialised query
	// (a SQLite scope has one connection), as the stored reader reads one per chunk.
	src.body = func(id string) (chunkBody, error) {
		b, err := d.query(ctx, key, `SELECT body, fields FROM ontology_release_bodies WHERE sha = ?`, shaOf[id])
		if err != nil {
			return chunkBody{}, err
		}
		if len(b.Rows) == 0 || len(b.Rows[0]) < 2 {
			return chunkBody{}, fmt.Errorf("release %d: no body for %s", seq, id)
		}
		cb := chunkBody{Body: asStr(b.Rows[0][0])}
		if f := asStr(b.Rows[0][1]); f != "" {
			cb.Fields = json.RawMessage(f)
		}
		return cb, nil
	}
	rel.body = src.body
	read, treeParent := parseOntology(src, nil)
	rel.items, rel.partial = diffItems(read, treeParent), read.partial
	return rel, nil
}

// diffItem is one in-force entry of a side, as compared.
type diffItem struct {
	kind  string
	state connector.OntologyEntryState
}

// derivedKeys are the declaration keys a diff reports on their own (name, status,
// meaning, parent) or that follow from another entry (a concept's scheme, path and tag
// from its ancestors' names; an instance's parent kind from its parent): left out of
// directives, so renaming a scheme is not a directive change of each of its concepts.
var derivedKeys = []string{"name", "entry_status", "meaning", "parent", "parent_kind", "chunk_id", "scheme", "path", "tag"}

// diffItems lists a read's in-force entries by chunk id, each with its declared state.
func diffItems(r OntologyRead, treeParent map[string]string) map[string]diffItem {
	items := ontologyContentItems(r, treeParent)
	out := make(map[string]diffItem, len(items))
	names := make(map[string]string, len(items))
	decl := make(map[string]map[string]json.RawMessage, len(items))
	for _, it := range items {
		m := map[string]json.RawMessage{}
		if b, err := json.Marshal(it.Entry); err == nil {
			_ = json.Unmarshal(b, &m)
		}
		decl[it.ChunkID] = m
		names[it.ChunkID] = rawJSONString(m["name"])
	}
	for _, it := range items {
		m := decl[it.ChunkID]
		st := connector.OntologyEntryState{Name: names[it.ChunkID], Parent: it.Parent, ParentName: names[it.Parent],
			Status: rawJSONString(m["entry_status"]), Meaning: rawJSONString(m["meaning"])}
		for _, k := range derivedKeys {
			delete(m, k)
		}
		if it.Kind == planKindGrammar {
			delete(m, "class") // the class it sits under: its parent
		}
		st.Directives, _ = json.Marshal(m) // a map's keys are written sorted
		out[it.ChunkID] = diffItem{kind: it.Kind, state: st}
	}
	return out
}

func rawJSONString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// diffEntries compares two sides by chunk id.
func diffEntries(before, after map[string]diffItem) []connector.OntologyEntryDiff {
	out := []connector.OntologyEntryDiff{}
	for id, b := range before {
		bs := b.state
		a, ok := after[id]
		if !ok {
			out = append(out, connector.OntologyEntryDiff{ChunkID: id, Kind: b.kind, Change: connector.OntologyDiffRemoved, Before: &bs})
			continue
		}
		as := a.state
		var changed []string
		if b.kind != a.kind {
			changed = append(changed, connector.OntologyDiffKind)
		}
		if bs.Name != as.Name {
			changed = append(changed, connector.OntologyDiffRenamed)
		}
		if bs.Parent != as.Parent {
			changed = append(changed, connector.OntologyDiffMoved)
		}
		if bs.Status != as.Status {
			changed = append(changed, connector.OntologyDiffStatus)
		}
		if bs.Meaning != as.Meaning {
			changed = append(changed, connector.OntologyDiffMeaning)
		}
		if string(bs.Directives) != string(as.Directives) {
			changed = append(changed, connector.OntologyDiffDirectives)
		}
		if len(changed) > 0 {
			out = append(out, connector.OntologyEntryDiff{ChunkID: id, Kind: a.kind, Change: connector.OntologyDiffChanged,
				Changed: changed, Before: &bs, After: &as})
		}
	}
	for id, a := range after {
		if _, ok := before[id]; !ok {
			as := a.state
			out = append(out, connector.OntologyEntryDiff{ChunkID: id, Kind: a.kind, Change: connector.OntologyDiffAdded, After: &as})
		}
	}
	rank := map[string]int{planKindClass: 0, planKindGrammar: 1, planKindRelation: 2, planKindScheme: 3, planKindConcept: 4, planKindInstance: 5}
	name := func(e connector.OntologyEntryDiff) string {
		if e.After != nil {
			return e.After.Name
		}
		return e.Before.Name
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := rank[out[i].Kind], rank[out[j].Kind]; ri != rj {
			return ri < rj
		}
		if ni, nj := name(out[i]), name(out[j]); ni != nj {
			return ni < nj
		}
		return out[i].ChunkID < out[j].ChunkID
	})
	return out
}
