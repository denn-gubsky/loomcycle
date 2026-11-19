package builtin

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"encoding/json"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

type zipEnt struct {
	name      string
	body      []byte
	mode      fs.FileMode // 0: a regular file
	encrypted bool
}

func zipArchive(t testing.TB, ents ...zipEnt) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range ents {
		fh := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			fh.SetMode(e.mode)
		}
		if e.encrypted {
			fh.Flags |= 0x1
		}
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var (
	unpackPNG  = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 100)...)
	unpackEXE  = append([]byte("MZ\x90\x00\x03\x00\x00\x00"), bytes.Repeat([]byte{0xff, 0x01}, 200)...)
	unpackText = []byte("A chapter of plain text.\n")
)

// unpackRun is fileRun with the archive kept in the inbox, as a tool would
// have returned it: the run's context, the archive's reference and the
// read-write volume "docs".
func unpackRun(t *testing.T, svc inbox.Service, archive []byte, name string) (context.Context, string, string) {
	t.Helper()
	r, ctx, docs := fileRun(t, svc, false)
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: name, Origin: "mcp:library/get"}, archive)
	if !kept.Kept {
		t.Fatalf("capture: %s", kept.Stub)
	}
	return ctx, kept.Record.Ref, docs
}

func unpackExec(t *testing.T, f *File, ctx context.Context, in map[string]any) (map[string]any, tools.Result) {
	t.Helper()
	raw, _ := json.Marshal(in)
	if f == nil {
		f = &File{}
	}
	res, err := f.Execute(ctx, raw)
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

func refsOf(out map[string]any, key string) []string {
	var refs []string
	list, _ := out[key].([]any)
	for _, w := range list {
		refs = append(refs, w.(map[string]any)["ref"].(string))
	}
	sort.Strings(refs)
	return refs
}

func skippedOf(out map[string]any) map[string]string {
	got := map[string]string{}
	list, _ := out["skipped"].([]any)
	for _, s := range list {
		m := s.(map[string]any)
		got[m["name"].(string)] = m["reason"].(string)
	}
	return got
}

func count(out map[string]any, k string) int {
	c, _ := out["counts"].(map[string]any)
	v, _ := c[k].(float64)
	return int(v)
}

func unpackInboxRoot(t *testing.T, ctx context.Context) string {
	t.Helper()
	ref, ok := tools.EphemeralVolumes(ctx).Get(inbox.Name)
	if !ok {
		t.Fatal("no inbox")
	}
	return ref.Root
}

// The default writes what extract reads and the archives inside, and lists
// a nested archive without opening it.
func TestUnpack_DefaultWritesReadableFilesAndNestedArchives(t *testing.T) {
	inner := zipArchive(t, zipEnt{name: "deep.pdf", body: filePDF})
	arc := zipArchive(t,
		zipEnt{name: "books/", mode: fs.ModeDir | 0o755},
		zipEnt{name: "books/Звіт.pdf", body: filePDF},
		zipEnt{name: "books/notes.txt", body: unpackText},
		zipEnt{name: "cover.png", body: unpackPNG},
		zipEnt{name: "setup.exe", body: unpackEXE},
		zipEnt{name: "more/inner.zip", body: inner},
	)
	ctx, ref, _ := unpackRun(t, inbox.Service{}, arc, "library.zip")
	out, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	want := []string{
		"volume:inbox:unpacked/library/books/Zvit.pdf",
		"volume:inbox:unpacked/library/books/notes.txt",
		"volume:inbox:unpacked/library/cover.png",
		"volume:inbox:unpacked/library/more/inner.zip",
	}
	if got := refsOf(out, "written"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("written = %v", got)
	}
	if r := skippedOf(out)["setup.exe"]; !strings.Contains(r, "not a format extract reads") {
		t.Errorf("setup.exe: %q (skipped %v)", r, out["skipped"])
	}
	// The nested archive is one entry: its own deep.pdf is never listed.
	if count(out, "listed") != 6 || count(out, "folders") != 1 || count(out, "written") != 4 {
		t.Errorf("counts = %v", out["counts"])
	}
	if out["to"] != "volume:inbox:unpacked/library/" || out["format"] != "zip" {
		t.Errorf("answer = %v", out)
	}
	got, err := os.ReadFile(filepath.Join(unpackInboxRoot(t, ctx), "unpacked", "library", "books", "Zvit.pdf"))
	if err != nil || !bytes.Equal(got, filePDF) {
		t.Errorf("the unpacked pdf: %v", err)
	}
}

// Each file is a capture whose record names the archive and the entry, so
// extract and import say where it came from.
func TestUnpack_CaptureRecordNamesTheArchiveAndEntry(t *testing.T) {
	arc := zipArchive(t, zipEnt{name: "board/minutes.pdf", body: filePDF}, zipEnt{name: "x[1]`.txt", body: unpackText})
	ctx, ref, _ := unpackRun(t, inbox.Service{}, arc, "papers.zip")
	if _, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref}); res.IsError {
		t.Fatal(res.Text)
	}
	out, res := fileExec(t, ctx, map[string]any{"op": "stat", "path": "volume:inbox:unpacked/papers/board/minutes.pdf"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if out["origin"] != "unpack:"+ref+"#board/minutes.pdf" || out["media_type"] != "application/pdf" {
		t.Errorf("stat = %v", out)
	}
	rec, ok := inbox.Lookup(ctx, "unpacked/papers/x-1-.txt")
	if !ok || rec.Origin != "unpack:"+ref+"#x_1__.txt" {
		t.Errorf("record = %+v %v", rec, ok)
	}
}

func TestUnpack_AllWritesEveryRegularFile(t *testing.T) {
	arc := zipArchive(t, zipEnt{name: "setup.exe", body: unpackEXE}, zipEnt{name: "a.txt", body: unpackText})
	ctx, ref, _ := unpackRun(t, inbox.Service{}, arc, "tools.zip")
	out, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref, "all": true})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if count(out, "written") != 2 || count(out, "skipped") != 0 {
		t.Errorf("answer = %v", out)
	}
}

