package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"regexp"
	"strings"
	"testing"
)

// embedDoc is the fixture with a record whose fields try to break out of
// every carrier: a comment closer, markup, a Typst string end, a CDATA end.
func embedDoc(t *testing.T) *Doc {
	t.Helper()
	d := fixtureDoc(t)
	hostile := json.RawMessage(`{"note":"--> <script>alert(1)</script> \"); #panic(\"x\") ]]>","n":3}`)
	d.Embed = &Embed{Version: EmbedVersion, DocumentID: "doc1", Scope: "tenant", Title: d.Title, Date: d.Date,
		Chunks: []EmbedChunk{
			{ID: "root", Anchor: "chunk-root", Type: "rfc", Status: "draft", Tags: []string{"board"}, Fields: hostile},
			{ID: "s1", Anchor: "chunk-s1", Parent: "root", Type: "section"},
			{ID: "s2", Anchor: "chunk-s2", Parent: "root"},
			{ID: "s21", Anchor: "chunk-s21", Parent: "s2", Status: "review"},
		},
		Relations: []EmbedRelation{{From: "s1", To: "s2", Kind: "cites"}, {From: "s21", To: "s1", Kind: "related", Status: "proposed"}},
	}
	return d
}

func sameEmbed(t *testing.T, format string, got []byte, want *Embed) {
	t.Helper()
	var e Embed
	if err := json.Unmarshal(got, &e); err != nil {
		t.Fatalf("%s: the record does not parse: %v\n%s", format, err, got)
	}
	a, _ := json.Marshal(e)
	b, _ := json.Marshal(want)
	if !bytes.Equal(a, b) {
		t.Errorf("%s: the record changed on the way:\n got %s\nwant %s", format, a, b)
	}
}

// With metadata embed every format carries the whole record, unchanged, in
// its own carrier (D21), and nothing in a field becomes syntax.
func TestEmbed_EveryFormatCarriesTheRecord(t *testing.T) {
	d := embedDoc(t)

	h := string(write(t, d, FormatHTML, Options{}))
	m := regexp.MustCompile(`<pre id="loomcycle-metadata" hidden>([^<]*)</pre>`).FindStringSubmatch(h)
	if m == nil {
		t.Fatalf("html: no record:\n%s", h)
	}
	sameEmbed(t, "html", []byte(html.UnescapeString(m[1])), d.Embed)
	if !strings.Contains(h, `<section id="chunk-s1" data-loom-chunk="s1">`) {
		t.Error("html: a section does not name its chunk")
	}
	assertInertHTML(t, h)

	for format, name := range map[string]string{FormatODT: EmbedName, FormatEPUB: "META-INF/" + EmbedName} {
		parts := docxParts(t, write(t, d, format, Options{}))
		sameEmbed(t, format, []byte(parts[name]), d.Embed)
	}
	if man := docxParts(t, write(t, d, FormatODT, Options{}))["META-INF/manifest.xml"]; !strings.Contains(man, `manifest:full-path="loomcycle.json"`) {
		t.Errorf("odt: the manifest does not list the record:\n%s", man)
	}

	parts := docxParts(t, write(t, d, FormatDOCX, Options{}))
	var item struct {
		Text string `xml:",chardata"`
	}
	if err := xmlDecoder(parts["customXml/item1.xml"]).Decode(&item); err != nil {
		t.Fatalf("docx: the custom XML part does not parse: %v", err)
	}
	sameEmbed(t, "docx", []byte(item.Text), d.Embed)
	for name, want := range map[string]string{
		"word/_rels/document.xml.rels":   `Target="../customXml/item1.xml"`,
		"[Content_Types].xml":            `PartName="/customXml/itemProps1.xml"`,
		"customXml/_rels/item1.xml.rels": `Target="itemProps1.xml"`,
	} {
		if !strings.Contains(parts[name], want) {
			t.Errorf("docx: %s lacks %s", name, want)
		}
		if err := xmlWellFormed(parts[name]); err != nil {
			t.Errorf("docx: %s is not well-formed: %v", name, err)
		}
	}

	md := string(write(t, d, FormatMarkdown, Options{}))
	for _, want := range []string{
		"# Протокол засідання — Board minutes\n<!-- loom: {",
		`"id":"root"`, `"type":"rfc"`,
		"## Порядок денний\n<!-- loom: {\"id\":\"s1\",\"type\":\"section\"} -->",
		"<!-- loom-edges:\ns1 -> s2 [cites]\ns21 -> s1 [related] {\"status\":\"proposed\"}\n-->",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("md lacks %q:\n%s", want, md)
		}
	}
	// A field's text cannot end its comment: the root's comment is one line
	// that parses whole, its "-->" and "<" escaped inside the JSON.
	root := regexp.MustCompile(`(?m)^<!-- loom: (\{"fields".*\}) -->$`).FindStringSubmatch(md)
	if root == nil || strings.Contains(md, "--> <script>") {
		t.Fatalf("md: the root's comment was broken out of:\n%s", md)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(root[1]), &meta); err != nil || meta["id"] != "root" {
		t.Errorf("md: the root's comment does not parse (%v): %s", err, root[1])
	}

	src := typSource(t, d, Options{})
	if !strings.Contains(src, `#pdf.attach("loomcycle.json", bytes("`) {
		t.Fatalf("typst: no attachment:\n%s", src)
	}
	if code := outsideStrings(t, src); strings.Contains(code, "panic") || strings.Contains(code, "alert") {
		t.Errorf("typst: a field reached the source outside a string:\n%s", code)
	}
}

