package builtin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/conversion/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/conversion/worker"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"golang.org/x/text/encoding/charmap"
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

// An FB2 book in windows-1251 read through a conversion worker gives what
// it gives in this process: the same text, notes and metadata. It is kept
// with no name, so its type comes from its root element.
func TestFileExtract_FB2WorkerAndInProcessAgree(t *testing.T) {
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
	book, err := charmap.Windows1251.NewEncoder().Bytes([]byte(`<?xml version="1.0" encoding="windows-1251"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0" xmlns:l="http://www.w3.org/1999/xlink">
<description><title-info><author><first-name>Лев</first-name><last-name>Толстой</last-name></author><book-title>Война и мир</book-title><lang>ru</lang><date value="1869-01-01"/></title-info>
<publish-info><isbn>9780000000002</isbn></publish-info></description>
<body><section><title><p>Часть первая</p></title><p>Eh bien, mon prince<a l:href="#n1" type="note">1</a>.</p></section></body>
<body name="notes"><section id="n1"><p>Итак, князь.</p></section></body>
</FictionBook>`))
	if err != nil {
		t.Fatal(err)
	}
	extractBook := func(f *File) (map[string]any, string) {
		t.Helper()
		r, ctx, _ := fileRun(t, inbox.Service{}, false)
		kept := inbox.Capture(r.Ctx, inbox.Item{Origin: "http", Fallback: "book"}, book)
		if kept.Record.MediaType != "application/x-fictionbook+xml" {
			t.Fatalf("kept as %q", kept.Record.MediaType)
		}
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
	if here["format"] != "fb2" || meta["title"] != "Война и мир" || meta["year"] != float64(1869) ||
		!strings.Contains(fmt.Sprint(meta["authors"]), "Лев Толстой") || !strings.Contains(fmt.Sprint(meta["identifiers"]), "9780000000002") {
		t.Errorf("format %v, metadata %v", here["format"], meta)
	}
	if !strings.Contains(thereText, "Часть первая") || !strings.Contains(thereText, "Итак, князь.") {
		t.Errorf("the chapter or its note was not read:\n%s", thereText)
	}
	if st := d.Stats(); st.Jobs["extract/ok"] != 1 {
		t.Fatalf("the pool ran %v, want one extract", st.Jobs)
	}
}

// An RTF document in windows-1251, saved under a .doc name, read through a
// conversion worker gives what it gives in this process: the same text,
// footnote, table and metadata. Its header, not its name, types it.
func TestFileExtract_RTFWorkerAndInProcessAgree(t *testing.T) {
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
	cp1251 := func(s string) string {
		b, err := charmap.Windows1251.NewEncoder().Bytes([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		for _, c := range b {
			if c < 0x80 {
				out.WriteByte(c)
			} else {
				fmt.Fprintf(&out, `\'%02x`, c)
			}
		}
		return out.String()
	}
	doc := []byte(`{\rtf1\ansi\ansicpg1251\deff0{\fonttbl{\f0\fcharset204 Times;}}` +
		`{\info{\title ` + cp1251("Война и мир") + `}{\author ` + cp1251("Лев Толстой") + `}{\creatim\yr1869\mo1\dy1}}` +
		`\pard\outlinelevel0 ` + cp1251("Часть первая") + `\par` +
		`\pard Eh bien, {\b mon prince}{\footnote ` + cp1251("Итак, князь.") + `}.\par` +
		`\trowd\pard\intbl a\cell b\cell\row\pard\par}`)
	extractDoc := func(f *File) (map[string]any, string) {
		t.Helper()
		r, ctx, _ := fileRun(t, inbox.Service{}, false)
		kept := inbox.Capture(r.Ctx, inbox.Item{Name: "war.doc", MediaType: "application/msword", Origin: "http"}, doc)
		if kept.Record.MediaType != "application/rtf" {
			t.Fatalf("kept as %q", kept.Record.MediaType)
		}
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
	here, hereText := extractDoc(&File{})
	there, thereText := extractDoc(&File{Convert: d.Client()})
	if hereText != thereText {
		t.Fatalf("through the worker:\n%s\nin process:\n%s", thereText, hereText)
	}
	meta, _ := there["metadata"].(map[string]any)
	if here["format"] != "rtf" || here["tables"] != float64(1) || meta["title"] != "Война и мир" || meta["year"] != float64(1869) ||
		!strings.Contains(fmt.Sprint(meta["authors"]), "Лев Толстой") || meta["source_format"] != "rtf" {
		t.Errorf("format %v, tables %v, metadata %v", here["format"], here["tables"], meta)
	}
	if !strings.Contains(thereText, "## Часть первая") || !strings.Contains(thereText, "**mon prince**[1]") || !strings.Contains(thereText, "Итак, князь.") {
		t.Errorf("the heading, the run or the footnote was not read:\n%s", thereText)
	}
	if st := d.Stats(); st.Jobs["extract/ok"] != 1 {
		t.Fatalf("the pool ran %v, want one extract", st.Jobs)
	}
}

// A hybrid MOBI book kept with no name or type is typed by its header and
// reads the same through a conversion worker as in this process: its KF8
// part's text, its picture and its metadata (RFC GV §4.4).
func TestFileExtract_MOBIWorkerAndInProcessAgree(t *testing.T) {
	book, err := os.ReadFile(filepath.Join("..", "..", "files", "extract", "testdata", "mobi", "book-hybrid.mobi"))
	if err != nil {
		t.Fatal(err)
	}
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
	extractBook := func(f *File) (map[string]any, string) {
		t.Helper()
		r, ctx, _ := fileRun(t, inbox.Service{}, false)
		kept := inbox.Capture(r.Ctx, inbox.Item{Fallback: "book", MediaType: "application/octet-stream", Origin: "http"}, book)
		if kept.Record.MediaType != "application/x-mobipocket-ebook" {
			t.Fatalf("kept as %q", kept.Record.MediaType)
		}
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
	if here["format"] != "azw3" || here["images"] != float64(1) || meta["title"] != "The Lantern Ledger" || meta["year"] != float64(2021) ||
		!strings.Contains(fmt.Sprint(meta["authors"]), "Ben Rowe") || meta["source_format"] != "azw3" {
		t.Errorf("format %v, images %v, metadata %v", here["format"], here["images"], meta)
	}
	if !strings.Contains(thereText, "Chapter 2: Part Two") {
		t.Errorf("the text was not read:\n%s", thereText)
	}
	if st := d.Stats(); st.Jobs["extract/ok"] != 1 {
		t.Fatalf("the pool ran %v, want one extract", st.Jobs)
	}
}

// A compiled help file kept with no name or type is typed by its ITSF
// header and reads the same through a conversion worker as in this
// process: its topics in contents order, its picture and its metadata
// (RFC GX §4.3).
func TestFileExtract_CHMWorkerAndInProcessAgree(t *testing.T) {
	help, err := os.ReadFile(filepath.Join("..", "..", "files", "extract", "testdata", "chm", "book.chm"))
	if err != nil {
		t.Fatal(err)
	}
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
	extractHelp := func(f *File) (map[string]any, string) {
		t.Helper()
		r, ctx, _ := fileRun(t, inbox.Service{}, false)
		kept := inbox.Capture(r.Ctx, inbox.Item{Fallback: "help", MediaType: "application/octet-stream", Origin: "http"}, help)
		if kept.Record.MediaType != "application/vnd.ms-htmlhelp" {
			t.Fatalf("kept as %q", kept.Record.MediaType)
		}
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
	here, hereText := extractHelp(&File{})
	there, thereText := extractHelp(&File{Convert: d.Client()})
	if hereText != thereText {
		t.Fatalf("through the worker:\n%s\nin process:\n%s", thereText, hereText)
	}
	meta, _ := there["metadata"].(map[string]any)
	if here["format"] != "chm" || here["images"] != float64(2) || meta["title"] != "The Lantern Help" || meta["language"] != "en" || meta["source_format"] != "chm" {
		t.Errorf("format %v, images %v, metadata %v", here["format"], here["images"], meta)
	}
	if !strings.Contains(thereText, "Страница на кириллице") {
		t.Errorf("the windows-1251 page was not read:\n%s", thereText)
	}
	if st := d.Stats(); st.Jobs["extract/ok"] != 1 {
		t.Fatalf("the pool ran %v, want one extract", st.Jobs)
	}
}
