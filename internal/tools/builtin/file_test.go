package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox/inboxtest"
)

var filePDF = append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("p"), 1000)...)

// fileRun is a run tree with an inbox, a read-write volume "docs" and a
// read-only volume "ref", the inbox writable to the run unless readOnly.
func fileRun(t *testing.T, svc inbox.Service, readOnly bool) (*inboxtest.Run, context.Context, string) {
	t.Helper()
	r := inboxtest.New(t, "root-1", svc)
	docs, ref := t.TempDir(), t.TempDir()
	ctx := tools.UpdateRunPolicy(r.Ctx, func(p *tools.RunPolicy) {
		p.Tools = []string{"File", "Read", "Write"}
		p.EphemeralWritable = !readOnly
		p.Volumes = tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{
			{Name: "docs", Root: docs, Default: true},
			{Name: "ref", Root: ref, ReadOnly: true},
		}}
	})
	return r, ctx, docs
}

func fileExec(t *testing.T, ctx context.Context, in map[string]any) (map[string]any, tools.Result) {
	t.Helper()
	raw, _ := json.Marshal(in)
	res, err := (&File{}).Execute(ctx, raw)
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

func TestFile_StatReturnsTheCaptureRecord(t *testing.T) {
	r, ctx, _ := fileRun(t, inbox.Service{}, false)
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "board.pdf", Origin: "mcp:ms365/get"}, filePDF)
	out, res := fileExec(t, ctx, map[string]any{"op": "stat", "path": kept.Record.Ref})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if out["sha256"] != kept.Record.SHA256 || out["origin"] != "mcp:ms365/get" || out["media_type"] != "application/pdf" {
		t.Errorf("stat = %v", out)
	}
	// A bare path names the inbox too.
	if out, _ := fileExec(t, ctx, map[string]any{"op": "stat", "path": "documents/board.pdf"}); out["sha256"] != kept.Record.SHA256 {
		t.Errorf("bare-path stat = %v", out)
	}
}

func TestFile_StatWithoutARecordReadsTheBytes(t *testing.T) {
	r, ctx, _ := fileRun(t, inbox.Service{}, false)
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, filePDF)
	ref, _ := r.Set.Get(inbox.Name)
	if err := os.WriteFile(filepath.Join(ref.Root, "documents", "own.pdf"), filePDF, 0o600); err != nil {
		t.Fatal(err)
	}
	out, res := fileExec(t, ctx, map[string]any{"op": "stat", "path": "volume:inbox:documents/own.pdf"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if out["sha256"] != kept.Record.SHA256 || out["origin"] != "unknown" || out["media_type"] != "application/pdf" {
		t.Errorf("stat = %v", out)
	}
}

func TestFile_LsBeforeAnyCaptureIsEmpty(t *testing.T) {
	_, ctx, _ := fileRun(t, inbox.Service{}, false)
	out, res := fileExec(t, ctx, map[string]any{"op": "ls"})
	if res.IsError || len(out["entries"].([]any)) != 0 || out["note"] == nil {
		t.Errorf("ls = %v %q", out, res.Text)
	}
}

func TestFile_LsListsTheInboxAndHidesPartialFiles(t *testing.T) {
	r, ctx, _ := fileRun(t, inbox.Service{}, false)
	inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "mcp:s/t"}, filePDF)
	inbox.Capture(r.Ctx, inbox.Item{Name: "b.png", Origin: "mcp:s/t"}, []byte("\x89PNG\r\n\x1a\nxx"))
	ref, _ := r.Set.Get(inbox.Name)
	_ = os.WriteFile(filepath.Join(ref.Root, "documents", ".part-123"), []byte("half"), 0o600)

	out, _ := fileExec(t, ctx, map[string]any{"op": "ls", "recursive": true})
	var refs []string
	for _, e := range out["entries"].([]any) {
		m := e.(map[string]any)
		refs = append(refs, m["ref"].(string))
		if m["ref"] == "volume:inbox:documents/a.pdf" && (m["media_type"] != "application/pdf" || m["origin"] != "mcp:s/t") {
			t.Errorf("entry = %v", m)
		}
	}
	if strings.Join(refs, ",") != "volume:inbox:documents/a.pdf,volume:inbox:images/b.png" {
		t.Errorf("refs = %v", refs)
	}
}

func TestFile_KeepCopiesIntoAVolumeFolder(t *testing.T) {
	r, ctx, docs := fileRun(t, inbox.Service{}, false)
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "board.pdf", Origin: "o"}, filePDF)
	if err := os.Mkdir(filepath.Join(docs, "board"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"volume:docs:board/", "volume:docs:board"} {
		out, res := fileExec(t, ctx, map[string]any{"op": "keep", "from": kept.Record.Ref, "to": to, "overwrite": true})
		if res.IsError {
			t.Fatalf("keep to %s: %s", to, res.Text)
		}
		if out["ref"] != "volume:docs:board/board.pdf" {
			t.Errorf("keep to %s: ref = %v", to, out["ref"])
		}
	}
	got, err := os.ReadFile(filepath.Join(docs, "board", "board.pdf"))
	if err != nil || !bytes.Equal(got, filePDF) {
		t.Errorf("kept copy: %v", err)
	}
}

