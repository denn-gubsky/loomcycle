package extract

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// PPTX reads a presentation: its slides in the order presentation.xml lists
// them, each a "## Slide N — <title>" section with the slide's text, tables
// and pictures in shape order and its speaker notes as a blockquote.
type PPTX struct{}

const pptxType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"

func (PPTX) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{pptxType, "application/vnd.ms-powerpoint.presentation.macroenabled.12"}, []string{".pptx", ".pptm"})
}

func (PPTX) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "pptx"}
	a, err := openZip(ctx, r, size, opts.limits)
	if err != nil {
		return rep, err
	}
	pkg, err := a.rels("")
	if err != nil {
		return rep, err
	}
	main := "ppt/presentation.xml"
	for _, rl := range pkg {
		if relType(rl.Type, "officeDocument") && !rl.External {
			main = rl.Target
		}
	}
	pres, err := a.readXML(main)
	if err != nil {
		return rep, err
	}
	rels, err := a.rels(main)
	if err != nil {
		return rep, err
	}
	w := newWriter(ctx, sink, opts)
	docTitle, _ := packageProps(a, pkg)
	if docTitle == "" {
		docTitle = title(opts.Name, "Presentation")
	}
	if err := w.text(heading(1, docTitle)); err != nil {
		return rep, err
	}
	for _, id := range pres.path("presentation", "sldIdLst").all("sldId") {
		rl, ok := rels[id.attrRel("id")]
		if !ok || rl.External || !relType(rl.Type, "slide") {
			continue
		}
		rep.Slides++
		md, err := pptxSlide(a, w, &rep, rl.Target, rep.Slides)
		if err != nil {
			rep.finish(w)
			return rep, err
		}
		if err := w.text("\n\n" + md); err != nil {
			rep.finish(w)
			return rep, err
		}
	}
	rep.finish(w)
	return rep, nil
}

func pptxSlide(a *archive, w *writer, rep *Report, part string, n int) (string, error) {
	slide, err := a.readXML(part)
	if err != nil {
		return "", err
	}
	rels, err := a.rels(part)
	if err != nil {
		return "", err
	}
	s := &slideState{a: a, w: w, rep: rep, rels: rels}
	if tree := slide.path("sld", "cSld", "spTree"); tree != nil {
		if err := s.shapes(tree); err != nil {
			return "", err
		}
	}
	head := fmt.Sprintf("Slide %d", n)
	if t := oneLine(s.title); t != "" {
		head += " — " + t
	}
	parts := []string{heading(2, head)}
	parts = append(parts, s.blocks...)
	for _, rl := range rels {
		if rl.External || !relType(rl.Type, "notesSlide") || !a.has(rl.Target) {
			continue
		}
		notes, err := a.readXML(rl.Target)
		if err != nil {
			if IsRefused(err) || w.ctx.Err() != nil {
				return "", err
			}
			continue
		}
		if q := notesQuote(notes); q != "" {
			parts = append(parts, q)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

type slideState struct {
	a      *archive
	w      *writer
	rep    *Report
	rels   map[string]rel
	title  string
	blocks []string
}

// shapes walks a shape tree in order: text shapes, pictures, tables
// (graphic frames) and groups.
func (s *slideState) shapes(tree *node) error {
	for _, k := range tree.kids {
		if err := s.w.ctx.Err(); err != nil {
			return err
		}
		switch k.name {
		case "sp":
			ph := k.path("nvSpPr", "nvPr", "ph")
			typ := ph.attr("type")
			paras := shapeParagraphs(k.child("txBody"))
			if ph != nil && (typ == "title" || typ == "ctrTitle") && s.title == "" {
				s.title = strings.Join(paras, " ")
				continue
			}
			for _, p := range paras {
				if p := paragraph(p); p != "" {
					s.blocks = append(s.blocks, p)
				}
			}
		case "pic":
			alt := k.path("nvPicPr", "cNvPr").attr("descr")
			if alt == "" {
				alt = k.path("nvPicPr", "cNvPr").attr("title")
			}
			for _, b := range k.all("blip") {
				md, err := relImage(s.a, s.w, s.rels, b.attrRel("embed"), alt)
				if err != nil {
					return err
				}
				if md != "" {
					s.blocks = append(s.blocks, md)
				}
			}
		case "graphicFrame":
			for _, tbl := range k.all("tbl") {
				var rows [][]string
				for _, tr := range tbl.kids {
					if tr.name != "tr" {
						continue
					}
					var row []string
					for _, tc := range tr.kids {
						if tc.name == "tc" {
							row = append(row, cell(strings.Join(shapeParagraphs(tc.child("txBody")), " ")))
						}
					}
					rows = append(rows, row)
				}
				if len(rows) > 0 {
					s.rep.Tables++
					s.blocks = append(s.blocks, table(rows))
				}
			}
		case "grpSp":
			if err := s.shapes(k); err != nil {
				return err
			}
		}
	}
	return nil
}

// shapeParagraphs returns the text of each non-empty a:p of a text body.
func shapeParagraphs(body *node) []string {
	if body == nil {
		return nil
	}
	var out []string
	for _, p := range body.kids {
		if p.name != "p" {
			continue
		}
		if t := strings.TrimSpace(plainText(p)); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// notesQuote renders a notes slide's body placeholder as a blockquote; the
// slide image and number placeholders on a notes page are not notes.
func notesQuote(notes *node) string {
	tree := notes.path("notes", "cSld", "spTree")
	if tree == nil {
		return ""
	}
	var lines []string
	for _, sp := range tree.all("sp") {
		if sp.path("nvSpPr", "nvPr", "ph").attr("type") != "body" {
			continue
		}
		for _, p := range shapeParagraphs(sp.child("txBody")) {
			for _, l := range strings.Split(paragraph(p), "\n") {
				lines = append(lines, "> "+l)
			}
		}
	}
	return strings.Join(lines, "\n")
}
