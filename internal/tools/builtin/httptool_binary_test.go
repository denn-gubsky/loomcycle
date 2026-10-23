package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox/inboxtest"
)

// binaryPDF is a PDF whose body is not valid UTF-8, as a real one's
// compressed streams are.
var binaryPDF = append([]byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n"), bytes.Repeat([]byte{0x00, 0xff, 0x9c, 0x78}, 300)...)

// fileServer serves body with the given headers, and suppresses Date so a
// response head can be compared byte for byte.
func fileServer(t *testing.T, header map[string]string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Date"] = nil
		for k, v := range header {
			w.Header().Set(k, v)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func localHTTP() *HTTP {
	return &HTTP{HostAllowlist: []string{"127.0.0.1"}, AllowPrivateIPs: true}
}

func httpExec(t *testing.T, h *HTTP, ctx context.Context, in map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(in)
	res, err := h.Execute(ctx, raw)
	if err != nil || res.IsError {
		t.Fatalf("Execute(%s): %v %q", raw, err, res.Text)
	}
	return res.Text
}

// TestHTTP_BinaryResponseStubbed: a PDF body is kept in the run's inbox and
// the model gets the status line, the headers and the stub — never the bytes.
// Before the binary branch the PDF came back as text.
func TestHTTP_BinaryResponseStubbed(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	srv := fileServer(t, map[string]string{"Content-Type": "application/pdf"}, binaryPDF)

	out := httpExec(t, localHTTP(), r.Ctx, map[string]any{"method": "GET", "url": srv.URL + "/papers/board%20papers.pdf"})
	if strings.Contains(out, "%PDF") || strings.Contains(out, "\xff") {
		t.Fatalf("the PDF's bytes reached the result: %q", out)
	}
	if !strings.HasPrefix(out, "HTTP GET "+srv.URL+"/papers/board%20papers.pdf -> 200\n") || !strings.Contains(out, "Content-Type: application/pdf\n") {
		t.Errorf("status line and headers missing: %q", out)
	}
	if !strings.Contains(out, "\n\n[file: volume:inbox:documents/board-papers.pdf · application/pdf · ") {
		t.Errorf("no stub for the kept file: %q", out)
	}
	if !strings.HasSuffix(out, "from http]") {
		t.Errorf("stub origin: %q", out)
	}
	ref, _ := r.Set.Get(inbox.Name)
	got, err := os.ReadFile(filepath.Join(ref.Root, "documents", "board-papers.pdf"))
	if err != nil || !bytes.Equal(got, binaryPDF) {
		t.Errorf("kept file: %v", err)
	}
}

// TestHTTP_TextResponseUnchanged pins the text path byte for byte: a JSON body,
// an HTML body and a text body declared as opaque bytes all come back exactly
// as they did before the binary branch.
func TestHTTP_TextResponseUnchanged(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	for _, c := range []struct {
		name, ct, body string
	}{
		{"json", "application/json", `{"ok":true}`},
		{"html", "text/html; charset=utf-8", "<p>hello</p>"},
		{"octet text", "application/octet-stream", "plain words, declared as bytes"},
		{"plain text", "text/plain", "%%% just text"},
	} {
		srv := fileServer(t, map[string]string{"Content-Type": c.ct}, []byte(c.body))
		u := srv.URL + "/data"
		out := httpExec(t, localHTTP(), r.Ctx, map[string]any{"method": "GET", "url": u})
		want := "HTTP GET " + u + " -> 200\nContent-Length: " + strconv.Itoa(len(c.body)) + "\nContent-Type: " + c.ct + "\n\n" + c.body
		if out != want {
			t.Errorf("%s: got %q, want %q", c.name, out, want)
		}
	}
	if len(r.Events()) != 0 {
		t.Errorf("a text body was captured: %v", r.Events())
	}
}

// TestHTTP_HeadOfAFileIsNotCaptured: a HEAD has no body to keep.
func TestHTTP_HeadOfAFileIsNotCaptured(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	srv := fileServer(t, map[string]string{"Content-Type": "application/pdf"}, binaryPDF)
	out := httpExec(t, localHTTP(), r.Ctx, map[string]any{"method": "HEAD", "url": srv.URL + "/a.pdf"})
	if strings.Contains(out, "[file") || r.Set.Has(inbox.Name) {
		t.Errorf("HEAD captured: %q", out)
	}
}

// TestHTTP_BinaryWithoutInboxGivesRefusalStub: a run with no inbox gets the
// refusal stub, not the bytes.
func TestHTTP_BinaryWithoutInboxGivesRefusalStub(t *testing.T) {
	r := inboxtest.NewWithoutDynamicRoot(t, "root-1")
	srv := fileServer(t, map[string]string{"Content-Type": "image/png"}, append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...))
	out := httpExec(t, localHTTP(), r.Ctx, map[string]any{"method": "GET", "url": srv.URL + "/a.png"})
	if !strings.Contains(out, "\n\n[file not kept: ") || strings.Contains(out, "PNG") {
		t.Errorf("want a refusal stub: %q", out)
	}
}

// TestHTTP_ContentDispositionNamesTheFile: the server's filename wins over
// the URL's last segment, and a body with no Content-Type is sniffed.
func TestHTTP_ContentDispositionNamesTheFile(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	srv := fileServer(t, map[string]string{"Content-Disposition": `attachment; filename*=UTF-8''%D0%97%D0%B2%D1%96%D1%82.pdf`}, binaryPDF)
	out := httpExec(t, localHTTP(), r.Ctx, map[string]any{"method": "GET", "url": srv.URL + "/download.aspx?UniqueId=1"})
	if !strings.Contains(out, "[file: volume:inbox:documents/Zvit.pdf · application/pdf") {
		t.Errorf("stub = %q", out)
	}
}

// TestWebFetch_BinaryResponseStubbed: WebFetch of an image returns just the
// stub.
func TestWebFetch_BinaryResponseStubbed(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0, 1, 2, 0xfe}, 100)...)
	srv := fileServer(t, map[string]string{"Content-Type": "image/png"}, png)
	raw, _ := json.Marshal(map[string]any{"url": srv.URL + "/logo.png"})
	res, err := (&WebFetch{HTTP: localHTTP()}).Execute(r.Ctx, raw)
	if err != nil || res.IsError {
		t.Fatalf("%v %q", err, res.Text)
	}
	if !strings.HasPrefix(res.Text, "[file: volume:inbox:images/logo.png · image/png · ") || !strings.HasSuffix(res.Text, "from webfetch]") || strings.Contains(res.Text, "\n") {
		t.Errorf("result = %q", res.Text)
	}
}

// TestWebFetch_TextResponseUnchanged: a page's text is what it was.
func TestWebFetch_TextResponseUnchanged(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	srv := fileServer(t, map[string]string{"Content-Type": "text/html"}, []byte(`<h1>Board</h1><p>See <img src="/a.png" alt="chart"> and <a href="/b.pdf">papers</a>.</p>`))
	raw, _ := json.Marshal(map[string]any{"url": srv.URL + "/"})
	res, err := (&WebFetch{HTTP: localHTTP()}).Execute(r.Ctx, raw)
	if err != nil || res.IsError {
		t.Fatalf("%v %q", err, res.Text)
	}
	if res.Text != "BoardSee and papers." {
		t.Errorf("text = %q", res.Text)
	}
}
