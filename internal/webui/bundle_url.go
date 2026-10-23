package webui

// bundle_url.go — a Web UI bundle fetched by URL, pinned by SHA-256.
//
// LOOMCYCLE_WEBUI_BUNDLE_URL names a .tar.gz whose top level IS the bundle
// (manifest.json at the root of the tar, no leading directory) and
// LOOMCYCLE_WEBUI_BUNDLE_SHA256 pins the archive's digest. At startup:
//
//  1. If <DataDir>/webui/<sha256>/ exists it is used with no network call,
//     and still goes through the full bundle check (loadRoot), so a file
//     changed in the cache since it was written is refused.
//  2. Otherwise the archive is downloaded over https only, through the
//     egress guard (netguard) with the operator's private-host allowlist,
//     with a total timeout and a cap on the body.
//  3. Its SHA-256 must equal the pinned digest before anything is unpacked.
//  4. It is unpacked through an os.Root into a fresh temporary directory:
//     regular files and directories only, clean relative names only, no
//     duplicate members, and the bundle's byte and file caps enforced from
//     the headers, before the bytes are written.
//  5. The unpacked directory must pass the bundle check; only then is it
//     renamed into place, so the cache only ever holds bundles that passed.
//
// Any failure serves the embedded UI and reports a fixed reason code, as for
// LOOMCYCLE_WEBUI_DIR; a bad download never stops the server. The pinned
// digest is what makes the downloaded bytes trustworthy: TLS and the guard
// only decide where they come from. The cache directory is trusted as the
// data directory is, since anyone who can write there can already rewrite
// the database; re-verifying it on every boot catches accidents, not an
// attacker who rewrites the manifest along with the files.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
)

// URLSource is where to fetch a bundle from and where to cache it.
type URLSource struct {
	// URL is LOOMCYCLE_WEBUI_BUNDLE_URL: an https URL of the archive.
	URL string
	// SHA256 is LOOMCYCLE_WEBUI_BUNDLE_SHA256: the archive's digest as 64
	// lowercase hex characters.
	SHA256 string
	// DataDir is LOOMCYCLE_DATA_DIR; the cache is <DataDir>/webui.
	DataDir string
	// PrivateHostAllowlist is LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST, the
	// operator's vouch list for hosts on a private network (an internal
	// mirror). Without an entry, a URL resolving to a private address is
	// refused.
	PrivateHostAllowlist []string
	// Logger, when set, receives progress lines at Info. They name the URL's
	// host, never its path or query string. Nil discards them.
	Logger *slog.Logger
}

// fetchOptions bound the download.
type fetchOptions struct {
	timeout      time.Duration
	archiveBytes int64
	// rootCAs replaces the system roots when set. Tests only, to trust an
	// httptest server's certificate: certificates are always verified.
	rootCAs *x509.CertPool
}

// tarMemberOverhead bounds what a tar spends on one member besides its data:
// a 512-byte header, a PAX extended header (another header plus its records)
// and up to 511 bytes of padding come to under 2 KiB; this doubles it.
const tarMemberOverhead = 4 << 10

// defaultFetch:
//   - timeout bounds the whole request, body included. The fetch runs during
//     boot, before the listener opens, so a slow or hanging host delays the
//     server by at most this much. A bundle is a few MiB, which 60 s moves
//     over any working link, and only the first boot with a new digest
//     fetches at all.
//   - archiveBytes is what the largest bundle the loader accepts (64 MiB of
//     files and a 1 MiB manifest) can take as a tar, 2 KiB of headers and
//     padding for each of its 10,001 members, rounded up to 96 MiB. gzip
//     does not grow its input by more than a few bytes per 32 KiB block.
var defaultFetch = fetchOptions{timeout: 60 * time.Second, archiveBytes: 96 << 20}

// cacheDirName is the cache's directory under the data directory.
const cacheDirName = "webui"

// maxRedirects bounds the redirects the fetch follows; a release asset
// usually takes one, to the object store.
const maxRedirects = 5

// LoadFromURL fetches, verifies and caches the bundle src names and returns
// it ready to serve. On failure it returns a nil bundle, a Status naming the
// embedded UI and the reason, and a *LoadError for the caller to log; the
// error names the URL's host at most.
func LoadFromURL(src URLSource) (*Bundle, Status, error) {
	return loadFromURL(src, APILevel, defaultLimits, defaultFetch)
}

