package builtin

import (
	"bytes"
	"context"
	"io/fs"
	"strings"
	"testing"
)

// fuzzUnpacker is a dry-run walk with small bounds: what the listing and the
// decisions do with any archive, writing nothing.
func fuzzUnpacker() *unpacker {
	u := &unpacker{
		ctx:     context.Background(),
		lim:     unpackLimits{listed: 50, written: 20, total: 1 << 20, ratio: 100, ratioFree: 1 << 16},
		perFile: 1 << 18, budget: 1 << 20, dryRun: true, archive: "volume:inbox:documents/x.zip",
	}
	u.dest = &inboxDest{u: u, dir: "unpacked/x"}
	return u
}

// checkUnpackAnswer holds what any walk must: bounds kept, and every planned
// file inside the archive's folder, by sanitised, visible segments.
func checkUnpackAnswer(t *testing.T, u *unpacker) {
	t.Helper()
	if u.counts.Listed > u.lim.listed || u.counts.Written > u.lim.written || u.counts.Written != len(u.written) {
		t.Fatalf("counts = %+v (%d written)", u.counts, len(u.written))
	}
	// One read may pass the budget; the next is refused.
	if u.counts.BytesInflated > u.lim.total+32<<10 {
		t.Fatalf("inflated %d past a budget of %d", u.counts.BytesInflated, u.lim.total)
	}
	for _, w := range u.written {
		rel, ok := strings.CutPrefix(w.Ref, "volume:inbox:unpacked/x/")
		if !ok {
			t.Fatalf("planned %q outside the archive's folder", w.Ref)
		}
		segs := strings.Split(rel, "/")
		if len(segs) > unpackMaxDepth {
			t.Fatalf("planned %q deeper than %d", w.Ref, unpackMaxDepth)
		}
		for _, s := range segs {
			if s == "" || s == "." || s == ".." || strings.HasPrefix(s, ".") || strings.ContainsAny(s, "\\:") {
				t.Fatalf("planned %q has segment %q", w.Ref, s)
			}
		}
	}
}

func FuzzUnpackZip(f *testing.F) {
	f.Add(zipArchive(f,
		zipEnt{name: "books/a.pdf", body: filePDF},
		zipEnt{name: "../x.txt", body: unpackText},
		zipEnt{name: "l", body: []byte("t"), mode: fs.ModeSymlink | 0o777},
		zipEnt{name: "d/", mode: fs.ModeDir | 0o755},
	))
	f.Add(zipArchive(f, zipEnt{name: `C:\w\.h`, body: unpackEXE, encrypted: true}, zipEnt{name: "Звіт.txt", body: unpackText}))
	f.Add(zipArchive(f, zipEnt{name: "a/b/c/d/e/f/g/h/i.txt", body: unpackText}, zipEnt{name: "inner.zip", body: zipArchive(f, zipEnt{name: "x.pdf", body: filePDF})}))
	f.Add([]byte("PK\x05\x06" + strings.Repeat("\x00", 18)))
	f.Fuzz(func(t *testing.T, data []byte) {
		u := fuzzUnpacker()
		_ = u.walkZip(bytes.NewReader(data), int64(len(data)))
		checkUnpackAnswer(t, u)
	})
}

func FuzzUnpackRAR(f *testing.F) {
	ents := []rarEnt{{name: "books", dir: true}, {name: "books/a.pdf", body: filePDF}, {name: "l", symlink: "a"}, {name: "../x.txt", body: unpackText}}
	f.Add(rar5Archive(rarOpts{}, ents...))
	f.Add(rar5Archive(rarOpts{solid: true}, ents...))
	f.Add(rar4Archive(rarOpts{}, ents...))
	f.Add(rar5Archive(rarOpts{}, rarEnt{name: "u.txt", body: unpackText, unknownSize: true}))
	f.Fuzz(func(t *testing.T, data []byte) {
		u := fuzzUnpacker()
		_ = u.walkRAR(bytes.NewReader(data))
		checkUnpackAnswer(t, u)
	})
}

// Any name either is skipped with a reason or becomes at most 8 sanitised,
// visible, non-climbing segments.
func FuzzUnpackPath(f *testing.F) {
	for _, s := range []string{"a/b.pdf", "../x", "/abs", `C:\x`, "a/../../b", ".h", "%%%", "a//./b", "Звіт/№1.pdf"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		segs, reason := unpackPath(name)
		if reason != "" {
			if segs != nil {
				t.Fatalf("%q skipped with segments %v", name, segs)
			}
			return
		}
		if len(segs) == 0 || len(segs) > unpackMaxDepth {
			t.Fatalf("%q -> %v", name, segs)
		}
		for _, s := range segs {
			if s == "" || s == "." || s == ".." || strings.HasPrefix(s, ".") || strings.ContainsAny(s, "/\\:") {
				t.Fatalf("%q -> segment %q", name, s)
			}
		}
	})
}
