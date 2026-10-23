package webui

// bundle.go — a Web UI bundle loaded from a directory at startup, served in
// place of the embedded SPA.
//
// A bundle runs same-origin with the HttpOnly session cookie and can call any
// /v1/* route the signed-in operator can, so it is a code-delivery path into an
// authenticated origin. The loader is strict for that reason:
//
//   - The directory is opened as an os.Root, so neither a symlink nor ".."
//     can reach outside it, and every read goes through that root.
//   - manifest.json must list every file with its SHA-256; a file that does not
//     match, a listed file that is missing and a file on disk that is not listed
//     are all refused, as is any symlink or other non-regular file.
//   - The bundle must declare the range of UI API levels it works with, and the
//     binary's APILevel must be inside it, so a UI built for another API is
//     refused instead of half-working.
//   - The verified bytes are held in memory and served from there. Serving never
//     touches the directory again, so nothing written to it after the check —
//     by a deploy in progress or by anyone else — reaches a browser.
//
// The manifest's digests prove the files are the ones it lists, not who wrote
// them: the directory is trusted because only the operator can name it
// (LOOMCYCLE_WEBUI_DIR). Any failure falls back to the embedded UI; a bundle
// problem never stops the server.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
)

// Status.Source values.
const (
	SourceEmbedded = "embedded" // the SPA compiled into the binary
	SourceDir      = "dir"      // a bundle loaded from LOOMCYCLE_WEBUI_DIR
)

// LoadError reasons. They are fixed codes because /healthz (unauthenticated)
// reports them: a reason never carries the configured path, a file name from
// the manifest or file contents. LoadError.Detail carries those, for the log.
const (
	ReasonDirUnreadable    = "dir_unreadable"        // the directory cannot be opened or walked
	ReasonManifestInvalid  = "manifest_invalid"      // missing, malformed, unknown field, bad value
	ReasonManifestTooLarge = "manifest_too_large"    // manifest.json over its size cap
	ReasonAPILevel         = "api_level_unsupported" // APILevel outside [api_level_min, api_level_max]
	ReasonPathInvalid      = "path_invalid"          // a manifest path that is not a clean relative path
	ReasonFileMissing      = "file_missing"          // a listed file is not on disk
	ReasonFileUnlisted     = "file_unlisted"         // a file on disk is not in the manifest
	ReasonFileNotRegular   = "file_not_regular"      // a symlink, device, pipe or directory where a file is listed
	ReasonDigestMismatch   = "digest_mismatch"       // a file's SHA-256 differs from the manifest
	ReasonIndexMissing     = "index_missing"         // the manifest does not list index.html
	ReasonBundleTooLarge   = "bundle_too_large"      // over the total byte or file-count cap
)

// manifestName is the bundle's manifest file. It is not part of the SPA: it is
// never served, and the manifest may not list it.
const manifestName = "manifest.json"

// Status says which Web UI the server is serving, for /healthz.
type Status struct {
	Source         string `json:"source"`
	UIVersion      string `json:"ui_version,omitempty"`
	FallbackReason string `json:"fallback_reason,omitempty"`
}

// LoadError is why a bundle was refused. Reason is one of the Reason*
// constants; Detail names the file or value at fault and is for the log only.
type LoadError struct {
	Reason string
	Detail string
}

func (e *LoadError) Error() string { return e.Reason + ": " + e.Detail }

