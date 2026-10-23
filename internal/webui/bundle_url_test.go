package webui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tarEntry is one archive member. A nil hdr.Typeflag defaults to a regular
// file; body is the file content.
type tarEntry struct {
	hdr  tar.Header
	body string
}

func regular(name, body string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}, body: body}
}

func dirEntry(name string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}}
}

func tarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := e.hdr
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		h.Format = tar.FormatPAX
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("header %q: %v", h.Name, err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// bundleEntries is a valid bundle as archive members: the manifest, an
// explicit directory entry and every file of testFiles, in a fixed order.
func bundleEntries(t *testing.T) []tarEntry {
	t.Helper()
	files := testFiles()
	raw, err := json.Marshal(manifestFor(files, APILevel, APILevel))
	if err != nil {
		t.Fatal(err)
	}
	entries := []tarEntry{regular("manifest.json", string(raw)), dirEntry("assets/")}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entries = append(entries, regular(name, files[name]))
	}
	return entries
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// archiveServer serves archive over TLS at /ui/bundle.tar.gz and counts the
// requests it gets. Its certificate is trusted only through the returned pool.
func archiveServer(t *testing.T, archive []byte) (*httptest.Server, *x509.CertPool, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.URL.Path != "/ui/bundle.tar.gz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	return srv, certPool(srv), &n
}

func certPool(srv *httptest.Server) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}

// testFetch is defaultFetch with the test server's certificate trusted and a
// short timeout, so a hung case fails fast instead of stalling the suite.
func testFetch(pool *x509.CertPool) fetchOptions {
	f := defaultFetch
	f.timeout = 5 * time.Second
	f.rootCAs = pool
	return f
}

// urlSrc names srvURL+path pinned to digest, cached under dataDir. The
// httptest server listens on loopback, which the egress guard refuses unless
// the operator vouched for it, as an operator would for an internal mirror.
func urlSrc(srvURL, path, digest, dataDir string) URLSource {
	return URLSource{
		URL:                  srvURL + path,
		SHA256:               digest,
		DataDir:              dataDir,
		PrivateHostAllowlist: []string{"127.0.0.1"},
	}
}

// cacheEntries lists <dataDir>/webui, or nil when it does not exist.
func cacheEntries(t *testing.T, dataDir string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(dataDir, "webui"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("error %v (%T) is not a *LoadError", err, err)
	}
	return le.Reason
}

// wantFallback asserts the load served the embedded UI for reason.
func wantFallback(t *testing.T, b *Bundle, st Status, err error, reason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("loaded, want refusal %s", reason)
	}
	if b != nil {
		t.Errorf("a bundle came back with the refusal")
	}
	if got := reasonOf(t, err); got != reason {
		t.Errorf("reason = %s, want %s (%v)", got, reason, err)
	}
	if st.Source != SourceEmbedded || st.FallbackReason != reason || st.UIVersion != "" {
		t.Errorf("status = %+v, want embedded with fallback_reason %s", st, reason)
	}
}

func TestLoadFromURL_PinnedArchiveIsServedWithURLSource(t *testing.T) {
	archive := tarGz(t, bundleEntries(t))
	srv, pool, _ := archiveServer(t, archive)
	dataDir := t.TempDir()

	b, st, err := loadFromURL(urlSrc(srv.URL, "/ui/bundle.tar.gz", sum(archive), dataDir), APILevel, defaultLimits, testFetch(pool))
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if st != (Status{Source: SourceURL, UIVersion: "1.2.3"}) {
		t.Errorf("status = %+v", st)
	}
	rec := get(t, Handler("/ui", false, nil, WithBundle(b)), "/ui/")
	if !strings.Contains(rec.Body.String(), "<title>bundle</title>") {
		t.Errorf("/ui/ served %q, want the downloaded bundle", rec.Body.String())
	}
	if got := cacheEntries(t, dataDir); len(got) != 1 || got[0] != sum(archive) {
		t.Errorf("cache holds %v, want exactly the digest directory", got)
	}
	if _, err := LoadDir(filepath.Join(dataDir, "webui", sum(archive))); err != nil {
		t.Errorf("cached directory does not verify on its own: %v", err)
	}
}

