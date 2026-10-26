package http

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/dynvol"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// fileRouteFixture is the File twin's fixture behind the real mux and token
// auth, with one token per kind of caller.
type fileRouteFixture struct {
	*fileTwinFixture
	mux http.Handler
}

const (
	fileTokOwner    = "lct_file_owner"
	fileTokReader   = "lct_file_reader"
	fileTokOperator = "lct_file_operator"
	fileTokOutsider = "lct_file_outsider"
	fileTokNoRead   = "lct_file_noread"
)

func newFileRouteFixture(t *testing.T) *fileRouteFixture {
	t.Helper()
	f := newFileTwinFixture(t)
	cfg := f.srv.cfg()
	cfg.Env.AuthToken = "legacy-file-route"
	cfg.Env.OperatorTokenPepper = testTokenPepper
	seedToken(t, f.st, fileTokOwner, "acme", "alice", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, time.Time{})
	seedToken(t, f.st, fileTokReader, "acme", "dave", []string{auth.ScopeRunsRead}, time.Time{})
	seedToken(t, f.st, fileTokOperator, "acme", "bob", []string{auth.ScopeTenant}, time.Time{})
	seedToken(t, f.st, fileTokOutsider, "evil", "mallory", []string{auth.ScopeTenant}, time.Time{})
	seedToken(t, f.st, fileTokNoRead, "acme", "erin", []string{auth.ScopeChannelPublish}, time.Time{})
	return &fileRouteFixture{fileTwinFixture: f, mux: f.srv.Mux()}
}

// do sends one request through the mux. target is sent as given, so an
// encoded or unclean path reaches the mux unchanged.
func (f *fileRouteFixture) do(t *testing.T, method, target, token, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func inboxURL(runID, rel string) string { return "/v1/runs/" + runID + "/inbox/" + rel }

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// POST /v1/runs/{run_id}/file runs the twin on the run the path names, and
// answers as the other tool routes do: the tool's JSON on success, a 422
// envelope when it refuses.
func TestFileRoute_RunsTheTwinOnThePathsRun(t *testing.T) {
	f := newFileRouteFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "report.pdf", "mcp:ms365/get-attachment", []byte("%PDF-1.7\nhello"), true)

	resp := f.do(t, "POST", "/v1/runs/"+runID+"/file", fileTokReader, `{"op":"stat","path":"`+rec.Ref+`"}`, nil)
	var out map[string]any
	if resp.Code != http.StatusOK || json.Unmarshal(resp.Body.Bytes(), &out) != nil || out["sha256"] != rec.SHA256 {
		t.Fatalf("stat = %d %s", resp.Code, resp.Body.String())
	}
	// run_id in the body may repeat the path's; it may not name another run.
	if resp := f.do(t, "POST", "/v1/runs/"+runID+"/file", fileTokReader, `{"run_id":"`+runID+`","op":"ls"}`, nil); resp.Code != http.StatusOK {
		t.Errorf("ls with the same run_id = %d %s", resp.Code, resp.Body.String())
	}
	if resp := f.do(t, "POST", "/v1/runs/"+runID+"/file", fileTokReader, `{"run_id":"run_other","op":"ls"}`, nil); resp.Code != http.StatusBadRequest {
		t.Errorf("ls with another run_id = %d %s, want 400", resp.Code, resp.Body.String())
	}
	// The strict decoding of the twin still applies to the body.
	if resp := f.do(t, "POST", "/v1/runs/"+runID+"/file", fileTokReader, `{"op":"stat","op":"rm","path":"`+rec.Ref+`"}`, nil); resp.Code != http.StatusUnprocessableEntity || !strings.Contains(resp.Body.String(), "more than once") {
		t.Errorf("a doubled op = %d %s", resp.Code, resp.Body.String())
	}
	// A reader is refused rm, in the tool-refusal envelope.
	resp = f.do(t, "POST", "/v1/runs/"+runID+"/file", fileTokReader, `{"op":"rm","path":"`+rec.Ref+`"}`, nil)
	if resp.Code != http.StatusUnprocessableEntity || !strings.Contains(resp.Body.String(), "owner or an admin") || !strings.Contains(resp.Body.String(), `"tool_refused"`) {
		t.Errorf("rm by a reader = %d %s", resp.Code, resp.Body.String())
	}
	// The tenant's operator may.
	if resp := f.do(t, "POST", "/v1/runs/"+runID+"/file", fileTokOperator, `{"op":"rm","path":"`+rec.Ref+`"}`, nil); resp.Code != http.StatusOK {
		t.Errorf("rm by the tenant operator = %d %s", resp.Code, resp.Body.String())
	}
}