func refuse(reason, format string, args ...any) error {
	return &LoadError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Manifest is a bundle's manifest.json. Unknown fields are refused, so a
// manifest written for a later loader with a check this one lacks is not
// silently accepted.
type Manifest struct {
	// UIVersion is the bundle's own version, reported in /healthz.
	UIVersion string `json:"ui_version"`
	// APILevelMin and APILevelMax bound the server API levels (APILevel) the
	// bundle works with, inclusive.
	APILevelMin int `json:"api_level_min"`
	APILevelMax int `json:"api_level_max"`
	// Files maps every file of the bundle, as a slash-separated path relative
	// to the bundle directory, to the lowercase hex SHA-256 of its bytes.
	Files map[string]string `json:"files"`
}

// limits bound what loading a bundle can cost. The whole bundle is held in
// memory, so bundleBytes is also the memory a bundle may take.
type limits struct {
	manifestBytes int64
	bundleBytes   int64
	files         int
}

// defaultLimits: a production build of web/ is a few MiB across fewer than a
// hundred files (source maps included), so these leave an order of magnitude of
// headroom while keeping a wrong directory — a home directory, a data
// directory — from being read into memory. A manifest entry is about 100 bytes,
// so 1 MiB fits the 10,000 files allowed.
var defaultLimits = limits{
	manifestBytes: 1 << 20,
	bundleBytes:   64 << 20,
	files:         10_000,
}

// maxUIVersionLen bounds ui_version, which /healthz echoes.
const maxUIVersionLen = 64

// maxPathLen bounds one manifest path.
const maxPathLen = 512

// Bundle is a verified Web UI held in memory.
type Bundle struct {
	version string
	files   map[string][]byte
}

// UIVersion is the manifest's ui_version.
func (b *Bundle) UIVersion() string { return b.version }

func (b *Bundle) file(name string) ([]byte, bool) {
	d, ok := b.files[name]
	return d, ok
}

// LoadDir loads and verifies the bundle in dir against the binary's APILevel.
// The error is a *LoadError.
func LoadDir(dir string) (*Bundle, error) {
	return loadDir(dir, APILevel, defaultLimits)
}

// LoadConfigured picks the UI to serve for the LOOMCYCLE_WEBUI_DIR value dir.
// Empty dir serves the embedded UI. Otherwise the bundle is loaded; on failure
// it returns a nil bundle, a Status naming the embedded UI and the reason, and
// the error for the caller to log.
func LoadConfigured(dir string) (*Bundle, Status, error) {
	if dir == "" {
		return nil, Status{Source: SourceEmbedded}, nil
	}
	b, err := LoadDir(dir)
	if err != nil {
		reason := ReasonDirUnreadable
		var le *LoadError
		if errors.As(err, &le) {
			reason = le.Reason
		}
		return nil, Status{Source: SourceEmbedded, FallbackReason: reason}, err
	}
	return b, Status{Source: SourceDir, UIVersion: b.version}, nil
}

func loadDir(dir string, level int, lim limits) (*Bundle, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, refuse(ReasonDirUnreadable, "open: %v", err)
	}
	defer root.Close()
	return loadRoot(root, level, lim)
}

// loadRoot verifies the bundle under root, which the caller opened and closes.
func loadRoot(root *os.Root, level int, lim limits) (*Bundle, error) {
	m, err := readManifest(root, lim)
	if err != nil {
		return nil, err
	}
	if level < m.APILevelMin || level > m.APILevelMax {
		return nil, refuse(ReasonAPILevel, "server UI API level %d is outside the bundle's range [%d, %d]",
			level, m.APILevelMin, m.APILevelMax)
	}
	if err := checkOnDisk(root, m); err != nil {
		return nil, err
	}

	files := make(map[string][]byte, len(m.Files))
	remaining := lim.bundleBytes
	for _, name := range sortedNames(m.Files) {
		data, err := readListed(root, name, remaining)
		if err != nil {
			return nil, err
		}
		remaining -= int64(len(data))
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != m.Files[name] {
			return nil, refuse(ReasonDigestMismatch, "%s: SHA-256 does not match the manifest", name)
		}
		files[name] = data
	}
	return &Bundle{version: m.UIVersion, files: files}, nil
}

// readManifest reads and validates manifest.json from root.
func readManifest(root *os.Root, lim limits) (*Manifest, error) {
	info, err := root.Lstat(manifestName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, refuse(ReasonManifestInvalid, "%s not found", manifestName)
	}
	if err != nil {
		return nil, refuse(ReasonDirUnreadable, "stat %s: %v", manifestName, err)
	}
	if !info.Mode().IsRegular() {
		return nil, refuse(ReasonFileNotRegular, "%s is not a regular file (%s)", manifestName, info.Mode().Type())
	}
	raw, err := readCapped(root, manifestName, lim.manifestBytes)
	if errors.Is(err, errOverCap) {
		return nil, refuse(ReasonManifestTooLarge, "%s exceeds %d bytes", manifestName, lim.manifestBytes)
	}
	if err != nil {
		return nil, refuse(ReasonDirUnreadable, "read %s: %v", manifestName, err)
	}

	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, refuse(ReasonManifestInvalid, "decode %s: %v", manifestName, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, refuse(ReasonManifestInvalid, "%s has data after the JSON object", manifestName)
	}

	if !validUIVersion(m.UIVersion) {
		return nil, refuse(ReasonManifestInvalid,
			"ui_version must be 1-%d characters of letters, digits and . _ + -", maxUIVersionLen)
	}
	if m.APILevelMin < 1 || m.APILevelMax < m.APILevelMin {
		return nil, refuse(ReasonManifestInvalid, "api level range [%d, %d] is not 1 <= min <= max",
			m.APILevelMin, m.APILevelMax)
	}
	if len(m.Files) > lim.files {
		return nil, refuse(ReasonBundleTooLarge, "manifest lists %d files, the limit is %d", len(m.Files), lim.files)
	}
	for _, name := range sortedNames(m.Files) {
		sum := m.Files[name]
		if err := validPath(name); err != nil {
			return nil, refuse(ReasonPathInvalid, "%q: %v", name, err)
		}
		if !validDigest(sum) {
			return nil, refuse(ReasonManifestInvalid, "%s: digest must be 64 lowercase hex characters", name)
		}
	}
	if _, ok := m.Files["index.html"]; !ok {
		return nil, refuse(ReasonIndexMissing, "the manifest does not list index.html")
	}
	return &m, nil
}

