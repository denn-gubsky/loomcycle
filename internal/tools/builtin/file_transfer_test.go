package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox/inboxtest"
)

// transferFile is a File tool on a loopback-reaching HTTP policy that records
// what it hands the redactor.
type transferFile struct {
	*File
	mu         sync.Mutex
	registered []string
}

func newTransferFile(h *HTTP) *transferFile {
	tf := &transferFile{}
	tf.File = &File{HTTP: h, RegisterSecret: func(v string) {
		tf.mu.Lock()
		tf.registered = append(tf.registered, v)
		tf.mu.Unlock()
	}}
	return tf
}

func (tf *transferFile) exec(t *testing.T, ctx context.Context, in map[string]any) (map[string]any, tools.Result) {
	t.Helper()
	raw, _ := json.Marshal(in)
	res, err := tf.File.Execute(ctx, raw)
	if err != nil {
		t.Fatalf("Execute(%s): %v", raw, err)
	}
	if res.IsError {
		return nil, res
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode %q: %v", res.Text, err)
	}
	return out, res
}

// countingServer serves body with header, counting requests.
func countingServer(t *testing.T, header map[string]string, body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		for k, v := range header {
			w.Header().Set(k, v)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// regularFiles lists the regular files under dir, hidden ones included.
func regularFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// transferRun is fileRun with the HTTP tool granted, as a download or upload
// needs.
func transferRun(t *testing.T, svc inbox.Service) (*inboxtest.Run, context.Context, string) {
	t.Helper()
	r, ctx, docs := fileRun(t, svc, false)
	return r, withTools(ctx, "File", "Read", "Write", "HTTP"), docs
}

func withTools(ctx context.Context, names ...string) context.Context {
	return tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) { p.Tools = names })
}

func TestFileDownload_RefusesDisallowedHost(t *testing.T) {
	_, ctx, _ := transferRun(t, inbox.Service{})
	srv, hits := countingServer(t, nil, binaryPDF)
	tf := newTransferFile(&HTTP{HostAllowlist: []string{"example.com"}, AllowPrivateIPs: true})
	_, res := tf.exec(t, ctx, map[string]any{"op": "download", "url": srv.URL + "/a.pdf"})
	if !res.IsError || !strings.Contains(res.Text, "not on the allowlist") || hits.Load() != 0 {
		t.Errorf("res = %q, hits = %d", res.Text, hits.Load())
	}
	// A hook's grant for another host does not cover this one either.
	_, res = tf.exec(t, tools.WithExtraAllowedHosts(ctx, []string{"other.example"}), map[string]any{"op": "download", "url": srv.URL + "/a.pdf"})
	if !res.IsError || hits.Load() != 0 {
		t.Errorf("res = %q, hits = %d", res.Text, hits.Load())
	}
}

func TestFileDownload_RefusesPrivateAddressUnlessAllowlisted(t *testing.T) {
	r, ctx, _ := transferRun(t, inbox.Service{})
	srv, hits := countingServer(t, map[string]string{"Content-Type": "application/pdf"}, binaryPDF)
	host := mustHost(t, srv.URL)

	_, res := newTransferFile(&HTTP{HostAllowlist: []string{host}}).exec(t, ctx, map[string]any{"op": "download", "url": srv.URL + "/a.pdf"})
	if !res.IsError || !strings.Contains(res.Text, "blocked") || hits.Load() != 0 {
		t.Fatalf("a loopback address was reached: %q, hits = %d", res.Text, hits.Load())
	}

	out, res := newTransferFile(&HTTP{HostAllowlist: []string{host}, PrivateHostAllowlist: []string{host}}).exec(t, ctx, map[string]any{"op": "download", "url": srv.URL + "/a.pdf"})
	if res.IsError || out["ref"] != "volume:inbox:documents/a.pdf" {
		t.Fatalf("operator-exempted host: %v %q", out, res.Text)
	}
	ref, _ := r.Set.Get(inbox.Name)
	if got, err := os.ReadFile(filepath.Join(ref.Root, "documents", "a.pdf")); err != nil || !bytes.Equal(got, binaryPDF) {
		t.Errorf("downloaded file: %v", err)
	}
}