func TestLoadFromURL_DigestMismatchUnpacksNothing(t *testing.T) {
	archive := tarGz(t, bundleEntries(t))
	srv, pool, _ := archiveServer(t, archive)
	dataDir := t.TempDir()
	wrong := sum([]byte("some other archive"))

	b, st, err := loadFromURL(urlSrc(srv.URL, "/ui/bundle.tar.gz", wrong, dataDir), APILevel, defaultLimits, testFetch(pool))
	wantFallback(t, b, st, err, ReasonArchiveDigestMismatch)
	if got := cacheEntries(t, dataDir); len(got) != 0 {
		t.Errorf("cache holds %v after a digest mismatch", got)
	}
}

// A plain-http URL is refused before any request, and so is an https URL that
// redirects to plain http: the archive would arrive unauthenticated.
func TestLoadFromURL_PlainHTTPIsRefused(t *testing.T) {
	archive := tarGz(t, bundleEntries(t))
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		_, _ = w.Write(archive)
	}))
	defer plain.Close()

	t.Run("http URL", func(t *testing.T) {
		dataDir := t.TempDir()
		b, st, err := loadFromURL(urlSrc(plain.URL, "/ui/bundle.tar.gz", sum(archive), dataDir), APILevel, defaultLimits, testFetch(nil))
		wantFallback(t, b, st, err, ReasonSourceInvalid)
		if got := cacheEntries(t, dataDir); len(got) != 0 {
			t.Errorf("cache holds %v", got)
		}
	})
	t.Run("redirect to http", func(t *testing.T) {
		redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, plain.URL+"/ui/bundle.tar.gz", http.StatusFound)
		}))
		defer redirector.Close()
		dataDir := t.TempDir()
		b, st, err := loadFromURL(urlSrc(redirector.URL, "/x", sum(archive), dataDir), APILevel, defaultLimits, testFetch(certPool(redirector)))
		wantFallback(t, b, st, err, ReasonDownloadFailed)
	})
	if n := plainHits.Load(); n != 0 {
		t.Errorf("the plain-http server got %d requests, want 0", n)
	}
}

// The egress guard is in the path: a loopback server the operator did not
// vouch for is never dialled.
func TestLoadFromURL_PrivateHostIsRefusedWithoutAllowlist(t *testing.T) {
	archive := tarGz(t, bundleEntries(t))
	srv, pool, hits := archiveServer(t, archive)
	src := urlSrc(srv.URL, "/ui/bundle.tar.gz", sum(archive), t.TempDir())
	src.PrivateHostAllowlist = nil

	b, st, err := loadFromURL(src, APILevel, defaultLimits, testFetch(pool))
	wantFallback(t, b, st, err, ReasonDownloadFailed)
	if n := hits.Load(); n != 0 {
		t.Errorf("the server got %d requests, want 0", n)
	}
}

func TestLoadFromURL_NonOKStatusFallsBack(t *testing.T) {
	archive := tarGz(t, bundleEntries(t))
	srv, pool, _ := archiveServer(t, archive)
	b, st, err := loadFromURL(urlSrc(srv.URL, "/missing.tar.gz", sum(archive), t.TempDir()), APILevel, defaultLimits, testFetch(pool))
	wantFallback(t, b, st, err, ReasonDownloadFailed)
}

// An oversized body is refused without reading it: with a declared length
// over the cap the refusal comes before the body, and without one it comes at
// cap+1 bytes. Either server then blocks until the client hangs up, so a
// client that kept reading would run into the timeout and report
// download_failed instead.
func TestLoadFromURL_OversizeBodyIsRefusedWithoutReadingIt(t *testing.T) {
	const capBytes = 64 << 10
	for _, declared := range []bool{true, false} {
		t.Run(fmt.Sprintf("content-length=%v", declared), func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if declared {
					w.Header().Set("Content-Length", fmt.Sprint(1<<30))
				} else {
					chunk := bytes.Repeat([]byte{'x'}, 16<<10)
					for i := 0; i < 4*capBytes/len(chunk); i++ {
						if _, err := w.Write(chunk); err != nil {
							return
						}
					}
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer srv.Close()
			f := testFetch(certPool(srv))
			f.archiveBytes = capBytes
			dataDir := t.TempDir()

			start := time.Now()
			b, st, err := loadFromURL(urlSrc(srv.URL, "/ui.tar.gz", sum(nil), dataDir), APILevel, defaultLimits, f)
			wantFallback(t, b, st, err, ReasonArchiveTooLarge)
			if d := time.Since(start); d >= f.timeout {
				t.Errorf("refusal took %v, the whole timeout", d)
			}
			if got := cacheEntries(t, dataDir); len(got) != 0 {
				t.Errorf("cache holds %v", got)
			}
		})
	}
}

