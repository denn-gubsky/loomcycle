package worker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func mkArchive(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, Size: int64(len(e.body))}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A template archive is unpacked only inside its folder (D-P6-33): a name
// that is absolute or climbs out, and a write through a link out of the
// folder, are refused, and nothing is left behind; a link inside is made as
// a link and never followed.
func TestUnpackTemplate_StaysInsideItsFolder(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, archive := range map[string][]byte{
		"a name that climbs out": mkArchive(t, tarEntry{name: "../escaped", body: "x", typ: tar.TypeReg}),
		"an absolute name":       mkArchive(t, tarEntry{name: filepath.Join(outside, "abs"), body: "x", typ: tar.TypeReg}),
		"a write through a link": mkArchive(t,
			tarEntry{name: "out", link: outside, typ: tar.TypeSymlink},
			tarEntry{name: "out/through", body: "x", typ: tar.TypeReg}),
		"a folder through a link": mkArchive(t,
			tarEntry{name: "out", link: outside, typ: tar.TypeSymlink},
			tarEntry{name: "out/sub/", typ: tar.TypeDir, mode: 0o755}),
	} {
		dst := filepath.Join(base, "dst-"+strings.ReplaceAll(name, " ", "-"))
		if _, err := unpackTemplate(bytes.NewReader(archive), dst, 1<<20); err == nil {
			t.Errorf("%s: unpacked", name)
		}
		if _, err := os.Lstat(dst); err == nil {
			t.Errorf("%s: left %s behind", name, dst)
		}
		for _, p := range []string{filepath.Join(base, "escaped"), filepath.Join(outside, "abs"), filepath.Join(outside, "through"), filepath.Join(outside, "sub")} {
			if _, err := os.Lstat(p); err == nil {
				t.Errorf("%s: wrote %s outside the folder", name, p)
			}
		}
	}

	dst := filepath.Join(base, "ok")
	res, err := unpackTemplate(bytes.NewReader(mkArchive(t,
		tarEntry{name: "venv/", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "venv/bin/", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "venv/bin/tool", body: "#!/bin/sh\n", typ: tar.TypeReg, mode: 0o4755},
		tarEntry{name: "venv/python", link: "/usr/bin/python3", typ: tar.TypeSymlink},
		tarEntry{name: "hard", link: "venv/bin/tool", typ: tar.TypeLink},
	)), dst, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "venv/bin/tool")); err != nil || string(b) != "#!/bin/sh\n" {
		t.Errorf("file = %q, %v", b, err)
	}
	if fi, err := os.Lstat(filepath.Join(dst, "venv/bin/tool")); err != nil || fi.Mode()&os.ModeSetuid != 0 || fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, %v; want 0755 without setuid", fi.Mode(), err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "venv/python")); err != nil || target != "/usr/bin/python3" {
		t.Errorf("link = %q, %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "hard")); err == nil {
		t.Error("a hard link entry was made")
	}
	if res.files != 1 || len(res.skipped) != 1 || res.skipped[0] != "hard" {
		t.Errorf("result = %+v", res)
	}
}

// The unpacked bytes are bounded: an archive whose files pass the bound is
// refused and nothing is left, whatever its headers claim.
func TestUnpackTemplate_StopsAtItsByteBound(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "big")
	big := strings.Repeat("a", 4096)
	_, err := unpackTemplate(bytes.NewReader(mkArchive(t,
		tarEntry{name: "one", body: big, typ: tar.TypeReg},
		tarEntry{name: "two", body: big, typ: tar.TypeReg})), dst, 6000)
	if err == nil {
		t.Fatal("an archive past its bound was unpacked")
	}
	if _, err := os.Lstat(dst); err == nil {
		t.Error("left a partial tree")
	}
}