// The route reads a run, so it needs runs:read, as the MCP twin does.
func TestFileRoute_NeedsRunsRead(t *testing.T) {
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/runs/run_1/file"},
		{"GET", "/v1/runs/run_1/inbox/documents/a.pdf"},
		{"HEAD", "/v1/runs/run_1/inbox/documents/a.pdf"},
	} {
		if got := requiredScopeFor(c.method, c.path); got != auth.ScopeRunsRead {
			t.Errorf("requiredScopeFor(%s %s) = %q, want %q", c.method, c.path, got, auth.ScopeRunsRead)
		}
	}
	f := newFileRouteFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "a.txt", "http", []byte("hello\n"), true)
	if resp := f.do(t, "GET", inboxURL(runID, rec.Path), fileTokNoRead, "", nil); resp.Code != http.StatusForbidden {
		t.Errorf("GET without runs:read = %d %s", resp.Code, resp.Body.String())
	}
	if resp := f.do(t, "POST", "/v1/runs/"+runID+"/file", fileTokNoRead, `{"op":"ls"}`, nil); resp.Code != http.StatusForbidden {
		t.Errorf("POST without runs:read = %d %s", resp.Code, resp.Body.String())
	}
}

// Any reader of the run downloads a kept file's bytes, with the record's type
// and origin, the hash of the bytes and headers that keep a browser from
// rendering them.
func TestInboxDownload_ServesTheBytesWithSafeHeaders(t *testing.T) {
	f := newFileRouteFixture(t)
	runID := f.run(t, "acme", "alice", "")
	data := []byte("%PDF-1.7\nhello")
	rec := f.capture(t, runID, "acme", "board report.pdf", "mcp:ms365/get-attachment", data, true)

	resp := f.do(t, "GET", inboxURL(runID, rec.Path), fileTokReader, "", nil)
	if resp.Code != http.StatusOK || resp.Body.String() != string(data) {
		t.Fatalf("GET = %d %q", resp.Code, resp.Body.String())
	}
	h := resp.Header()
	for k, want := range map[string]string{
		"Content-Type":            "application/pdf",
		"Content-Disposition":     "attachment; filename*=UTF-8''" + strings.ReplaceAll(filepath.Base(rec.Path), " ", "%20"),
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "sandbox; default-src 'none'",
		"Cache-Control":           "private, no-store",
		"X-Loomcycle-Sha256":      sha(data),
		"X-Loomcycle-Origin":      "mcp:ms365/get-attachment",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// A type a browser would run or render is served as bytes to save: an HTML
// page and an SVG picture are application/octet-stream, as attachments, with
// nosniff and a sandboxing policy.
func TestInboxDownload_ServesActiveTypesAsOctetStream(t *testing.T) {
	f := newFileRouteFixture(t)
	runID := f.run(t, "acme", "alice", "")
	for name, mt := range map[string]string{
		"page.html": "text/html",
		"pic.svg":   "image/svg+xml",
		"feed.xml":  "application/xml",
		"app.js":    "text/javascript",
	} {
		rec := f.captureTyped(t, runID, name, mt, []byte("<html><script>alert(1)</script></html>"))
		if rec.MediaType != mt {
			t.Fatalf("%s recorded as %q, want %q", name, rec.MediaType, mt)
		}
		resp := f.do(t, "GET", inboxURL(runID, rec.Path), fileTokOwner, "", nil)
		h := resp.Header()
		if resp.Code != http.StatusOK || h.Get("Content-Type") != "application/octet-stream" ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") != "sandbox; default-src 'none'" ||
			!strings.HasPrefix(h.Get("Content-Disposition"), "attachment;") {
			t.Errorf("%s: %d %v", name, resp.Code, h)
		}
	}
}

// captureTyped keeps a file with a declared media type.
func (f *fileRouteFixture) captureTyped(t *testing.T, root, name, mt string, data []byte) inbox.Record {
	t.Helper()
	return f.captureItem(t, root, "acme", inbox.Item{Name: name, MediaType: mt, Origin: "http"}, data)
}

// A range of the bytes is served, and HEAD answers with the headers alone.
func TestInboxDownload_ServesRangesAndHead(t *testing.T) {
	f := newFileRouteFixture(t)
	runID := f.run(t, "acme", "alice", "")
	data := []byte("0123456789abcdef")
	rec := f.capture(t, runID, "acme", "digits.txt", "http", data, true)

	resp := f.do(t, "GET", inboxURL(runID, rec.Path), fileTokReader, "", map[string]string{"Range": "bytes=2-5"})
	if resp.Code != http.StatusPartialContent || resp.Body.String() != "2345" {
		t.Errorf("range = %d %q", resp.Code, resp.Body.String())
	}
	resp = f.do(t, "HEAD", inboxURL(runID, rec.Path), fileTokReader, "", nil)
	if resp.Code != http.StatusOK || resp.Body.Len() != 0 || resp.Header().Get("X-Loomcycle-Sha256") != sha(data) {
		t.Errorf("HEAD = %d body %d bytes, headers %v", resp.Code, resp.Body.Len(), resp.Header())
	}
}

// A file changed since it was kept is served for what it is now: its hash
// is the new bytes', and its origin is unknown.
func TestInboxDownload_ModifiedFileHasUnknownOrigin(t *testing.T) {
	f := newFileRouteFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "a.txt", "mcp:x/y", []byte("as kept\n"), true)
	dir := dynvol.DerivedEphemeralPath(f.dynRoot, runID, inbox.Name)
	changed := []byte("changed since\n")
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rec.Path)), changed, 0o600); err != nil {
		t.Fatal(err)
	}
	resp := f.do(t, "GET", inboxURL(runID, rec.Path), fileTokReader, "", nil)
	if resp.Code != http.StatusOK || resp.Body.String() != string(changed) ||
		resp.Header().Get("X-Loomcycle-Origin") != "unknown" || resp.Header().Get("X-Loomcycle-Sha256") != sha(changed) {
		t.Errorf("GET = %d %q %v", resp.Code, resp.Body.String(), resp.Header())
	}
}