func TestLoadFromURL_TimeoutFallsBack(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	f := testFetch(certPool(srv))
	f.timeout = 200 * time.Millisecond
	dataDir := t.TempDir()

	b, st, err := loadFromURL(urlSrc(srv.URL, "/ui.tar.gz", sum(nil), dataDir), APILevel, defaultLimits, f)
	wantFallback(t, b, st, err, ReasonDownloadFailed)
	if got := cacheEntries(t, dataDir); len(got) != 0 {
		t.Errorf("cache holds %v", got)
	}
}

// Every archive below carries a valid bundle plus one bad member, and is
// served under its true digest, so the refusal can only come from the unpack.
// Nothing may be left in the cache, and nothing may land outside it.
func TestLoadFromURL_UnsafeArchiveEntriesAreRefused(t *testing.T) {
	small := defaultLimits
	small.files = len(testFiles())
	tight := defaultLimits
	tight.bundleBytes = 16

	link := func(name, target string, typ byte) tarEntry {
		return tarEntry{hdr: tar.Header{Name: name, Linkname: target, Typeflag: typ}}
	}
	device := func(name string, typ byte) tarEntry {
		return tarEntry{hdr: tar.Header{Name: name, Typeflag: typ, Devmajor: 1, Devminor: 3}}
	}
	for _, c := range []struct {
		name   string
		extra  []tarEntry
		lim    limits
		reason string
	}{
		{"dot-dot", []tarEntry{regular("../evil", "x")}, defaultLimits, ReasonArchiveInvalid},
		{"nested dot-dot", []tarEntry{regular("assets/../../evil", "x")}, defaultLimits, ReasonArchiveInvalid},
		{"absolute", []tarEntry{regular("/tmp/evil", "x")}, defaultLimits, ReasonArchiveInvalid},
		{"leading dot", []tarEntry{regular("./extra.js", "x")}, defaultLimits, ReasonArchiveInvalid},
		{"backslash", []tarEntry{regular(`assets\evil.js`, "x")}, defaultLimits, ReasonArchiveInvalid},
		{"dot-dot directory", []tarEntry{dirEntry("../evil/")}, defaultLimits, ReasonArchiveInvalid},
		{"symlink", []tarEntry{link("assets/link.js", "../../../../etc/passwd", tar.TypeSymlink)}, defaultLimits, ReasonArchiveInvalid},
		{"hard link", []tarEntry{link("assets/link.js", "index.html", tar.TypeLink)}, defaultLimits, ReasonArchiveInvalid},
		{"char device", []tarEntry{device("assets/null", tar.TypeChar)}, defaultLimits, ReasonArchiveInvalid},
		{"block device", []tarEntry{device("assets/disk", tar.TypeBlock)}, defaultLimits, ReasonArchiveInvalid},
		{"fifo", []tarEntry{{hdr: tar.Header{Name: "assets/pipe", Typeflag: tar.TypeFifo}}}, defaultLimits, ReasonArchiveInvalid},
		{"duplicate file", []tarEntry{regular("index.html", "<!doctype html><title>second</title>")}, defaultLimits, ReasonArchiveInvalid},
		{"duplicate directory", []tarEntry{dirEntry("assets/")}, defaultLimits, ReasonArchiveInvalid},
		{"file over a directory", []tarEntry{regular("assets", "x")}, defaultLimits, ReasonArchiveInvalid},
		{"too many files", []tarEntry{regular("extra.js", "x")}, small, ReasonBundleTooLarge},
		{"too many bytes", nil, tight, ReasonBundleTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			archive := tarGz(t, append(bundleEntries(t), c.extra...))
			srv, pool, _ := archiveServer(t, archive)
			parent := t.TempDir()
			dataDir := filepath.Join(parent, "data")

			b, st, err := loadFromURL(urlSrc(srv.URL, "/ui/bundle.tar.gz", sum(archive), dataDir), APILevel, c.lim, testFetch(pool))
			wantFallback(t, b, st, err, c.reason)
			if got := cacheEntries(t, dataDir); len(got) != 0 {
				t.Errorf("cache holds %v after a refused archive", got)
			}
			for _, p := range []string{
				filepath.Join(parent, "evil"),
				filepath.Join(dataDir, "evil"),
				filepath.Join(dataDir, "webui", "evil"),
				"/tmp/evil",
			} {
				if _, err := os.Lstat(p); err == nil {
					t.Errorf("%s was written", p)
				}
			}
		})
	}
}

