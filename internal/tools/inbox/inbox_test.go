package inbox_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox/inboxtest"
)

var pdf = append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("x"), 2000)...)

func readKept(t *testing.T, r *inboxtest.Run, rec inbox.Record) []byte {
	t.Helper()
	ref, ok := r.Set.Get(inbox.Name)
	if !ok {
		t.Fatal("the inbox is not in the run tree's volume set")
	}
	b, err := os.ReadFile(filepath.Join(ref.Root, filepath.FromSlash(rec.Path)))
	if err != nil {
		t.Fatalf("read kept file: %v", err)
	}
	return b
}

func TestInbox_CaptureKeepsBytesAndReturnsStub(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	res := inbox.Capture(r.Ctx, inbox.Item{Name: "Board papers 2026/10.pdf", Origin: "mcp:ms365/get"}, pdf)
	if !res.Kept {
		t.Fatalf("not kept: %s", res.Stub)
	}
	if res.Record.Path != "documents/10.pdf" {
		t.Errorf("path = %q, want documents/10.pdf (last segment only)", res.Record.Path)
	}
	if res.Record.MediaType != "application/pdf" {
		t.Errorf("media type = %q", res.Record.MediaType)
	}
	for _, want := range []string{"[file: volume:inbox:documents/10.pdf", "application/pdf", "from mcp:ms365/get", "sha256:"} {
		if !strings.Contains(res.Stub, want) {
			t.Errorf("stub %q lacks %q", res.Stub, want)
		}
	}
	if !bytes.Equal(readKept(t, r, res.Record), pdf) {
		t.Error("kept bytes differ from the captured ones")
	}
	// Recorded like any ephemeral volume, so the purge and the sweeper find it.
	if len(r.Store.Ephemeral) != 1 || r.Store.Ephemeral[0].Name != "inbox" || r.Store.Ephemeral[0].RootRunID != "root-1" {
		t.Errorf("ephemeral rows = %+v", r.Store.Ephemeral)
	}
	evs := r.Events()
	if len(evs) != 1 || evs[0].Type != providers.EventFileCaptured || evs[0].File == nil || evs[0].File.Ref != res.Record.Ref {
		t.Errorf("events = %+v", evs)
	}
}

func TestInbox_ImagesGoToImagesAndNamesAreDeduplicated(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	png := []byte("\x89PNG\r\n\x1a\n0000")
	a := inbox.Capture(r.Ctx, inbox.Item{Fallback: "screenshot", Origin: "o"}, png)
	b := inbox.Capture(r.Ctx, inbox.Item{Fallback: "screenshot", Origin: "o"}, png)
	if a.Record.Path != "images/screenshot.png" || b.Record.Path != "images/screenshot-2.png" {
		t.Errorf("paths = %q, %q", a.Record.Path, b.Record.Path)
	}
}

func TestInbox_OfficeFileIsTypedByItsExtension(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	zip := append([]byte("PK\x03\x04"), bytes.Repeat([]byte{0}, 100)...)
	res := inbox.Capture(r.Ctx, inbox.Item{Name: "minutes.docx", MediaType: "application/octet-stream", Origin: "o"}, zip)
	if !strings.Contains(res.Record.MediaType, "wordprocessingml") {
		t.Errorf("media type = %q", res.Record.MediaType)
	}
}

func TestInbox_RefusalStubWithoutDynamicRoot(t *testing.T) {
	r := inboxtest.NewWithoutDynamicRoot(t, "root-1")
	res := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "mcp:s/t"}, pdf)
	if res.Kept || !strings.HasPrefix(res.Stub, "[file not kept: no dynamic_root volume") {
		t.Errorf("stub = %q", res.Stub)
	}
	if strings.Contains(res.Stub, "PDF-1.7") {
		t.Error("the refusal leaked the bytes")
	}
}

func TestInbox_PerFileCapRefuses(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{MaxFileBytes: 1000})
	res := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, pdf)
	if res.Kept || !strings.Contains(res.Stub, "per-file limit") {
		t.Errorf("stub = %q", res.Stub)
	}
}

func TestInbox_StreamOverCapLeavesNothing(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{MaxFileBytes: 1000})
	res := inbox.CaptureReader(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, bytes.NewReader(pdf), -1)
	if res.Kept || !strings.Contains(res.Stub, "per-file limit") {
		t.Fatalf("stub = %q", res.Stub)
	}
	ref, _ := r.Set.Get(inbox.Name)
	ents, _ := os.ReadDir(filepath.Join(ref.Root, "documents"))
	if len(ents) != 0 {
		t.Errorf("left behind: %v", ents)
	}
}

func TestInbox_TreeCapRefusesAndForgetGivesItBack(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{MaxTreeBytes: 3000})
	first := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, pdf)
	second := inbox.Capture(r.Ctx, inbox.Item{Name: "b.pdf", Origin: "o"}, pdf)
	if !first.Kept || second.Kept || !strings.Contains(second.Stub, "inbox is full") {
		t.Fatalf("first=%q second=%q", first.Stub, second.Stub)
	}
	inbox.Forget(r.Ctx, first.Record.Path)
	if third := inbox.Capture(r.Ctx, inbox.Item{Name: "c.pdf", Origin: "o"}, pdf); !third.Kept {
		t.Errorf("after forget: %q", third.Stub)
	}
}

