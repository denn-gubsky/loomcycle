package builtin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/unpack"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// unpackLabels are a conversion child's labels: no helper program, and
// the unpack job.
var unpackLabels = append(append([]string(nil), convLabels...), wire.LabelUnpack)

// unpackConverter is the worker's conversion child in a test: it unpacks
// in this process, counts the unpacks it was asked for, and crashes (as the
// child's death reads to the worker) on an archive holding crashMark.
type unpackConverter struct {
	convert.Client
	unpacks atomic.Int32
}

func (c *unpackConverter) Unpack(ctx context.Context, r io.ReaderAt, size int64, opt unpack.Options, out io.Writer) error {
	c.unpacks.Add(1)
	b, _ := io.ReadAll(io.NewSectionReader(r, 0, size))
	if bytes.Contains(b, []byte(crashMark)) {
		return convert.ErrQuarantined
	}
	return c.Client.Unpack(ctx, r, size, opt, out)
}

func unpackFixture(t *testing.T, svc inbox.Service, l lifecycle.Limits, capacity int) (*fcFixture, *unpackConverter) {
	t.Helper()
	cv := &unpackConverter{Client: &convert.InProcess{}}
	return newFCFixtureWith(t, svc, l, worker.Options{Capacity: capacity, Convert: cv, Labels: unpackLabels}), cv
}

// archiveBooks is a ZIP of two documents in a folder, and its file names.
func archiveBooks(t *testing.T) []byte {
	return zipArchive(t, zipEnt{name: "books/a.txt", body: unpackText}, zipEnt{name: "books/b.txt", body: []byte("Another chapter.\n")})
}

// An archive in a workspace, ZIP or RAR, is unpacked into a folder of the
// same workspace on its worker: the files land there, and none of the
// archive's bytes cross to the core (no get ran, and what came back is far
// smaller than the archive).
func TestFileUnpack_AWorkspaceArchiveIsUnpackedIntoItsWorkspaceWithNoByteCrossing(t *testing.T) {
	f, cv := unpackFixture(t, inbox.Service{}, lifecycle.Limits{}, 10)
	f.open(t, "w")
	big := bytes.Repeat([]byte("a long chapter of plain text\n"), 4000)
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"books.zip", zipArchive(t, zipEnt{name: "books/a.txt", body: big}, zipEnt{name: "books/b.txt", body: unpackText})},
		{"books.rar", rar5Archive(rarOpts{}, rarEnt{name: "books", dir: true}, rarEnt{name: "books/a.txt", body: big}, rarEnt{name: "books/b.txt", body: unpackText})},
	} {
		f.wsPutBytes(t, "w", "in/"+c.name, c.data)
		before := f.crossed()
		out, res := f.exec(t, f.ctx, map[string]any{"op": "unpack", "path": "workspace:w:in/" + c.name, "to": "workspace:w:out/" + c.name})
		if res.IsError {
			t.Fatalf("%s: %s", c.name, res.Text)
		}
		total, byOp := crossedSince(before, f.crossed())
		if byOp["get"] != 0 || total >= uint64(len(big)) {
			t.Errorf("%s: %d bytes crossed (%v) for an archive of %d inflated bytes", c.name, total, byOp, len(big))
		}
		want := []string{"workspace:w:out/" + c.name + "/books/a.txt", "workspace:w:out/" + c.name + "/books/b.txt"}
		if got := refsOf(out, "written"); strings.Join(got, ",") != strings.Join(want, ",") || out["to"] != "workspace:w:out/"+c.name+"/" {
			t.Errorf("%s: answer %v", c.name, out)
		}
		if a, ok := f.wsGet(t, "w", "out/"+c.name+"/books/a.txt"); !ok || a != string(big) {
			t.Errorf("%s: a.txt in the workspace is %d bytes", c.name, len(a))
		}
	}
	if cv.unpacks.Load() != 2 {
		t.Errorf("unpacks on the worker = %d", cv.unpacks.Load())
	}
}

