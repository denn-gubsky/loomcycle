package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"strings"
	"time"
	"unicode/utf8"
)

// RFC GR P4d (D23–D25): a Mermaid diagram is drawn as a picture by a helper
// program, before the Doc is bundled, so the render log's hash covers the
// pictures and every writer sees an ordinary picture. A diagram the helper
// cannot draw, or whose picture is not one a file may carry, stays its
// source, shown as code (D10), as it always was.
//
// Every format takes the drawing as a PNG at twice its size, shown at its
// own (Density 2). The owner ruled this over D24's SVG on 2026-11-18: the
// renderer fits an SVG's labels to its own font metrics, so a reader with
// other fonts (Typst, a browser, an e-reader) clips them, while the PNG is
// laid out by the renderer itself.

const (
	// MaxDiagrams is how many diagrams one render draws; the rest stay
	// code (D25).
	MaxDiagrams = 200
	// DiagramTimeout bounds drawing one diagram (D25).
	DiagramTimeout = 10 * time.Second
	// DiagramsTimeout bounds drawing all of one render's diagrams: past it,
	// the rest stay code.
	DiagramsTimeout = 2 * time.Minute
	// maxDiagramTimeouts is how many diagrams may run out of time before a
	// render stops asking: a renderer that hangs is not asked 200 times.
	maxDiagramTimeouts = 2
	// MaxDiagramSource is the longest diagram source drawn; a longer one
	// stays code.
	MaxDiagramSource = 64 << 10
	// MaxDiagramEdge is the longest side, in pixels, of a drawn diagram
	// (D25).
	MaxDiagramEdge = 4096
	// maxDiagramAlt caps the source kept as a drawn diagram's alt text.
	maxDiagramAlt = 2000
)

// ErrNoDrawer is a draw function's answer when nothing here can draw a
// diagram: no helper is configured, or no worker carries one. Drawing stops
// and every diagram stays code.
var ErrNoDrawer = errors.New("render: no diagram renderer")

// DrawFunc draws a Mermaid diagram's source as a PNG at twice its size.
type DrawFunc func(ctx context.Context, src string) ([]byte, error)

// Draws reports whether a render of format draws its diagrams: every
// format but Markdown, whose diagrams are its round-trippable source.
func Draws(format string) bool { return format != FormatMarkdown }

// DrawDiagrams replaces each Mermaid diagram of d with its drawing, counting
// in rep the diagrams drawn and those left as code. An error is only the
// caller's context ending: a diagram that does not draw is left as code,
// never a refusal.
func DrawDiagrams(ctx context.Context, d *Doc, format string, draw DrawFunc, rep *Report) error {
	if !Draws(format) || draw == nil {
		return nil
	}
	pass, cancel := context.WithTimeout(ctx, DiagramsTimeout)
	defer cancel()
	dr := &drawer{ctx: pass, d: d, draw: draw, rep: rep, done: map[string]string{}}
	for _, im := range d.Images {
		dr.bytes += len(im.Data)
	}
	dr.blocks(d.Blocks)
	var walk func(ss []*Section)
	walk = func(ss []*Section) {
		for _, s := range ss {
			if s != nil {
				dr.blocks(s.Blocks)
				walk(s.Children)
			}
		}
	}
	walk(d.Sections)
	if err := ctx.Err(); err != nil {
		return err
	}
	if rep.DiagramsLeftAsCode > 0 {
		note := fmt.Sprintf("%d diagrams could not be drawn and are shown as code", rep.DiagramsLeftAsCode)
		if dr.noDrawer {
			note = "this server has no diagram renderer: diagrams are shown as code"
		}
		rep.Notes = append(rep.Notes, note)
	}
	return nil
}

type drawer struct {
	ctx      context.Context
	d        *Doc
	draw     DrawFunc
	rep      *Report
	bytes    int
	tried    int
	timeouts int
	noDrawer bool
	// done maps a diagram's source to its picture, so a diagram repeated
	// is drawn once and held once.
	done map[string]string
}

func (dr *drawer) blocks(ns []Node) {
	for i := range ns {
		n := &ns[i]
		if n.Kind == KindDiagram && strings.EqualFold(n.Lang, "mermaid") {
			if name, ok := dr.picture(n.Text); ok {
				*n = Node{Kind: KindImage, Asset: name, Alt: diagramAlt(n.Text)}
				dr.rep.DiagramsDrawn++
			} else {
				dr.rep.DiagramsLeftAsCode++
			}
			continue
		}
		dr.blocks(n.Children)
	}
}

// picture is the drawing of src as a picture of the Doc, by name; ok false
// leaves the diagram as code.
func (dr *drawer) picture(src string) (string, bool) {
	if name, ok := dr.done[src]; ok {
		return name, name != ""
	}
	if dr.noDrawer || dr.ctx.Err() != nil || dr.tried >= MaxDiagrams || dr.timeouts >= maxDiagramTimeouts ||
		len(dr.d.Images) >= MaxImages || len(src) > MaxDiagramSource {
		return "", false
	}
	dr.tried++
	ctx, cancel := context.WithTimeout(dr.ctx, DiagramTimeout)
	data, err := dr.draw(ctx, src)
	// Its own time ran out (not the render's): the helper's answer may or
	// may not say so, so the context is asked too.
	timedOut := dr.ctx.Err() == nil && (ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded))
	cancel()
	switch {
	case errors.Is(err, ErrNoDrawer):
		dr.noDrawer = true
		return "", false
	case timedOut:
		dr.timeouts++
		dr.done[src] = ""
		return "", false
	case err != nil || !diagramPictureOK(data) || dr.bytes+len(data) > MaxImageBytes:
		dr.done[src] = ""
		return "", false
	}
	name := fmt.Sprintf("img-%d.png", len(dr.d.Images)+1)
	if _, taken := dr.d.image(name); taken {
		return "", false
	}
	dr.d.Images = append(dr.d.Images, Image{Name: name, MediaType: "image/png", Density: 2, Data: data})
	dr.bytes += len(data)
	dr.rep.Images++
	dr.done[src] = name
	return name, true
}

// diagramPictureOK holds a drawing to D25: a PNG that decodes whole, at
// most MaxImage bytes and MaxDiagramEdge pixels a side.
func diagramPictureOK(data []byte) bool {
	if len(data) == 0 || len(data) > MaxImage {
		return false
	}
	cfg, kind, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || kind != "png" || cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width > MaxDiagramEdge || cfg.Height > MaxDiagramEdge {
		return false
	}
	_, _, err = image.Decode(bytes.NewReader(data))
	return err == nil
}

// diagramAlt is a drawn diagram's alt text: its source on one line, capped.
func diagramAlt(src string) string {
	a := oneLine(src)
	if utf8.RuneCountInString(a) > maxDiagramAlt {
		r := []rune(a)
		a = string(r[:maxDiagramAlt]) + "…"
	}
	return a
}