func TestFile_KeepRefusesToReplaceUnlessAsked(t *testing.T) {
	r, ctx, docs := fileRun(t, inbox.Service{}, false)
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, filePDF)
	_ = os.WriteFile(filepath.Join(docs, "a.pdf"), []byte("mine"), 0o600)
	if _, res := fileExec(t, ctx, map[string]any{"op": "keep", "from": kept.Record.Ref, "to": "volume:docs:a.pdf"}); !res.IsError || !strings.Contains(res.Text, "already exists") {
		t.Errorf("res = %q", res.Text)
	}
	if b, _ := os.ReadFile(filepath.Join(docs, "a.pdf")); string(b) != "mine" {
		t.Error("the existing file was replaced")
	}
	if _, res := fileExec(t, ctx, map[string]any{"op": "keep", "from": kept.Record.Ref, "to": "volume:docs:a.pdf", "overwrite": true}); res.IsError {
		t.Errorf("overwrite: %q", res.Text)
	}
}

func TestFile_KeepRefusals(t *testing.T) {
	r, ctx, _ := fileRun(t, inbox.Service{}, false)
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, filePDF)
	for name, in := range map[string]map[string]any{
		"read-only target":    {"from": kept.Record.Ref, "to": "volume:ref:a.pdf"},
		"source not in inbox": {"from": "volume:docs:x.pdf", "to": "volume:docs:y.pdf"},
		"target in inbox":     {"from": kept.Record.Ref, "to": "volume:inbox:keep.pdf"},
		"missing folder":      {"from": kept.Record.Ref, "to": "volume:docs:no/such/a.pdf"},
		"escape":              {"from": kept.Record.Ref, "to": "volume:docs:../../a.pdf"},
		"unbound volume":      {"from": kept.Record.Ref, "to": "volume:other:a.pdf"},
	} {
		in["op"] = "keep"
		if _, res := fileExec(t, ctx, in); !res.IsError {
			t.Errorf("%s: keep succeeded: %q", name, res.Text)
		}
	}
}

func TestFile_RmRemovesAndGivesTheAllowanceBack(t *testing.T) {
	r, ctx, _ := fileRun(t, inbox.Service{MaxTreeBytes: 1500}, false)
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, filePDF)
	if again := inbox.Capture(r.Ctx, inbox.Item{Name: "b.pdf", Origin: "o"}, filePDF); again.Kept {
		t.Fatal("the cap did not hold")
	}
	if _, res := fileExec(t, ctx, map[string]any{"op": "rm", "path": kept.Record.Ref}); res.IsError {
		t.Fatal(res.Text)
	}
	if again := inbox.Capture(r.Ctx, inbox.Item{Name: "b.pdf", Origin: "o"}, filePDF); !again.Kept {
		t.Errorf("after rm: %q", again.Stub)
	}
	if _, res := fileExec(t, ctx, map[string]any{"op": "rm", "path": "volume:docs:a.pdf"}); !res.IsError {
		t.Error("rm outside the inbox succeeded")
	}
}

// D10: a run given the tree's ephemeral volumes read-only can look at the
// inbox but not change it.
func TestFile_ReadOnlyRunCannotChangeTheInbox(t *testing.T) {
	r, ctx, _ := fileRun(t, inbox.Service{}, true)
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, filePDF)
	if _, res := fileExec(t, ctx, map[string]any{"op": "stat", "path": kept.Record.Ref}); res.IsError {
		t.Errorf("stat: %q", res.Text)
	}
	for _, in := range []map[string]any{{"op": "rm", "path": kept.Record.Ref}, {"op": "clear"}} {
		if _, res := fileExec(t, ctx, in); !res.IsError || !strings.Contains(res.Text, "read-only") {
			t.Errorf("%v: %q", in, res.Text)
		}
	}
}

func TestFile_ClearEmptiesThePrefixOrTheInbox(t *testing.T) {
	r, ctx, _ := fileRun(t, inbox.Service{}, false)
	inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, filePDF)
	inbox.Capture(r.Ctx, inbox.Item{Name: "b.png", Origin: "o"}, []byte("\x89PNG\r\n\x1a\nxx"))
	out, res := fileExec(t, ctx, map[string]any{"op": "clear", "prefix": "images"})
	if res.IsError || out["files"] != float64(1) {
		t.Fatalf("clear images = %v %q", out, res.Text)
	}
	out, _ = fileExec(t, ctx, map[string]any{"op": "clear"})
	if out["files"] != float64(1) || out["bytes"] != float64(len(filePDF)) {
		t.Errorf("clear = %v", out)
	}
	ref, _ := r.Set.Get(inbox.Name)
	if ents, _ := os.ReadDir(ref.Root); len(ents) != 0 {
		t.Errorf("left: %v", ents)
	}
	if _, err := os.Stat(ref.Root); err != nil {
		t.Errorf("clear removed the inbox itself: %v", err)
	}
}

