package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// An imported document says where it came from (RFC FF D18). Its chunk
// bodies are stamped with the memory origin store.OriginImport, so the text
// stays in document search and never reads as a fact; its root chunk carries
// the provenance in one field, importProvenanceKey: what the runtime recorded
// of the file it was imported from, and the link to its source the importer
// gave.
//
// The provenance is the server's. It is one reserved field rather than keys
// beside the root's own, so a caller's ordinary fields (set_asset's
// media_type, a document's own "file" or "origin") are never mistaken for
// it; every body write drops a caller's value for it and carries the stored
// one forward (putBody); and it is shown only while the root's body row has
// the import origin, which no tool input can set.

// Limits on a source the importer gives.
const (
	importSourceMaxURLBytes      = 2048
	importSourceMaxTitleRunes    = 300
	importSourceMaxRevisionRunes = 64
)

// ImportSourceKinds are the kinds of place a document is imported from.
var ImportSourceKinds = []string{"sharepoint", "email", "attachment", "web", "skybits", "other"}

// importProvenanceKey is the root field the provenance is kept in.
const importProvenanceKey = "provenance"

// The provenance's own fields.
const (
	importFieldOrigin       = "origin"
	importFieldFile         = "file"
	importFieldMediaType    = "media_type"
	importFieldSHA256       = "sha256"
	importFieldCapturedFrom = "captured_from"
	importFieldCapturedAt   = "captured_at"
	importFieldRunID        = "run_id"
	importFieldSourceURL    = "source_url"
	importFieldSourceKind   = "source_kind"
	importFieldSourceTitle  = "source_title"
	// importFieldSourceRevision / importFieldSourceUpdatedAt are the source's
	// own revision (doc:r12) and update time (RFC 3339, UTC), for a source
	// that keeps changing (RFC GB §5).
	importFieldSourceRevision  = "source_revision"
	importFieldSourceUpdatedAt = "source_updated_at"
	// importFieldSourceStatus says whether the source is still there:
	// present, removed or trashed. Only Document op=mark_source sets it; a
	// re-import carries it over.
	importFieldSourceStatus = "source_status"
	// importFieldStructured records that a run added headings to the file
	// on import (File op=import headings): {headings: <n>, by: <run id>}.
	importFieldStructured = "structured"
)

// importProvenanceFields are those fields, in the order they are shown.
var importProvenanceFields = []string{
	importFieldOrigin, importFieldFile, importFieldMediaType, importFieldSHA256,
	importFieldCapturedFrom, importFieldCapturedAt, importFieldRunID,
	importFieldSourceURL, importFieldSourceKind, importFieldSourceTitle,
	importFieldSourceRevision, importFieldSourceUpdatedAt, importFieldSourceStatus,
	importFieldStructured,
}

// ImportSource is the link to where an imported document came from, as the
// importer gives it: the File tool's `source`, or Document import_md's
// source_url / source_kind / source_title / source_revision /
// source_updated_at.
type ImportSource struct {
	URL   string `json:"url,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Title string `json:"title,omitempty"`
	// Revision is the source's own name for the version imported (doc:r12,
	// sheet:r3, file:v2); UpdatedAt its own update time, RFC 3339.
	Revision  string `json:"revision,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	// kindDefaulted: normalize filled Kind in, the importer gave none. A
	// re-import then keeps the stored kind rather than reading "other" as a
	// different source.
	kindDefaulted bool
}

func (s ImportSource) empty() bool {
	return s.URL == "" && s.Kind == "" && s.Title == "" && s.Revision == "" && s.UpdatedAt == ""
}