func TestUnpack_DryRunWritesNothing(t *testing.T) {
	arc := zipArchive(t, zipEnt{name: "a.pdf", body: filePDF}, zipEnt{name: "setup.exe", body: unpackEXE}, zipEnt{name: "../up.txt", body: unpackText})
	ctx, ref, _ := unpackRun(t, inbox.Service{}, arc, "set.zip")
	out, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref, "dry_run": true})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := refsOf(out, "would_write"); strings.Join(got, ",") != "volume:inbox:unpacked/set/a.pdf" || out["written"] != nil || out["dry_run"] != true {
		t.Errorf("answer = %v", out)
	}
	if count(out, "skipped") != 2 {
		t.Errorf("skipped = %v", out["skipped"])
	}
	if _, err := os.Stat(filepath.Join(unpackInboxRoot(t, ctx), "unpacked")); !os.IsNotExist(err) {
		t.Errorf("a dry run wrote: %v", err)
	}
}

func TestUnpack_UnsafeNamesAreSkipped(t *testing.T) {
	deep8 := "a/b/c/d/e/f/g/h.txt"
	deep9 := "a/b/c/d/e/f/g/h/i.txt"
	arc := zipArchive(t,
		zipEnt{name: "../escape.txt", body: unpackText},
		zipEnt{name: "/abs.txt", body: unpackText},
		zipEnt{name: "a/../../climb.txt", body: unpackText},
		zipEnt{name: `C:\win.txt`, body: unpackText},
		zipEnt{name: `dir\..\..\back.txt`, body: unpackText},
		zipEnt{name: ".hidden.txt", body: unpackText},
		zipEnt{name: "repo/.git/config.txt", body: unpackText},
		zipEnt{name: "%%%", body: unpackText},
		zipEnt{name: deep9, body: unpackText},
		zipEnt{name: deep8, body: unpackText},
	)
	ctx, ref, _ := unpackRun(t, inbox.Service{}, arc, "evil.zip")
	out, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := refsOf(out, "written"); strings.Join(got, ",") != "volume:inbox:unpacked/evil/"+deep8 {
		t.Errorf("written = %v", got)
	}
	skipped := skippedOf(out)
	for name, want := range map[string]string{
		"../escape.txt": "climbs", "/abs.txt": "absolute", "a/../../climb.txt": "climbs", `C:\win.txt`: "absolute",
		`dir\..\..\back.txt`: "climbs", ".hidden.txt": "hidden", "repo/.git/config.txt": "hidden", "%%%": "nothing left",
		deep9: "more than 8 levels",
	} {
		if !strings.Contains(skipped[name], want) {
			t.Errorf("%s: skipped for %q, want %q", name, skipped[name], want)
		}
	}
	// Nothing landed outside the archive's folder.
	root := unpackInboxRoot(t, ctx)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(root, p)
		if !d.IsDir() && !strings.HasPrefix(rel, "unpacked/evil/") && !strings.HasPrefix(rel, "documents/") {
			t.Errorf("wrote %s", rel)
		}
		return nil
	})
}