// Another tenant's run, a missing run and a missing file are the same 404;
// so is a finished run's inbox, which went with the run.
func TestInboxDownload_UnreadableOrGoneIsNotFound(t *testing.T) {
	f := newFileRouteFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "a.txt", "http", []byte("secret\n"), true)

	cross := f.do(t, "GET", inboxURL(runID, rec.Path), fileTokOutsider, "", nil)
	missing := f.do(t, "GET", inboxURL("run_nosuch", rec.Path), fileTokOutsider, "", nil)
	if cross.Code != http.StatusNotFound || strings.Replace(cross.Body.String(), runID, "X", 1) != strings.Replace(missing.Body.String(), "run_nosuch", "X", 1) {
		t.Errorf("another tenant = %d %q, missing run = %d %q", cross.Code, cross.Body.String(), missing.Code, missing.Body.String())
	}
	if resp := f.do(t, "GET", inboxURL(runID, "documents/nosuch.pdf"), fileTokReader, "", nil); resp.Code != http.StatusNotFound {
		t.Errorf("missing file = %d %s", resp.Code, resp.Body.String())
	}
	f.srv.purgeEphemeralVolumesForRun(runID)
	if err := f.st.FinishRun(t.Context(), runID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	if resp := f.do(t, "GET", inboxURL(runID, rec.Path), fileTokOwner, "", nil); resp.Code != http.StatusNotFound || !strings.Contains(resp.Body.String(), "inbox is gone") {
		t.Errorf("finished run = %d %s", resp.Code, resp.Body.String())
	}
}