// Without metadata embed no format carries a record: a file people send
// holds no ids (D21).
func TestEmbed_NoneUnlessAsked(t *testing.T) {
	d := fixtureDoc(t)
	if h := string(write(t, d, FormatHTML, Options{})); strings.Contains(h, "loomcycle-metadata") || strings.Contains(h, "data-loom") {
		t.Error("html carries a record")
	}
	for _, format := range []string{FormatODT, FormatEPUB, FormatDOCX} {
		for name := range docxParts(t, write(t, d, format, Options{})) {
			if strings.Contains(name, EmbedName) || strings.HasPrefix(name, "customXml/") {
				t.Errorf("%s carries %s", format, name)
			}
		}
	}
	if md := string(write(t, d, FormatMarkdown, Options{})); strings.Contains(md, "<!-- loom") {
		t.Error("md carries a record")
	}
	if src := typSource(t, d, Options{}); strings.Contains(src, "pdf.attach") {
		t.Error("typst carries a record")
	}
}

func TestEmbed_TooLargeIsRefused(t *testing.T) {
	d := fixtureDoc(t)
	big, _ := json.Marshal(map[string]string{"x": strings.Repeat("a", MaxEmbed)})
	d.Embed = &Embed{Version: EmbedVersion, DocumentID: "doc1", Scope: "user", Chunks: []EmbedChunk{{ID: "root", Anchor: "chunk-root", Fields: big}}}
	for _, format := range []string{FormatHTML, FormatODT, FormatEPUB, FormatDOCX} {
		if err := Write(context.Background(), &bytes.Buffer{}, d, format, Options{}); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: %v, want ErrTooLarge", format, err)
		}
	}
}

// A worker takes the record from the bundle as the core wrote it, and
// refuses one the core never makes.
func TestEmbed_BundleCarriesItAndRefusesAForgedOne(t *testing.T) {
	d := embedDoc(t)
	b, err := EncodeBundle(d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := DecodeBundle(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(got.Embed)
	sameEmbed(t, "bundle", js, d.Embed)

	for name, forge := range map[string]func(e *Embed){
		"version":      func(e *Embed) { e.Version = 9 },
		"scope":        func(e *Embed) { e.Scope = "everyone" },
		"chunk id":     func(e *Embed) { e.Chunks[0].ID = "a b" },
		"anchor":       func(e *Embed) { e.Chunks[1].Anchor = `x" onclick="y` },
		"fields":       func(e *Embed) { e.Chunks[0].Fields = json.RawMessage(`"not an object"`) },
		"kind":         func(e *Embed) { e.Relations[0].Kind = "cites] -> x" },
		"relation end": func(e *Embed) { e.Relations[0].To = "s2\n-->" },
	} {
		d := embedDoc(t)
		forge(d.Embed)
		b, err := EncodeBundle(d, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := DecodeBundle(bytes.NewReader(b), int64(len(b))); !errors.Is(err, ErrBadBundle) {
			t.Errorf("%s: %v, want ErrBadBundle", name, err)
		}
	}
}
