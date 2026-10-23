package builtin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox/inboxtest"
)

var assetPNG = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0, 7, 0xfe}, 50)...)

// assetSite serves a page that shows an image, links a PDF and a page, and
// shows an image on a host that is not allowlisted.
func assetSite(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<h1>Board</h1>
<p><img src="/img/chart.png?v=2" alt="Q3 [chart]"> and
<a href="/files/papers.pdf">the papers</a>, <a href="/about" title="about">about <img src="/img/icon.png" alt="i"></a>,
<img srcset="/img/logo.png 1x, /img/logo@2x.png 2x" alt="logo">
<img src="http://cdn.example.org/x.png" alt="cdn">
<img src="/img/chart.png?v=2" alt="again"></p>
<script>var u = "/img/hidden.png";</script>`))
	})
	mux.HandleFunc("/img/", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(assetPNG)
	})
	mux.HandleFunc("/files/papers.pdf", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(binaryPDF)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func webFetchExec(t *testing.T, r *inboxtest.Run, in map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(in)
	res, err := (&WebFetch{HTTP: localHTTP()}).Execute(r.Ctx, raw)
	if err != nil || res.IsError {
		t.Fatalf("%v %q", err, res.Text)
	}
	return res.Text
}

// TestWebFetch_AssetsRewriteLinksToVolume: with assets, the page's images and
// its PDF link are kept under web/<host>/ and the text links to their inbox
// references; an image on a host off the allowlist keeps its URL with the
// reason; a link to a page is not an asset; a URL used twice is fetched once.
func TestWebFetch_AssetsRewriteLinksToVolume(t *testing.T) {
	var hits atomic.Int32
	r := inboxtest.New(t, "root-1", inbox.Service{})
	srv := assetSite(t, &hits)

	out := webFetchExec(t, r, map[string]any{"url": srv.URL + "/page", "assets": true})
	for _, want := range []string{
		"![Q3 chart](volume:inbox:web/127.0.0.1/chart.png)",
		"[the papers](volume:inbox:web/127.0.0.1/papers.pdf)",
		"![logo](volume:inbox:web/127.0.0.1/logo.png)",
		"![cdn](http://cdn.example.org/x.png) [not fetched: host not on the allowlist]",
		"![again](volume:inbox:web/127.0.0.1/chart.png)",
		", about ![i](volume:inbox:web/127.0.0.1/icon.png) ,",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hidden") {
		t.Errorf("a URL in a script was fetched: %s", out)
	}
	if n := hits.Load(); n != 4 {
		t.Errorf("asset requests = %d, want 4 (chart once, papers, icon, logo)", n)
	}

	_, line, ok := strings.Cut(out, "\n\n[assets] ")
	if !ok {
		t.Fatalf("no assets line: %s", out)
	}
	var list []webAsset
	if err := json.Unmarshal([]byte(line), &list); err != nil {
		t.Fatalf("assets line %q: %v", line, err)
	}
	if len(list) != 5 || list[0].URL != srv.URL+"/img/chart.png?…" || list[0].Ref != "volume:inbox:web/127.0.0.1/chart.png" ||
		list[0].MediaType != "image/png" || list[0].Size != int64(len(assetPNG)) || list[4].Skipped == "" {
		t.Errorf("assets = %+v", list)
	}

	ref, _ := r.Set.Get(inbox.Name)
	if got, err := os.ReadFile(filepath.Join(ref.Root, "web", "127.0.0.1", "papers.pdf")); err != nil || !bytes.Equal(got, binaryPDF) {
		t.Errorf("papers.pdf: %v", err)
	}
}

// TestWebFetch_AssetsMaxBoundsTheDownloads: past assets_max a link is left
// with the reason, and nothing more is requested.
func TestWebFetch_AssetsMaxBoundsTheDownloads(t *testing.T) {
	var hits atomic.Int32
	r := inboxtest.New(t, "root-1", inbox.Service{})
	srv := assetSite(t, &hits)
	out := webFetchExec(t, r, map[string]any{"url": srv.URL + "/page", "assets": true, "assets_max": 1})
	if hits.Load() != 1 || !strings.Contains(out, "[not fetched: assets_max (1) reached]") {
		t.Errorf("hits = %d, text = %s", hits.Load(), out)
	}
}

// TestWebFetch_AssetsWithoutInboxFetchNothing: a run that cannot keep files
// makes no asset requests.
func TestWebFetch_AssetsWithoutInboxFetchNothing(t *testing.T) {
	var hits atomic.Int32
	r := inboxtest.NewWithoutDynamicRoot(t, "root-1")
	srv := assetSite(t, &hits)
	out := webFetchExec(t, r, map[string]any{"url": srv.URL + "/page", "assets": true})
	if hits.Load() != 0 || !strings.Contains(out, "[not fetched: this run has no inbox to keep files in]") {
		t.Errorf("hits = %d, text = %s", hits.Load(), out)
	}
}