func loadFromURL(src URLSource, level int, lim limits, f fetchOptions) (*Bundle, Status, error) {
	b, err := fetchAndLoad(src, level, lim, f)
	if err != nil {
		reason := ReasonDownloadFailed
		var le *LoadError
		if errors.As(err, &le) {
			reason = le.Reason
		}
		return nil, Status{Source: SourceEmbedded, FallbackReason: reason}, err
	}
	return b, Status{Source: SourceURL, UIVersion: b.version}, nil
}

func fetchAndLoad(src URLSource, level int, lim limits, f fetchOptions) (*Bundle, error) {
	logger := src.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	// Config validation refuses these at load; checked again here because
	// this is the function that would otherwise send the request.
	u, err := url.Parse(src.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, refuse(ReasonSourceInvalid, "the bundle URL must be https, with a host and no user info")
	}
	if !validDigest(src.SHA256) {
		return nil, refuse(ReasonSourceInvalid, "the pinned digest must be 64 lowercase hex characters")
	}
	origin := "https://" + u.Host

	cache, err := openCache(src.DataDir)
	if err != nil {
		return nil, err
	}
	defer cache.Close()

	if b, found, err := loadCached(cache, src.SHA256, level, lim); found {
		if err == nil {
			logger.Info("webui: using the cached UI bundle", "sha256", src.SHA256)
		}
		return b, err
	}

	logger.Info("webui: fetching the UI bundle", "host", u.Host)
	archive, err := download(u, src.PrivateHostAllowlist, f)
	if err != nil {
		return nil, err
	}
	if got := sha256Hex(archive); got != src.SHA256 {
		return nil, refuse(ReasonArchiveDigestMismatch, "the archive from %s has SHA-256 %s, the pinned digest is %s",
			origin, got, src.SHA256)
	}
	b, err := install(cache, src.SHA256, archive, level, lim)
	if err != nil {
		return nil, err
	}
	logger.Info("webui: cached the UI bundle", "sha256", src.SHA256, "bytes", len(archive), "host", u.Host)
	return b, nil
}

// openCache opens <dataDir>/webui, creating it if needed. The data directory
// is the operator's and is resolved like any configured path; everything
// below it goes through an os.Root, and the cache directory itself must be a
// real directory, not a link to one.
func openCache(dataDir string) (*os.Root, error) {
	if dataDir == "" {
		return nil, refuse(ReasonCacheUnusable, "no data directory is configured")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, refuse(ReasonCacheUnusable, "create the data directory: %v", err)
	}
	data, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, refuse(ReasonCacheUnusable, "open the data directory: %v", err)
	}
	defer data.Close()
	if err := data.Mkdir(cacheDirName, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, refuse(ReasonCacheUnusable, "create %s: %v", cacheDirName, err)
	}
	info, err := data.Lstat(cacheDirName)
	if err != nil {
		return nil, refuse(ReasonCacheUnusable, "stat %s: %v", cacheDirName, err)
	}
	if !info.IsDir() {
		return nil, refuse(ReasonCacheUnusable, "%s in the data directory is not a directory (%s)",
			cacheDirName, info.Mode().Type())
	}
	cache, err := data.OpenRoot(cacheDirName)
	if err != nil {
		return nil, refuse(ReasonCacheUnusable, "open %s: %v", cacheDirName, err)
	}
	return cache, nil
}

// loadCached loads the cached bundle for digest. found is false only when
// there is none; a cached bundle that fails its check is not replaced by a
// fresh download, because the same digest names the same bytes and a
// changed cache is worth an operator's attention.
func loadCached(cache *os.Root, digest string, level int, lim limits) (b *Bundle, found bool, err error) {
	info, err := cache.Lstat(digest)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, refuse(ReasonCacheUnusable, "stat the cached bundle %s: %v", digest, err)
	}
	if !info.IsDir() {
		return nil, true, refuse(ReasonCacheUnusable, "the cached bundle %s is not a directory (%s)", digest, info.Mode().Type())
	}
	dir, err := cache.OpenRoot(digest)
	if err != nil {
		return nil, true, refuse(ReasonCacheUnusable, "open the cached bundle %s: %v", digest, err)
	}
	defer dir.Close()
	b, err = loadRoot(dir, level, lim)
	return b, true, err
}

