package http

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/dynvol"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

func twinZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// unpack writes into the run's inbox, so it follows extract's rule: the
// run's owner, an admin, or a tenant operator of the run's tenant. Anyone
// else gets the not-found a missing run gets, and nothing is written.
func TestFileTwin_UnpackNeedsTheOwnerOrAnAdmin(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	arc := f.capture(t, runID, "acme", "books.zip", "http", twinZip(t, map[string]string{"a.txt": "chapter one\n"}), true)
	unpacked := filepath.Join(dynvol.DerivedEphemeralPath(f.dynRoot, runID, inbox.Name), "unpacked", "books")

	if _, text, isErr := f.call(t, twinReader, map[string]any{"run_id": runID, "op": "unpack", "path": arc.Ref}); !isErr || !strings.Contains(text, "not found") {
		t.Errorf("unpack by a reader = %s", text)
	}
	if _, text, isErr := f.call(t, twinOutsider, map[string]any{"run_id": runID, "op": "unpack", "path": arc.Ref}); !isErr || !strings.Contains(text, "not found") {
		t.Errorf("unpack by another tenant's operator = %s", text)
	}
	if _, err := os.Stat(unpacked); !os.IsNotExist(err) {
		t.Fatalf("a refused unpack wrote: %v", err)
	}

	out, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": runID, "op": "unpack", "path": arc.Ref, "dry_run": true})
	if isErr || out["would_write"] == nil {
		t.Fatalf("owner dry run: %s", text)
	}
	if _, err := os.Stat(unpacked); !os.IsNotExist(err) {
		t.Fatalf("a dry run wrote: %v", err)
	}
	if _, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": runID, "op": "unpack", "path": arc.Ref}); isErr {
		t.Fatalf("owner unpack: %s", text)
	}
	if b, err := os.ReadFile(filepath.Join(unpacked, "a.txt")); err != nil || string(b) != "chapter one\n" {
		t.Errorf("unpacked a.txt = %q %v", b, err)
	}
	if _, text, isErr := f.call(t, twinOperator, map[string]any{"run_id": runID, "op": "unpack", "path": arc.Ref}); isErr {
		t.Errorf("tenant operator unpack: %s", text)
	}
}

// With to, unpack writes into a dynamic volume of the caller's tenant: the
// target keep takes, under the same refusals.
func TestFileTwin_UnpackIntoTheCallersDynamicVolume(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	arc := f.capture(t, runID, "acme", "books.zip", "http", twinZip(t, map[string]string{"shelf/a.txt": "chapter one\n"}), true)
	docs := f.mkVolume(t, "acme", "docs", "rw")
	f.mkVolume(t, "acme", "archive", "ro")
	if err := os.MkdirAll(filepath.Join(docs, "lib"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": runID, "op": "unpack", "path": arc.Ref, "to": "volume:docs:lib/"}); isErr {
		t.Fatalf("unpack to docs: %s", text)
	}
	if b, err := os.ReadFile(filepath.Join(docs, "lib", "shelf", "a.txt")); err != nil || string(b) != "chapter one\n" {
		t.Errorf("unpacked = %q %v", b, err)
	}
	for to, want := range map[string]string{
		"volume:archive:.":   "read-only",
		"volume:work:x/":     "configured volume",
		"volume:inbox:x/":    "deleted with the run",
		"volume:nosuch:lib/": "no volume named",
	} {
		if _, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": runID, "op": "unpack", "path": arc.Ref, "to": to}); !isErr || !strings.Contains(text, want) {
			t.Errorf("unpack to %s = %s, want %q", to, text, want)
		}
	}
}

// dry_run is taken here by unpack only: extract's prices a model pass this
// surface does not run.
func TestFileTwin_DryRunIsUnpacksOnly(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "a.txt", "http", []byte("text\n"), true)
	if _, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": runID, "op": "extract", "path": rec.Ref, "dry_run": true}); !isErr || !strings.Contains(text, "unpack only") {
		t.Errorf("extract with dry_run = %s", text)
	}
}