func TestUnpack_NeverOverwritesAndNumbersDuplicates(t *testing.T) {
	arc := zipArchive(t, zipEnt{name: "a.txt", body: []byte("first\n")}, zipEnt{name: "A.txt/../a.txt", body: unpackText}, zipEnt{name: "a.txt", body: []byte("second\n")})
	ctx, ref, docs := unpackRun2(t, arc)
	out, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := refsOf(out, "written"); strings.Join(got, ",") != "volume:inbox:unpacked/dup/a-2.txt,volume:inbox:unpacked/dup/a.txt" {
		t.Errorf("written = %v", got)
	}
	// Again, into the same folder: the first files stay as they were.
	out, _ = unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref})
	if got := refsOf(out, "written"); strings.Join(got, ",") != "volume:inbox:unpacked/dup/a-3.txt,volume:inbox:unpacked/dup/a-4.txt" {
		t.Errorf("second unpack wrote %v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(unpackInboxRoot(t, ctx), "unpacked", "dup", "a.txt")); string(b) != "first\n" {
		t.Errorf("a.txt = %q", b)
	}
	// Into a volume folder holding a file of the same name: kept as it is.
	_ = os.Mkdir(filepath.Join(docs, "lib"), 0o700)
	_ = os.WriteFile(filepath.Join(docs, "lib", "a.txt"), []byte("mine"), 0o600)
	out, res = unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref, "to": "volume:docs:lib/"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := refsOf(out, "written"); strings.Join(got, ",") != "volume:docs:lib/a-2.txt,volume:docs:lib/a-3.txt" {
		t.Errorf("written = %v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(docs, "lib", "a.txt")); string(b) != "mine" {
		t.Errorf("the volume's own a.txt = %q", b)
	}
}

func unpackRun2(t *testing.T, arc []byte) (context.Context, string, string) {
	t.Helper()
	r, ctx, docs := fileRun(t, inbox.Service{}, false)
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "dup.zip", Origin: "o"}, arc)
	return ctx, kept.Record.Ref, docs
}

