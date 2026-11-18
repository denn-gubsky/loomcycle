package http

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// exportFixture is a server with the Document and File tools on a real store
// and SQL Memory, its audit recorded.
func exportFixture(t *testing.T) (*httptest.Server, *recordingSink) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1}}
	cfg.Env.AuthToken = "test-token"
	docTool := &builtin.Document{Store: st, SqlMem: mgr, Bus: channels.NewBus()}
	fileTool := &builtin.File{Docs: docTool}
	srv := New(cfg, &stubResolver{}, []tools.Tool{&builtin.Path{Store: st}, docTool, fileTool}, concurrency.New(1, 1, time.Second), st)
	srv.SetSqlMem(mgr)
	sink := &recordingSink{}
	srv.tokenAudit = sink
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts, sink
}

// exportDoc imports a small document with a picture and returns its id.
func exportDoc(t *testing.T, ts *httptest.Server, withPicture bool) string {
	t.Helper()
	md := "# Протокол засідання\n\n## Порядок денний\n\nПерше питання <script>alert(1)</script>.\n"
	body, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": md})
	r := postAdmin(t, ts, "/v1/_document", string(body))
	var doc map[string]any
	_ = json.NewDecoder(r.Body).Decode(&doc)
	r.Body.Close()
	id, _ := doc["document_id"].(string)
	if id == "" {
		t.Fatalf("import_md: %v", doc)
	}
	if withPicture {
		root, _ := doc["root_chunk_id"].(string)
		r = postAdmin(t, ts, "/v1/_document", `{"op":"create_chunk","scope":"user","document_id":"`+id+`","parent_id":"`+root+`","title":"Схема"}`)
		var ch map[string]any
		_ = json.NewDecoder(r.Body).Decode(&ch)
		r.Body.Close()
		png := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
		r = postAdmin(t, ts, "/v1/_document", `{"op":"set_asset","scope":"user","id":"`+ch["id"].(string)+`","media_type":"image/png","data":"`+png+`"}`)
		if r.StatusCode != 200 {
			b, _ := io.ReadAll(r.Body)
			t.Fatalf("set_asset: %d %s", r.StatusCode, b)
		}
		r.Body.Close()
	}
	return id
}

func getExport(t *testing.T, ts *httptest.Server, query string) (*http.Response, []byte) {
	t.Helper()
	resp := getAsset(t, ts, "/v1/_document/export/"+query, true)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

// A document downloads as an attachment of its format, named after its
// title (UTF-8 kept), never sniffed, and the bulk read is audited.
func TestDocumentExport_ServesTheFileAsAnAttachment(t *testing.T) {
	ts, sink := exportFixture(t)
	id := exportDoc(t, ts, false)
	formats := map[string]string{
		"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"odt":  "application/vnd.oasis.opendocument.text",
		"epub": "application/epub+zip",
		"html": "text/html",
		"md":   "text/markdown",
	}
	for format, ctype := range formats {
		resp, body := getExport(t, ts, id+"?scope=user&format="+format+"&toc=true")
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d: %s", format, resp.StatusCode, body)
		}
		h := resp.Header
		if h.Get("Content-Type") != ctype || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: headers %v", format, h)
		}
		cd := h.Get("Content-Disposition")
		if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, "%D0%9F%D1%80%D0%BE%D1%82%D0%BE%D0%BA%D0%BE%D0%BB") || !strings.Contains(cd, "."+format) {
			t.Errorf("%s: Content-Disposition %q", format, cd)
		}
		if h.Get("X-Loomcycle-Render-Id") == "" {
			t.Errorf("%s: no render id", format)
		}
		if format == "html" && (bytes.Contains(body, []byte("<script>alert")) || !bytes.Contains(body, []byte("&lt;script&gt;"))) {
			t.Errorf("html: the body's script is not text")
		}
	}
	if len(sink.events) != len(formats) {
		t.Fatalf("audit events = %+v, want %d document_exported", sink.events, len(formats))
	}
	ev := sink.events[0]
	if ev.Action != "document_exported" || ev.TargetName != "document:"+id || ev.ExportBytes == 0 || ev.ExportScope != "user" || ev.ExportFormat == "" {
		t.Errorf("audit event = %+v", ev)
	}
}

// Markdown with pictures comes as a zip: the .md and its pictures beside it,
// linked where they sit.
func TestDocumentExport_MarkdownWithPicturesIsAZip(t *testing.T) {
	ts, _ := exportFixture(t)
	id := exportDoc(t, ts, true)
	resp, body := getExport(t, ts, id+"?format=md")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" || !strings.Contains(resp.Header.Get("Content-Disposition"), ".zip") {
		t.Fatalf("status %d, headers %v: %s", resp.StatusCode, resp.Header, body)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var md string
	pics := 0
	for _, f := range zr.File {
		switch {
		case strings.HasSuffix(f.Name, ".md"):
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			md = string(b)
		case strings.Contains(f.Name, "/img-"):
			pics++
		}
	}
	if md == "" || pics != 1 || !strings.Contains(md, "/img-1.png") || !strings.Contains(md, "Перше питання") {
		t.Errorf("zip: md %q, %d pictures, entries %v", md, pics, zr.File)
	}
}

