package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/files/render"
)

// recordSink is a memSink that takes a loomcycle record and notes, for
// each heading anchor, how much text had been written when it was named.
type recordSink struct {
	memSink
	record  []byte
	anchors []string
	at      []int
}

func (s *recordSink) Record(b []byte) { s.record = append([]byte(nil), b...) }
func (s *recordSink) HeadingAnchor(name string) {
	s.anchors = append(s.anchors, name)
	s.at = append(s.at, s.md.Len())
}

// renderedWithRecord is a small document rendered by loomcycle with its
// record embedded (RFC GR P4b), in format.
func renderedWithRecord(t *testing.T, format string) ([]byte, *render.Embed) {
	t.Helper()
	b := render.NewBuilder("Services")
	b.Doc.Anchor = "chunk-root"
	b.Doc.Blocks = b.Body("Intro.")
	b.AddSection()
	b.AddSection()
	s1 := &render.Section{ID: "s1", Anchor: "chunk-s1", Title: "Basic", Level: 2, Blocks: b.Body("No test plan.")}
	s2 := &render.Section{ID: "s2", Anchor: "chunk-s2", Title: "Standard", Level: 2, Blocks: b.Body("A test plan.\n\n```\nnot the record\n```")}
	b.Doc.Sections = []*render.Section{s1, s2}
	d, _ := b.Finish()
	d.Embed = &render.Embed{Version: render.EmbedVersion, DocumentID: "doc1", Scope: "user", Title: "Services",
		Chunks: []render.EmbedChunk{{ID: "root", Anchor: "chunk-root"}, {ID: "s1", Anchor: "chunk-s1", Parent: "root"}, {ID: "s2", Anchor: "chunk-s2", Parent: "root"}}}
	var out bytes.Buffer
	if err := render.Write(context.Background(), &out, d, format, render.Options{}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), d.Embed
}

// An extractor finds the record a loomcycle render embedded and names each
// heading's anchor just before that heading's text; the record itself is
// never part of the text (RFC GR P4b, D22).
func TestExtract_FindsTheRecordAndEachHeadingsAnchor(t *testing.T) {
	for _, c := range []struct {
		format  string
		e       Extractor
		anchors []string
	}{
		{render.FormatDOCX, DOCX{}, []string{"chunk_s1", "chunk_s2"}},
		{render.FormatODT, ODT{}, []string{"chunk_s1", "chunk_s2"}},
		{render.FormatHTML, HTML{}, []string{"chunk-s1", "chunk-s2"}},
	} {
		data, want := renderedWithRecord(t, c.format)
		s := &recordSink{}
		if _, err := c.e.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), s, Options{Name: "x." + c.format}); err != nil {
			t.Fatalf("%s: %v", c.format, err)
		}
		var got render.Embed
		if err := json.Unmarshal(s.record, &got); err != nil || got.DocumentID != want.DocumentID || len(got.Chunks) != 3 {
			t.Errorf("%s: record = %s (%v)", c.format, s.record, err)
		}
		if strings.Join(s.anchors, ",") != strings.Join(c.anchors, ",") {
			t.Errorf("%s: anchors = %v, want %v", c.format, s.anchors, c.anchors)
			continue
		}
		md := s.md.String()
		for i, title := range []string{"Basic", "Standard"} {
			// HTML's <h2> comes one level below the document's "#" too.
			if rest := strings.TrimLeft(md[s.at[i]:], "\n"); !strings.HasPrefix(strings.TrimLeft(rest, "#"), " "+title) || !strings.HasPrefix(rest, "##") {
				t.Errorf("%s: anchor %s was named before %q, not its heading", c.format, s.anchors[i], firstLine(rest))
			}
		}
		if strings.Contains(md, "loomcycle_render") || strings.Contains(md, `"document_id"`) {
			t.Errorf("%s: the record reached the text:\n%s", c.format, md)
		}
		if !strings.Contains(md, "not the record") {
			t.Errorf("%s: a code block of the document was dropped:\n%s", c.format, md)
		}
	}
}

// A sink that takes no record gets none, and no anchors; a file with no
// record names none either.
func TestExtract_NoRecordNoAnchors(t *testing.T) {
	data, _ := renderedWithRecord(t, render.FormatDOCX)
	if _, err := (DOCX{}).Extract(context.Background(), bytes.NewReader(data), int64(len(data)), &memSink{}, Options{}); err != nil {
		t.Fatal(err)
	}
	plain := write(t, render.FormatHTML)
	s := &recordSink{}
	if _, err := (HTML{}).Extract(context.Background(), bytes.NewReader(plain), int64(len(plain)), s, Options{}); err != nil {
		t.Fatal(err)
	}
	if s.record != nil || len(s.anchors) != 0 {
		t.Errorf("a page with no record gave record %q and anchors %v", s.record, s.anchors)
	}
}

func write(t *testing.T, format string) []byte {
	t.Helper()
	b := render.NewBuilder("Plain")
	b.AddSection()
	b.Doc.Sections = []*render.Section{{ID: "a", Anchor: "chunk-a", Title: "A", Level: 2, Blocks: b.Body("x")}}
	d, _ := b.Finish()
	var out bytes.Buffer
	if err := render.Write(context.Background(), &out, d, format, render.Options{}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