func TestUnpack_ToAVolumeNeedsWriteAndItsHooks(t *testing.T) {
	arc := zipArchive(t, zipEnt{name: "shelf/a.pdf", body: filePDF}, zipEnt{name: "shelf/secret.pdf", body: filePDF})
	ctx, ref, docs := unpackRun2(t, arc)
	_ = os.Mkdir(filepath.Join(docs, "lib"), 0o700)

	noWrite := tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) { p.Tools = []string{"File", "Read"} })
	if _, res := unpackExec(t, nil, noWrite, map[string]any{"op": "unpack", "path": ref, "to": "volume:docs:lib/"}); !res.IsError || !strings.Contains(res.Text, "needs the Write tool") {
		t.Errorf("without Write: %q", res.Text)
	}
	if _, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref, "to": "volume:ref:."}); !res.IsError {
		t.Errorf("into a read-only volume: %q", res.Text)
	}
	if _, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref, "to": "volume:docs:no/such/"}); !res.IsError {
		t.Errorf("into a missing folder: %q", res.Text)
	}
	if ents, _ := os.ReadDir(filepath.Join(docs, "lib")); len(ents) != 0 {
		t.Fatalf("refused unpacks wrote %v", ents)
	}

	hooked := tools.WithHookPreCheck(ctx, func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
		if name == "Write" && strings.Contains(string(input), "secret") {
			return tools.Result{Text: "no secrets here", IsError: true}, false
		}
		return tools.Result{}, true
	})
	out, res := unpackExec(t, nil, hooked, map[string]any{"op": "unpack", "path": ref, "to": "volume:docs:lib/"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := refsOf(out, "written"); strings.Join(got, ",") != "volume:docs:lib/shelf/a.pdf" {
		t.Errorf("written = %v", got)
	}
	if r := skippedOf(out)["shelf/secret.pdf"]; !strings.Contains(r, "no secrets here") {
		t.Errorf("secret: %q", r)
	}
	if b, err := os.ReadFile(filepath.Join(docs, "lib", "shelf", "a.pdf")); err != nil || !bytes.Equal(b, filePDF) {
		t.Errorf("a.pdf: %v", err)
	}
	if _, err := os.Stat(filepath.Join(docs, "lib", "shelf", "secret.pdf")); !os.IsNotExist(err) {
		t.Error("a hook-denied file was written")
	}
}

func TestUnpack_ArchiveOutsideTheInboxNeedsRead(t *testing.T) {
	_, ctx, docs := fileRun(t, inbox.Service{}, false)
	_ = os.WriteFile(filepath.Join(docs, "set.zip"), zipArchive(t, zipEnt{name: "a.pdf", body: filePDF}), 0o600)
	noRead := tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) { p.Tools = []string{"File"} })
	if _, res := unpackExec(t, nil, noRead, map[string]any{"op": "unpack", "path": "volume:docs:set.zip"}); !res.IsError || !strings.Contains(res.Text, "needs the Read tool") {
		t.Errorf("without Read: %q", res.Text)
	}
	out, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": "volume:docs:set.zip"})
	if res.IsError || count(out, "written") != 1 {
		t.Errorf("with Read: %v %q", out, res.Text)
	}
}

// The inbox writer is a runtime write: a run whose inbox is read-only to it
// still unpacks into it.
func TestUnpack_ReadOnlyInboxStillUnpacks(t *testing.T) {
	r, ctx, _ := fileRun(t, inbox.Service{}, true)
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "a.zip", Origin: "o"}, zipArchive(t, zipEnt{name: "a.pdf", body: filePDF}))
	out, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": kept.Record.Ref})
	if res.IsError || count(out, "written") != 1 {
		t.Errorf("unpack = %v %q", out, res.Text)
	}
}

func TestUnpack_SymlinksDevicesAndEncryptedEntriesAreSkipped(t *testing.T) {
	arc := zipArchive(t,
		zipEnt{name: "link.txt", body: []byte("/etc/passwd"), mode: fs.ModeSymlink | 0o777},
		zipEnt{name: "dev.txt", body: unpackText, mode: fs.ModeDevice | 0o600},
		zipEnt{name: "locked.pdf", body: filePDF, encrypted: true},
		zipEnt{name: "open.pdf", body: filePDF},
	)
	ctx, ref, _ := unpackRun(t, inbox.Service{}, arc, "mixed.zip")
	out, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref, "all": true})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := refsOf(out, "written"); strings.Join(got, ",") != "volume:inbox:unpacked/mixed/open.pdf" {
		t.Errorf("written = %v", got)
	}
	s := skippedOf(out)
	if !strings.Contains(s["link.txt"], "symbolic link") || !strings.Contains(s["dev.txt"], "not a regular file") || !strings.Contains(s["locked.pdf"], "encrypted") {
		t.Errorf("skipped = %v", s)
	}
}