// A document that is missing, or not in the scope asked, is the same opaque
// 404; a bad parameter is a 400 saying what to pass; no bearer is a 401.
func TestDocumentExport_RefusesWhatItMayNotServe(t *testing.T) {
	ts, sink := exportFixture(t)
	id := exportDoc(t, ts, false)
	for name, q := range map[string]string{
		"missing":     "deadbeefdeadbeef?format=docx",
		"other scope": id + "?format=docx&scope=agent",
	} {
		resp, body := getExport(t, ts, q)
		if resp.StatusCode != 404 || bytes.Contains(body, []byte(id)) {
			t.Errorf("%s: %d %s, want an opaque 404", name, resp.StatusCode, body)
		}
	}
	for name, q := range map[string]string{
		"no format":      id,
		"unknown format": id + "?format=rtf",
		"page size":      id + "?format=docx&page_size=a3",
		"scope":          id + "?format=docx&scope=everyone",
		"metadata":       id + "?format=docx&metadata=all",
	} {
		if resp, body := getExport(t, ts, q); resp.StatusCode != 400 {
			t.Errorf("%s: %d %s, want 400", name, resp.StatusCode, body)
		}
	}
	resp := getAsset(t, ts, "/v1/_document/export/"+id+"?format=docx", false)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("no bearer: %d, want 401", resp.StatusCode)
	}
	if len(sink.events) != 0 {
		t.Errorf("a refused export was audited: %+v", sink.events)
	}
}

// The bulk read is recorded or it is not served.
func TestDocumentExport_NotServedWhenItsAuditCannotBeWritten(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1}}
	cfg.Env.AuthToken = "test-token"
	docTool := &builtin.Document{Store: st, SqlMem: mgr, Bus: channels.NewBus()}
	srv := New(cfg, &stubResolver{}, []tools.Tool{&builtin.Path{Store: st}, docTool, &builtin.File{Docs: docTool}}, concurrency.New(1, 1, time.Second), st)
	srv.SetSqlMem(mgr)
	srv.tokenAudit = failingSink{errAuditDown}
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	id := exportDoc(t, ts, false)
	resp, body := getExport(t, ts, id+"?format=html")
	if resp.StatusCode == 200 || bytes.Contains(body, []byte("Протокол")) {
		t.Fatalf("served without an audit record: %d %s", resp.StatusCode, body)
	}
}

func TestIsDocumentExportPath_MatchesOnlyTheRoute(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/_document/export/abc":      true,
		"/v1/_document/export/":         false,
		"/v1/_document/export/a/b":      false,
		"/v1/_document/abc/export":      false,
		"/v1/_document/asset/abc":       false,
		"/v1/_documentsourcedef/export": false,
	} {
		if got := isDocumentExportPath(path); got != want {
			t.Errorf("isDocumentExportPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// metadata=embed puts the document's record into the downloaded file (RFC GR
// P4b); without it the file carries none.
func TestDocumentExport_MetadataEmbedIsOptIn(t *testing.T) {
	ts, _ := exportFixture(t)
	id := exportDoc(t, ts, false)
	resp, body := getExport(t, ts, id+"?scope=user&format=html&metadata=embed")
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte(`<pre id="loomcycle-metadata" hidden>`)) || !bytes.Contains(body, []byte(id)) {
		t.Fatalf("metadata=embed: %d, the page has no record:\n%s", resp.StatusCode, body)
	}
	if _, body := getExport(t, ts, id+"?scope=user&format=html"); bytes.Contains(body, []byte("loomcycle-metadata")) {
		t.Error("a download without metadata carries a record")
	}
}

// ?style= writes the download in a house style of the tenant (RFC GR P4c);
// a style that is not there is a bad parameter, the same 400 for one that
// is missing as for one the person may not read.
func TestDocumentExport_WritesAHouseStyle(t *testing.T) {
	ts, _ := exportFixture(t)
	id := exportDoc(t, ts, false)
	style := "# House\n\n```json\n{\"loomcycle_style\": 1, \"footer\": \"Acme internal\", \"colors\": {\"link\": \"#aa0000\"}}\n```\n"
	body, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "tenant", "path": "/render/styles/house", "markdown": style})
	r := postAdmin(t, ts, "/v1/_document", string(body))
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("put style: %d", r.StatusCode)
	}
	resp, page := getExport(t, ts, id+"?scope=user&format=html&style=/render/styles/house")
	if resp.StatusCode != 200 || !bytes.Contains(page, []byte(`<p class="style-footer">Acme internal</p>`)) || !bytes.Contains(page, []byte("a{color:#aa0000}")) {
		t.Fatalf("styled download: %d\n%s", resp.StatusCode, page)
	}
	if resp, body := getExport(t, ts, id+"?scope=user&format=html&style=/render/styles/missing"); resp.StatusCode != 400 || !bytes.Contains(body, []byte("no style at")) {
		t.Errorf("a missing style: %d %s, want 400", resp.StatusCode, body)
	}
}
