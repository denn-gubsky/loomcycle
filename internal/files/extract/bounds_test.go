package extract

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"os"
	"strings"
	"testing"
)

func extractDOCX(t *testing.T, data []byte, opts Options) (*memSink, Report, error) {
	t.Helper()
	s := &memSink{}
	opts.Images = true
	rep, err := DOCX{}.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), s, opts)
	return s, rep, err
}

// docxWith is a minimal DOCX whose document part is body.
func docxWith(t *testing.T, body string, more ...entry) []byte {
	t.Helper()
	es := []entry{
		xmlEntry("_rels/.rels", relsXML([3]string{"rId1", "officeDocument", "word/document.xml"})),
		{name: "word/document.xml", data: []byte(body)},
	}
	return zipOf(t, append(es, more...)...)
}

func TestExtract_ZipBombRefused(t *testing.T) {
	if MaxZipEntries != 10000 || MaxZipExpansion != 8 || MaxZipUncompressed != 256<<20 {
		t.Fatalf("bounds changed: entries %d, expansion %d, total %d", MaxZipEntries, MaxZipExpansion, MaxZipUncompressed)
	}

	t.Run("compression ratio", func(t *testing.T) {
		// 4 MiB of spaces deflate to a few KiB: far past 100x, under the
		// total bound, so only the ratio guard can refuse it.
		body := `<w:document ` + nsW + `><w:body>` + strings.Repeat(" ", 4<<20) + `</w:body></w:document>`
		_, _, err := extractDOCX(t, docxWith(t, body), Options{})
		if !IsRefused(err) || !strings.Contains(err.Error(), "times its compressed size") {
			t.Fatalf("err = %v, want a ratio refusal", err)
		}
	})

	t.Run("total declared", func(t *testing.T) {
		// Honest headers that add up past the budget are refused before any
		// entry is inflated.
		lim := &zipLimits{entries: 100, expansion: 8, maxTotal: 1 << 20, floor: 0, ratio: 1 << 20, ratioFree: 1 << 30}
		big := bytes.Repeat([]byte("Рішення "), (1<<20)/len("Рішення ")+10)
		_, _, err := extractDOCX(t, docxWith(t, `<w:document `+nsW+`><w:body/></w:document>`, entry{name: "word/media/big.bin", data: big}), Options{limits: lim})
		if !IsRefused(err) || !strings.Contains(err.Error(), "expands to more than") {
			t.Fatalf("err = %v, want a total refusal", err)
		}
	})

	t.Run("total as read", func(t *testing.T) {
		// Headers that understate the size: the bytes inflated are what
		// count, so the read stops at the budget.
		lim := &zipLimits{entries: 100, expansion: 8, maxTotal: 1 << 20, floor: 0, ratio: 1 << 20, ratioFree: 1 << 30}
		var raw bytes.Buffer
		fw, _ := flate.NewWriter(&raw, flate.BestCompression)
		body := `<w:document ` + nsW + `><w:body>` + strings.Repeat(" ", 2<<20) + `</w:body></w:document>`
		_, _ = fw.Write([]byte(body))
		_ = fw.Close()
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		w, _ := zw.Create("_rels/.rels")
		_, _ = w.Write([]byte(relsXML([3]string{"rId1", "officeDocument", "word/document.xml"})))
		rw, err := zw.CreateRaw(&zip.FileHeader{Name: "word/document.xml", Method: zip.Deflate,
			CompressedSize64: uint64(raw.Len()), UncompressedSize64: 100})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = rw.Write(raw.Bytes())
		_ = zw.Close()
		_, _, err = extractDOCX(t, b.Bytes(), Options{limits: lim})
		if err == nil {
			t.Fatal("an archive whose entry inflates past what it declares was read")
		}
	})

	t.Run("entry count", func(t *testing.T) {
		es := make([]entry, 0, MaxZipEntries+1)
		for i := range MaxZipEntries + 1 {
			es = append(es, entry{name: "x/" + itoa(i), data: nil})
		}
		_, _, err := extractDOCX(t, zipOf(t, es...), Options{})
		if !IsRefused(err) || !strings.Contains(err.Error(), "entries") {
			t.Fatalf("err = %v, want an entry-count refusal", err)
		}
	})

	t.Run("xml entities", func(t *testing.T) {
		// A DTD's entities are never expanded: an undeclared-to-the-parser
		// entity is an error, so a billion-laughs part goes nowhere.
		body := `<?xml version="1.0"?><!DOCTYPE d [<!ENTITY a "aaaaaaaaaa"><!ENTITY b "&a;&a;&a;&a;&a;&a;&a;&a;&a;&a;">]>` +
			`<w:document ` + nsW + `><w:body><w:p><w:r><w:t>&b;</w:t></w:r></w:p></w:body></w:document>`
		_, _, err := extractDOCX(t, docxWith(t, body), Options{})
		if err == nil {
			t.Fatal("a part using DTD entities was parsed")
		}
	})
}

func TestExtract_EntryNameNeverAPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	pic := tinyPNG(t)
	doc := `<w:document ` + nsW + ` ` + nsR + ` ` + nsA + ` ` + nsWP + `><w:body>` +
		wDrawing("rIdUp", "вгору") + wDrawing("rIdAbs", "абсолютний") + wDrawing("rIdOut", "назовні") +
		`</w:body></w:document>`
	data := docxWith(t, doc,
		xmlEntry("word/_rels/document.xml.rels", relsXML(
			[3]string{"rIdUp", "image", "../../evil/up.png"},
			[3]string{"rIdAbs", "image", "/etc/abs.png"},
			[3]string{"rIdOut", "image", "../../../../../../tmp/out.png"},
		)),
		entry{name: "../evil/up.png", data: pic},
		entry{name: "etc/abs.png", data: pic},
	)
	s, _, err := extractDOCX(t, data, Options{Name: "../../x.docx"})
	if err != nil {
		t.Fatal(err)
	}
	// The two entries the archive holds are read as bytes, under generated
	// names; the target that names nothing in the archive leaves alt text.
	if len(s.images) != 2 {
		t.Fatalf("images = %d, want 2:\n%s", len(s.images), s.md.String())
	}
	for i, img := range s.images {
		if img.Name != "img-"+itoa(i+1)+".png" {
			t.Errorf("image %d named %q", i, img.Name)
		}
	}
	if !strings.Contains(s.md.String(), "[image: назовні]") {
		t.Errorf("md:\n%s", s.md.String())
	}
	// Nothing was written anywhere: the extractor has no file system.
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("the extraction wrote %v into its working directory", ents)
	}
}