func TestUnpack_PerFileLimit(t *testing.T) {
	big := bytes.Repeat([]byte("words and more words\n"), 200) // 4200 bytes
	arc := zipArchive(t, zipEnt{name: "big.txt", body: big}, zipEnt{name: "small.txt", body: unpackText})
	ctx, ref, _ := unpackRun(t, inbox.Service{MaxFileBytes: 4000}, arc, "lim.zip")
	out, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if r := skippedOf(out)["big.txt"]; !strings.Contains(r, "per-file limit") || count(out, "written") != 1 {
		t.Errorf("answer = %v", out)
	}

	// A size the header does not give is bounded while inflating: the walk
	// stops there, keeping what it wrote.
	rar := rar5Archive(rarOpts{}, rarEnt{name: "first.txt", body: unpackText}, rarEnt{name: "big.txt", body: big, unknownSize: true}, rarEnt{name: "after.txt", body: unpackText})
	// Stored, the archive is itself over the inbox's per-file cap: it sits
	// in a volume of the run's.
	_, ctx, docs := fileRun(t, inbox.Service{MaxFileBytes: 4000}, false)
	if err := os.WriteFile(filepath.Join(docs, "lim.rar"), rar, 0o600); err != nil {
		t.Fatal(err)
	}
	out, res = unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": "volume:docs:lim.rar"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := refsOf(out, "written"); strings.Join(got, ",") != "volume:inbox:unpacked/lim/first.txt" || !strings.HasPrefix(out["stopped"].(string), "per-file limit: big.txt") {
		t.Errorf("answer = %v", out)
	}
	if _, err := os.Stat(filepath.Join(unpackInboxRoot(t, ctx), "unpacked", "lim", "big.txt")); !os.IsNotExist(err) {
		t.Errorf("the file over the cap was kept: %v", err)
	}
	// A volume has no writer of its own to hold the cap: the stream does.
	_ = os.Mkdir(filepath.Join(docs, "out"), 0o700)
	out, res = unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": "volume:docs:lim.rar", "to": "volume:docs:out/"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := refsOf(out, "written"); strings.Join(got, ",") != "volume:docs:out/first.txt" || !strings.HasPrefix(out["stopped"].(string), "per-file limit: big.txt") {
		t.Errorf("into a volume: %v", out)
	}
	if ents, _ := os.ReadDir(filepath.Join(docs, "out")); len(ents) != 1 {
		t.Errorf("the volume holds %v", ents)
	}
}

func TestUnpack_TotalLimitStopsKeepingWhatWasWritten(t *testing.T) {
	page := bytes.Repeat([]byte("x"), 1000)
	arc := zipArchive(t, zipEnt{name: "1.txt", body: page}, zipEnt{name: "2.txt", body: page}, zipEnt{name: "3.txt", body: page}, zipEnt{name: "4.txt", body: page})
	ctx, ref, _ := unpackRun(t, inbox.Service{}, arc, "pages.zip")
	lim := defaultUnpackLimits
	lim.total = 2500
	out, res := unpackExec(t, &File{unpackLimits: &lim}, ctx, map[string]any{"op": "unpack", "path": ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := refsOf(out, "written"); len(got) != 2 || !strings.Contains(out["stopped"].(string), "total limit") {
		t.Errorf("answer = %v", out)
	}
	if ents, _ := os.ReadDir(filepath.Join(unpackInboxRoot(t, ctx), "unpacked", "pages")); len(ents) != 2 {
		t.Errorf("kept %v", ents)
	}

	// The inbox's free space bounds it the same way.
	ctx, ref, _ = unpackRun(t, inbox.Service{MaxTreeBytes: 4000}, arc, "pages.zip")
	out, _ = unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref})
	if s, _ := out["stopped"].(string); !strings.HasPrefix(s, "total limit") || count(out, "written") >= 4 {
		t.Errorf("past the inbox's space: %v", out)
	}
}