// normalize checks a source and returns the form that is stored: the URL
// http(s), within its limit, without user information and with its
// credential-bearing query parameters masked as a signed URL is at rest, and
// no fragment; the kind one of ImportSourceKinds ("other" when a source is given without
// one); the title one line, cut to its limit; the revision one line within
// its limit (refused, not cut: a cut revision names another one); the update
// time RFC 3339, kept in UTC. An all-empty source is no source.
func (s ImportSource) normalize() (ImportSource, error) {
	out := ImportSource{
		URL:       strings.TrimSpace(s.URL),
		Kind:      strings.ToLower(strings.TrimSpace(s.Kind)),
		Title:     oneLine(s.Title),
		Revision:  strings.TrimSpace(s.Revision),
		UpdatedAt: strings.TrimSpace(s.UpdatedAt),
	}
	if out.empty() {
		return ImportSource{}, nil
	}
	if out.URL != "" {
		u, err := checkSourceURL(out.URL)
		if err != nil {
			return ImportSource{}, err
		}
		out.URL = u
	}
	if out.Revision != "" {
		if strings.IndexFunc(out.Revision, unicode.IsControl) >= 0 {
			return ImportSource{}, errors.New("source revision contains a control character")
		}
		if n := utf8.RuneCountInString(out.Revision); n > importSourceMaxRevisionRunes {
			return ImportSource{}, fmt.Errorf("source revision is %d characters, over the limit of %d", n, importSourceMaxRevisionRunes)
		}
	}
	if out.UpdatedAt != "" {
		t, err := time.Parse(time.RFC3339, out.UpdatedAt)
		if err != nil {
			return ImportSource{}, fmt.Errorf("source updated_at %q is not an RFC 3339 time, e.g. 2026-10-01T08:00:00Z", out.UpdatedAt)
		}
		out.UpdatedAt = t.UTC().Format(time.RFC3339Nano)
	}
	if out.Kind == "" {
		out.Kind = "other"
		out.kindDefaulted = true
	}
	if !slices.Contains(ImportSourceKinds, out.Kind) {
		return ImportSource{}, fmt.Errorf("source kind %q is not one of %s", out.Kind, strings.Join(ImportSourceKinds, ", "))
	}
	if utf8.RuneCountInString(out.Title) > importSourceMaxTitleRunes {
		out.Title, _ = cutRunes(out.Title, importSourceMaxTitleRunes)
		out.Title = strings.TrimSpace(out.Title)
	}
	return out, nil
}

// checkSourceURL returns the stored form of a source URL, or why it is
// refused. Meant for an item's web link (a SharePoint webUrl, a message's
// webLink), never a download or upload link. A user name or password in it is refused rather than dropped: the
// importer should not be handing the runtime a credential at all.
func checkSourceURL(raw string) (string, error) {
	if len(raw) > importSourceMaxURLBytes {
		return "", fmt.Errorf("source url is %d bytes, over the limit of %d", len(raw), importSourceMaxURLBytes)
	}
	if strings.IndexFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", errors.New("source url contains a space or a control character")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("source url does not parse")
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return "", errors.New("source url must be http or https")
	}
	if u.Host == "" {
		return "", errors.New("source url has no host")
	}
	if u.User != nil {
		return "", errors.New("source url carries a user name or password")
	}
	// The query stays: an item's web link needs it (an Outlook message's
	// ItemID). Its credential-bearing parameters are masked; the fragment,
	// which an implicit grant uses for a token, is dropped.
	u.Fragment, u.RawFragment = "", ""
	return redact.MaskSignedURL(u.String()), nil
}

// oneLine joins s onto one line: every run of white space or control
// characters becomes one space.
func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
}

// importProvenance is what the runtime stamps on a document it imports.
type importProvenance struct {
	// fields go on the root chunk, over whatever the Markdown gave.
	fields map[string]any
	// urlGiven / kindGiven / titleGiven: the importer named the source's
	// URL, kind or title, rather than the runtime deriving or defaulting it
	// (a download's URL, kind other). A re-import holds what was named to
	// the stored source, and keeps the stored value of what was not.
	urlGiven, kindGiven, titleGiven bool
}

type importProvenanceCtxKey struct{}

// withImportProvenance attaches the provenance the File tool recorded for an
// import to the context its import_md call runs in. A context value, never
// tool input: these fields describe what the runtime captured, and the text
// being imported must not be able to supply them.
func withImportProvenance(ctx context.Context, p importProvenance) context.Context {
	return context.WithValue(ctx, importProvenanceCtxKey{}, p)
}

func importProvenanceFrom(ctx context.Context) (importProvenance, bool) {
	p, ok := ctx.Value(importProvenanceCtxKey{}).(importProvenance)
	return p, ok
}

// newImportProvenance is a provenance with only the origin set.
func newImportProvenance() importProvenance {
	return importProvenance{fields: map[string]any{importFieldOrigin: store.OriginImport}}
}