func TestLoadFromURL_MalformedArchiveIsRefused(t *testing.T) {
	good := tarGz(t, bundleEntries(t))
	for name, archive := range map[string][]byte{
		"not gzip":  []byte("this is not a gzip stream"),
		"truncated": good[:len(good)/2],
		"gzip of junk": func() []byte {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			_, _ = gz.Write(bytes.Repeat([]byte("not a tar header "), 100))
			_ = gz.Close()
			return buf.Bytes()
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			srv, pool, _ := archiveServer(t, archive)
			dataDir := t.TempDir()
			b, st, err := loadFromURL(urlSrc(srv.URL, "/ui/bundle.tar.gz", sum(archive), dataDir), APILevel, defaultLimits, testFetch(pool))
			wantFallback(t, b, st, err, ReasonArchiveInvalid)
			if got := cacheEntries(t, dataDir); len(got) != 0 {
				t.Errorf("cache holds %v", got)
			}
		})
	}
}

// A bundle that unpacks cleanly but fails P0a's checks is not cached either,
// so a later binary it does fit fetches it again rather than trusting a
// directory this one refused.
func TestLoadFromURL_BundleRefusedByLoaderIsNotCached(t *testing.T) {
	archive := tarGz(t, bundleEntries(t))
	srv, pool, _ := archiveServer(t, archive)
	dataDir := t.TempDir()

	b, st, err := loadFromURL(urlSrc(srv.URL, "/ui/bundle.tar.gz", sum(archive), dataDir), APILevel+1, defaultLimits, testFetch(pool))
	wantFallback(t, b, st, err, ReasonAPILevel)
	if got := cacheEntries(t, dataDir); len(got) != 0 {
		t.Errorf("cache holds %v", got)
	}
}

func TestLoadFromURL_CachedDigestIsReusedWithoutRequest(t *testing.T) {
	archive := tarGz(t, bundleEntries(t))
	srv, pool, hits := archiveServer(t, archive)
	dataDir := t.TempDir()
	src := urlSrc(srv.URL, "/ui/bundle.tar.gz", sum(archive), dataDir)
	if _, _, err := loadFromURL(src, APILevel, defaultLimits, testFetch(pool)); err != nil {
		t.Fatalf("first load: %v", err)
	}
	srv.Close()

	var logs []string
	src.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	b, st, err := loadFromURL(src, APILevel, defaultLimits, testFetch(pool))
	if err != nil {
		t.Fatalf("second load with the server down: %v", err)
	}
	if b == nil || st != (Status{Source: SourceURL, UIVersion: "1.2.3"}) {
		t.Errorf("status = %+v", st)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("the server got %d requests, want 1", n)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "cached") {
		t.Errorf("no log line says the cache was used: %q", logs)
	}
}

// The cache is re-verified on every boot, and a cache entry that is not a
// plain directory is not followed.
func TestLoadFromURL_TamperedCacheIsRefused(t *testing.T) {
	archive := tarGz(t, bundleEntries(t))
	digestDir := func(dataDir string) string { return filepath.Join(dataDir, "webui", sum(archive)) }
	prime := func(t *testing.T) (URLSource, *x509.CertPool) {
		t.Helper()
		srv, pool, _ := archiveServer(t, archive)
		src := urlSrc(srv.URL, "/ui/bundle.tar.gz", sum(archive), t.TempDir())
		if _, _, err := loadFromURL(src, APILevel, defaultLimits, testFetch(pool)); err != nil {
			t.Fatalf("first load: %v", err)
		}
		srv.Close()
		return src, pool
	}

	t.Run("file changed", func(t *testing.T) {
		src, pool := prime(t)
		if err := os.WriteFile(filepath.Join(digestDir(src.DataDir), "index.html"), []byte("<script>evil()</script>"), 0o600); err != nil {
			t.Fatal(err)
		}
		b, st, err := loadFromURL(src, APILevel, defaultLimits, testFetch(pool))
		wantFallback(t, b, st, err, ReasonDigestMismatch)
	})
	t.Run("digest entry is a symlink", func(t *testing.T) {
		src, pool := prime(t)
		elsewhere := filepath.Join(t.TempDir(), "bundle")
		if err := os.Rename(digestDir(src.DataDir), elsewhere); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, digestDir(src.DataDir)); err != nil {
			t.Fatal(err)
		}
		b, st, err := loadFromURL(src, APILevel, defaultLimits, testFetch(pool))
		wantFallback(t, b, st, err, ReasonCacheUnusable)
	})
	t.Run("cache directory is a symlink", func(t *testing.T) {
		src, pool := prime(t)
		elsewhere := filepath.Join(t.TempDir(), "webui")
		if err := os.Rename(filepath.Join(src.DataDir, "webui"), elsewhere); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, filepath.Join(src.DataDir, "webui")); err != nil {
			t.Fatal(err)
		}
		b, st, err := loadFromURL(src, APILevel, defaultLimits, testFetch(pool))
		wantFallback(t, b, st, err, ReasonCacheUnusable)
	})
}