func TestFileDownload_RedirectToOtherHostRefused(t *testing.T) {
	r, ctx, _ := transferRun(t, inbox.Service{})
	other, otherHits := countingServer(t, nil, binaryPDF)
	otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/same" {
			http.Redirect(w, r, "/a.pdf", http.StatusFound)
			return
		}
		if r.URL.Path == "/a.pdf" {
			_, _ = w.Write(binaryPDF)
			return
		}
		http.Redirect(w, r, otherURL+"/a.pdf", http.StatusFound)
	}))
	t.Cleanup(src.Close)
	// Both hosts are allowlisted: the refusal is the same-host rule's.
	tf := newTransferFile(&HTTP{HostAllowlist: []string{"127.0.0.1", "localhost"}, AllowPrivateIPs: true})

	_, res := tf.exec(t, ctx, map[string]any{"op": "download", "url": src.URL + "/away?sig=s3cr3t-signature"})
	if !res.IsError || !strings.Contains(res.Text, "another host") || otherHits.Load() != 0 {
		t.Fatalf("res = %q, other host hits = %d", res.Text, otherHits.Load())
	}
	if strings.Contains(res.Text, "s3cr3t") {
		t.Errorf("the URL's query leaked into the error: %q", res.Text)
	}
	if r.Set.Has(inbox.Name) {
		if ref, _ := r.Set.Get(inbox.Name); len(regularFiles(t, ref.Root)) != 0 {
			t.Error("a refused redirect left a file")
		}
	}
	// A redirect on the same host is followed.
	if out, res := tf.exec(t, ctx, map[string]any{"op": "download", "url": src.URL + "/same"}); res.IsError || out["ref"] != "volume:inbox:documents/a.pdf" {
		t.Errorf("same-host redirect: %v %q", out, res.Text)
	}
}

