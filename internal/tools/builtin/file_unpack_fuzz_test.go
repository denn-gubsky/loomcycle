package builtin

import (
	"bytes"
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/files/unpack"
)

// fuzzLimits are small bounds for a dry-run walk: what the listing and
// the decisions do with any archive, writing nothing.
var fuzzLimits = unpack.Limits{Listed: 50, Written: 20, Total: 1 << 20, Ratio: 100, RatioFree: 1 << 16}

func fuzzUnpack(kind string, data []byte) *unpack.Result {
	opt := unpack.Options{Limits: fuzzLimits, PerFile: 1 << 18, Budget: 1 << 20, DryRun: true}
	dest := &inboxDest{ctx: context.Background(), archive: "volume:inbox:documents/x.zip", dir: "unpacked/x"}
	res, err := unpack.Run(context.Background(), kind, bytes.NewReader(data), int64(len(data)), opt, dest)
	if err != nil {
		return &unpack.Result{}
	}
	return res
}

// checkUnpackAnswer holds what any walk must: bounds kept, and every planned
// file inside the archive's folder, by sanitised, visible segments.
func checkUnpackAnswer(t *testing.T, u *unpack.Result) {
	t.Helper()
	if u.Counts.Listed > fuzzLimits.Listed || u.Counts.Written > fuzzLimits.Written || u.Counts.Written != len(u.Written) {
		t.Fatalf("counts = %+v (%d written)", u.Counts, len(u.Written))
	}
	// One read may pass the budget; the next is refused.
	if u.Counts.BytesInflated > fuzzLimits.Total+32<<10 {
		t.Fatalf("inflated %d past a budget of %d", u.Counts.BytesInflated, fuzzLimits.Total)
	}
	for _, w := range u.Written {
		rel, ok := strings.CutPrefix(w.Ref, "volume:inbox:unpacked/x/")
		if !ok {
			t.Fatalf("planned %q outside the archive's folder", w.Ref)
		}
		segs := strings.Split(rel, "/")
		if len(segs) > unpack.MaxDepth {
			t.Fatalf("planned %q deeper than %d", w.Ref, unpack.MaxDepth)
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
		checkUnpackAnswer(t, fuzzUnpack("zip", data))
	})
}

func FuzzUnpackRAR(f *testing.F) {
	ents := []rarEnt{{name: "books", dir: true}, {name: "books/a.pdf", body: filePDF}, {name: "l", symlink: "a"}, {name: "../x.txt", body: unpackText}}
	f.Add(rar5Archive(rarOpts{}, ents...))
	f.Add(rar5Archive(rarOpts{solid: true}, ents...))
	f.Add(rar4Archive(rarOpts{}, ents...))
	f.Add(rar5Archive(rarOpts{}, rarEnt{name: "u.txt", body: unpackText, unknownSize: true}))
	f.Fuzz(func(t *testing.T, data []byte) {
		checkUnpackAnswer(t, fuzzUnpack("rar", data))
	})
}

// Any name either is skipped with a reason or becomes at most 8 sanitised,
// visible, non-climbing segments.
func FuzzUnpackPath(f *testing.F) {
	for _, s := range []string{"a/b.pdf", "../x", "/abs", `C:\x`, "a/../../b", ".h", "%%%", "a//./b", "Звіт/№1.pdf"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		segs, reason := unpack.Path(name)
		if reason != "" {
			if segs != nil {
				t.Fatalf("%q skipped with segments %v", name, segs)
			}
			return
		}
		if len(segs) == 0 || len(segs) > unpack.MaxDepth {
			t.Fatalf("%q -> %v", name, segs)
		}
		for _, s := range segs {
			if s == "" || s == "." || s == ".." || strings.HasPrefix(s, ".") || strings.ContainsAny(s, "/\\:") {
				t.Fatalf("%q -> segment %q", name, s)
			}
		}
	})
}