// checkOnDisk walks the bundle through root and refuses anything that is not a
// directory or a listed regular file, so a link or a stray file cannot hide in
// a bundle that otherwise verifies. WalkDir does not follow symlinks; it
// reports them as entries, which is what lets this refuse them. Names found on
// disk have passed no validation, so the details quote them (%q): a file name
// with a newline cannot forge a log line.
func checkOnDisk(root *os.Root, m *Manifest) error {
	return fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return refuse(ReasonDirUnreadable, "walk %q: %v", name, err)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return refuse(ReasonFileNotRegular, "%q is not a regular file (%s)", name, d.Type())
		}
		if name == manifestName {
			return nil
		}
		if _, ok := m.Files[name]; !ok {
			return refuse(ReasonFileUnlisted, "%q is not listed in the manifest", name)
		}
		return nil
	})
}

// readListed reads one listed file through root, refusing a missing file and
// anything that is not a regular file. The walk already refused links, but the
// directory can change between the walk and the read; the digest check after
// this read is what the served bytes are held to.
func readListed(root *os.Root, name string, remaining int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, refuse(ReasonFileMissing, "%s is listed but not present", name)
	}
	if err != nil {
		// A path through a link or a non-directory lands here too.
		return nil, refuse(ReasonFileNotRegular, "stat %s: %v", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, refuse(ReasonFileNotRegular, "%s is not a regular file (%s)", name, info.Mode().Type())
	}
	data, err := readCapped(root, name, remaining)
	if errors.Is(err, errOverCap) {
		return nil, refuse(ReasonBundleTooLarge, "the bundle exceeds the total size limit at %s", name)
	}
	if err != nil {
		return nil, refuse(ReasonDirUnreadable, "read %s: %v", name, err)
	}
	return data, nil
}

// sortedNames returns the manifest's paths in a fixed order, so a bundle with
// several defects is refused for the same one, naming the same file, every
// time.
func sortedNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

var errOverCap = errors.New("over the size cap")

// readCapped reads name through root, failing with errOverCap past capBytes
// rather than reading an oversized file into memory first.
func readCapped(root *os.Root, name string, capBytes int64) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (%s)", info.Mode().Type())
	}
	if capBytes < 0 {
		capBytes = 0
	}
	data, err := io.ReadAll(io.LimitReader(f, capBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > capBytes {
		return nil, errOverCap
	}
	return data, nil
}

// validPath accepts a clean relative slash-separated path: no leading slash,
// no "." or ".." element, no empty element, no backslash, no control
// character, and not the manifest itself.
func validPath(name string) error {
	switch {
	case name == "":
		return errors.New("empty path")
	case len(name) > maxPathLen:
		return fmt.Errorf("longer than %d bytes", maxPathLen)
	case strings.ContainsRune(name, '\\'):
		return errors.New("backslash in path")
	case strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return errors.New("control character in path")
	case !fs.ValidPath(name) || name == ".":
		// fs.ValidPath refuses a leading or trailing slash, an empty element
		// and any "." or ".." element.
		return errors.New("not a clean relative path")
	case name == manifestName:
		return errors.New("the manifest cannot list itself")
	}
	return nil
}

func validDigest(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validUIVersion(s string) bool {
	if s == "" || len(s) > maxUIVersionLen {
		return false
	}
	for _, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '+' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// Option configures Handler.
type Option func(*handlerOptions)

type handlerOptions struct{ bundle *Bundle }

// WithBundle serves b instead of the embedded SPA. A nil b keeps the embedded
// SPA, so a caller can pass LoadConfigured's result straight through.
func WithBundle(b *Bundle) Option { return func(o *handlerOptions) { o.bundle = b } }