func TestInbox_ReservedNameClashRefuses(t *testing.T) {
	t.Run("stored volume", func(t *testing.T) {
		r := inboxtest.New(t, "root-1", inbox.Service{})
		r.Store.Volumes["t1/inbox"] = true
		res := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, pdf)
		if res.Kept || !strings.Contains(res.Stub, "reserved") {
			t.Errorf("stub = %q", res.Stub)
		}
	})
	t.Run("shared stored volume", func(t *testing.T) {
		r := inboxtest.New(t, "root-1", inbox.Service{})
		r.Store.Volumes["/inbox"] = true
		if res := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, pdf); res.Kept {
			t.Errorf("kept despite a shared volume named inbox: %q", res.Stub)
		}
	})
	t.Run("configured volume", func(t *testing.T) {
		r := inboxtest.New(t, "root-1", inbox.Service{StaticVolumeNames: []string{"data", "inbox"}})
		if res := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, pdf); res.Kept {
			t.Errorf("kept despite a configured volume named inbox: %q", res.Stub)
		}
	})
}

// D10: the runtime keeps a sub-agent's tool result even when the run tree
// gave that sub-agent its ephemeral volumes read-only.
func TestInbox_ReadOnlyEphemeralStillCaptures(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	first := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, pdf)
	r.Set.SetReadOnly(inbox.Name)
	second := inbox.Capture(r.Ctx, inbox.Item{Name: "b.pdf", Origin: "o"}, pdf)
	if !first.Kept || !second.Kept {
		t.Errorf("first=%q second=%q", first.Stub, second.Stub)
	}
}

// A resumed tree rebuilds its set from the rows; the inbox it made before the
// pause is used again, not provisioned a second time.
func TestInbox_ResumedTreeReusesItsInbox(t *testing.T) {
	dyn := t.TempDir()
	r := inboxtest.New(t, "root-1", inbox.Service{DynamicRoot: dyn})
	first := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, pdf)
	ref, _ := r.Set.Get(inbox.Name)

	resumed := inboxtest.New(t, "root-1", inbox.Service{DynamicRoot: dyn, Store: r.Store})
	resumed.Set.Add(inbox.Name, tools.EphemeralVolumeRef{Root: ref.Root})
	second := inbox.Capture(resumed.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, pdf)
	if second.Record.Path != "documents/a-2.pdf" {
		t.Errorf("resumed capture path = %q (first was %q)", second.Record.Path, first.Record.Path)
	}
	if len(r.Store.Ephemeral) != 1 {
		t.Errorf("rows = %d, want the one row", len(r.Store.Ephemeral))
	}
}

func TestInbox_NoInboxOnContextRefuses(t *testing.T) {
	res := inbox.Capture(t.Context(), inbox.Item{Name: "a.pdf", Origin: "o"}, pdf)
	if res.Kept || !strings.Contains(res.Stub, "no inbox") {
		t.Errorf("stub = %q", res.Stub)
	}
}

func TestSanitizeName(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":                   "report.pdf",
		"../../etc/passwd":             "passwd",
		"C:\\Users\\x\\Звіт 2026.docx": "Zvit-2026.docx",
		"Звіт.pdf":                     "Zvit.pdf",
		"Протокол засідання №3.pdf": "Protokol-zasidannya-3.pdf",
		"№.pdf":                          "file.pdf",
		".hidden":                        "hidden",
		"-rf":                            "rf",
		"https://h/a/b.pdf?sig=SECRET#x": "b.pdf",
		"":                               "",
		"a b  c.txt":                     "a-b-c.txt",
		"%D0%97%D0%B2%D1%96%D1%82.pdf":   "Zvit.pdf",
		"a%2F..%2F..%2Fb.pdf":            "b.pdf",
	} {
		if got := inbox.SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("a", 300) + ".pdf"
	if got := inbox.SanitizeName(long); len(got) > 120 || !strings.HasSuffix(got, ".pdf") {
		t.Errorf("long name = %q (%d)", got, len(got))
	}
}

// The inbox is only ever the path the runtime derives for the tree; a set
// entry by the name pointing anywhere else is not written to.
func TestInbox_SetEntryOutsideTheDerivedPathRefuses(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	elsewhere := t.TempDir()
	r.Set.Add(inbox.Name, tools.EphemeralVolumeRef{Root: elsewhere})
	res := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, pdf)
	if res.Kept || !strings.Contains(res.Stub, "not its inbox") {
		t.Errorf("stub = %q", res.Stub)
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Errorf("wrote into %s: %v", elsewhere, ents)
	}
}