// withSource adds a normalized source's fields.
func (p importProvenance) withSource(s ImportSource) importProvenance {
	if s.URL != "" {
		p.fields[importFieldSourceURL] = s.URL
	}
	if s.Kind != "" {
		p.fields[importFieldSourceKind] = s.Kind
	}
	if s.Title != "" {
		p.fields[importFieldSourceTitle] = s.Title
	}
	if s.Revision != "" {
		p.fields[importFieldSourceRevision] = s.Revision
	}
	if s.UpdatedAt != "" {
		p.fields[importFieldSourceUpdatedAt] = s.UpdatedAt
	}
	p.urlGiven = p.urlGiven || s.URL != ""
	p.kindGiven = p.kindGiven || (s.Kind != "" && !s.kindDefaulted)
	p.titleGiven = p.titleGiven || s.Title != ""
	return p
}

// tags are the document tags the provenance adds: source/<kind>.
func (p importProvenance) tags() []string {
	if k, _ := p.fields[importFieldSourceKind].(string); k != "" {
		return []string{"source/" + k}
	}
	return nil
}

// fieldsObject decodes a chunk's fields as an object; ok is false for fields
// that are not one (absent fields decode as an empty object).
func fieldsObject(fields json.RawMessage) (map[string]json.RawMessage, bool) {
	m := map[string]json.RawMessage{}
	if len(fields) == 0 {
		return m, true
	}
	if json.Unmarshal(fields, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

// setProvenanceField returns fields with the provenance field set to p,
// whatever the caller's fields held there.
func setProvenanceField(fields json.RawMessage, p map[string]any) json.RawMessage {
	m, ok := fieldsObject(fields)
	if !ok {
		m = map[string]json.RawMessage{}
	}
	m[importProvenanceKey], _ = json.Marshal(p)
	b, _ := json.Marshal(m)
	return b
}

// carryProvenanceField returns a caller's fields with its provenance field
// replaced by the stored body's: dropped when the stored body has none. A
// write that touches neither comes back byte-for-byte as it was given.
func carryProvenanceField(fields, stored json.RawMessage) json.RawMessage {
	var keep json.RawMessage
	if s, ok := fieldsObject(stored); ok {
		keep = s[importProvenanceKey]
	}
	m, ok := fieldsObject(fields)
	if !ok {
		return fields // not an object: it cannot hold the field
	}
	if _, has := m[importProvenanceKey]; !has && keep == nil {
		return fields
	}
	delete(m, importProvenanceKey)
	if keep != nil {
		m[importProvenanceKey] = keep
	}
	b, _ := json.Marshal(m)
	return b
}

// provenanceView is the provenance a root chunk's provenance field holds,
// its known keys only; nil when it holds none.
func provenanceView(fields json.RawMessage) map[string]any {
	m, ok := fieldsObject(fields)
	if !ok || m[importProvenanceKey] == nil {
		return nil
	}
	var p map[string]any
	if json.Unmarshal(m[importProvenanceKey], &p) != nil {
		return nil
	}
	out := map[string]any{}
	for _, k := range importProvenanceFields {
		if v, ok := p[k]; ok && v != nil && v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// importedProvenance is provenanceView for a chunk whose body row carries the
// import origin, and nil for any other: the field is the server's by every
// write path, and the origin, which no tool input sets, is checked as well.
func (d *Document) importedProvenance(ctx context.Context, mscope store.MemoryScope, key sqlmem.ScopeKey, chunkID string, cb chunkBody) map[string]any {
	view := provenanceView(cb.Fields)
	if view == nil {
		return nil
	}
	prov, err := d.Store.MemoryProvenanceGet(ctx, direntTenant(ctx), mscope, docMemScopeID(mscope, key), chunkBodyKey(chunkID))
	if err != nil || prov.Origin != store.OriginImport {
		return nil
	}
	return view
}

// provenanceLine is a clean export's one line under the title saying where
// the document came from; "" when it says nothing.
func provenanceLine(p map[string]any) string {
	str := func(k string) string { s, _ := p[k].(string); return s }
	var parts []string
	if title, u := str(importFieldSourceTitle), str(importFieldSourceURL); title != "" || u != "" {
		src := strings.TrimSpace(title + " " + u)
		if k := str(importFieldSourceKind); k != "" {
			src += " (" + k + ")"
		}
		parts = append(parts, "Source: "+src)
	}
	if f := str(importFieldFile); f != "" {
		parts = append(parts, "File: "+f)
	}
	if len(parts) == 0 {
		return ""
	}
	return "> " + strings.Join(parts, " · ")
}

// importSource is import_md's source fields as one source.
func (in docInput) importSource() ImportSource {
	return ImportSource{URL: in.SourceURL, Kind: in.SourceKind, Title: in.SourceTitle,
		Revision: in.SourceRevision, UpdatedAt: in.SourceUpdatedAt}
}

// importSourceRefusal is import_md's refusal of a source normalize refused.
func importSourceRefusal(err error) tools.Result {
	return errValidation("import_md: "+err.Error()+". Nothing was imported.",
		"Pass source_url as an http(s) link without a user name or password, source_kind as one of "+strings.Join(ImportSourceKinds, ", ")+
			", source_revision as at most 64 characters on one line, and source_updated_at as an RFC 3339 time.")
}

// sourceMismatch refuses a re-import whose source is not the one the
// document was imported from.
func sourceMismatch(stored, given string) *tools.Result {
	if stored == "" {
		stored = "none"
	}
	r := errBusiness(fmt.Sprintf("import_md: source_mismatch: the document was imported from source url %s, and this re-import names %s. Nothing was imported.", stored, given),
		"Re-import a new version of the same source onto its document, or import another source as a new document.")
	return &r
}

// refreshedProvenance is the provenance a re-import leaves on a document
// whose root holds stored: what the import recorded now (the captured
// file, its sha256, when and by which run, the source's revision and update
// time), the source being the stored one. A URL the importer names must be
// the stored one (source_mismatch otherwise). A kind it names replaces the
// stored one: the URL says which source it is, and the kind only what sort of
// place that is — documents first imported as kind other are named skybits by
// their next re-import. What it leaves out of the URL, kind and title is kept
// from the stored source, and so is the source_status marker, which only
// mark_source sets. What the new import
// does not record (a file, for an import_md) is not carried over: it
// described the earlier text.
func refreshedProvenance(stored map[string]any, in importProvenance) (map[string]any, *tools.Result) {
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	next := map[string]any{}
	for k, v := range in.fields {
		next[k] = v
	}
	keep := func(k string) {
		if v := str(stored, k); v != "" {
			next[k] = v
		}
	}
	if u := str(in.fields, importFieldSourceURL); in.urlGiven && u != str(stored, importFieldSourceURL) {
		return nil, sourceMismatch(str(stored, importFieldSourceURL), u)
	} else if !in.urlGiven {
		keep(importFieldSourceURL)
	}
	if !in.kindGiven {
		keep(importFieldSourceKind)
	}
	if !in.titleGiven {
		keep(importFieldSourceTitle)
	}
	keep(importFieldSourceStatus)
	return next, nil
}

// reimportProvenance is what a re-import onto the document rooted at rootID
// will leave as its provenance; nil when it changes nothing: the re-import
// names no source and is not the File tool's, or the document was not
// imported (its text stays the document's own, as before). Asked before
// anything is written, so a refusal leaves the document as it was.
//
// storedKind is the kind the document holds now, for the caller to move the
// source/<kind> tag when the re-import names another.
func (d *Document) reimportProvenance(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput, rootID string) (next map[string]any, storedKind string, refused *tools.Result, err error) {
	prov, imported := importProvenanceFrom(ctx)
	src, err := in.importSource().normalize()
	if err != nil {
		r := importSourceRefusal(err)
		return nil, "", &r, nil
	}
	if !imported && src.empty() {
		return nil, "", nil, nil
	}
	if !imported {
		prov = newImportProvenance()
	}
	prov = prov.withSource(src)
	cb, err := d.readBody(ctx, mscope, key, rootID)
	if err != nil {
		return nil, "", nil, err
	}
	stored := d.importedProvenance(ctx, mscope, key, rootID, cb)
	if stored == nil {
		return nil, "", nil, nil
	}
	storedKind, _ = stored[importFieldSourceKind].(string)
	next, refused = refreshedProvenance(stored, prov)
	return next, storedKind, refused, nil
}

// errProvenanceConflict: the root changed between the read and the bump.
var errProvenanceConflict = errors.New("the document's root was changed by a concurrent write; nothing was changed, try again")

// reviseProvenance makes next the provenance of the imported root rootID, as
// a revision of the root: the revision is bumped, and the provenance in force
// before stays on the revision it was in force at (chunk_revisions'
// provenance column), so history, get_version and diff show what the
// document said of its source then. The body text is written back as it is.
// changed is false, and nothing is written, when next is what is stored.
//
// The caller holds the document's editions lock: a re-import and mark_source
// both take it, so of two revisions one reads the other's result.
func (d *Document) reviseProvenance(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, rootID string, next map[string]any) (bool, error) {
	row, ok, err := d.getChunkRow(ctx, key, rootID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("no such chunk %s", rootID)
	}
	cb, err := d.readBody(ctx, mscope, key, rootID)
	if err != nil {
		return false, err
	}
	var prevRaw json.RawMessage
	if m, ok := fieldsObject(cb.Fields); ok {
		prevRaw = m[importProvenanceKey]
	}
	nextRaw, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	if sameJSONObject(prevRaw, nextRaw) {
		return false, nil
	}
	if err := d.recordRevisionProvenance(ctx, key, rootID, row.Revision, cb.Body, prevRaw); err != nil {
		return false, err
	}
	bumped, err := d.docSQL().Exec(ctx, key, d.docSQL().Rebind(`UPDATE chunks SET revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`),
		[]any{time.Now().UnixNano(), rootID, row.Revision}, 0)
	if err != nil {
		return false, err
	}
	if bumped.RowsAffected == 0 {
		return false, errProvenanceConflict
	}
	if err := d.putBody(ctx, mscope, key, rootID, row.Type, cb.Body, cb.Fields, "", next); err != nil {
		return false, err
	}
	return true, d.recordRevisionProvenance(ctx, key, rootID, row.Revision+1, cb.Body, nextRaw)
}

// recordRevisionProvenance keeps prov as the provenance of the root's
// revision rev: the revision's row is written if the log has none (a root's
// first body write records no revision) and its provenance set if it holds
// none, so a row the log already has keeps what it recorded.
func (d *Document) recordRevisionProvenance(ctx context.Context, key sqlmem.ScopeKey, chunkID string, rev int, body string, prov json.RawMessage) error {
	if err := d.recordRevision(ctx, key, chunkID, rev, body); err != nil {
		return err
	}
	if len(prov) == 0 {
		return nil
	}
	return d.exec(ctx, key, `UPDATE chunk_revisions SET provenance = ? WHERE chunk_id = ? AND revision = ? AND provenance IS NULL`,
		string(prov), chunkID, rev)
}

// sameJSONObject reports whether a and b decode to the same object.
func sameJSONObject(a, b json.RawMessage) bool {
	var x, y map[string]any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return len(a) == 0 && len(b) == 0
	}
	xs, _ := json.Marshal(x)
	ys, _ := json.Marshal(y)
	return string(xs) == string(ys)
}

// revisionProvenance decodes a revision's recorded provenance; nil when it
// recorded none.
func revisionProvenance(v any) map[string]any {
	s := asStr(v)
	if s == "" {
		return nil
	}
	var p map[string]any
	if json.Unmarshal([]byte(s), &p) != nil || len(p) == 0 {
		return nil
	}
	return p
}

// migrateRevisionProvenance adds chunk_revisions.provenance: an imported
// root's provenance as it was at that revision (RFC GB §5), NULL for every
// revision that recorded none — every revision of a chunk that is not an
// imported root. Probed before it alters, as every migration on the hot path.
func (d *Document) migrateRevisionProvenance(ctx context.Context, key sqlmem.ScopeKey) error {
	if d.tableHasColumn(ctx, key, "chunk_revisions", "provenance") {
		return nil
	}
	return d.exec(ctx, key, `ALTER TABLE chunk_revisions ADD COLUMN provenance TEXT`)
}

// retagSourceKind moves a document's source/<kind> tag from one kind to
// another, as a re-import that names a new kind changes its provenance.
func (d *Document) retagSourceKind(ctx context.Context, key sqlmem.ScopeKey, docID, from, to string) error {
	if from == to || to == "" {
		return nil
	}
	if from != "" {
		if err := d.removeTagRows(ctx, key, "document_tags", "document_id", docID, []string{"source/" + from}); err != nil {
			return err
		}
	}
	return d.addTagRows(ctx, key, "document_tags", "document_id", docID, []string{"source/" + to})
}
