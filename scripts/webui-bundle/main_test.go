package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/webui"
)

// writeDist lays out a small built UI: an index, nested assets, a .gitkeep
// and a stale manifest.json from an earlier bundle, which the tool must drop.
func writeDist(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	for name, body := range map[string]string{
		"index.html":           "<!doctype html><title>ui</title>",
		"favicon.svg":          "<svg/>",
		"assets/index-abc.js":  "console.log('ui')",
		"assets/index-abc.css": "body{}",
		"assets/fonts/a.woff2": "\x00\x01binary",
		".gitkeep":             "",
		"manifest.json":        `{"stale": true}`,
	} {
		p := filepath.Join(dist, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dist
}

func opts(dist, out string) options {
	return options{dist: dist, version: "1.2.3-rc.1", out: out, apiMin: webui.APILevel, apiMax: webui.APILevel}
}

func TestRun_ArchiveUnpacksToABundleTheServerLoads(t *testing.T) {
	out := t.TempDir()
	res, err := run(opts(writeDist(t), out))
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(res.archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(res.archivePath) != "loomcycle-webui-1.2.3-rc.1.tar.gz" {
		t.Errorf("archive name = %s", filepath.Base(res.archivePath))
	}

	// The .sha256 file is what `shasum -a 256 -c` reads: "<hex>  <name>\n".
	sum := sha256.Sum256(archive)
	wantSum := hex.EncodeToString(sum[:]) + "  " + filepath.Base(res.archivePath) + "\n"
	if got, err := os.ReadFile(res.sumPath); err != nil || string(got) != wantSum {
		t.Fatalf(".sha256 = %q, %v; want %q", got, err, wantSum)
	}

	dir := t.TempDir()
	if err := unpackForTest(t, archive, dir); err != nil {
		t.Fatal(err)
	}
	b, err := webui.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir refused the bundle: %v", err)
	}
	if b.UIVersion() != "1.2.3-rc.1" {
		t.Errorf("UIVersion = %q", b.UIVersion())
	}

	raw, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	var m webui.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.APILevelMin != webui.APILevel || m.APILevelMax != webui.APILevel {
		t.Errorf("api level range = [%d, %d], want [%d, %d]", m.APILevelMin, m.APILevelMax, webui.APILevel, webui.APILevel)
	}
	for _, left := range []string{".gitkeep", manifestName} {
		if _, ok := m.Files[left]; ok {
			t.Errorf("manifest lists %s", left)
		}
	}
	if len(m.Files) != 5 {
		t.Errorf("manifest lists %d files, want 5: %v", len(m.Files), m.Files)
	}
}

// The archive's top level is the bundle: manifest.json is the first entry, no
// entry sits under a leading directory, every entry is a file or directory
// with fixed metadata, and a directory precedes what it holds.
func TestPack_EntriesAreTopLevelFixedAndOrdered(t *testing.T) {
	res, err := run(opts(writeDist(t), t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(res.archivePath)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if zr.Name != "" || !zr.ModTime.IsZero() {
		t.Errorf("gzip header carries name %q, time %v", zr.Name, zr.ModTime)
	}
	tr := tar.NewReader(zr)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			t.Errorf("%s: type %q", h.Name, h.Typeflag)
		}
		if !h.ModTime.Equal(entryTime) || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Errorf("%s: metadata not fixed: %+v", h.Name, h)
		}
		if strings.HasPrefix(h.Name, "/") || strings.HasPrefix(h.Name, "./") || strings.Contains(h.Name, "..") {
			t.Errorf("%s: not a clean relative name", h.Name)
		}
	}
	want := []string{
		"manifest.json",
		"assets/",
		"assets/fonts/",
		"assets/fonts/a.woff2",
		"assets/index-abc.css",
		"assets/index-abc.js",
		"favicon.svg",
		"index.html",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("entries = %v\nwant      %v", names, want)
	}
}

func TestRun_SameDistGivesIdenticalBytes(t *testing.T) {
	dist := writeDist(t)
	a, err := run(opts(dist, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	// Touch every file so only the mtimes differ between the two runs.
	later := entryTime.AddDate(30, 0, 0)
	_ = filepath.WalkDir(dist, func(p string, _ os.DirEntry, _ error) error { return os.Chtimes(p, later, later) })
	b, err := run(opts(dist, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	ab, _ := os.ReadFile(a.archivePath)
	bb, _ := os.ReadFile(b.archivePath)
	if !bytes.Equal(ab, bb) || a.digest != b.digest {
		t.Fatalf("two runs differ: %s vs %s", a.digest, b.digest)
	}
}

func TestRun_RefusesASymlinkInDist(t *testing.T) {
	dist := writeDist(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(dist, "assets", "link.js")); err != nil {
		t.Fatal(err)
	}
	assertRefused(t, opts(dist, t.TempDir()), "not a regular file")
}

func TestRun_RefusesADistWithoutIndexHTML(t *testing.T) {
	dist := writeDist(t)
	if err := os.Remove(filepath.Join(dist, "index.html")); err != nil {
		t.Fatal(err)
	}
	assertRefused(t, opts(dist, t.TempDir()), "no index.html")
}

func TestRun_RefusesAnInvalidVersion(t *testing.T) {
	for _, v := range []string{"1.2.3/../x", "1 2", "v1;rm", strings.Repeat("9", 65)} {
		o := opts(writeDist(t), t.TempDir())
		o.version = v
		assertRefused(t, o, webui.ReasonManifestInvalid)
	}
}

func TestRun_RefusesARangeWithoutThisTreesAPILevel(t *testing.T) {
	o := opts(writeDist(t), t.TempDir())
	o.apiMin, o.apiMax = webui.APILevel+1, webui.APILevel+2
	assertRefused(t, o, "must include")
}

func TestRun_AcceptsAWiderRangeAroundThisTreesAPILevel(t *testing.T) {
	o := opts(writeDist(t), t.TempDir())
	o.apiMax = webui.APILevel + 3
	if _, err := run(o); err != nil {
		t.Fatal(err)
	}
}

// assertRefused checks run fails with want in the error and writes nothing.
func assertRefused(t *testing.T, o options, want string) {
	t.Helper()
	_, err := run(o)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("version %q: err = %v, want one containing %q", o.version, err, want)
	}
	entries, rerr := os.ReadDir(o.out)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Errorf("a refused run wrote %d files into -out", len(entries))
	}
}

// unpackForTest extracts with a reader written independently of the tool's
// own unpack, so a bug shared by pack and unpack cannot hide here.
func unpackForTest(t *testing.T, archive []byte, dir string) error {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		p := filepath.Join(dir, filepath.FromSlash(h.Name))
		if !strings.HasPrefix(p, dir+string(filepath.Separator)) {
			t.Fatalf("%q escapes the directory", h.Name)
		}
		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
	}
}
