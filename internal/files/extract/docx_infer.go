package extract

import (
	"fmt"
	"strconv"
	"strings"
)

// Heading recovery for DOCX (Options.InferHeadings). A run's formatting is
// the document defaults, then its paragraph's style (or the default
// paragraph style), then its character style, then its own w:rPr: each
// layer overrides the one before it, a style after the styles it is based
// on. Word's toggle semantics for bold across style layers are not
// modelled: a later layer's w:b wins.

// docxRPr is the formatting a w:rPr sets; nil and 0 leave a property to
// the layer below.
type docxRPr struct {
	bold  *bool
	size  float64 // points
	color *string
}

func (a docxRPr) over(b docxRPr) docxRPr {
	if b.bold != nil {
		a.bold = b.bold
	}
	if b.size > 0 {
		a.size = b.size
	}
	if b.color != nil {
		a.color = b.color
	}
	return a
}

func (a docxRPr) fmt() runFmt {
	f := runFmt{size: a.size}
	if a.bold != nil {
		f.bold = *a.bold
	}
	if a.color != nil {
		f.color = normalizeColor(*a.color)
	}
	return f
}

// parseRPr reads bold (w:b), size (w:sz, in half-points) and colour
// (w:color) from a w:rPr.
func parseRPr(n *node) docxRPr {
	var r docxRPr
	if n == nil {
		return r
	}
	if b := n.child("b"); b != nil {
		v := b.attr("val") == "" || onOff(b.attr("val"))
		r.bold = &v
	}
	if sz := n.child("sz"); sz != nil {
		// Word caps a font at 1638pt; anything past it is not a size.
		if hp, err := strconv.ParseFloat(strings.TrimSpace(sz.attr("val")), 64); err == nil && hp > 0 && hp <= 3276 {
			r.size = hp / 2
		}
	}
	if c := n.child("color"); c != nil {
		v := c.attr("val")
		r.color = &v
	}
	return r
}

// onOff reads an ST_OnOff value.
func onOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on":
		return true
	}
	return false
}

// styleRPr is the formatting a style sets, the styles it is based on first.
func (d *docx) styleRPr(id string) docxRPr {
	var chain []docxRPr
	for range 16 { // basedOn chains are short; a cycle must not loop
		st, ok := d.styles[id]
		if !ok {
			break
		}
		chain = append(chain, st.rpr)
		if st.basedOn == "" {
			break
		}
		id = st.basedOn
	}
	var r docxRPr
	for i := len(chain) - 1; i >= 0; i-- {
		r = r.over(chain[i])
	}
	return r
}

// infer decides which paragraphs of the body are headings and which
// pictures are decoration, before anything is written: both depend on the
// whole document.
func (d *docx) infer(body *node) {
	var nodes []*node
	var paras []inferPara
	d.collect(body, false, &nodes, &paras, 0)
	res := inferLevels(paras)
	d.inferred = map[*node]int{}
	for i, lv := range res.levels {
		if lv > 0 {
			d.inferred[nodes[i]] = lv
		}
	}
	d.rep.InferredHeadings = res.headings
	res.note(d.rep)
	d.decor = decorative{}
	for _, b := range body.all("blip") {
		d.decor[d.imageKey(b.attrRel("embed"))]++
	}
	for _, v := range body.all("imagedata") {
		d.decor[d.imageKey(v.attrRel("id"))]++
	}
}

// collect lists the paragraphs blocks renders, in its order: those of
// tables and lists are body text but never headings.
func (d *docx) collect(parent *node, inTable bool, nodes *[]*node, paras *[]inferPara, depth int) {
	if depth > maxXMLDepth {
		return
	}
	for _, k := range parent.kids {
		switch k.name {
		case "p":
			if k == d.titlePara {
				continue
			}
			p := inferPara{runs: d.paraRuns(k), eligible: !inTable && !docxListItem(k)}
			switch lv := d.paraLevel(k); {
			case lv > 0:
				p.real = lv
			case lv == levelTitle:
				p.real = 1 // a second Title paragraph is written as a Heading 1
			}
			*nodes = append(*nodes, k)
			*paras = append(*paras, p)
		case "tbl":
			for _, p := range k.outermost("p") {
				*nodes = append(*nodes, p)
				*paras = append(*paras, inferPara{runs: d.paraRuns(p)})
			}
		case "sdt":
			if c := k.child("sdtContent"); c != nil {
				d.collect(c, inTable, nodes, paras, depth+1)
			}
		case "customXml", "ins", "smartTag":
			d.collect(k, inTable, nodes, paras, depth+1)
		}
	}
}

// docxListItem reports a paragraph para renders as a list item.
func docxListItem(p *node) bool {
	np := p.path("pPr", "numPr")
	if np == nil {
		return false
	}
	id := np.child("numId").attr("val")
	return id != "" && id != "0"
}

// paraRuns is a paragraph's text, run by run, with each run's formatting.
// Deleted text, field codes, pictures and text boxes are left out, as they
// are not the line a reader sees as the paragraph.
func (d *docx) paraRuns(p *node) []fmtRun {
	pstyle := p.path("pPr", "pStyle").attr("val")
	if pstyle == "" {
		pstyle = d.defaultPara
	}
	base := d.defaults.over(d.styleRPr(pstyle))
	var out []fmtRun
	var walk func(*node, int)
	walk = func(n *node, depth int) {
		if depth > maxXMLDepth {
			return
		}
		for _, k := range n.kids {
			switch k.name {
			case "r":
				rpr := k.child("rPr")
				f := base.over(d.styleRPr(rpr.child("rStyle").attr("val"))).over(parseRPr(rpr)).fmt()
				var b strings.Builder
				for _, t := range k.kids {
					switch t.name {
					case "t":
						b.WriteString(t.text)
					case "tab", "ptab":
						b.WriteString("\t")
					case "br", "cr":
						b.WriteString("\n")
					case "noBreakHyphen":
						b.WriteString("-")
					}
				}
				if b.Len() > 0 {
					out = append(out, fmtRun{text: b.String(), fmt: f})
				}
			case "del", "pPr", "rPr", "drawing", "pict", "object", "txbxContent", "fldData", "instrText", "delText":
			default:
				walk(k, depth+1)
			}
		}
	}
	walk(p, 0)
	return out
}

// imageKey names the picture an image relationship points to: its bytes'
// CRC-32 and size as the archive lists them, so one picture stored as
// several parts is still one picture. An archive without CRCs (a stored
// repack) is keyed by part name. "" for a relationship that is not an
// image in the archive.
func (d *docx) imageKey(id string) string {
	rl, ok := d.rels[id]
	if id == "" || !ok || rl.External || !relType(rl.Type, "image") {
		return ""
	}
	f, ok := d.a.lookup(rl.Target)
	if !ok {
		return ""
	}
	if f.CRC32 == 0 {
		return "part:" + f.Name
	}
	return fmt.Sprintf("%08x:%d", f.CRC32, f.UncompressedSize64)
}