// Nothing outside the run's inbox is served: not by "..", an absolute or an
// encoded path, a backslash, a hidden (still being written) file, nor a
// symlink that leads out.
func TestInboxDownload_RefusesPathsOutsideTheInbox(t *testing.T) {
	f := newFileRouteFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "a.txt", "http", []byte("inside\n"), true)
	dir := dynvol.DerivedEphemeralPath(f.dynRoot, runID, inbox.Name)
	secret := filepath.Join(f.dynRoot, "outside.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "documents", "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "documents", ".partial"), []byte("TOP-SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(dir, secret)
	if err != nil {
		t.Fatal(err)
	}
	up := filepath.ToSlash(rel) // ../../…/outside.txt
	base := "/v1/runs/" + runID + "/inbox/"
	for _, target := range []string{
		base + up,
		base + "documents/" + up,
		base + strings.ReplaceAll(up, "..", "%2e%2e"),
		base + strings.ReplaceAll(up, "/", "%2F"),
		base + "%2F" + strings.TrimPrefix(filepath.ToSlash(secret), "/"),
		base + "/" + strings.TrimPrefix(filepath.ToSlash(secret), "/"),
		base + strings.ReplaceAll(up, "/", "%5C"),
		base + "documents/link.txt",
		base + "documents/.partial",
		base + "documents",
		base,
	} {
		for _, method := range []string{"GET", "HEAD"} {
			resp := f.do(t, method, target, fileTokOwner, "", nil)
			t.Logf("%s %s = %d", method, target, resp.Code)
			if resp.Code == http.StatusOK || resp.Code == http.StatusPartialContent || strings.Contains(resp.Body.String(), "TOP-SECRET") {
				t.Errorf("%s %s = %d %q", method, target, resp.Code, resp.Body.String())
			}
		}
	}
	if resp := f.do(t, "GET", inboxURL(runID, rec.Path), fileTokOwner, "", nil); resp.Code != http.StatusOK {
		t.Errorf("the file inside = %d %s", resp.Code, resp.Body.String())
	}
}

// Over HTTP the twin runs with the caller's isolated bit, as over MCP: an
// isolated member reads and imports from its own run, into its own user
// scope, and may not write a tenant-scope document.
func TestFileRoute_IsolatedMemberImportsOnlyIntoItsOwnScope(t *testing.T) {
	f := newFileRouteFixture(t)
	seedToken(t, f.st, "lct_file_isolated", "acme", "carol", []string{auth.ScopeUser}, time.Time{})
	runID := f.run(t, "acme", "carol", "")
	rec := f.capture(t, runID, "acme", "notes.md", "http", []byte("# Notes\n\nBody.\n"), true)

	resp := f.do(t, "POST", "/v1/runs/"+runID+"/file", "lct_file_isolated", `{"op":"import","path":"`+rec.Ref+`","scope":"tenant","doc_path":"/shared/notes"}`, nil)
	if resp.Code != http.StatusUnprocessableEntity || !strings.Contains(resp.Body.String(), "isolated") {
		t.Errorf("tenant-scope import by an isolated member = %d %s", resp.Code, resp.Body.String())
	}
	resp = f.do(t, "POST", "/v1/runs/"+runID+"/file", "lct_file_isolated", `{"op":"import","path":"`+rec.Ref+`","doc_path":"/mine/notes"}`, nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "document_id") {
		t.Errorf("own-scope import by an isolated member = %d %s", resp.Code, resp.Body.String())
	}
}