func TestUnpack_ExpansionBombIsRefused(t *testing.T) {
	zeros := bytes.Repeat([]byte{0}, 4<<20)
	arc := zipArchive(t, zipEnt{name: "bomb.txt", body: zeros}, zipEnt{name: "ok.txt", body: unpackText})
	ctx, ref, _ := unpackRun(t, inbox.Service{}, arc, "bomb.zip")
	out, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	// archive/zip never yields more than an entry declares, so the
	// declared size bounds it: refused before a byte is inflated.
	if r := skippedOf(out)["bomb.txt"]; !strings.Contains(r, "expands more than 100 times") || count(out, "bytes_inflated") > 100 {
		t.Errorf("answer = %v", out)
	}

	// Where the header gives no size, the ratio is enforced on the bytes as
	// they come, and the walk stops.
	rar := rar5Archive(rarOpts{}, rarEnt{name: "grows.txt", body: bytes.Repeat([]byte("y"), 500), unknownSize: true})
	ctx, ref, _ = unpackRun(t, inbox.Service{}, rar, "grows.rar")
	lim := defaultUnpackLimits
	lim.ratio, lim.ratioFree = 0, 100
	out, res = unpackExec(t, &File{unpackLimits: &lim}, ctx, map[string]any{"op": "unpack", "path": ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if s, _ := out["stopped"].(string); !strings.Contains(s, "expansion limit") || count(out, "written") != 0 {
		t.Errorf("answer = %v", out)
	}
}

func TestUnpack_EntryAndWriteLimits(t *testing.T) {
	var ents []zipEnt
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		ents = append(ents, zipEnt{name: n + ".txt", body: unpackText})
	}
	arc := zipArchive(t, ents...)
	ctx, ref, _ := unpackRun(t, inbox.Service{}, arc, "five.zip")
	lim := defaultUnpackLimits
	lim.listed = 3
	out, _ := unpackExec(t, &File{unpackLimits: &lim}, ctx, map[string]any{"op": "unpack", "path": ref})
	if count(out, "listed") != 3 || count(out, "written") != 3 || !strings.Contains(out["stopped"].(string), "entry limit") {
		t.Errorf("entry limit: %v", out)
	}
	lim = defaultUnpackLimits
	lim.written = 2
	out, _ = unpackExec(t, &File{unpackLimits: &lim}, ctx, map[string]any{"op": "unpack", "path": ref, "dry_run": true})
	if count(out, "written") != 2 || !strings.Contains(out["stopped"].(string), "write limit") {
		t.Errorf("write limit: %v", out)
	}
}

func TestUnpack_RARIsReadInBothVersions(t *testing.T) {
	ents := []rarEnt{
		{name: "books", dir: true},
		{name: "books/a.pdf", body: filePDF},
		{name: "books/link.pdf", symlink: "a.pdf"},
		{name: "setup.exe", body: unpackEXE},
		{name: "../up.txt", body: unpackText},
	}
	for name, arc := range map[string][]byte{"v5": rar5Archive(rarOpts{}, ents...), "v4": rar4Archive(rarOpts{}, ents...)} {
		ctx, ref, _ := unpackRun(t, inbox.Service{}, arc, name+".rar")
		out, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref})
		if res.IsError {
			t.Fatalf("%s: %s", name, res.Text)
		}
		if got := refsOf(out, "written"); strings.Join(got, ",") != "volume:inbox:unpacked/"+name+"/books/a.pdf" || out["format"] != "rar" {
			t.Errorf("%s: answer = %v", name, out)
		}
		s := skippedOf(out)
		if !strings.Contains(s["books/link.pdf"], "symbolic link") || !strings.Contains(s["setup.exe"], "not a format") || !strings.Contains(s["../up.txt"], "climbs") {
			t.Errorf("%s: skipped = %v", name, s)
		}
		rec, ok := inbox.Lookup(ctx, "unpacked/"+name+"/books/a.pdf")
		if !ok || rec.Origin != "unpack:"+ref+"#books/a.pdf" {
			t.Errorf("%s: record = %+v", name, rec)
		}
	}
}

func TestUnpack_EncryptedOrMultiVolumeRARIsRefused(t *testing.T) {
	one := []rarEnt{{name: "a.pdf", body: filePDF}}
	for name, c := range map[string]struct {
		arc  []byte
		want string
	}{
		"v5 encrypted headers": {rar5Archive(rarOpts{encryptedHeaders: true}, one...), "encrypted headers"},
		"v4 encrypted headers": {rar4Archive(rarOpts{encryptedHeaders: true}, one...), "encrypted headers"},
		"v5 volume":            {rar5Archive(rarOpts{volume: true}, one...), "multi-volume"},
		"v4 volume":            {rar4Archive(rarOpts{volume: true}, one...), "multi-volume"},
		"v5 encrypted file":    {rar5Archive(rarOpts{}, rarEnt{name: "a.pdf", body: filePDF, encrypted: true}), "encrypted"},
		"v4 encrypted file":    {rar4Archive(rarOpts{}, rarEnt{name: "a.pdf", body: filePDF, encrypted: true}), "encrypted"},
	} {
		ctx, ref, _ := unpackRun(t, inbox.Service{}, c.arc, "x.rar")
		if _, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref}); !res.IsError || !strings.Contains(res.Text, c.want) {
			t.Errorf("%s: %q", name, res.Text)
		}
		if _, err := os.Stat(filepath.Join(unpackInboxRoot(t, ctx), "unpacked")); !os.IsNotExist(err) {
			t.Errorf("%s: wrote before refusing", name)
		}
	}
}