// install unpacks archive into a temporary directory in the cache, checks the
// result as a bundle and renames it to digest. On any failure the temporary
// directory is removed, so a refused archive leaves nothing behind.
func install(cache *os.Root, digest string, archive []byte, level int, lim limits) (*Bundle, error) {
	tmpName, err := tempDirName(digest)
	if err != nil {
		return nil, refuse(ReasonCacheUnusable, "name a temporary directory: %v", err)
	}
	if err := cache.Mkdir(tmpName, 0o700); err != nil {
		return nil, refuse(ReasonCacheUnusable, "create a temporary directory: %v", err)
	}
	// After a successful rename tmpName no longer exists and this is a no-op.
	defer func() { _ = cache.RemoveAll(tmpName) }()

	b, err := unpackAndLoad(cache, tmpName, archive, level, lim)
	if err != nil {
		return nil, err
	}
	if err := cache.Rename(tmpName, digest); err != nil {
		// Another replica sharing the data directory may have installed the
		// same digest first. Its directory holds the same archive's bytes, and
		// this process already holds the verified bundle.
		if info, lerr := cache.Lstat(digest); lerr == nil && info.IsDir() {
			return b, nil
		}
		return nil, refuse(ReasonCacheUnusable, "install the bundle %s: %v", digest, err)
	}
	return b, nil
}

func unpackAndLoad(cache *os.Root, tmpName string, archive []byte, level int, lim limits) (*Bundle, error) {
	tmp, err := cache.OpenRoot(tmpName)
	if err != nil {
		return nil, refuse(ReasonCacheUnusable, "open the temporary directory: %v", err)
	}
	defer tmp.Close()
	if err := unpack(tmp, archive, lim); err != nil {
		return nil, err
	}
	return loadRoot(tmp, level, lim)
}

// tempDirName is unique per call, so two processes sharing a data directory
// never unpack into the same directory. The leading dot keeps it apart from
// the digest names.
func tempDirName(digest string) (string, error) {
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", err
	}
	return ".tmp-" + digest[:12] + "-" + hex.EncodeToString(r[:]), nil
}

// newFetchClient is the client for the archive download: dials go through the
// egress guard, every redirect hop must stay on https (and is dialled through
// the guard again), and the body is not transparently decompressed, so the
// digest is taken over exactly the bytes the server sent. It uses no proxy
// from the environment: the guard has to see the address it dials.
func newFetchClient(f fetchOptions, privateHostAllowlist []string) *http.Client {
	redirects := netguard.RedirectPolicy(maxRedirects)
	return &http.Client{
		Timeout: f.timeout,
		Transport: &http.Transport{
			DialContext:         netguard.GuardedDialContext(false, privateHostAllowlist),
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: f.rootCAs},
			TLSHandshakeTimeout: 15 * time.Second,
			DisableCompression:  true,
			ForceAttemptHTTP2:   true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errors.New("refusing a redirect to a non-https URL")
			}
			return redirects(req, via)
		},
	}
}

// download GETs the archive, refusing a body over f.archiveBytes without
// reading past the cap: a declared length over it is refused before the body
// is read at all.
func download(u *url.URL, privateHostAllowlist []string, f fetchOptions) ([]byte, error) {
	origin := "https://" + u.Host
	client := newFetchClient(f, privateHostAllowlist)
	defer client.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, refuse(ReasonSourceInvalid, "build the request for %s: %v", origin, scrubURL(err, u))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, refuse(ReasonDownloadFailed, "GET %s: %v", origin, scrubURL(err, u))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, refuse(ReasonDownloadFailed, "GET %s: status %d", origin, resp.StatusCode)
	}
	if resp.ContentLength > f.archiveBytes {
		return nil, refuse(ReasonArchiveTooLarge, "%s declares %d bytes, over the %d-byte cap",
			origin, resp.ContentLength, f.archiveBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, f.archiveBytes+1))
	if err != nil {
		return nil, refuse(ReasonDownloadFailed, "read the archive from %s: %v", origin, scrubURL(err, u))
	}
	if int64(len(data)) > f.archiveBytes {
		return nil, refuse(ReasonArchiveTooLarge, "the archive from %s is over the %d-byte cap", origin, f.archiveBytes)
	}
	return data, nil
}

// scrubURL renders a fetch error without the URL. net/http wraps its errors
// in *url.Error, whose text quotes the request URL (or a redirect target)
// whole, query string included; the inner error does not. As a second line
// of defence, any remaining copy of the path or query is cut out.
func scrubURL(err error, u *url.URL) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error()
	for _, s := range []string{u.RawQuery, u.EscapedPath(), u.Path} {
		if len(s) > 1 {
			msg = strings.ReplaceAll(msg, s, "[redacted]")
		}
	}
	return msg
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

var errStreamCap = errors.New("the unpacked archive is over its size cap")