// A resumed tree starts a new process's inbox state; the cap counts the files
// the tree already kept, or a resume would reset the allowance.
func TestInbox_ResumedTreeCountsWhatItAlreadyHolds(t *testing.T) {
	dyn := t.TempDir()
	r := inboxtest.New(t, "root-1", inbox.Service{DynamicRoot: dyn, MaxTreeBytes: 3000})
	if first := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, pdf); !first.Kept {
		t.Fatal(first.Stub)
	}
	ref, _ := r.Set.Get(inbox.Name)

	resumed := inboxtest.New(t, "root-1", inbox.Service{DynamicRoot: dyn, MaxTreeBytes: 3000, Store: r.Store})
	resumed.Set.Add(inbox.Name, tools.EphemeralVolumeRef{Root: ref.Root})
	if second := inbox.Capture(resumed.Ctx, inbox.Item{Name: "b.pdf", Origin: "o"}, pdf); second.Kept {
		t.Errorf("a resumed tree kept past its cap: %q", second.Stub)
	}
}

type sharedKey struct{}

// A value kept beside the inbox is made once per tree and seen by every run
// of it; another tree, and a context with no inbox, do not see it.
func TestShared_OneValuePerTree(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	made := 0
	mk := func() any { made++; return new(int) }
	a, ok := inbox.Shared(r.Ctx, sharedKey{}, mk)
	b, ok2 := inbox.Shared(tools.WithRunID(r.Ctx, "child-1"), sharedKey{}, mk)
	if !ok || !ok2 || a != b || made != 1 {
		t.Fatalf("shared = %v %v (%v %v), made %d", a, b, ok, ok2, made)
	}
	other := inboxtest.New(t, "root-2", inbox.Service{})
	if c, _ := inbox.Shared(other.Ctx, sharedKey{}, mk); c == a || made != 2 {
		t.Fatalf("another tree shares the value (made %d)", made)
	}
	if _, ok := inbox.Shared(t.Context(), sharedKey{}, mk); ok {
		t.Fatal("a context with no inbox has a shared value")
	}
}

// A download's URL is kept so an import can link back to it, but never in a
// form that still authorises a request: no user information, no query.
func TestInbox_RecordURLIsMasked(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	const secret = "0123456789abcdefSECRET"
	res := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "download:files.example",
		URL: "https://user:pa55word@files.example/dl/a.pdf?id=7&tempauth=" + secret + "&sig=" + secret + "&X-Amz-Signature=" + secret}, pdf)
	if !res.Kept {
		t.Fatal(res.Stub)
	}
	u := res.Record.URL
	if strings.Contains(u, secret) || strings.Contains(u, "pa55word") || strings.Contains(u, "user@") {
		t.Errorf("record URL keeps a credential: %q", u)
	}
	if u != "https://files.example/dl/a.pdf?…" {
		t.Errorf("record URL = %q", u)
	}
	got, ok := inbox.Lookup(r.Ctx, res.Record.Path)
	if !ok || got.URL != u {
		t.Errorf("looked-up record URL = %q", got.URL)
	}
	// What is not a URL is not kept at all.
	if res := inbox.Capture(r.Ctx, inbox.Item{Name: "b.pdf", URL: "not a url ?token=" + secret}, pdf); res.Record.URL != "" {
		t.Errorf("a non-URL was recorded: %q", res.Record.URL)
	}
}

// The record of an extracted file carries its source's record one level deep.
func TestInbox_ExtractedFromIsOneLevel(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	grand := &inbox.Record{Path: "documents/x.pdf"}
	src := inbox.Record{Path: "documents/a.pdf", SHA256: "abc", Origin: "mcp:m/get", ExtractedFrom: grand}
	res := inbox.Capture(r.Ctx, inbox.Item{Name: "a.md", MediaType: "text/markdown", Origin: "extract:mcp:m/get", ExtractedFrom: &src}, []byte("# A\n"))
	from := res.Record.ExtractedFrom
	if from == nil || from.Path != "documents/a.pdf" || from.SHA256 != "abc" || from.ExtractedFrom != nil {
		t.Errorf("extracted_from = %+v", from)
	}
	if src.ExtractedFrom != grand {
		t.Error("the caller's record was changed")
	}
}

// A credential can sit in any query parameter, not only the ones the signed
// URL rule names, so a URL the runtime records keeps no query value at all:
// scheme, host and path, a "?…" where the query was, no fragment.
func TestInbox_RecordURLKeepsNoQueryValue(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	const secret = "0123456789abcdefSECRET"
	res := inbox.Capture(r.Ctx, inbox.Item{Name: "a.pdf", Origin: "download:files.example",
		URL: "https://files.example/dl/a.pdf?code=" + secret + "&k=" + secret + "#frag=" + secret}, pdf)
	if !res.Kept {
		t.Fatal(res.Stub)
	}
	if want := "https://files.example/dl/a.pdf?…"; res.Record.URL != want {
		t.Errorf("record URL = %q, want %q", res.Record.URL, want)
	}
	plain := inbox.Capture(r.Ctx, inbox.Item{Name: "b.pdf", URL: "https://files.example/dl/b.pdf#x"}, pdf)
	if want := "https://files.example/dl/b.pdf"; plain.Record.URL != want {
		t.Errorf("record URL without a query = %q, want %q", plain.Record.URL, want)
	}
}