// A solid RAR's skipped entry is decompressed all the same, and charged; a
// plain one's costs only the head read to sniff it.
func TestUnpack_SolidRARChargesSkippedEntries(t *testing.T) {
	blob := append([]byte("MZ"), bytes.Repeat([]byte{1, 2, 3, 4}, 5000)...) // 20002 bytes
	ents := []rarEnt{{name: "tool.exe", body: blob}, {name: "a.txt", body: unpackText}}
	inflated := map[bool]int{}
	for _, solid := range []bool{false, true} {
		ctx, ref, _ := unpackRun(t, inbox.Service{}, rar5Archive(rarOpts{solid: solid}, ents...), "s.rar")
		out, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref})
		if res.IsError || count(out, "written") != 1 {
			t.Fatalf("solid %v: %v %q", solid, out, res.Text)
		}
		inflated[solid] = count(out, "bytes_inflated")
	}
	if inflated[false] != unpackHead+len(unpackText) || inflated[true] != len(blob)+len(unpackText) {
		t.Errorf("inflated = %v", inflated)
	}
	// And the budget holds over it: the skipped entry alone spends it.
	ctx, ref, _ := unpackRun(t, inbox.Service{}, rar5Archive(rarOpts{solid: true}, ents...), "s.rar")
	lim := defaultUnpackLimits
	lim.total = 10000
	out, _ := unpackExec(t, &File{unpackLimits: &lim}, ctx, map[string]any{"op": "unpack", "path": ref})
	if s, _ := out["stopped"].(string); !strings.Contains(s, "total limit") || count(out, "written") != 0 {
		t.Errorf("solid over budget: %v", out)
	}
}

func TestUnpack_OnlyZIPAndRARByContent(t *testing.T) {
	for name, c := range map[string]struct {
		data []byte
		want string
	}{
		"a pdf named .zip": {filePDF, "not a ZIP or RAR"},
		"7z":               {[]byte("7z\xbc\xaf\x27\x1c\x00\x04rest"), "7z"},
		"gzip":             {[]byte("\x1f\x8b\x08\x00rest of it"), "gzip"},
	} {
		ctx, ref, _ := unpackRun(t, inbox.Service{}, c.data, "archive.zip")
		if _, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref}); !res.IsError || !strings.Contains(res.Text, c.want) {
			t.Errorf("%s: %q", name, res.Text)
		}
	}
	// A zip with a name that says otherwise is still a zip.
	ctx, ref, _ := unpackRun(t, inbox.Service{}, zipArchive(t, zipEnt{name: "a.pdf", body: filePDF}), "blob.dat")
	if out, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref}); res.IsError || count(out, "written") != 1 {
		t.Errorf("zip named .dat: %v %q", out, res.Text)
	}
}

// noWorker is a conversion pool any call to which fails the test: unpack
// runs in this process only.
type noWorker struct{ convert.Client }

func TestUnpack_RunsInProcessUnderTheExtractionSlots(t *testing.T) {
	arc := zipArchive(t, zipEnt{name: "a.pdf", body: filePDF})
	ctx, ref, _ := unpackRun(t, inbox.Service{}, arc, "a.zip")
	lim := extract.NewLimiter(1)
	f := &File{Extractions: lim, Convert: noWorker{}, slotWait: 50 * time.Millisecond}
	release, err := lim.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, res := unpackExec(t, f, ctx, map[string]any{"op": "unpack", "path": ref}); !res.IsError || !strings.Contains(res.Text, "extracting as many files") {
		t.Errorf("with every slot taken: %q", res.Text)
	}
	release()
	if out, res := unpackExec(t, f, ctx, map[string]any{"op": "unpack", "path": ref}); res.IsError || count(out, "written") != 1 {
		t.Errorf("with a slot: %v %q", out, res.Text)
	}
}

func TestUnpack_CancelledCallReturnsItsError(t *testing.T) {
	arc := zipArchive(t, zipEnt{name: "a.pdf", body: filePDF})
	ctx, ref, _ := unpackRun(t, inbox.Service{}, arc, "a.zip")
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	raw, _ := json.Marshal(map[string]any{"op": "unpack", "path": ref})
	if _, err := (&File{}).Execute(cctx, raw); err == nil {
		t.Error("a cancelled unpack returned no error")
	}
}

