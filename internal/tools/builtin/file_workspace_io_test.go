package builtin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/cryptoops"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// ioFixture is an fcFixture whose File tool also imports (Document) and
// transfers (HTTP, on loopback).
func ioFixture(t *testing.T, l lifecycle.Limits) (*fcFixture, *Document) {
	t.Helper()
	f := newFCFixture(t, inbox.Service{}, l)
	d, _, _ := documentFixture(t)
	f.file.Docs = d
	f.file.HTTP = localHTTP()
	f.ctx = tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Tools = append(p.Tools, "Document", "HTTP") })
	f.open(t, "w")
	return f, d
}

func (f *fcFixture) wsPutBytes(t *testing.T, name, p string, b []byte) {
	t.Helper()
	if _, err := f.ws.tool.Manager.PutFile(context.Background(), f.owner(), name, workspace.PutArgs{Path: p, Size: int64(len(b))}, bytes.NewReader(b)); err != nil {
		t.Fatalf("put %s:%s: %v", name, p, err)
	}
}

func bytesSHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ---- import ----

// A Markdown file in a workspace with two relative image links becomes a
// document holding both images, and its provenance names the workspace (by
// name and id), the run and the file's digest, tagged source/workspace. A
// link with `..`, an absolute one and a workspace reference the text spelled
// out itself stay text: only relative links inside the workspace are read.
func TestFileImport_FromAWorkspaceKeepsItsImagesAndNamesTheWorkspaceAndRun(t *testing.T) {
	f, d := ioFixture(t, lifecycle.Limits{})
	png := testPNG(t)
	f.wsPutBytes(t, "w", "out/img/one.png", png)
	f.wsPutBytes(t, "w", "out/two.png", png)
	f.wsPutBytes(t, "w", "secret.png", png)
	md := "# Report\n\nIntro text.\n\n![First](img/one.png)\n\n## Part\n\nMore text.\n\n![Second](./two.png)\n\n" +
		"## Elsewhere\n\n![Up](../secret.png)\n\n![Abs](/secret.png)\n\n## Spelled\n\n![Spelled](workspace:w:secret.png)\n"
	f.wsPut(t, "w", "out/report.md", md)

	out, res := f.exec(t, f.ctx, map[string]any{"op": "import", "path": "workspace:w:out/report.md"})
	if res.IsError {
		t.Fatalf("import: %s", res.Text)
	}
	if asInt(out["image_chunks"]) != 2 {
		t.Fatalf("image_chunks = %v, want 2: %v", out["image_chunks"], out)
	}
	docID := asStr(out["document_id"])
	for _, title := range []string{"First", "Second"} {
		c, _, asset := importedChunk(t, d, f.ctx, docID, title)
		if c["type"] != "image" || asset == nil || asInt(asset["size"]) != len(png) {
			t.Errorf("%s is not an image chunk with the picture: %v %v", title, c, asset)
		}
	}
	_, body, _ := importedChunk(t, d, f.ctx, docID, "Elsewhere")
	for _, want := range []string{"](../secret.png)", "](/secret.png)"} {
		if !strings.Contains(body, want) {
			t.Errorf("Elsewhere's body %q lost the link %s", body, want)
		}
	}
	// A reference the text spelled out, as a section's whole body, is not
	// one the import wrote: it is not read.
	if c, body, asset := importedChunk(t, d, f.ctx, docID, "Spelled"); asset != nil || !strings.Contains(body, "workspace:w:secret.png") {
		t.Errorf("the spelled-out reference was attached: %v %q %v", c, body, asset)
	}

	got, r := docExec(t, d, f.ctx, `{"op":"get_document","scope":"user","id":"`+docID+`"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	row, err := f.ws.tool.Manager.Get(context.Background(), f.owner(), "w")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := got["provenance"].(map[string]any)
	for k, want := range map[string]any{
		"captured_from": "workspace:w", "workspace_id": row.ID, "run_id": f.runID,
		"sha256": bytesSHA256([]byte(md)), "file": "report.md",
	} {
		if p[k] != want {
			t.Errorf("provenance %s = %v, want %v", k, p[k], want)
		}
	}
	if tags, _ := got["tags"].([]any); !slices.Contains(tags, any("source/workspace")) || !slices.Contains(tags, any("untrusted")) {
		t.Errorf("tags = %v", got["tags"])
	}
	if out["source"] != "workspace:w" {
		t.Errorf("source = %v", out["source"])
	}
}

// Document import_md itself never reads a workspace image reference: the
// form is attached only by File's import from a workspace, which supplies
// the reader. Through the Document tool it is the text it always was.
func TestDocumentImportMD_AWorkspaceImageReferenceStaysText(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out := importMarkdown(t, d, ctx, "# Doc\n\n## Pic\n\n![x](workspace:w:a.png)\n")
	if asInt(out["image_chunks"]) != 0 {
		t.Fatalf("import_md attached a workspace image: %v", out)
	}
	if _, body, _ := importedChunk(t, d, ctx, asStr(out["document_id"]), "Pic"); body != "![x](workspace:w:a.png)" {
		t.Errorf("body = %q, want the reference as written", body)
	}
}

// A Workspace hook judges each image an import reads (via File, op read, on
// the image's own path), and an image it refuses is left as a link.
func TestFileImport_AWorkspaceHookJudgesEachImage(t *testing.T) {
	f, _ := ioFixture(t, lifecycle.Limits{})
	png := testPNG(t)
	f.wsPutBytes(t, "w", "a.png", png)
	f.wsPutBytes(t, "w", "b.png", png)
	f.wsPut(t, "w", "doc.md", "# Doc\n\n## A\n\n![A](a.png)\n\n## B\n\n![B](b.png)\n")
	var seen []string
	ctx := tools.WithHookPreCheck(f.ctx, func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
		var in struct{ Op, Path, Via string }
		_ = json.Unmarshal(input, &in)
		if name == "Workspace" {
			seen = append(seen, in.Op+":"+in.Path+":"+in.Via)
			if in.Path == "b.png" {
				return tools.Result{Text: "not b", IsError: true}, false
			}
		}
		return tools.Result{}, true
	})
	out, res := f.exec(t, ctx, map[string]any{"op": "import", "path": "workspace:w:doc.md"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if asInt(out["image_chunks"]) != 1 || !strings.Contains(fmt.Sprint(out["skipped_images"]), "not b") {
		t.Errorf("import = %v", out)
	}
	for _, want := range []string{"read:doc.md:File", "read:a.png:File", "read:b.png:File"} {
		if !slices.Contains(seen, want) {
			t.Errorf("hooks saw %v, want %s", seen, want)
		}
	}
}

// ---- download ----

// A download into a workspace streams the body into a put: into a folder it
// takes the response's name, and a body of unknown length arrives too.
func TestFileDownload_IntoAWorkspaceWritesTheFile(t *testing.T) {
	f, _ := ioFixture(t, lifecycle.Limits{})
	srv, _ := countingServer(t, map[string]string{"Content-Type": "application/pdf"}, binaryPDF)
	out, res := f.exec(t, f.ctx, map[string]any{"op": "download", "url": srv.URL + "/a.pdf", "to": "workspace:w:in/"})
	if res.IsError {
		t.Fatalf("download: %s", res.Text)
	}
	if out["ref"] != "workspace:w:in/a.pdf" || out["sha256"] != bytesSHA256(binaryPDF) || out["media_type"] != "application/pdf" {
		t.Errorf("result = %v", out)
	}
	if got, _ := f.wsGet(t, "w", "in/a.pdf"); got != string(binaryPDF) {
		t.Errorf("the workspace holds %d bytes, want the body", len(got))
	}

	// No Content-Length: a flushed, chunked response.
	chunked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(binaryPDF[:10])
		w.(http.Flusher).Flush()
		_, _ = w.Write(binaryPDF[10:])
	}))
	t.Cleanup(chunked.Close)
	f.file.ScratchDir = t.TempDir()
	out, res = f.exec(t, f.ctx, map[string]any{"op": "download", "url": chunked.URL + "/b.pdf", "to": "workspace:w:b.pdf"})
	if res.IsError {
		t.Fatalf("chunked download: %s", res.Text)
	}
	if got, _ := f.wsGet(t, "w", "b.pdf"); got != string(binaryPDF) || out["size"] != float64(len(binaryPDF)) {
		t.Errorf("chunked: %v, %d bytes in the workspace", out, len(got))
	}
	if ents, _ := os.ReadDir(f.file.ScratchDir); len(ents) != 0 {
		t.Errorf("the spool was left: %v", ents)
	}
}

// A download into a workspace over its quota is refused and leaves nothing.
func TestFileDownload_IntoAWorkspaceOverTheQuotaLeavesNothing(t *testing.T) {
	f, _ := ioFixture(t, lifecycle.Limits{DiskQuotaBytes: 1000})
	srv, _ := countingServer(t, nil, bytes.Repeat([]byte("q"), 1500))
	_, res := f.exec(t, f.ctx, map[string]any{"op": "download", "url": srv.URL + "/big.bin", "to": "workspace:w:big.bin"})
	wantFileRefusal(t, "download over quota", res, workspace.ClassOverQuota)
	ls, res := f.exec(t, f.ctx, map[string]any{"op": "ls", "path": "workspace:w:", "recursive": true})
	if res.IsError || len(ls["entries"].([]any)) != 0 {
		t.Errorf("after a refused download the workspace holds %v %s", ls, res.Text)
	}
}

// ---- upload ----

// An upload from a workspace sends the bytes a volume upload of the same
// file sends, single and ranged, and the HTTP hook sees the same sha256.
func TestFileUpload_FromAWorkspaceSendsWhatAVolumeSends(t *testing.T) {
	f, _ := ioFixture(t, lifecycle.Limits{})
	data := make([]byte, 3<<20+12345)
	for i := range data {
		data[i] = byte(i*31 + i>>9)
	}
	f.wsPutBytes(t, "w", "deck.bin", data)
	writeFiles(t, f.docs, map[string]string{"deck.bin": string(data)})

	var received [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var a, total int64
		if cr := r.Header.Get("Content-Range"); cr != "" {
			var e int64
			_, _ = fmt.Sscanf(cr, "bytes %d-%d/%d", &a, &e, &total)
			if a == 0 {
				received = append(received, make([]byte, total))
			}
			copy(received[len(received)-1][a:], b)
			if e+1 < total {
				w.WriteHeader(http.StatusAccepted)
				return
			}
		} else {
			received = append(received, b)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	var digests []string
	ctx := tools.WithHookPreCheck(f.ctx, func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
		if name == "HTTP" {
			var in struct{ Body struct{ SHA256 string } }
			_ = json.Unmarshal(input, &in)
			digests = append(digests, in.Body.SHA256)
		}
		return tools.Result{}, true
	})
	for _, from := range []string{"workspace:w:deck.bin", "volume:docs:deck.bin"} {
		for _, ranged := range []bool{false, true} {
			out, res := f.exec(t, ctx, map[string]any{"op": "upload", "from": from, "url": srv.URL + "/up", "ranged": ranged, "chunk_bytes": 1 << 20})
			if res.IsError {
				t.Fatalf("upload %s ranged=%v: %s", from, ranged, res.Text)
			}
			if out["size"] != float64(len(data)) || out["status"] != float64(201) {
				t.Errorf("upload %s ranged=%v = %v", from, ranged, out)
			}
		}
	}
	if len(received) != 4 {
		t.Fatalf("the server got %d uploads, want 4", len(received))
	}
	for i, b := range received {
		if !bytes.Equal(b, data) {
			t.Errorf("upload %d: the server got %d bytes that differ from the file", i, len(b))
		}
	}
	want := bytesSHA256(data)
	if len(digests) != 4 || slices.ContainsFunc(digests, func(d string) bool { return d != want }) {
		t.Errorf("the HTTP hook saw digests %v, want %s each time", digests, want)
	}
}

// ---- Crypto ----

// cryptoWS is a Crypto tool on the fixture's workspaces, with a stored AES
// key "files" in the tenant scope of the fixture's run.
func cryptoWS(t *testing.T, f *fcFixture) (*Crypto, context.Context) {
	t.Helper()
	cd, _ := credFixture(t, "alice", testKEK())
	ctx := tools.UpdateRunPolicy(f.ctx, func(rp *tools.RunPolicy) {
		rp.Crypto = tools.CryptoPolicyValue{Scopes: []string{"tenant"}}
		rp.Tools = append(rp.Tools, "Crypto")
	})
	// Keys are made in the tenant scope off a run (inside one only the agent
	// scope is writable); the run then uses it.
	gen := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_alice", UserID: "alice", TenantID: "acme"})
	gen = tools.UpdateRunPolicy(gen, func(rp *tools.RunPolicy) { rp.Crypto = tools.CryptoPolicyValue{Scopes: []string{"tenant"}} })
	_, res := credExec(t, cd, gen, `{"op":"generate","scope":"tenant","name":"files","key_type":"aes-256-gcm"}`)
	mustOK(t, "generate", res)
	return &Crypto{Engine: cd.Engine, Workspaces: f.ws.tool.Manager}, ctx
}

// Crypto hashes a workspace file, encrypts one into a file of the
// workspace and decrypts it back, the data never in a result; its hooks
// are judged as Workspace calls via Crypto.
func TestCrypto_FromAndToAWorkspaceKeepTheDataOutOfTheResult(t *testing.T) {
	f := newFCFixture(t, inbox.Service{}, lifecycle.Limits{})
	f.open(t, "w")
	secret := append([]byte("SECRET-WS-BODY\x00\xff"), bytes.Repeat([]byte{9}, 200)...)
	f.wsPutBytes(t, "w", "in.bin", secret)
	c, ctx := cryptoWS(t, f)
	var seen []string
	ctx = tools.WithHookPreCheck(ctx, func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
		var in struct{ Op, Path, Via string }
		_ = json.Unmarshal(input, &in)
		seen = append(seen, name+":"+in.Op+":"+in.Path+":"+in.Via)
		return tools.Result{}, true
	})

	out, res := cryptoExec(t, c, ctx, `{"op":"hash","from":"workspace:w:in.bin"}`)
	mustOK(t, "hash", res)
	want, _ := cryptoops.Hash("sha256", secret)
	if !strings.Contains(res.Text, encodeHex(want)) {
		t.Errorf("hash of the workspace file = %v", out)
	}
	out, res = cryptoExec(t, c, ctx, `{"op":"encrypt","key":"files","from":"workspace:w:in.bin","to":"workspace:w:out/in.bin.lcx"}`)
	mustOK(t, "encrypt", res)
	if strings.Contains(res.Text, "SECRET") || strings.Contains(res.Text, "lcx1:") || out["to"] != "workspace:w:out/in.bin.lcx" {
		t.Fatalf("encrypt result leaks or is incomplete: %s", res.Text)
	}
	if stored, _ := f.wsGet(t, "w", "out/in.bin.lcx"); !strings.HasPrefix(stored, "lcx1:kt:files:1:") {
		t.Fatalf("stored ciphertext %q", stored)
	}
	_, res = cryptoExec(t, c, ctx, `{"op":"decrypt","from":"workspace:w:out/in.bin.lcx","to":"workspace:w:back.bin"}`)
	mustOK(t, "decrypt", res)
	if got, _ := f.wsGet(t, "w", "back.bin"); got != string(secret) {
		t.Error("the decrypted file differs from the original")
	}
	for _, w := range []string{"Workspace:read:in.bin:Crypto", "Workspace:write:out/in.bin.lcx:Crypto", "Workspace:write:back.bin:Crypto"} {
		if !slices.Contains(seen, w) {
			t.Errorf("hooks saw %v, want %s", seen, w)
		}
	}
}

// ---- grants, hooks and pins: refused before any byte moves ----

// No workspace grant refuses every reference; a read-only grant refuses a
// workspace target and lets a source through; a Workspace hook on the final
// path refuses; another run's workspace is not found. Nothing is requested
// from the server and nothing is written.
func TestFileTransfers_GrantsHooksAndPinsRefuseBeforeAnyByteMoves(t *testing.T) {
	f, _ := ioFixture(t, lifecycle.Limits{})
	f.wsPut(t, "w", "doc.md", "# Doc\n")
	f.wsPutBytes(t, "w", "a.bin", []byte("abc"))
	srv, hits := countingServer(t, nil, binaryPDF)
	c, cctx := cryptoWS(t, f)

	none := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace = tools.WorkspacePolicyValue{} })
	readOnly := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace = tools.WorkspacePolicyValue{Read: true} })
	hook := func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
		var in struct{ Path string }
		_ = json.Unmarshal(input, &in)
		if name == "Workspace" && slices.Contains([]string{"got.pdf", "a.bin", "doc.md", "x.lcx"}, in.Path) {
			return tools.Result{Text: "hook says no", IsError: true}, false
		}
		return tools.Result{}, true
	}
	hooked := tools.WithHookPreCheck(f.ctx, hook)
	other, _ := f.runOf(t, f.ctx, "bob")
	other = tools.UpdateRunPolicy(other, func(p *tools.RunPolicy) { p.Tools = append(p.Tools, "Document", "HTTP", "Crypto") })

	file := []map[string]any{
		{"op": "import", "path": "workspace:w:doc.md"},
		{"op": "download", "url": srv.URL + "/a.pdf", "to": "workspace:w:got.pdf"},
		{"op": "upload", "from": "workspace:w:a.bin", "url": srv.URL + "/up"},
	}
	for _, in := range file {
		_, res := f.exec(t, none, in)
		wantFileRefusal(t, "no grant: "+in["op"].(string), res, "workspace grant does not allow")
		_, res = f.exec(t, hooked, in)
		wantFileRefusal(t, "hooked: "+in["op"].(string), res, "hook says no")
		_, res = f.exec(t, other, in)
		wantFileRefusal(t, "other run: "+in["op"].(string), res, `no workspace named "w"`)
	}
	_, res := f.exec(t, readOnly, file[1])
	wantFileRefusal(t, "read only: download", res, "does not allow write")

	crypto := []string{`{"op":"hash","from":"workspace:w:a.bin"}`, `{"op":"encrypt","key":"files","plaintext":"x","to":"workspace:w:x.lcx"}`}
	for _, body := range crypto {
		_, res := cryptoExec(t, c, tools.UpdateRunPolicy(cctx, func(p *tools.RunPolicy) { p.Workspace = tools.WorkspacePolicyValue{} }), body)
		wantFileRefusal(t, "crypto no grant: "+body, res, "workspace grant does not allow")
		_, res = cryptoExec(t, c, tools.WithHookPreCheck(cctx, hook), body)
		wantFileRefusal(t, "crypto hooked: "+body, res, "hook says no")
		octx := tools.UpdateRunPolicy(other, func(rp *tools.RunPolicy) { rp.Crypto = tools.CryptoPolicyValue{Scopes: []string{"tenant"}} })
		_, res = cryptoExec(t, c, octx, body)
		wantFileRefusal(t, "crypto other run: "+body, res, `no workspace named "w"`)
	}
	_, res = cryptoExec(t, c, tools.UpdateRunPolicy(cctx, func(p *tools.RunPolicy) { p.Workspace = tools.WorkspacePolicyValue{Read: true} }), crypto[1])
	wantFileRefusal(t, "crypto read only: to", res, "does not allow write")

	if hits.Load() != 0 {
		t.Errorf("refused calls reached the server %d times", hits.Load())
	}
	if f.wsHas(t, "w", "got.pdf") || f.wsHas(t, "w", "x.lcx") {
		t.Error("a refused call wrote the workspace")
	}

	// A folder target's file name comes with the response: its hook runs
	// then, before a byte of the body is written.
	_, res = f.exec(t, tools.WithHookPreCheck(f.ctx, func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
		var in struct{ Path string }
		_ = json.Unmarshal(input, &in)
		if name == "Workspace" && in.Path == "dl/a.pdf" {
			return tools.Result{Text: "hook says no", IsError: true}, false
		}
		return tools.Result{}, true
	}), map[string]any{"op": "download", "url": srv.URL + "/a.pdf", "to": "workspace:w:dl/"})
	wantFileRefusal(t, "hooked folder download", res, "hook says no")
	if f.wsHas(t, "w", "dl") {
		t.Error("a refused folder download wrote the workspace")
	}
}
