package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
)

// execFile runs one File call on f, returning the result and the Go error.
func execFile(f *File, ctx context.Context, in map[string]any) (tools.Result, error) {
	raw, _ := json.Marshal(in)
	return f.Execute(ctx, raw)
}

// pollWaiting waits until n calls are queued for one of lim's slots.
func pollWaiting(t *testing.T, lim *extract.Limiter, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for lim.Stats().Waiting != n {
		if time.Now().After(deadline) {
			t.Fatalf("waiting = %d, want %d", lim.Stats().Waiting, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestFileExtract_WaitsForAnExtractionSlotAndGivesUpWithItsCaller: with every
// slot of the process's limit held, op=extract waits rather than parsing, and
// runs once a slot frees; a waiting call whose caller goes away returns that
// error at once; and one that waits past its bound is answered busy, not
// left hanging.
func TestFileExtract_WaitsForAnExtractionSlotAndGivesUpWithItsCaller(t *testing.T) {
	r, ctx, _ := fileRun(t, inbox.Service{}, false)
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "notes.txt", Origin: "http"}, []byte("Рішення: затвердити бюджет"))
	lim := extract.NewLimiter(1)
	f := &File{Extractions: lim}
	held, err := lim.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		res tools.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := execFile(f, ctx, map[string]any{"op": "extract", "path": kept.Record.Ref})
		done <- outcome{res, err}
	}()
	pollWaiting(t, lim, 1)
	select {
	case o := <-done:
		t.Fatalf("extract ran with every slot held: %+v %v", o.res, o.err)
	case <-time.After(50 * time.Millisecond):
	}
	held()
	select {
	case o := <-done:
		if o.err != nil || o.res.IsError {
			t.Fatalf("extract after the slot freed: %+v %v", o.res, o.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("extract did not run once a slot freed")
	}
	if st := lim.Stats(); st.InFlight != 0 || st.Waiting != 0 {
		t.Fatalf("after: %+v", st)
	}

	// A caller that goes away while waiting gets its own error back at once.
	held, _ = lim.Acquire(context.Background())
	defer held()
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		res, err := execFile(f, cctx, map[string]any{"op": "extract", "path": kept.Record.Ref})
		done <- outcome{res, err}
	}()
	pollWaiting(t, lim, 1)
	cancel()
	select {
	case o := <-done:
		if !errors.Is(o.err, context.Canceled) {
			t.Errorf("cancelled wait: %+v %v, want context.Canceled", o.res, o.err)
		}
	case <-time.After(time.Second):
		t.Fatal("a cancelled extract kept waiting for a slot")
	}

	// A wait past its bound is a busy answer the model may retry.
	f.slotWait = 20 * time.Millisecond
	res, err := execFile(f, ctx, map[string]any{"op": "extract", "path": kept.Record.Ref})
	if err != nil || !res.IsError || !strings.Contains(res.Text, "as many files as it allows at once") {
		t.Errorf("bounded wait: %+v %v", res, err)
	}
}

// TestFile_ExtractionBudgetIsSharedAcrossOps: op=extract, op=import and the
// PDF page cut an OCR pass makes all take a slot of the same limiter, and a
// per-run copy of the tool (narrowing.go) shares it with the original.
func TestFile_ExtractionBudgetIsSharedAcrossOps(t *testing.T) {
	f, ctx, _, _ := fileDocRun(t)
	lim := extract.NewLimiter(1)
	f.Extractions = lim
	kept := inbox.Capture(ctx, inbox.Item{Name: "notes.md", Origin: "http"}, []byte("# Нотатки\n\nРішення: затвердити бюджет\n"))
	pdf := scannedPDF(t, 2)

	held, err := lim.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.slotWait = 20 * time.Millisecond
	copied := NarrowHosts([]tools.Tool{f}, []string{"example.com"}, "", true)[0].(*File)
	if copied == f {
		t.Fatal("narrowing did not copy the tool; the test needs a per-run copy")
	}
	for _, c := range []struct {
		name string
		f    *File
		in   map[string]any
	}{
		{"extract", f, map[string]any{"op": "extract", "path": kept.Record.Ref}},
		{"import", f, map[string]any{"op": "import", "path": kept.Record.Ref, "doc_path": "/notes"}},
		{"extract on a per-run copy", copied, map[string]any{"op": "extract", "path": kept.Record.Ref}},
	} {
		res, err := execFile(c.f, ctx, c.in)
		if err != nil || !res.IsError || !strings.Contains(res.Text, "as many files as it allows at once") {
			t.Errorf("%s with the only slot held: %+v %v", c.name, res, err)
		}
	}
	_, _, cutErr := f.ocrUnitFor(ctx, transcribe.Plan{WholePDF: true}, bytes.NewReader(pdf), int64(len(pdf)), "application/pdf", []int{1}, "")
	if !errors.Is(cutErr, extract.ErrBusy) {
		t.Errorf("OCR page cut with the only slot held: err = %v, want ErrBusy", cutErr)
	}

	held()
	if res, err := execFile(f, ctx, map[string]any{"op": "extract", "path": kept.Record.Ref}); err != nil || res.IsError {
		t.Errorf("extract with the slot free: %+v %v", res, err)
	}
	if res, err := execFile(f, ctx, map[string]any{"op": "import", "path": kept.Record.Ref, "doc_path": "/notes"}); err != nil || res.IsError {
		t.Errorf("import with the slot free: %+v %v", res, err)
	}
	if u, _, err := f.ocrUnitFor(ctx, transcribe.Plan{WholePDF: true}, bytes.NewReader(pdf), int64(len(pdf)), "application/pdf", []int{1}, ""); err != nil || len(u.data) == 0 {
		t.Errorf("OCR page cut with the slot free: %v", err)
	}
	if st := lim.Stats(); st.InFlight != 0 || st.Waiting != 0 {
		t.Errorf("a slot was kept: %+v", st)
	}
}

// A File built outside boot (no limiter) still extracts: nil bounds nothing.
func TestFileExtract_NilLimiterBoundsNothing(t *testing.T) {
	_, ctx, docs := fileRun(t, inbox.Service{}, false)
	if err := os.WriteFile(filepath.Join(docs, "a.txt"), []byte("текст"), 0o600); err != nil {
		t.Fatal(err)
	}
	inbox.Capture(ctx, inbox.Item{Name: "seed.txt", Origin: "test"}, []byte("x"))
	if res, err := execFile(&File{}, ctx, map[string]any{"op": "extract", "path": "volume:docs:a.txt"}); err != nil || res.IsError {
		t.Fatalf("extract: %+v %v", res, err)
	}
	if l, in, w := (&File{}).ExtractionStats(); l != 0 || in != 0 || w != 0 {
		t.Errorf("stats of an unwired File = %d %d %d", l, in, w)
	}
}
