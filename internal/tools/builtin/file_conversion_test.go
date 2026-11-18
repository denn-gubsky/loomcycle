package builtin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/conversion/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/conversion/worker"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// refusingPool is a worker pool that refuses every extraction with err.
type refusingPool struct {
	convert.InProcess
	err error
}

func (p *refusingPool) Extract(context.Context, string, io.ReaderAt, int64, extract.Sink, extract.Options) (extract.Report, error) {
	return extract.Report{}, p.err
}

// The pool's two refusals reach the model as what they are: a file that
// crashed the converter is not to be retried as it is, and a missing worker is
// a fault of the server that may pass.
func TestFileExtract_PoolRefusalsAreWorded(t *testing.T) {
	r, ctx, _ := fileRun(t, inbox.Service{}, false)
	kept := inbox.Capture(r.Ctx, inbox.Item{Name: "notes.txt", Origin: "http"}, []byte("text"))
	for _, c := range []struct {
		err  error
		want string
	}{
		{convert.ErrQuarantined, "crashed the file converter"},
		{convert.ErrNoWorkers, "no file conversion worker is running"},
	} {
		f := &File{Convert: &refusingPool{err: c.err}}
		res, err := execFile(f, ctx, map[string]any{"op": "extract", "path": kept.Record.Ref})
		if err != nil || !res.IsError || !strings.Contains(res.Text, c.want) {
			t.Errorf("%v: %+v %v, want an error saying %q", c.err, res, err, c.want)
		}
	}
}

// End to end through the File tool: op=extract with the conversion worker
// pool writes the same text as op=extract in this process.
func TestFileExtract_ThroughAWorkerWritesWhatInProcessWrites(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := dispatch.New(dispatch.Options{Token: "tok"})
	go func() { _ = d.Serve(l) }()
	defer d.Stop()
	wctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		_ = worker.Run(wctx, worker.Options{Target: l.Addr().String(), Token: "tok", ID: "w", Scratch: t.TempDir()})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for d.Stats().Idle == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the worker did not attach")
		}
		time.Sleep(5 * time.Millisecond)
	}

	extractText := func(f *File) string {
		t.Helper()
		r, ctx, _ := fileRun(t, inbox.Service{}, false)
		kept := inbox.Capture(r.Ctx, inbox.Item{Name: "budget.csv", Origin: "http"}, []byte("item,amount\nroof,1200\nwindows,800\n"))
		res, err := execFile(f, ctx, map[string]any{"op": "extract", "path": kept.Record.Ref})
		if err != nil || res.IsError {
			t.Fatalf("extract: %+v %v", res, err)
		}
		return res.Text
	}
	here := extractText(&File{})
	there := extractText(&File{Convert: d.Client()})
	if here != there || !strings.Contains(there, "roof") {
		t.Fatalf("through the worker:\n%s\nin process:\n%s", there, here)
	}
	if st := d.Stats(); st.Jobs["extract/ok"] != 1 {
		t.Fatalf("the pool ran %v, want one extract", st.Jobs)
	}
}

// With a pool, every conversion the File tool makes goes to it: the
// extraction, the OCR page cuts and renders, and a recording's conversion for
// speech-to-text. None is left in this process (RFC GO P2).
func TestFile_EveryConversionGoesToThePool(t *testing.T) {
	pool := &refusingPool{}
	f := &File{Convert: pool, STT: &FileSTT{}}
	if got := f.converter(); got != convert.Client(pool) {
		t.Errorf("converter() = %T, want the pool", got)
	}
	if got := f.sttDeps().Convert; got != convert.Client(pool) {
		t.Errorf("sttDeps().Convert = %T, want the pool", got)
	}
}

// tinyEPUB is a one-chapter EPUB 3 book with Dublin Core metadata.
func tinyEPUB(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	add := func(name, body string, method uint16) {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("mimetype", "application/epub+zip", zip.Store)
	add("META-INF/container.xml", `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="c.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`, zip.Deflate)
	add("c.opf", `<package xmlns="http://www.idpf.org/2007/opf" version="3.0"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Лісова пісня</dc:title><dc:creator>Леся Українка</dc:creator><dc:language>uk</dc:language><dc:date>1911</dc:date><dc:identifier>urn:isbn:9780000000002</dc:identifier></metadata><manifest><item id="a" href="a.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="a"/></spine></package>`, zip.Deflate)
	add("a.xhtml", `<html xmlns="http://www.w3.org/1999/xhtml"><body><h1>Пролог</h1><p>Старий праліс на Волині.</p></body></html>`, zip.Deflate)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A book read through a conversion worker gives what it gives in this
// process: the same text, and the same metadata beside the report (RFC GV
// D10), which crosses the worker protocol whole.
func TestFileExtract_EPUBWorkerAndInProcessAgree(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := dispatch.New(dispatch.Options{Token: "tok"})
	go func() { _ = d.Serve(l) }()
	defer d.Stop()
	wctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		_ = worker.Run(wctx, worker.Options{Target: l.Addr().String(), Token: "tok", ID: "w", Scratch: t.TempDir()})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for d.Stats().Idle == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the worker did not attach")
		}
		time.Sleep(5 * time.Millisecond)
	}
	book := tinyEPUB(t)
	extractBook := func(f *File) (map[string]any, string) {
		t.Helper()
		r, ctx, _ := fileRun(t, inbox.Service{}, false)
		kept := inbox.Capture(r.Ctx, inbox.Item{Name: "lisova", Origin: "http"}, book)
		res, err := execFile(f, ctx, map[string]any{"op": "extract", "path": kept.Record.Ref})
		if err != nil || res.IsError {
			t.Fatalf("extract: %+v %v", res, err)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("result is not JSON: %v\n%s", err, res.Text)
		}
		return out, res.Text
	}
	here, hereText := extractBook(&File{})
	there, thereText := extractBook(&File{Convert: d.Client()})
	if hereText != thereText {
		t.Fatalf("through the worker:\n%s\nin process:\n%s", thereText, hereText)
	}
	meta, _ := there["metadata"].(map[string]any)
	if here["format"] != "epub" || meta["title"] != "Лісова пісня" || meta["year"] != float64(1911) ||
		!strings.Contains(fmt.Sprint(meta["authors"]), "Леся Українка") || !strings.Contains(fmt.Sprint(meta["identifiers"]), "9780000000002") {
		t.Errorf("format %v, metadata %v", here["format"], meta)
	}
	if !strings.Contains(thereText, "Пролог") {
		t.Errorf("the chapter was not read:\n%s", thereText)
	}
	if st := d.Stats(); st.Jobs["extract/ok"] != 1 {
		t.Fatalf("the pool ran %v, want one extract", st.Jobs)
	}
}
