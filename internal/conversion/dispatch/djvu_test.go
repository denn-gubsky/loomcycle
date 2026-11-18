//go:build unix

package dispatch_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/conversion/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
	"github.com/denn-gubsky/loomcycle/internal/conversion/worker"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/convert/djvutest"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
)

func djvuBook(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "files", "extract", "testdata", "djvu", "book.djvu"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func djvuHelpers() (text, inspect *convert.Helper) {
	return &convert.Helper{Key: convert.DjVuTextHelperKey, Command: convert.DefaultDjVuTextCommand},
		&convert.Helper{Key: convert.DjVuInspectHelperKey, Command: convert.DefaultDjVuInspectCommand}
}

// A worker whose DjVu text helper does not resolve does not carry djvu, and
// a DjVu extraction with no such worker attached is refused at once as a
// program to install, naming DjVuLibre.
func TestPool_AWorkerWithoutDjvutxtDoesNotOfferDjVu(t *testing.T) {
	d, target := pool(t, dispatch.Options{QueueWait: time.Minute, Configured: map[string]bool{wire.LabelDjVu: true}})
	helperWorker(t, target, "w1", worker.Options{
		DjVuText: &convert.Helper{Key: convert.DjVuTextHelperKey, Command: "loomcycle-no-such-djvutxt --page={pages} {in}"},
	})
	waitIdle(t, d, 1)
	book := djvuBook(t)
	start := time.Now()
	_, err := d.Client().Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(book), int64(len(book)), &recSink{}, extract.Options{Name: "book.djvu"})
	if !errors.Is(err, convert.ErrNoHelper) || !strings.Contains(err.Error(), "djvulibre-bin") || !strings.Contains(err.Error(), "djvutxt") {
		t.Errorf("err = %v, want ErrNoHelper naming djvulibre-bin and djvutxt", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("the refusal waited %v; it must not queue", time.Since(start))
	}
	// Other extractions still run there.
	csv := []byte("a,b\n1,2\n")
	if _, err := d.Client().Extract(context.Background(), "text/csv", bytes.NewReader(csv), int64(len(csv)), &recSink{}, extract.Options{Name: "t.csv"}); err != nil {
		t.Errorf("csv on the same worker: %v", err)
	}
}

// A DjVu goes to the worker that has DjVuLibre and reads as it reads in
// this process, pages and all: only the pages asked for reach the helper.
func TestPool_DjVuOnAWorkerMatchesInProcess(t *testing.T) {
	fake := djvutest.Install(t)
	text, inspect := djvuHelpers()
	d, target := pool(t, dispatch.Options{Configured: map[string]bool{wire.LabelDjVu: true}})
	helperWorker(t, target, "plain", worker.Options{})
	helperWorker(t, target, "djvu", worker.Options{DjVuText: text, DjVuInspect: inspect})
	waitIdle(t, d, 2)
	book := djvuBook(t)
	here := &convert.InProcess{DjVuText: text, DjVuInspect: inspect, Scratch: func(context.Context) (string, error) { return t.TempDir(), nil }}
	for _, pages := range [][]int{nil, {3}} {
		opts := extract.Options{Name: "book.djvu", Pages: pages}
		var want, got recSink
		wantRep, err := here.Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(book), int64(len(book)), &want, opts)
		if err != nil {
			t.Fatal(err)
		}
		// Each extraction lands on the djvu worker, whichever slot is idle first.
		for range 3 {
			got = recSink{}
			gotRep, err := d.Client().Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(book), int64(len(book)), &got, opts)
			if err != nil {
				t.Fatalf("pages %v: %v", pages, err)
			}
			if got.md.String() != want.md.String() || !reflect.DeepEqual(got.starts, want.starts) || !reflect.DeepEqual(gotRep, wantRep) {
				t.Fatalf("pages %v: pool\n%s\n%+v\nin process\n%s\n%+v", pages, got.md.String(), gotRep, want.md.String(), wantRep)
			}
		}
	}
	var asked []string
	for _, c := range fake.Calls(t) {
		if strings.HasPrefix(c, "djvutxt ") {
			asked = append(asked, strings.Fields(c)[2])
		}
	}
	if strings.Join(asked, " ") != strings.Repeat("--page=1,3 ", 4)+strings.TrimSpace(strings.Repeat("--page=3 ", 4)) {
		t.Errorf("text helper asked %q", asked)
	}
}

func TestExtractLabels_DjVuNeedsTheDjVuWorker(t *testing.T) {
	for _, c := range []struct {
		mt, name string
		pages    bool
		want     []string
	}{
		{"application/pdf", "a.pdf", false, []string{wire.LabelExtract}},
		{"application/pdf", "a.pdf", true, []string{wire.LabelExtract, wire.LabelExtractPages}},
		{extract.DjVuMediaType, "", false, []string{wire.LabelDjVu}},
		{"image/x-djvu", "", true, []string{wire.LabelDjVu, wire.LabelExtractPages}},
		{"application/octet-stream", "scan.djvu", false, []string{wire.LabelDjVu}},
		{"application/pdf", "misnamed.djvu", false, []string{wire.LabelExtract}},
	} {
		if got := wire.ExtractLabels(c.mt, c.name, c.pages); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ExtractLabels(%q, %q, %v) = %v, want %v", c.mt, c.name, c.pages, got, c.want)
		}
	}
	if !wire.HelperLabel(wire.LabelDjVu) {
		t.Error("djvu is not a helper label")
	}
}