func TestFileDownload_OverCapLeavesNothing(t *testing.T) {
	r, ctx, docs := transferRun(t, inbox.Service{MaxFileBytes: 1000})
	big := bytes.Repeat([]byte{0xfe, 0x01}, 2500)
	// Streamed without a Content-Length, so the cap is found while reading.
	chunked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		for i := 0; i < len(big); i += 500 {
			_, _ = w.Write(big[i : i+500])
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(chunked.Close)
	sized, _ := countingServer(t, map[string]string{"Content-Type": "application/octet-stream"}, big)
	tf := newTransferFile(localHTTP())

	for _, c := range []struct{ name, url, to string }{
		{"inbox, streamed", chunked.URL + "/big.bin", ""},
		{"inbox, sized", sized.URL + "/big.bin", ""},
		{"volume, streamed", chunked.URL + "/big.bin", "volume:docs:big.bin"},
		{"volume, sized", sized.URL + "/big.bin", "volume:docs:big.bin"},
	} {
		in := map[string]any{"op": "download", "url": c.url}
		if c.to != "" {
			in["to"] = c.to
		}
		if _, res := tf.exec(t, ctx, in); !res.IsError {
			t.Errorf("%s: an over-cap file was accepted: %q", c.name, res.Text)
		}
	}
	if ref, ok := r.Set.Get(inbox.Name); ok {
		if left := regularFiles(t, ref.Root); len(left) != 0 {
			t.Errorf("inbox holds %v", left)
		}
	}
	if left := regularFiles(t, docs); len(left) != 0 {
		t.Errorf("volume holds %v", left)
	}
	// max_bytes narrows the cap and is refused above it.
	if _, res := tf.exec(t, ctx, map[string]any{"op": "download", "url": sized.URL + "/big.bin", "max_bytes": 2000}); !res.IsError || !strings.Contains(res.Text, "over the per-file limit") {
		t.Errorf("max_bytes over the cap: %q", res.Text)
	}
}

func TestFileDownload_DurableTargetNeedsWrite(t *testing.T) {
	_, ctx, docs := transferRun(t, inbox.Service{})
	srv, hits := countingServer(t, map[string]string{"Content-Type": "application/pdf", "Content-Disposition": `attachment; filename="minutes.pdf"`}, binaryPDF)
	tf := newTransferFile(localHTTP())

	_, res := tf.exec(t, withTools(ctx, "File", "Read", "HTTP"), map[string]any{"op": "download", "url": srv.URL + "/dl", "to": "volume:docs:minutes.pdf"})
	if !res.IsError || !strings.Contains(res.Text, "needs the Write tool") || hits.Load() != 0 {
		t.Fatalf("res = %q, hits = %d", res.Text, hits.Load())
	}
	_, res = tf.exec(t, ctx, map[string]any{"op": "download", "url": srv.URL + "/dl", "to": "volume:ref:minutes.pdf"})
	if !res.IsError || hits.Load() != 0 {
		t.Fatalf("read-only volume: %q, hits = %d", res.Text, hits.Load())
	}
	if left := regularFiles(t, docs); len(left) != 0 {
		t.Fatalf("refused downloads wrote %v", left)
	}

	// With Write, a folder target takes the server's file name.
	if err := os.Mkdir(filepath.Join(docs, "board"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, res := tf.exec(t, ctx, map[string]any{"op": "download", "url": srv.URL + "/dl", "to": "volume:docs:board/"})
	if res.IsError || out["ref"] != "volume:docs:board/minutes.pdf" || out["media_type"] != "application/pdf" {
		t.Fatalf("download to a volume: %v %q", out, res.Text)
	}
	if got, err := os.ReadFile(filepath.Join(docs, "board", "minutes.pdf")); err != nil || !bytes.Equal(got, binaryPDF) {
		t.Errorf("written file: %v", err)
	}
	if _, res := tf.exec(t, ctx, map[string]any{"op": "download", "url": srv.URL + "/dl", "to": "volume:docs:board/"}); !res.IsError || !strings.Contains(res.Text, "already exists") {
		t.Errorf("second download replaced the file: %q", res.Text)
	}
}

func TestFileDownload_RedactsQueryInResult(t *testing.T) {
	r, ctx, _ := transferRun(t, inbox.Service{})
	srv, _ := countingServer(t, map[string]string{"Content-Type": "application/pdf"}, binaryPDF)
	const secret = "tempauth-0123456789abcdef"
	full := srv.URL + "/_layouts/15/download.aspx/board.pdf?UniqueId=7&tempauth=" + secret
	tf := newTransferFile(localHTTP())

	out, res := tf.exec(t, ctx, map[string]any{"op": "download", "url": full})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if strings.Contains(res.Text, secret) {
		t.Errorf("result carries the URL's credential: %s", res.Text)
	}
	if out["url"] != srv.URL+"/_layouts/15/download.aspx/board.pdf?…" || out["origin"] != "download:127.0.0.1" {
		t.Errorf("result = %v", out)
	}
	if !slices.Contains(tf.registered, full) {
		t.Errorf("the redactor was not given the URL: %v", tf.registered)
	}
	rec, ok := inbox.Lookup(r.Ctx, "documents/board.pdf")
	if b, _ := json.Marshal(rec); !ok || strings.Contains(string(b), secret) || strings.Contains(string(b), "UniqueId") {
		t.Errorf("inbox record = %s", b)
	}
	// A failed download's error does not show it either.
	gone := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(gone.Close)
	if _, res := tf.exec(t, ctx, map[string]any{"op": "download", "url": gone.URL + "/x?tempauth=" + secret}); !res.IsError || strings.Contains(res.Text, secret) || !strings.Contains(res.Text, "?…") {
		t.Errorf("error = %q", res.Text)
	}
	// Nor does a request that fails outright: net/http's error quotes the URL.
	guarded := newTransferFile(&HTTP{HostAllowlist: []string{"127.0.0.1"}})
	if _, res := guarded.exec(t, ctx, map[string]any{"op": "download", "url": srv.URL + "/x?tempauth=" + secret}); !res.IsError || !strings.Contains(res.Text, "blocked") || strings.Contains(res.Text, secret) {
		t.Errorf("dial error = %q", res.Text)
	}
}

func TestFileDownload_RefusesCredentialHeaders(t *testing.T) {
	_, ctx, _ := transferRun(t, inbox.Service{})
	srv, hits := countingServer(t, nil, binaryPDF)
	tf := newTransferFile(localHTTP())
	for _, h := range []map[string]string{
		{"Authorization": "x"},
		{"Cookie": "a=b"},
		{"X-Api-Key": "k"},
		{"X-Custom": "Bearer abc"},
		{"Content-Length": "5"},
	} {
		if _, res := tf.exec(t, ctx, map[string]any{"op": "download", "url": srv.URL + "/a.pdf", "headers": h}); !res.IsError {
			t.Errorf("headers %v accepted", h)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("hits = %d", hits.Load())
	}
	if _, res := tf.exec(t, ctx, map[string]any{"op": "download", "url": srv.URL + "/a.pdf", "headers": map[string]string{"Accept": "application/pdf"}}); res.IsError {
		t.Errorf("a plain header was refused: %q", res.Text)
	}
}

// uploadSink is an upload endpoint that records each request.
type uploadSink struct {
	mu       sync.Mutex
	methods  []string
	ranges   []string
	types    []string
	body     []byte
	received int64
}

func TestFileUpload_SinglePut(t *testing.T) {
	r, ctx, _ := transferRun(t, inbox.Service{})
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "board.pdf", Origin: "o"}, binaryPDF)
	var sink uploadSink
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sink.mu.Lock()
		sink.methods, sink.types, sink.body = append(sink.methods, r.Method), append(sink.types, r.Header.Get("Content-Type")), b
		sink.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":"01AB","@microsoft.graph.downloadUrl":"https://tenant.sharepoint.com/dl?tempauth=fresh-secret"}`)
	}))
	t.Cleanup(srv.Close)
	tf := newTransferFile(localHTTP())

	out, res := tf.exec(t, ctx, map[string]any{"op": "upload", "from": kept.Record.Ref, "url": srv.URL + "/up?sig=abc"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if !bytes.Equal(sink.body, binaryPDF) || sink.methods[0] != "PUT" || sink.types[0] != "application/pdf" {
		t.Errorf("server got %v %v %d bytes", sink.methods, sink.types, len(sink.body))
	}
	if out["status"] != float64(201) || out["bytes_sent"] != float64(len(binaryPDF)) || out["url"] != srv.URL+"/up?…" {
		t.Errorf("result = %v", out)
	}
	if strings.Contains(res.Text, "fresh-secret") || !strings.Contains(out["response"].(string), `"id":"01AB"`) {
		t.Errorf("response excerpt = %v", out["response"])
	}
}

func TestFileUpload_RangedFollowsNextExpectedRanges(t *testing.T) {
	_, ctx, docs := transferRun(t, inbox.Service{})
	data := make([]byte, 800<<10)
	for i := range data {
		data[i] = byte(i * 7)
	}
	if err := os.WriteFile(filepath.Join(docs, "deck.pptx"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var sink uploadSink
	sink.body = make([]byte, len(data))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		cr := r.Header.Get("Content-Range")
		sink.ranges = append(sink.ranges, cr)
		var a, b, total int64
		if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &a, &b, &total); err != nil || total != int64(len(data)) || r.Header.Get("Authorization") != "" {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		chunk, _ := io.ReadAll(r.Body)
		copy(sink.body[a:], chunk)
		// The first chunk is acknowledged only in part: the client must
		// resend from where the session says.
		next := b + 1
		if len(sink.ranges) == 1 {
			next = 100000
		}
		if next >= total {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"done"}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"expirationDateTime":"2026-10-24T00:00:00Z","nextExpectedRanges":["%d-"]}`, next)
	}))
	t.Cleanup(srv.Close)
	tf := newTransferFile(localHTTP())

	out, res := tf.exec(t, ctx, map[string]any{"op": "upload", "from": "volume:docs:deck.pptx", "url": srv.URL + "/session", "ranged": true, "chunk_bytes": 400000})
	if res.IsError {
		t.Fatal(res.Text)
	}
	want := []string{"bytes 0-327679/819200", "bytes 100000-427679/819200", "bytes 427680-755359/819200", "bytes 755360-819199/819200"}
	if strings.Join(sink.ranges, ",") != strings.Join(want, ",") {
		t.Errorf("ranges = %v, want %v", sink.ranges, want)
	}
	if !bytes.Equal(sink.body, data) {
		t.Error("the server's copy differs")
	}
	if out["status"] != float64(201) || out["bytes_sent"] != float64(327680+327680+327680+63840) || out["size"] != float64(len(data)) {
		t.Errorf("result = %v", out)
	}
}

func TestFileUpload_NonInboxSourceNeedsRead(t *testing.T) {
	_, ctx, docs := transferRun(t, inbox.Service{})
	if err := os.WriteFile(filepath.Join(docs, "a.pdf"), binaryPDF, 0o600); err != nil {
		t.Fatal(err)
	}
	srv, hits := countingServer(t, nil, nil)
	tf := newTransferFile(localHTTP())
	_, res := tf.exec(t, withTools(ctx, "File", "Write", "HTTP"), map[string]any{"op": "upload", "from": "volume:docs:a.pdf", "url": srv.URL + "/up"})
	if !res.IsError || !strings.Contains(res.Text, "needs the Read tool") || hits.Load() != 0 {
		t.Errorf("res = %q, hits = %d", res.Text, hits.Load())
	}
	if _, res := tf.exec(t, ctx, map[string]any{"op": "upload", "from": "volume:docs:a.pdf", "url": srv.URL + "/up", "method": "POST"}); res.IsError || hits.Load() != 1 {
		t.Errorf("with Read: %q, hits = %d", res.Text, hits.Load())
	}
}

func TestFileTransfer_NotAvailableWithoutHTTPPolicy(t *testing.T) {
	_, ctx, _ := transferRun(t, inbox.Service{})
	if _, res := fileExec(t, ctx, map[string]any{"op": "download", "url": "https://example.com/a.pdf"}); !res.IsError || !strings.Contains(res.Text, "not available") {
		t.Errorf("res = %q", res.Text)
	}
}

func TestClampChunk_RoundsToTheUnit(t *testing.T) {
	for in, want := range map[int64]int64{1: 327680, 400000: 327680, 3932160: 3932160, 1 << 40: 62914560} {
		if got := clampChunk(in); got != want {
			t.Errorf("clampChunk(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestFileTransfer_NeedsTheHTTPTool: a download or upload is an HTTP request,
// so a run without HTTP — narrowed to File, say — makes none.
func TestFileTransfer_NeedsTheHTTPTool(t *testing.T) {
	r, ctx, _ := transferRun(t, inbox.Service{})
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, binaryPDF)
	srv, hits := countingServer(t, nil, binaryPDF)
	tf := newTransferFile(localHTTP())
	noHTTP := withTools(ctx, "File", "Read", "Write")
	for _, in := range []map[string]any{
		{"op": "download", "url": srv.URL + "/a.pdf"},
		{"op": "upload", "from": kept.Record.Ref, "url": srv.URL + "/up"},
	} {
		if _, res := tf.exec(t, noHTTP, in); !res.IsError || !strings.Contains(res.Text, "needs the HTTP tool") {
			t.Errorf("%v: %q", in["op"], res.Text)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("requests made = %d", hits.Load())
	}
}

// TestFileTransfer_HTTPHookJudgesTheRequest: HTTP's pre-hooks see the request
// (query masked) and a denial stops it before it is made.
func TestFileTransfer_HTTPHookJudgesTheRequest(t *testing.T) {
	r, ctx, _ := transferRun(t, inbox.Service{})
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, binaryPDF)
	srv, hits := countingServer(t, nil, binaryPDF)
	var asked []string
	hooked := tools.WithHookPreCheck(ctx, func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
		asked = append(asked, name+" "+string(input))
		if name == "HTTP" {
			return tools.Result{Text: "no egress from File", IsError: true}, false
		}
		return tools.Result{}, true
	})
	tf := newTransferFile(localHTTP())
	for _, in := range []map[string]any{
		{"op": "download", "url": srv.URL + "/a.pdf?sig=hook-secret"},
		{"op": "upload", "from": kept.Record.Ref, "url": srv.URL + "/up?sig=hook-secret", "method": "POST"},
	} {
		if _, res := tf.exec(t, hooked, in); !res.IsError || !strings.Contains(res.Text, "no egress from File") {
			t.Errorf("%v: %q", in["op"], res.Text)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("requests made = %d", hits.Load())
	}
	joined := strings.Join(asked, "\n")
	if !strings.Contains(joined, `HTTP {"method":"GET","url":"`+srv.URL+`/a.pdf?…","via":"File"}`) ||
		!strings.Contains(joined, `"method":"POST"`) || strings.Contains(joined, "hook-secret") {
		t.Errorf("hook inputs = %s", joined)
	}
}
