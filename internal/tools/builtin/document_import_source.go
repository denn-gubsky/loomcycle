package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// An imported document says where it came from (RFC FF D18). Its chunk
// bodies are stamped with the memory origin store.OriginImport, so the text
// stays in document search and never reads as a fact; its root chunk carries
// the provenance as fields: what the runtime recorded of the file it was
// imported from, and the link to its source the importer gave.

// Limits on a source the importer gives.
const (
	importSourceMaxURLBytes   = 2048
	importSourceMaxTitleRunes = 300
)

// ImportSourceKinds are the kinds of place a document is imported from.
var ImportSourceKinds = []string{"sharepoint", "email", "attachment", "web", "other"}

// The root-chunk fields an import stamps. They are the server's: a value the
// imported Markdown gives for one of them is dropped, so a field that reads
// as provenance was never written by the text being imported.
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
)

// importProvenanceFields are those fields, in the order they are shown.
var importProvenanceFields = []string{
	importFieldOrigin, importFieldFile, importFieldMediaType, importFieldSHA256,
	importFieldCapturedFrom, importFieldCapturedAt, importFieldRunID,
	importFieldSourceURL, importFieldSourceKind, importFieldSourceTitle,
}

// ImportSource is the link to where an imported document came from, as the
// importer gives it: the File tool's `source`, or Document import_md's
// source_url / source_kind / source_title.
type ImportSource struct {
	URL   string `json:"url,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Title string `json:"title,omitempty"`
}

func (s ImportSource) empty() bool { return s.URL == "" && s.Kind == "" && s.Title == "" }

// normalize checks a source and returns the form that is stored: the URL
// http(s), within its limit, without user information and with its
// credential-bearing query parameters masked as a signed URL is at rest, and
// no fragment; the kind one of ImportSourceKinds ("other" when a source is given without
// one); the title one line, cut to its limit. An all-empty source is no
// source.
func (s ImportSource) normalize() (ImportSource, error) {
	out := ImportSource{
		URL:   strings.TrimSpace(s.URL),
		Kind:  strings.ToLower(strings.TrimSpace(s.Kind)),
		Title: oneLine(s.Title),
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
	if out.Kind == "" {
		out.Kind = "other"
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
}

type importProvenanceKey struct{}

// withImportProvenance attaches the provenance the File tool recorded for an
// import to the context its import_md call runs in. A context value, never
// tool input: these fields describe what the runtime captured, and the text
// being imported must not be able to supply them.
func withImportProvenance(ctx context.Context, p importProvenance) context.Context {
	return context.WithValue(ctx, importProvenanceKey{}, p)
}

func importProvenanceFrom(ctx context.Context) (importProvenance, bool) {
	p, ok := ctx.Value(importProvenanceKey{}).(importProvenance)
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
	return p
}

// tags are the document tags the provenance adds: source/<kind>.
func (p importProvenance) tags() []string {
	if k, _ := p.fields[importFieldSourceKind].(string); k != "" {
		return []string{"source/" + k}
	}
	return nil
}

// rootFields merges the provenance over the root chunk's own fields. Every
// provenance field the Markdown gave is dropped first, set or not here, so a
// field that reads as provenance is the server's or absent.
func (p importProvenance) rootFields(own json.RawMessage) json.RawMessage {
	m := map[string]any{}
	if len(own) > 0 {
		if err := json.Unmarshal(own, &m); err != nil || m == nil {
			m = map[string]any{}
		}
	}
	for _, k := range importProvenanceFields {
		delete(m, k)
	}
	for k, v := range p.fields {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return b
}

// provenanceView is the provenance a root chunk's fields carry, for
// get_document and export_md; nil when they carry none.
func provenanceView(fields json.RawMessage) map[string]any {
	if len(fields) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(fields, &m) != nil {
		return nil
	}
	out := map[string]any{}
	for _, k := range importProvenanceFields {
		if v, ok := m[k]; ok && v != nil && v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