// /healthz is unauthenticated, so the status never names the URL; the log and
// the error name at most the host, never the path or the query string, which
// can carry a token.
func TestLoadFromURL_StatusAndLogsCarryNoURLPathOrQuery(t *testing.T) {
	const path, query = "/private-bucket/ui.tar.gz", "X-Amz-Signature=t0ps3cr3t"
	archive := tarGz(t, bundleEntries(t))
	for _, c := range []struct {
		name    string
		handler http.HandlerFunc
		digest  string
		timeout time.Duration
	}{
		{"served", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }, sum(archive), 0},
		{"digest mismatch", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }, sum(nil), 0},
		{"not found", http.NotFound, sum(archive), 0},
		{"redirect loop", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, path+"?"+query+"&n=1", http.StatusFound)
		}, sum(archive), 0},
		{"timeout", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, sum(archive), 200 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(c.handler)
			defer srv.Close()
			f := testFetch(certPool(srv))
			if c.timeout > 0 {
				f.timeout = c.timeout
			}
			var mu sync.Mutex
			var logs []string
			src := urlSrc(srv.URL, path+"?"+query, c.digest, t.TempDir())
			src.Logf = func(format string, args ...any) {
				mu.Lock()
				defer mu.Unlock()
				logs = append(logs, fmt.Sprintf(format, args...))
			}

			_, st, err := loadFromURL(src, APILevel, defaultLimits, f)
			health, _ := json.Marshal(st)
			for _, leak := range []string{"t0ps3cr3t", "private-bucket", "127.0.0.1"} {
				if strings.Contains(string(health), leak) {
					t.Errorf("status %s carries %q", health, leak)
				}
			}
			var text []string
			text = append(text, logs...)
			if err != nil {
				text = append(text, err.Error())
			}
			for _, leak := range []string{"t0ps3cr3t", "private-bucket"} {
				for _, line := range text {
					if strings.Contains(line, leak) {
						t.Errorf("log/error %q carries %q", line, leak)
					}
				}
			}
		})
	}
}

// The unpack enforces the caps itself, before writing: a member over the byte
// budget is refused from its header, so a decompression bomb is never
// inflated onto disk, and the member and entry counts stop an archive of
// countless tiny members.
func TestUnpack_RefusesOverCapsBeforeWriting(t *testing.T) {
	lim := limits{manifestBytes: 1 << 10, bundleBytes: 1 << 10, files: 2}
	big := tarEntry{hdr: tar.Header{Name: "big.js", Typeflag: tar.TypeReg, Mode: 0o644, Size: 64 << 20}}
	manyDirs := []tarEntry{}
	for i := 0; i < 2*(lim.files+1)+1; i++ {
		manyDirs = append(manyDirs, dirEntry(fmt.Sprintf("d%d/", i)))
	}
	for _, c := range []struct {
		name    string
		archive func() []byte
		absent  string
	}{
		{"bytes", func() []byte {
			// A header claiming 64 MiB with a body of zeros: tar.Writer
			// refuses a short body, so write the zeros it asks for. gzip
			// shrinks them to a few hundred KiB.
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			h := big.hdr
			if err := tw.WriteHeader(&h); err != nil {
				t.Fatal(err)
			}
			zeros := make([]byte, 1<<20)
			for i := 0; i < 64; i++ {
				_, _ = tw.Write(zeros)
			}
			_ = tw.Close()
			_ = gz.Close()
			return buf.Bytes()
		}, "big.js"},
		{"manifest bytes", func() []byte {
			return tarGz(t, []tarEntry{regular("manifest.json", strings.Repeat(" ", 2<<10))})
		}, "manifest.json"},
		{"files", func() []byte {
			return tarGz(t, []tarEntry{regular("a.js", "a"), regular("b.js", "b"), regular("c.js", "c")})
		}, "c.js"},
		{"entries", func() []byte { return tarGz(t, manyDirs) }, fmt.Sprintf("d%d", len(manyDirs)-1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			err = unpack(root, c.archive(), lim)
			if got := reasonOf(t, err); got != ReasonBundleTooLarge {
				t.Errorf("reason = %s, want %s (%v)", got, ReasonBundleTooLarge, err)
			}
			if _, err := os.Lstat(filepath.Join(dir, c.absent)); err == nil {
				t.Errorf("%s was written past the cap", c.absent)
			}
		})
	}
}