func TestUnpack_ZipDirectoryTooLargeIsRefusedBeforeLoading(t *testing.T) {
	arc := zipArchive(t, zipEnt{name: "a.txt", body: unpackText})
	// Claim more records than the walk will load, in the end record.
	i := bytes.LastIndex(arc, []byte("PK\x05\x06"))
	arc[i+10], arc[i+11] = 0xfe, 0xff // 65534
	if n, ok := zipDirectoryRecords(bytes.NewReader(arc), int64(len(arc))); !ok || n != 65534 {
		t.Fatalf("records = %d %v", n, ok)
	}
	u := &unpacker{ctx: context.Background(), lim: defaultUnpackLimits}
	if err := u.walkZip(bytes.NewReader(zip64Claim(t, arc)), int64(len(zip64Claim(t, arc)))); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("walk = %v", err)
	}
}

// zip64Claim appends a zip64 end record and locator claiming 200,000
// records before arc's end record, whose count is set to 0xFFFF.
func zip64Claim(t *testing.T, arc []byte) []byte {
	t.Helper()
	i := bytes.LastIndex(arc, []byte("PK\x05\x06"))
	out := append([]byte(nil), arc[:i]...)
	off := len(out)
	rec := append([]byte("PK\x06\x06"), le64(44)...)
	rec = append(rec, 45, 0, 45, 0)
	rec = append(rec, make([]byte, 8)...) // disk numbers
	rec = append(rec, le64(200000)...)
	rec = append(rec, le64(200000)...)
	rec = append(rec, make([]byte, 16)...) // directory size and offset
	out = append(out, rec...)
	loc := append([]byte("PK\x06\x07"), make([]byte, 4)...)
	loc = append(loc, le64(uint64(off))...)
	loc = append(loc, 1, 0, 0, 0)
	out = append(out, loc...)
	end := append([]byte(nil), arc[i:]...)
	end[10], end[11] = 0xff, 0xff
	return append(out, end...)
}

func le64(v uint64) []byte {
	b := make([]byte, 8)
	for i := range b {
		b[i] = byte(v >> (8 * i))
	}
	return b
}

// deflated is raw deflate of b, for a zip entry written with CreateRaw.
func deflated(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestCompression)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

// A header that understates an entry's size does not let it past: archive/zip
// stops at the declared size, and the entry is reported as damaged.
func TestUnpack_UnderstatedZipEntryStops(t *testing.T) {
	body := bytes.Repeat([]byte("z"), 50000)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{Name: "liar.txt", Method: zip.Deflate, CRC32: crc32.ChecksumIEEE(body),
		CompressedSize64: uint64(len(deflated(t, body))), UncompressedSize64: 100})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(deflated(t, body))
	_ = zw.Close()
	ctx, ref, _ := unpackRun(t, inbox.Service{}, buf.Bytes(), "liar.zip")
	out, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if count(out, "written") != 0 || out["stopped"] == nil || count(out, "bytes_inflated") > 100 {
		t.Errorf("answer = %v", out)
	}
}

// An archive damaged past its first files keeps what was written before the
// damage, and says where it stopped.
func TestUnpack_DamageAfterWritesKeepsThem(t *testing.T) {
	first := rar5Archive(rarOpts{}, rarEnt{name: "a.txt", body: unpackText})
	both := rar5Archive(rarOpts{}, rarEnt{name: "a.txt", body: unpackText}, rarEnt{name: "b.txt", body: unpackText})
	// The second file header starts where the one-file archive's end
	// header did: break its CRC.
	end := len(first) - len(rar5Block(5, 0, rarVintBytes(0), nil, -1))
	both[end] ^= 0xff
	ctx, ref, _ := unpackRun(t, inbox.Service{}, both, "broken.rar")
	out, res := unpackExec(t, nil, ctx, map[string]any{"op": "unpack", "path": ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := refsOf(out, "written"); strings.Join(got, ",") != "volume:inbox:unpacked/broken/a.txt" || !strings.Contains(out["stopped"].(string), "could not be read") {
		t.Errorf("answer = %v", out)
	}
}
