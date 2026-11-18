package extract

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The DjVu container (RFC GV §4.5, D6): an IFF85 file of chunks, read here
// without decoding any of them. The walk gives what the extractor needs
// before it asks DjVuLibre for anything: the page count, which pages carry a
// text layer, and whether there is an outline or annotations to read. It is
// cheap enough to run on every file of a library.
//
// Every length in the file is untrusted. A chunk must lie inside its parent
// and the parent inside the file; a count past the caps below is refused,
// never allocated.

const (
	// maxDjVuChunks caps the chunks one walk visits. A 2,000-page book has
	// about five per page.
	maxDjVuChunks = 200_000
	// maxDjVuDepth caps how deep FORMs nest: a bundled document is
	// DJVM > FORM:DJVU > chunks, so 4 leaves room and no more.
	maxDjVuDepth = 4
	// maxDjVuComponents is the most components a bundled document's
	// directory may name; the count is 16 bits in the format.
	maxDjVuComponents = 1<<16 - 1
)

// errNotDjVu is a file whose bytes are not a DjVu document.
var errNotDjVu = errors.New("extract: not a DjVu file")

// djvuInfo is what the container walk found.
type djvuInfo struct {
	// pages is the page count; pageText[i] says page i+1 has a text layer
	// chunk (TXTa or TXTz) of its own.
	pages    int
	pageText []bool
	// sharedText: a shared component (FORM:DJVI) holds a text chunk, which
	// the pages including it may show, so no page can be skipped as
	// text-less.
	sharedText bool
	// outline: the document has a NAVM (bookmarks) chunk. anno: some
	// component has an annotation chunk (ANTa, ANTz), where metadata lives.
	outline, anno bool
}

// hasText reports whether page n (from 1) may have a text layer.
func (d *djvuInfo) hasText(n int) bool {
	return d.sharedText || (n >= 1 && n <= len(d.pageText) && d.pageText[n-1])
}

// djvuChunk is one chunk header: its id, the absolute offsets of its data
// and of its end (before padding), and for a FORM its secondary id.
type djvuChunk struct {
	id, form   string
	start, end int64 // data
	at         int64 // the header
}

// djvuWalker reads chunk headers inside a file of known size.
type djvuWalker struct {
	r      io.ReaderAt
	size   int64
	chunks int
}

// header reads the chunk header at off, which must end by limit.
func (w *djvuWalker) header(off, limit int64) (djvuChunk, error) {
	w.chunks++
	if w.chunks > maxDjVuChunks {
		return djvuChunk{}, refusedf("the DjVu file has more than %d chunks", maxDjVuChunks)
	}
	var h [12]byte
	if off+8 > limit {
		return djvuChunk{}, errDjVuDamaged
	}
	n := int64(8)
	if off+12 <= limit {
		n = 12
	}
	if _, err := w.r.ReadAt(h[:n], off); err != nil {
		return djvuChunk{}, fmt.Errorf("extract: reading the DjVu file: %w", err)
	}
	c := djvuChunk{id: string(h[:4]), at: off, start: off + 8}
	c.end = c.start + int64(binary.BigEndian.Uint32(h[4:8]))
	if c.end > limit {
		return djvuChunk{}, errDjVuDamaged
	}
	if c.id == "FORM" {
		if c.end-c.start < 4 || n < 12 {
			return djvuChunk{}, errDjVuDamaged
		}
		c.form = string(h[8:12])
		c.start += 4
	}
	return c, nil
}

// errDjVuDamaged is a chunk whose length runs past its parent or the file.
var errDjVuDamaged = errors.New("extract: the DjVu file is damaged: a part of it runs past its end")

// next is the offset after c: IFF pads every chunk to an even length.
func next(c djvuChunk) int64 {
	if c.end%2 == 1 {
		return c.end + 1
	}
	return c.end
}

// scan visits the chunks of [off, end) and, inside FORMs, theirs, calling
// leaf for each chunk that is not a FORM.
func (w *djvuWalker) scan(off, end int64, depth int, leaf func(id string)) error {
	if depth > maxDjVuDepth {
		return refusedf("the DjVu file nests its parts more than %d deep", maxDjVuDepth)
	}
	for off < end {
		if end-off < 8 {
			// A pad byte, or trailing bytes too short to be a chunk.
			return nil
		}
		c, err := w.header(off, end)
		if err != nil {
			return err
		}
		if c.id == "FORM" {
			if err := w.scan(c.start, c.end, depth+1, leaf); err != nil {
				return err
			}
		} else {
			leaf(c.id)
		}
		off = next(c)
	}
	return nil
}

