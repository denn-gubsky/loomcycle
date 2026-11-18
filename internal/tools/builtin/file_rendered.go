package builtin

import (
	"encoding/json"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/files/render"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// RFC GR P4b (D22): a file loomcycle rendered with metadata embed comes back.
// op=extract finds the record the file carries and the anchor on each of its
// headings, and keeps on the extracted text's inbox record which document it
// was and which chunk each heading line is. op=import with reimport then goes
// onto that document, its parts matched by those ids first; without reimport
// it makes a new document that says what it was rendered from.
//
// The record is someone's file, as untrusted as its text: it only names a
// document. The import still asks for write on it, in its scope, and an id
// that is not one of its parts matches nothing.

// headingAnchorAt is a heading anchor the extractor named, and how much text
// had been written when it did: the heading is the first one at or after.
type headingAnchorAt struct {
	offset int
	name   string
}

// Record and HeadingAnchor make extractSink an extract.RecordSink.
func (s *extractSink) Record(b []byte) { s.record = append([]byte(nil), b...) }

func (s *extractSink) HeadingAnchor(name string) {
	if len(s.anchors) < render.MaxSections {
		s.anchors = append(s.anchors, headingAnchorAt{offset: s.md.Len(), name: name})
	}
}

// rendered is what the extraction found the file was rendered from, nil
// when it carried no record or one that is not loomcycle's shape. md is the
// extracted text as kept, whose heading lines the anchors are mapped to.
func (s *extractSink) rendered(md string) *inbox.Rendered {
	if s.record == nil {
		return nil
	}
	e, err := render.ParseEmbed(s.record)
	if err != nil {
		return nil
	}
	byAnchor := make(map[string]string, 2*len(e.Chunks))
	for _, c := range e.Chunks {
		byAnchor[c.Anchor] = c.ID
		byAnchor[render.AnchorBookmark(c.Anchor)] = c.ID
	}
	r := &inbox.Rendered{DocumentID: e.DocumentID, Scope: e.Scope, Title: e.Title}
	starts := headingLineStarts(md)
	next := 0
	for _, a := range s.anchors {
		id := byAnchor[a.name]
		if id == "" {
			continue
		}
		for next < len(starts) && starts[next].offset < a.offset {
			next++
		}
		if next == len(starts) {
			break
		}
		if r.Headings == nil {
			r.Headings = map[int]string{}
		}
		r.Headings[starts[next].line] = id
		next++
	}
	return r
}

// headingLine is an ATX heading's line (counted from 1) and the offset its
// text starts at.
type headingLine struct{ line, offset int }

// headingLineStarts lists md's ATX headings outside code fences, as
// import_md reads them.
func headingLineStarts(md string) []headingLine {
	var out []headingLine
	var fence byte
	fenceRun, offset := 0, 0
	for i, l := range strings.SplitAfter(md, "\n") {
		line := strings.TrimRight(l, "\r\n")
		if c, run := mdFenceDelim(line); run > 0 {
			switch {
			case fenceRun == 0:
				fence, fenceRun = c, run
			case c == fence && run >= fenceRun:
				fenceRun = 0
			}
		} else if fenceRun == 0 && isATXHeadingLine(line) {
			out = append(out, headingLine{line: i + 1, offset: offset})
		}
		offset += len(l)
	}
	return out
}

// withChunkIDs gives each heading line the record mapped a chunk to the
// metadata comment import_md reads a part's id from. It runs on text already
// neutralised, so these are the only such comments the text holds, and only
// the runtime wrote them, from the extraction's own record.
func withChunkIDs(md string, headings map[int]string) string {
	if len(headings) == 0 {
		return md
	}
	lines := strings.Split(md, "\n")
	var b strings.Builder
	b.Grow(len(md) + 64*len(headings))
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
		if id := headings[i+1]; id != "" && isATXHeadingLine(l) {
			meta, _ := json.Marshal(map[string]string{"id": id})
			b.WriteString("\n<!-- loom: " + string(meta) + " -->")
		}
	}
	return b.String()
}