// cappedReader fails with errStreamCap once it has returned left bytes. It
// bounds the decompressed stream, so an archive of tar headers and padding
// with little data in it cannot inflate without limit.
type cappedReader struct {
	r    io.Reader
	left int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errStreamCap
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// unpack writes the gzip-compressed tar archive into dst, which must be empty.
// It accepts regular files and directories with clean relative names and
// refuses everything else, including any member that would replace one
// already written. The caps are those of a bundle, applied from each header
// before its data is read: the manifest is held to manifestBytes, the other
// files to lim.files and lim.bundleBytes between them, and the members of
// any kind to twice the file count. loadRoot applies the exact bundle rules
// afterwards; this keeps an archive from costing more than a bundle can on
// the way there.
func unpack(dst *os.Root, archive []byte, lim limits) error {
	maxMembers := 2 * (lim.files + 1)
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return refuse(ReasonArchiveInvalid, "not a gzip stream: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(&cappedReader{
		r:    gz,
		left: lim.bundleBytes + lim.manifestBytes + int64(maxMembers)*tarMemberOverhead,
	})

	seen := make(map[string]bool)
	members, files := 0, 0
	bundleLeft := lim.bundleBytes
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return archiveReadError(err)
		}
		members++
		if members > maxMembers {
			return refuse(ReasonBundleTooLarge, "the archive has more than %d members", maxMembers)
		}

		name := h.Name
		if h.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		// Names have passed no validation yet, so they are quoted (%q): a
		// name with a newline cannot forge a log line.
		if err := validRelPath(name); err != nil {
			return refuse(ReasonArchiveInvalid, "member %q: %v", h.Name, err)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return refuse(ReasonArchiveInvalid, "member %q is not a regular file or a directory (type %q)", name, h.Typeflag)
		}
		if seen[name] {
			return refuse(ReasonArchiveInvalid, "member %q appears more than once", name)
		}
		seen[name] = true

		if h.Typeflag == tar.TypeDir {
			if err := dst.MkdirAll(name, 0o700); err != nil {
				return refuse(ReasonArchiveInvalid, "create directory %q: %v", name, err)
			}
			continue
		}

		capBytes := lim.manifestBytes
		if name != manifestName {
			files++
			if files > lim.files {
				return refuse(ReasonBundleTooLarge, "the archive has more than %d files", lim.files)
			}
			capBytes = bundleLeft
		}
		if h.Size < 0 || h.Size > capBytes {
			return refuse(ReasonBundleTooLarge, "member %q is %d bytes, over what is left of the cap (%d)", name, h.Size, capBytes)
		}
		if name != manifestName {
			bundleLeft -= h.Size
		}
		if err := writeMember(dst, name, tr, h.Size); err != nil {
			return err
		}
	}
}

// writeMember creates name exclusively — an existing entry, a link included,
// is never opened or followed — and copies exactly size bytes into it.
func writeMember(dst *os.Root, name string, r io.Reader, size int64) error {
	if dir := path.Dir(name); dir != "." {
		if err := dst.MkdirAll(dir, 0o700); err != nil {
			return refuse(ReasonArchiveInvalid, "create directory for %q: %v", name, err)
		}
	}
	f, err := dst.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return refuse(ReasonArchiveInvalid, "create %q: %v", name, err)
	}
	src := &readTracker{r: io.LimitReader(r, size)}
	n, err := io.Copy(f, src)
	cerr := f.Close()
	switch {
	case src.err != nil:
		return archiveReadError(src.err)
	case err != nil:
		return refuse(ReasonCacheUnusable, "write %q: %v", name, err)
	case n != size:
		return refuse(ReasonArchiveInvalid, "member %q is truncated", name)
	case cerr != nil:
		return refuse(ReasonCacheUnusable, "write %q: %v", name, cerr)
	}
	return nil
}

// readTracker records a read error, so a failed copy can tell a bad archive
// from a failed write.
type readTracker struct {
	r   io.Reader
	err error
}

func (t *readTracker) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && err != io.EOF {
		t.err = err
	}
	return n, err
}

func archiveReadError(err error) error {
	if errors.Is(err, errStreamCap) {
		return refuse(ReasonBundleTooLarge, "%v", err)
	}
	return refuse(ReasonArchiveInvalid, "read the archive: %v", err)
}

// RedactedURL is rawURL reduced to its scheme and host, for a log line: the
// path and query string can carry a token.
func RedactedURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "(unparseable URL)"
	}
	return u.Scheme + "://" + u.Host
}