// packLikeTheReleaseTool builds an archive the way the webui-v* release tool
// (scripts/webui-bundle) does: gzip at BestCompression with an empty header,
// tar headers with no explicit format, manifest.json first, then every
// directory ("a/", mode 0755) and file (mode 0644) sorted by path, uid/gid 0,
// mtime the Unix epoch.
func packLikeTheReleaseTool(t *testing.T, files map[string]string) []byte {
	t.Helper()
	raw, err := json.Marshal(manifestFor(files, APILevel, APILevel))
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[string]bool{}
	var names []string
	for name := range files {
		names = append(names, name)
		for d := filepath.ToSlash(filepath.Dir(name)); d != "."; d = filepath.ToSlash(filepath.Dir(d)) {
			if !dirs[d] {
				dirs[d] = true
				names = append(names, d+"/")
			}
		}
	}
	sort.Strings(names)
	epoch := time.Unix(0, 0)
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	write := func(h *tar.Header, body string) {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write(&tar.Header{Typeflag: tar.TypeReg, Name: "manifest.json", Mode: 0o644, Size: int64(len(raw)), ModTime: epoch}, string(raw))
	for _, name := range names {
		if strings.HasSuffix(name, "/") {
			write(&tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: 0o755, ModTime: epoch}, "")
			continue
		}
		write(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(files[name])), ModTime: epoch}, files[name])
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The archives the release tool publishes are accepted as they are, nested
// directories included, and the cached directory verifies with LoadDir alone.
func TestLoadFromURL_ReleaseToolArchiveIsAccepted(t *testing.T) {
	files := testFiles()
	files["assets/fonts/inter.woff2"] = "font bytes"
	archive := packLikeTheReleaseTool(t, files)
	srv, pool, _ := archiveServer(t, archive)
	dataDir := t.TempDir()

	b, st, err := loadFromURL(urlSrc(srv.URL, "/ui/bundle.tar.gz", sum(archive), dataDir), APILevel, defaultLimits, testFetch(pool))
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if b == nil || st.Source != SourceURL {
		t.Fatalf("status = %+v", st)
	}
	if _, err := LoadDir(filepath.Join(dataDir, "webui", sum(archive))); err != nil {
		t.Errorf("the cached directory does not verify with LoadDir: %v", err)
	}
}

// Directory members are optional and the order is free: parents are created
// as files need them, and a directory member after its contents is fine.
func TestUnpack_DirectoryMembersAreOptionalAndOrderIsFree(t *testing.T) {
	files := testFiles()
	raw, err := json.Marshal(manifestFor(files, APILevel, APILevel))
	if err != nil {
		t.Fatal(err)
	}
	for name, entries := range map[string][]tarEntry{
		"no directory members": {
			regular("index.html", files["index.html"]),
			regular("assets/app-abc123.js", files["assets/app-abc123.js"]),
			regular("favicon.svg", files["favicon.svg"]),
			regular("manifest.json", string(raw)),
		},
		"directory after its contents": {
			regular("assets/app-abc123.js", files["assets/app-abc123.js"]),
			dirEntry("assets/"),
			regular("manifest.json", string(raw)),
			regular("favicon.svg", files["favicon.svg"]),
			regular("index.html", files["index.html"]),
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := unpack(root, tarGz(t, entries), defaultLimits); err != nil {
				t.Fatalf("unpack: %v", err)
			}
			if _, err := LoadDir(dir); err != nil {
				t.Errorf("LoadDir: %v", err)
			}
		})
	}
}