// The same archive is unpacked into a volume and into the inbox as an inbox
// archive is: the answer is the one an inbox archive's unpack gives, but
// for the archive's reference.
func TestFileUnpack_AWorkspaceArchiveIsUnpackedIntoAVolumeAndTheInbox(t *testing.T) {
	f, _ := unpackFixture(t, inbox.Service{}, lifecycle.Limits{}, 10)
	f.open(t, "w")
	arc := archiveBooks(t)
	f.wsPutBytes(t, "w", "books.zip", arc)
	if err := os.MkdirAll(filepath.Join(f.docs, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, res := f.exec(t, f.ctx, map[string]any{"op": "unpack", "path": "workspace:w:books.zip", "to": "volume:docs:out/"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := refsOf(out, "written"); strings.Join(got, ",") != "volume:docs:out/books/a.txt,volume:docs:out/books/b.txt" {
		t.Errorf("into a volume: %v", out)
	}
	if b := readFile(t, filepath.Join(f.docs, "out", "books", "a.txt")); b != string(unpackText) {
		t.Errorf("the volume holds %q", b)
	}

	fromWS, res := f.exec(t, f.ctx, map[string]any{"op": "unpack", "path": "workspace:w:books.zip"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := refsOf(fromWS, "written"); strings.Join(got, ",") != "volume:inbox:unpacked/books/books/a.txt,volume:inbox:unpacked/books/books/b.txt" {
		t.Errorf("into the inbox: %v", fromWS)
	}
	// The inbox's own copy of the archive, unpacked as before, answers the
	// same but for the source and where it went.
	kept := inbox.Capture(f.ctx, inbox.Item{Name: "books.zip", Dir: "documents"}, arc)
	fromInbox, res := f.exec(t, f.ctx, map[string]any{"op": "unpack", "path": kept.Record.Ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if count(fromWS, "written") != count(fromInbox, "written") || count(fromWS, "listed") != count(fromInbox, "listed") ||
		count(fromWS, "bytes_written") != count(fromInbox, "bytes_written") || fromWS["format"] != "zip" {
		t.Errorf("from a workspace %v, from the inbox %v", fromWS, fromInbox)
	}
}

// A hostile archive's entries are skipped exactly as an inbox archive's
// are: a climbing name, an absolute one and a link. Nothing lands outside
// the target folder.
func TestFileUnpack_AHostileWorkspaceArchiveSkipsAsAnInboxArchiveDoes(t *testing.T) {
	f, _ := unpackFixture(t, inbox.Service{}, lifecycle.Limits{}, 10)
	f.open(t, "w")
	arc := zipArchive(t,
		zipEnt{name: "../evil.txt", body: unpackText},
		zipEnt{name: "/abs.txt", body: unpackText},
		zipEnt{name: "link.txt", body: []byte("/etc/passwd"), mode: fs.ModeSymlink | 0o777},
		zipEnt{name: "good.txt", body: unpackText},
	)
	f.wsPutBytes(t, "w", "evil.zip", arc)
	out, res := f.exec(t, f.ctx, map[string]any{"op": "unpack", "path": "workspace:w:evil.zip", "to": "workspace:w:out"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	kept := inbox.Capture(f.ctx, inbox.Item{Name: "evil.zip", Dir: "documents"}, arc)
	inboxOut, res := f.exec(t, f.ctx, map[string]any{"op": "unpack", "path": kept.Record.Ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	got, want := skippedOf(out), skippedOf(inboxOut)
	if len(got) != 3 || len(got) != len(want) {
		t.Fatalf("skipped %v, an inbox archive's %v", got, want)
	}
	for name, reason := range want {
		if got[name] != reason {
			t.Errorf("%s: skipped %q, an inbox archive's %q", name, got[name], reason)
		}
	}
	if got := refsOf(out, "written"); strings.Join(got, ",") != "workspace:w:out/good.txt" {
		t.Errorf("written %v", got)
	}
	ls, res := f.exec(t, f.ctx, map[string]any{"op": "ls", "path": "workspace:w:", "recursive": true})
	if res.IsError {
		t.Fatal(res.Text)
	}
	for _, e := range ls["entries"].([]any) {
		if p := strings.TrimPrefix(e.(map[string]any)["ref"].(string), "workspace:w:"); p != "evil.zip" && p != "out" && p != "out/good.txt" {
			t.Errorf("the workspace holds %s", p)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.wsDir(t, "w")), "evil.txt")); !os.IsNotExist(err) {
		t.Errorf("a file climbed out of the workspace (%v)", err)
	}
}

// Into a workspace an archive goes whole or not at all: past the file
// count or the byte total the unpack is refused, naming the bound, and
// nothing is written, not even a stage.
func TestFileUnpack_IntoAWorkspaceOverABoundWritesNothing(t *testing.T) {
	f, _ := unpackFixture(t, inbox.Service{}, lifecycle.Limits{}, 10)
	f.open(t, "w")
	page := bytes.Repeat([]byte("x"), 1000)
	f.wsPutBytes(t, "w", "pages.zip", zipArchive(t, zipEnt{name: "1.txt", body: page}, zipEnt{name: "2.txt", body: page}, zipEnt{name: "3.txt", body: page}))
	for _, c := range []struct {
		set  func(*unpack.Limits)
		want string
	}{
		{func(l *unpack.Limits) { l.Written = 2 }, "write limit"},
		{func(l *unpack.Limits) { l.Total = 2500 }, "total limit"},
	} {
		lim := unpack.DefaultLimits
		c.set(&lim)
		f.file.unpackLimits = &lim
		_, res := f.exec(t, f.ctx, map[string]any{"op": "unpack", "path": "workspace:w:pages.zip", "to": "workspace:w:out"})
		wantFileRefusal(t, c.want, res, c.want)
		if !strings.Contains(res.Text, "nothing was written") {
			t.Errorf("%s: %s", c.want, res.Text)
		}
		if f.wsHas(t, "w", "out") || dirHasEntries(filepath.Join(f.wsDir(t, "w"), ".loomcycle-work", "stage")) {
			t.Errorf("%s: the refused unpack left files", c.want)
		}
	}
}

// Over the workspace's disk quota an unpack fails and leaves nothing.
func TestFileUnpack_IntoAWorkspaceOverItsQuotaLeavesNothing(t *testing.T) {
	f, _ := unpackFixture(t, inbox.Service{}, lifecycle.Limits{DiskQuotaBytes: 3000}, 10)
	f.open(t, "w")
	page := bytes.Repeat([]byte("q"), 1000)
	arc := zipArchive(t, zipEnt{name: "1.txt", body: page}, zipEnt{name: "2.txt", body: page}, zipEnt{name: "3.txt", body: page})
	f.wsPutBytes(t, "w", "pages.zip", arc)
	_, res := f.exec(t, f.ctx, map[string]any{"op": "unpack", "path": "workspace:w:pages.zip", "to": "workspace:w:out"})
	wantFileRefusal(t, "over the quota", res, workspace.ClassOverQuota)
	// Refused at the file that would pass the quota, before its bytes are
	// written, not only when the stage is committed.
	if !strings.Contains(res.Text, "this write needs") {
		t.Errorf("refused late: %s", res.Text)
	}
	if f.wsHas(t, "w", "out") || dirHasEntries(filepath.Join(f.wsDir(t, "w"), ".loomcycle-work", "stage")) {
		t.Error("the refused unpack left files")
	}
	// Freed of the stage, the workspace still takes a file.
	f.wsPut(t, "w", "after.txt", "x")
}

// A Workspace hook that refuses one file's final path refuses the whole
// unpack, before anything is committed.
func TestFileUnpack_AHookRefusingOneFileRefusesTheWholeUnpack(t *testing.T) {
	f, _ := unpackFixture(t, inbox.Service{}, lifecycle.Limits{}, 10)
	f.open(t, "w")
	f.wsPutBytes(t, "w", "books.zip", archiveBooks(t))
	var asked []string
	ctx := tools.WithHookPreCheck(f.ctx, func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
		var in struct{ Op, Path string }
		_ = json.Unmarshal(input, &in)
		if name == "Workspace" && in.Op == "write" {
			asked = append(asked, in.Path)
			if in.Path == "out/books/b.txt" {
				return tools.Result{Text: "not this one", IsError: true}, false
			}
		}
		return tools.Result{}, true
	})
	_, res := f.exec(t, ctx, map[string]any{"op": "unpack", "path": "workspace:w:books.zip", "to": "workspace:w:out"})
	wantFileRefusal(t, "hook", res, "not this one")
	if !containsCall(asked, "out") || !containsCall(asked, "out/books/a.txt") {
		t.Errorf("the hook was asked %v", asked)
	}
	if f.wsHas(t, "w", "out") || dirHasEntries(filepath.Join(f.wsDir(t, "w"), ".loomcycle-work", "stage")) {
		t.Error("a refused unpack left files")
	}
}

// The grant and the source's hook refuse before the worker is asked.
func TestFileUnpack_GrantsRefuseBeforeAnyUnpack(t *testing.T) {
	f, cv := unpackFixture(t, inbox.Service{}, lifecycle.Limits{}, 10)
	f.open(t, "w")
	f.wsPutBytes(t, "w", "books.zip", archiveBooks(t))
	none := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace = tools.WorkspacePolicyValue{} })
	readOnly := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace = tools.WorkspacePolicyValue{Read: true} })
	_, res := f.exec(t, none, map[string]any{"op": "unpack", "path": "workspace:w:books.zip"})
	wantFileRefusal(t, "no grant", res, "does not allow read")
	_, res = f.exec(t, readOnly, map[string]any{"op": "unpack", "path": "workspace:w:books.zip", "to": "workspace:w:out"})
	wantFileRefusal(t, "read only", res, "does not allow write")
	if n := cv.unpacks.Load(); n != 0 || f.wsHas(t, "w", "out") {
		t.Errorf("a refused call unpacked (%d) or wrote", n)
	}
}

// An archive that crashes the conversion child fails the call and is then
// refused in that workspace without reaching the worker; the same archive
// in another workspace is still tried.
func TestFileUnpack_AnArchiveThatCrashesTheChildIsQuarantinedForItsWorkspace(t *testing.T) {
	f, cv := unpackFixture(t, inbox.Service{}, lifecycle.Limits{}, 10)
	f.open(t, "w")
	f.open(t, "v")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("a.txt")
	_, _ = w.Write(unpackText)
	_ = zw.SetComment(crashMark)
	_ = zw.Close()
	f.wsPutBytes(t, "w", "bad.zip", buf.Bytes())
	f.wsPutBytes(t, "v", "bad.zip", buf.Bytes())
	_, res := f.exec(t, f.ctx, map[string]any{"op": "unpack", "path": "workspace:w:bad.zip", "to": "workspace:w:out"})
	wantFileRefusal(t, "first", res, "crashed")
	_, res = f.exec(t, f.ctx, map[string]any{"op": "unpack", "path": "workspace:w:bad.zip"})
	wantFileRefusal(t, "again in w", res, "will not be tried again")
	if cv.unpacks.Load() != 1 {
		t.Errorf("the quarantined archive reached the worker again (%d unpacks)", cv.unpacks.Load())
	}
	_, res = f.exec(t, f.ctx, map[string]any{"op": "unpack", "path": "workspace:v:bad.zip"})
	wantFileRefusal(t, "in v", res, "crashed")
	if cv.unpacks.Load() != 2 {
		t.Error("the archive was refused in another workspace too")
	}
}

// Into a workspace on another worker the files stream through the core
// into a stage there, committed whole.
func TestFileUnpack_IntoAWorkspaceOnAnotherWorkerStreamsTheFiles(t *testing.T) {
	f, _ := unpackFixture(t, inbox.Service{}, lifecycle.Limits{}, 1)
	f.ws.h.AddWorker("w2", 1)
	f.open(t, "a")
	f.open(t, "b")
	ra, _ := f.ws.tool.Manager.Get(context.Background(), f.owner(), "a")
	rb, _ := f.ws.tool.Manager.Get(context.Background(), f.owner(), "b")
	if ra.WorkerID == rb.WorkerID {
		t.Fatalf("both workspaces on %s", ra.WorkerID)
	}
	f.wsPutBytes(t, "a", "books.zip", archiveBooks(t))
	out, res := f.exec(t, f.ctx, map[string]any{"op": "unpack", "path": "workspace:a:books.zip", "to": "workspace:b:out"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := refsOf(out, "written"); strings.Join(got, ",") != "workspace:b:out/books/a.txt,workspace:b:out/books/b.txt" {
		t.Errorf("answer %v", out)
	}
	if b, ok := f.wsGet(t, "b", "out/books/b.txt"); !ok || b != "Another chapter.\n" {
		t.Errorf("b holds %q", b)
	}
}

// A dry run into a workspace lists the files it would write there and
// writes nothing.
func TestFileUnpack_ADryRunIntoAWorkspaceWritesNothing(t *testing.T) {
	f, _ := unpackFixture(t, inbox.Service{}, lifecycle.Limits{}, 10)
	f.open(t, "w")
	f.wsPutBytes(t, "w", "books.zip", archiveBooks(t))
	out, res := f.exec(t, f.ctx, map[string]any{"op": "unpack", "path": "workspace:w:books.zip", "to": "workspace:w:out", "dry_run": true})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := refsOf(out, "would_write"); strings.Join(got, ",") != "workspace:w:out/books/a.txt,workspace:w:out/books/b.txt" || f.wsHas(t, "w", "out") {
		t.Errorf("answer %v", out)
	}
}

// A file in a workspace that is not an archive is refused as an inbox
// file's would be, with the walk's own reason.
func TestFileUnpack_AWorkspaceFileThatIsNotAnArchiveIsRefusedWithItsReason(t *testing.T) {
	f, _ := unpackFixture(t, inbox.Service{}, lifecycle.Limits{}, 10)
	f.open(t, "w")
	f.wsPutBytes(t, "w", "a.gz", []byte("\x1f\x8b\x08\x00rest of it"))
	_, res := f.exec(t, f.ctx, map[string]any{"op": "unpack", "path": "workspace:w:a.gz", "to": "workspace:w:out"})
	wantFileRefusal(t, "gzip", res, "unpack: workspace:w:a.gz is not a ZIP or RAR archive (a gzip file)")
	if f.wsHas(t, "w", "out") {
		t.Error("a refused unpack wrote")
	}
}