// walkDjVu reads the container of the DjVu file in r.
func walkDjVu(r io.ReaderAt, size int64) (djvuInfo, error) {
	var head [16]byte
	if size < 16 {
		return djvuInfo{}, errNotDjVu
	}
	if _, err := r.ReadAt(head[:], 0); err != nil {
		return djvuInfo{}, fmt.Errorf("extract: reading the DjVu file: %w", err)
	}
	if string(head[:4]) != "AT&T" || string(head[4:8]) != "FORM" {
		return djvuInfo{}, errNotDjVu
	}
	w := &djvuWalker{r: r, size: size}
	top, err := w.header(4, size)
	if err != nil {
		return djvuInfo{}, err
	}
	switch top.form {
	case "DJVU":
		var d djvuInfo
		d.pages, d.pageText = 1, []bool{false}
		err := w.scan(top.start, top.end, 1, func(id string) {
			switch id {
			case "TXTa", "TXTz":
				d.pageText[0] = true
			case "ANTa", "ANTz":
				d.anno = true
			case "NAVM":
				d.outline = true
			}
		})
		return d, err
	case "DJVM":
		return w.bundled(top)
	}
	return djvuInfo{}, errNotDjVu
}

// djvuComponent is one component of a bundled document.
type djvuComponent struct {
	form string
	text bool
}

// bundled reads a multi-page document: its directory (DIRM) and the
// components it names, in the directory's order, which is the page order.
func (w *djvuWalker) bundled(top djvuChunk) (djvuInfo, error) {
	var d djvuInfo
	dirm, err := w.header(top.start, top.end)
	if err != nil {
		return d, err
	}
	if dirm.id != "DIRM" || dirm.end-dirm.start < 3 {
		return d, errors.New("extract: the DjVu document has no directory")
	}
	var dh [3]byte
	if _, err := w.r.ReadAt(dh[:], dirm.start); err != nil {
		return d, fmt.Errorf("extract: reading the DjVu file: %w", err)
	}
	if dh[0]&0x80 == 0 {
		// D11: the pages are separate files, named by the directory. Reading
		// them would let a file name paths to open.
		return d, refusedf("the DjVu document is indirect: its pages are separate files, and only a single-page or bundled DjVu file can be read")
	}
	n := int64(binary.BigEndian.Uint16(dh[1:3]))
	if n == 0 || n > maxDjVuComponents {
		return d, errors.New("extract: the DjVu document's directory names no pages")
	}
	if dirm.end-dirm.start < 3+4*n {
		return d, errDjVuDamaged
	}
	offs := make([]byte, 4*n)
	if _, err := w.r.ReadAt(offs, dirm.start+3); err != nil {
		return d, fmt.Errorf("extract: reading the DjVu file: %w", err)
	}

	comps := map[int64]*djvuComponent{}
	for off := next(dirm); off < top.end; {
		if top.end-off < 8 {
			break
		}
		c, err := w.header(off, top.end)
		if err != nil {
			return d, err
		}
		switch {
		case c.id == "NAVM":
			d.outline = true
		case c.id == "FORM":
			comp := &djvuComponent{form: c.form}
			if err := w.scan(c.start, c.end, 2, func(id string) {
				switch id {
				case "TXTa", "TXTz":
					comp.text = true
				case "ANTa", "ANTz":
					d.anno = true
				}
			}); err != nil {
				return d, err
			}
			comps[c.at] = comp
		}
		off = next(c)
	}
	named := make(map[int64]bool, n)
	for i := range n {
		at := int64(binary.BigEndian.Uint32(offs[4*i:]))
		comp := comps[at]
		if comp == nil || named[at] {
			// A part named twice would count its page twice, and every page
			// number after it would differ from the helper's.
			return d, errors.New("extract: the DjVu document is damaged: its directory names a part that is not there, or one twice")
		}
		named[at] = true
		switch comp.form {
		case "DJVU":
			d.pageText = append(d.pageText, comp.text)
		case "DJVI":
			d.sharedText = d.sharedText || comp.text
		}
	}
	d.pages = len(d.pageText)
	if d.pages == 0 {
		return d, errors.New("extract: the DjVu document has no pages")
	}
	return d, nil
}