func TestFile_PathsStayInsideTheirVolume(t *testing.T) {
	r, ctx, _ := fileRun(t, inbox.Service{}, false)
	inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, filePDF)
	for _, p := range []string{"volume:inbox:../../etc/passwd", "../../x", "volume:inbox:documents/../../../x"} {
		if _, res := fileExec(t, ctx, map[string]any{"op": "stat", "path": p}); !res.IsError {
			t.Errorf("stat %s succeeded", p)
		}
	}
}

// The reference in a stub is the one Read takes: the volume is the inbox,
// and the path is relative to its root.
func TestFile_StubReferenceOpensWithRead(t *testing.T) {
	r, ctx, _ := fileRun(t, inbox.Service{}, false)
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "minutes.md", MediaType: "text/markdown", Origin: "o"}, []byte("# Minutes\nAgreed."))
	ref, err := parseDataRef(kept.Record.Ref)
	if err != nil || ref.volume != inbox.Name {
		t.Fatalf("parse %q: %+v %v", kept.Record.Ref, ref, err)
	}
	raw, _ := json.Marshal(map[string]any{"volume": ref.volume, "path": ref.path})
	res, err := (&Read{}).Execute(ctx, raw)
	if err != nil || res.IsError || !strings.Contains(res.Text, "Agreed.") {
		t.Errorf("Read %s: %q %v", raw, res.Text, err)
	}
}

// Writing into a volume, or reading one that is not the inbox, needs the run
// to hold Write or Read itself, as Crypto's references do: a run narrowed to
// [File] reaches only the inbox.
func TestFile_OtherVolumesNeedTheRunsOwnReadAndWrite(t *testing.T) {
	r, ctx, docs := fileRun(t, inbox.Service{}, false)
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, filePDF)
	_ = os.WriteFile(filepath.Join(docs, "b.pdf"), filePDF, 0o600)
	narrowed := tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) { p.Tools = []string{"File"} })

	if _, res := fileExec(t, narrowed, map[string]any{"op": "keep", "from": kept.Record.Ref, "to": "volume:docs:a.pdf"}); !res.IsError || !strings.Contains(res.Text, "needs the Write tool") {
		t.Errorf("keep without Write: %q", res.Text)
	}
	if _, err := os.Stat(filepath.Join(docs, "a.pdf")); !os.IsNotExist(err) {
		t.Error("keep wrote without Write")
	}
	for _, in := range []map[string]any{{"op": "stat", "path": "volume:docs:b.pdf"}, {"op": "ls", "path": "volume:docs:."}} {
		if _, res := fileExec(t, narrowed, in); !res.IsError || !strings.Contains(res.Text, "needs the Read tool") {
			t.Errorf("%v without Read: %q", in, res.Text)
		}
	}
	if _, res := fileExec(t, narrowed, map[string]any{"op": "stat", "path": kept.Record.Ref}); res.IsError {
		t.Errorf("the inbox needs no other tool: %q", res.Text)
	}
}

// A record describes the bytes that were kept. Once the file changes (the
// inbox is writable to the run) it is reported for what it is, not for
// where the original came from — even at the same size.
func TestFile_ChangedFileLosesItsRecord(t *testing.T) {
	r, ctx, docs := fileRun(t, inbox.Service{}, false)
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "mcp:trusted/get"}, filePDF)
	ref, _ := r.Set.Get(inbox.Name)
	forged := bytes.Clone(filePDF)
	forged[len(forged)-1] = 'X'
	if err := os.WriteFile(filepath.Join(ref.Root, "documents", "a.pdf"), forged, 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := fileExec(t, ctx, map[string]any{"op": "stat", "path": kept.Record.Ref})
	if out["origin"] != "unknown" || out["sha256"] == kept.Record.SHA256 {
		t.Errorf("stat of a changed file = %v", out)
	}
	out, _ = fileExec(t, ctx, map[string]any{"op": "keep", "from": kept.Record.Ref, "to": "volume:docs:a.pdf"})
	if out["origin"] != nil || out["sha256"] == kept.Record.SHA256 {
		t.Errorf("keep of a changed file = %v", out)
	}
	if b, _ := os.ReadFile(filepath.Join(docs, "a.pdf")); !bytes.Equal(b, forged) {
		t.Error("keep did not copy the file as it is")
	}
}
