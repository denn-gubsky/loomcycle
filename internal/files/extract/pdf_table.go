package extract

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Tables read from a page's borders (RFC FU P1).
//
// A PDF has no tables, only text placed on a page and lines drawn on it. A
// table exported from Word, LibreOffice or a browser is drawn with its cell
// borders: thin filled boxes or stroked lines, one per edge. Those borders
// give the columns and the rows exactly, so the text inside them can be put
// back into its cells, and a cell's text that wraps onto several lines is
// one cell again. Text with no borders around it is read as before; this
// guesses nothing from the gaps between words.

const (
	// maxPDFPageRules caps the lines one page may draw before tables are no
	// longer looked for on it.
	maxPDFPageRules = 20000
	// maxPDFPagePathPoints caps the path points (every m, l, curve end and
	// re) one page may build, however many times its paths are painted:
	// a page of millions of path operators must not grow memory without
	// bound. Past it no more lines are kept and no tables are looked for.
	maxPDFPagePathPoints = 200_000
	// maxPDFPageGrids caps the grids one page is checked for.
	maxPDFPageGrids = 500
	// ruleThin is how thin a filled box must be, in points, to be a line.
	ruleThin = 2.0
	// ruleTol is how close, in points, two lines must be to be one border.
	ruleTol = 2.0
	// maxPDFTableCols and maxPDFTableRows bound one table's grid.
	maxPDFTableCols = 64
	maxPDFTableRows = 2000
	// maxPDFGridWork bounds the work of joining borders into grids and
	// reading the text inside them: crossings checked, rules sorted onto
	// edges, overlaps compared, runs and cells visited.
	maxPDFGridWork = 4_000_000
)

type pdfPoint struct{ x, y float64 }

// apply maps (x, y) through m.
func (m mat) apply(x, y float64) pdfPoint {
	return pdfPoint{x*m[0] + y*m[2] + m[4], x*m[1] + y*m[3] + m[5]}
}

// pdfRule is a horizontal or vertical line in page space: at pos (x for a
// vertical line, y for a horizontal one), from lo to hi along it.
type pdfRule struct {
	vertical    bool
	pos, lo, hi float64
}

// pdfPath is the path under construction, its points already in page space.
type pdfPath struct {
	subs []pdfSubpath
}

type pdfSubpath struct {
	pts []pdfPoint
	// straight[i] is whether the segment from pts[i] to pts[i+1] is a line
	// rather than a curve.
	straight []bool
	closed   bool
	curved   bool
}

// full charges n path points to the page and reports whether the page is
// past its budget, in which case the point is not kept. The count is the
// page's, not the path's: painting a path frees it, but does not refund it.
func (p *pdfPage) full(n int) bool {
	if p.pathPoints+n > maxPDFPagePathPoints {
		p.rulesOver = true
		return true
	}
	p.pathPoints += n
	return false
}

func (pa *pdfPath) moveTo(p *pdfPage, pt pdfPoint) {
	if p.full(1) {
		return
	}
	pa.subs = append(pa.subs, pdfSubpath{pts: []pdfPoint{pt}})
}

func (pa *pdfPath) to(p *pdfPage, pt pdfPoint, straight bool) {
	if len(pa.subs) == 0 {
		pa.moveTo(p, pt)
		return
	}
	if p.full(1) {
		return
	}
	s := &pa.subs[len(pa.subs)-1]
	s.pts = append(s.pts, pt)
	s.straight = append(s.straight, straight)
	if !straight {
		s.curved = true
	}
}

func (pa *pdfPath) lineTo(p *pdfPage, pt pdfPoint) { pa.to(p, pt, true) }

// curveTo keeps only where a curve ends: a curve is never a border.
func (pa *pdfPath) curveTo(p *pdfPage, pt pdfPoint) { pa.to(p, pt, false) }

func (pa *pdfPath) rect(p *pdfPage, ctm mat, x, y, w, h float64) {
	if p.full(4) {
		return
	}
	pa.subs = append(pa.subs, pdfSubpath{
		pts:      []pdfPoint{ctm.apply(x, y), ctm.apply(x+w, y), ctm.apply(x+w, y+h), ctm.apply(x, y+h)},
		straight: []bool{true, true, true},
		closed:   true,
	})
}

func (pa *pdfPath) close() {
	if n := len(pa.subs); n > 0 && len(pa.subs[n-1].pts) > 1 {
		pa.subs[n-1].closed = true
	}
}

func (pa *pdfPath) reset() {
	pa.subs = nil
}

// paint turns the path into rules as it is filled or stroked, then ends it.
// A filled shape thinner than ruleThin is a line along its long side; a
// stroked straight segment that is horizontal or vertical is a line. Curves,
// slanted lines and larger fills (a shaded cell) are not.
func (pa *pdfPath) paint(p *pdfPage, fill, stroke bool) {
	defer pa.reset()
	for _, s := range pa.subs {
		if len(s.pts) < 2 {
			continue
		}
		if stroke {
			for i := range len(s.pts) - 1 {
				if s.straight[i] {
					p.segmentRule(s.pts[i], s.pts[i+1])
				}
			}
			if s.closed {
				p.segmentRule(s.pts[len(s.pts)-1], s.pts[0])
			}
		}
		if fill && !s.curved {
			minX, maxX, minY, maxY := s.pts[0].x, s.pts[0].x, s.pts[0].y, s.pts[0].y
			for _, pt := range s.pts[1:] {
				minX, maxX = math.Min(minX, pt.x), math.Max(maxX, pt.x)
				minY, maxY = math.Min(minY, pt.y), math.Max(maxY, pt.y)
			}
			w, h := maxX-minX, maxY-minY
			switch {
			case w <= ruleThin && h > ruleThin:
				p.addRule(pdfRule{vertical: true, pos: (minX + maxX) / 2, lo: minY, hi: maxY})
			case h <= ruleThin && w > ruleThin:
				p.addRule(pdfRule{pos: (minY + maxY) / 2, lo: minX, hi: maxX})
			}
		}
	}
}

// segmentRule keeps a stroked segment that is horizontal or vertical.
func (p *pdfPage) segmentRule(a, b pdfPoint) {
	dx, dy := math.Abs(b.x-a.x), math.Abs(b.y-a.y)
	switch {
	case dx <= 0.01*dy && dy > ruleThin:
		p.addRule(pdfRule{vertical: true, pos: (a.x + b.x) / 2, lo: math.Min(a.y, b.y), hi: math.Max(a.y, b.y)})
	case dy <= 0.01*dx && dx > ruleThin:
		p.addRule(pdfRule{pos: (a.y + b.y) / 2, lo: math.Min(a.x, b.x), hi: math.Max(a.x, b.x)})
	}
}

func (p *pdfPage) addRule(r pdfRule) {
	if !finite(r.pos, r.lo, r.hi) {
		return
	}
	if len(p.rules) >= maxPDFPageRules {
		p.rulesOver = true
		return
	}
	p.rules = append(p.rules, r)
}

func finite(fs ...float64) bool {
	for _, f := range fs {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return false
		}
	}
	return true
}

// ---- the page's blocks ----

// pdfBlock is a piece of a page in reading order: text, or a table.
type pdfBlock struct {
	text  string
	table [][]string // rendered cells; the first row is the header
	// aligned marks a table read from text aligned in columns rather than
	// from drawn borders.
	aligned bool
	// nchars counts the text's characters, whitespace excluded.
	nchars int
}

func (b pdfBlock) chars() int { return b.nchars }

func (b pdfBlock) markdown() string {
	if b.table != nil {
		return table(b.table)
	}
	return pdfParagraphs(b.text)
}

func textBlock(t string) pdfBlock {
	return pdfBlock{text: t, nchars: nonSpace(t)}
}

func nonSpace(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// blocks reads the page: its tables, where its borders draw any, and the
// text around them, top to bottom.
func (p *pdfPage) blocks() ([]pdfBlock, bool) {
	if len(p.runs) == 0 {
		return nil, p.cut
	}
	var tables []pdfTable
	if !p.rulesOver && len(p.rules) > 0 {
		var ok bool
		if tables, ok = findTables(p.rules, p.runs, p.e.gridWork); !ok {
			p.rulesOver = true
		}
	}
	// The text outside the borders' tables is read for columns, under one
	// work budget for the page; a page whose lines were too many to read is
	// not, as the report says no tables were looked for there.
	var flowBudget *gridBudget
	if !p.rulesOver {
		work := p.e.gridWork
		if work <= 0 {
			work = maxPDFGridWork
		}
		flowBudget = &gridBudget{left: work}
	}
	flow := func(runs []pdfRun) []pdfBlock {
		if flowBudget == nil || p.denseText {
			if t := assemble(runs); t != "" {
				return []pdfBlock{textBlock(t)}
			}
			return nil
		}
		bs, ok := flowBlocks(runs, flowBudget)
		if !ok {
			p.denseText = true
		}
		return bs
	}
	if len(tables) == 0 {
		if p.rulesOver {
			return []pdfBlock{textBlock(assemble(p.runs))}, p.cut
		}
		return flow(p.runs), p.cut
	}
	sort.SliceStable(tables, func(i, j int) bool { return tables[i].mid > tables[j].mid })
	inTable := make([]bool, len(p.runs))
	for _, t := range tables {
		for _, i := range t.runs {
			inTable[i] = true
		}
	}
	// The text between tables: each run goes before the first table whose
	// middle is below it.
	between := make([][]pdfRun, len(tables)+1)
	for i, r := range p.runs {
		if inTable[i] {
			continue
		}
		cy := runCentreY(r)
		k := sort.Search(len(tables), func(k int) bool { return tables[k].mid <= cy })
		between[k] = append(between[k], r)
	}
	var out []pdfBlock
	for k := range between {
		out = append(out, flow(between[k])...)
		if k < len(tables) {
			out = append(out, pdfBlock{table: tables[k].rows, nchars: tables[k].nchars})
		}
	}
	return out, p.cut
}

func runCentreX(r pdfRun) float64 { return (r.x + math.Max(r.endX, r.x)) / 2 }

// runCentreY is about the middle of a line's lowercase letters.
func runCentreY(r pdfRun) float64 { return r.y + 0.3*r.size }

// ---- grids ----

// pdfTable is one table found on a page.
type pdfTable struct {
	top, bottom, mid float64
	rows             [][]string
	nchars           int
	runs             []int // indexes of the page's runs it holds
}

// gridBudget is the work findTables may still do on one page.
type gridBudget struct{ left int }

// spend charges n units and reports whether the budget still holds.
func (b *gridBudget) spend(n int) bool {
	b.left -= n
	return b.left >= 0
}

// findTables joins the page's rules into grids and reads the text inside
// each. ok is false when the page's lines are too many, or too dense, to
// read within work (maxPDFGridWork when zero): then no table is returned.
func findTables(rules []pdfRule, runs []pdfRun, work int) ([]pdfTable, bool) {
	if work <= 0 {
		work = maxPDFGridWork
	}
	budget := &gridBudget{left: work}
	var hs, vs []pdfRule
	for _, r := range rules {
		if r.vertical {
			vs = append(vs, r)
		} else {
			hs = append(hs, r)
		}
	}
	hs, vs = mergeRules(hs), mergeRules(vs)
	if len(hs) < 3 || len(vs) < 3 {
		return nil, true
	}
	// vs is sorted by position: the vertical rules a horizontal one may
	// cross are a range of it.
	parent := make([]int, len(hs)+len(vs))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for i, h := range hs {
		lo := sort.Search(len(vs), func(k int) bool { return vs[k].pos >= h.lo-ruleTol })
		for k := lo; k < len(vs) && vs[k].pos <= h.hi+ruleTol; k++ {
			if !budget.spend(1) {
				return nil, false
			}
			v := vs[k]
			if h.pos >= v.lo-ruleTol && h.pos <= v.hi+ruleTol {
				parent[find(i)] = find(len(hs) + k)
			}
		}
	}
	comps := map[int]*ruleSet{}
	var order []int
	for i, h := range hs {
		c := find(i)
		if comps[c] == nil {
			comps[c] = &ruleSet{}
			order = append(order, c)
		}
		comps[c].hs = append(comps[c].hs, h)
	}
	for k, v := range vs {
		c := find(len(hs) + k)
		if comps[c] == nil {
			comps[c] = &ruleSet{}
			order = append(order, c)
		}
		comps[c].vs = append(comps[c].vs, v)
	}
	var grids []*pdfGrid
	for _, c := range order {
		g, ok := comps[c].grid(budget)
		if !ok {
			return nil, false
		}
		if g != nil {
			if len(grids) == maxPDFPageGrids {
				return nil, false
			}
			grids = append(grids, g)
		}
	}
	if !budget.spend(len(grids) * len(grids)) {
		return nil, false
	}
	// A grid that overlaps another (a table drawn inside a cell of another)
	// is left to the smaller of the two: the larger one's text stays text.
	var keep []*pdfGrid
	for i, g := range grids {
		outer := false
		for j, o := range grids {
			if i != j && g.overlaps(o) && (g.area() > o.area() || (g.area() == o.area() && i > j)) {
				outer = true
				break
			}
		}
		if !outer {
			keep = append(keep, g)
		}
	}
	// The runs by height, so each grid looks only at the runs level with it.
	if !budget.spend(len(runs)) {
		return nil, false
	}
	byY := make([]int, len(runs))
	for i := range byY {
		byY[i] = i
	}
	sort.Slice(byY, func(a, b int) bool { return runCentreY(runs[byY[a]]) < runCentreY(runs[byY[b]]) })
	var out []pdfTable
	for _, g := range keep {
		lo := sort.Search(len(byY), func(k int) bool { return runCentreY(runs[byY[k]]) > g.bottom() })
		hi := sort.Search(len(byY), func(k int) bool { return runCentreY(runs[byY[k]]) >= g.top() })
		// Each grid pays for the runs level with it and for its cells.
		if !budget.spend(hi - lo + (len(g.xs)-1)*(len(g.ys)-1)) {
			return nil, false
		}
		cand := append([]int(nil), byY[lo:hi]...)
		sort.Ints(cand) // page order, as the runs were drawn
		if t, ok := g.read(runs, cand); ok {
			out = append(out, t)
		}
	}
	return out, true
}

// mergeRules joins rules on one line (within ruleTol) that touch or
// overlap into one, and returns them sorted by position.
func mergeRules(rs []pdfRule) []pdfRule {
	if len(rs) == 0 {
		return nil
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].pos < rs[j].pos })
	var out []pdfRule
	for i := 0; i < len(rs); {
		j := i + 1
		for j < len(rs) && rs[j].pos-rs[i].pos <= ruleTol {
			j++
		}
		group := append([]pdfRule(nil), rs[i:j]...)
		sum := 0.0
		for _, r := range group {
			sum += r.pos
		}
		pos := sum / float64(len(group))
		sort.Slice(group, func(a, b int) bool { return group[a].lo < group[b].lo })
		cur := group[0]
		cur.pos = pos
		for _, r := range group[1:] {
			if r.lo <= cur.hi+ruleTol {
				cur.hi = math.Max(cur.hi, r.hi)
				continue
			}
			out = append(out, cur)
			cur = r
			cur.pos = pos
		}
		out = append(out, cur)
		i = j
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].pos < out[j].pos })
	return out
}

type ruleSet struct {
	hs, vs []pdfRule
}

// pdfGrid is a component of joined rules read as a grid: column edges xs
// (left to right) and row edges ys (top to bottom).
type pdfGrid struct {
	xs, ys []float64
	// vAt[c] are the vertical rules along column edge xs[c], hAt[r] the
	// horizontal ones along row edge ys[r].
	vAt, hAt [][]pdfRule
}

// grid reads the rule set as a grid, or nil when it is not one. ok is false
// when the budget ran out.
func (s *ruleSet) grid(budget *gridBudget) (*pdfGrid, bool) {
	if !budget.spend(len(s.vs) + len(s.hs)) {
		return nil, false
	}
	xs := edges(s.vs)
	ys := edges(s.hs)
	if len(xs) < 3 || len(ys) < 3 || len(xs)-1 > maxPDFTableCols || len(ys)-1 > maxPDFTableRows {
		return nil, true
	}
	// Sorting the rules onto their edges costs a pass over the edges each.
	if !budget.spend(len(s.vs)*len(xs) + len(s.hs)*len(ys)) {
		return nil, false
	}
	// Rows read top to bottom: page y grows upwards.
	for i, j := 0, len(ys)-1; i < j; i, j = i+1, j-1 {
		ys[i], ys[j] = ys[j], ys[i]
	}
	return &pdfGrid{xs: xs, ys: ys, vAt: along(s.vs, xs), hAt: along(s.hs, ys)}, true
}

// along groups rules by the edge (of edgePos) each runs along.
func along(rs []pdfRule, edgePos []float64) [][]pdfRule {
	out := make([][]pdfRule, len(edgePos))
	for _, r := range rs {
		for i, e := range edgePos {
			if math.Abs(r.pos-e) <= ruleTol {
				out[i] = append(out[i], r)
				break
			}
		}
	}
	return out
}

// edges is the distinct positions of rules, clustered within ruleTol.
func edges(rs []pdfRule) []float64 {
	pos := make([]float64, 0, len(rs))
	for _, r := range rs {
		pos = append(pos, r.pos)
	}
	sort.Float64s(pos)
	var out []float64
	for i := 0; i < len(pos); {
		j, sum := i, 0.0
		for j < len(pos) && pos[j]-pos[i] <= ruleTol {
			sum += pos[j]
			j++
		}
		out = append(out, sum/float64(j-i))
		i = j
	}
	return out
}

func (g *pdfGrid) left() float64   { return g.xs[0] }
func (g *pdfGrid) right() float64  { return g.xs[len(g.xs)-1] }
func (g *pdfGrid) top() float64    { return g.ys[0] }
func (g *pdfGrid) bottom() float64 { return g.ys[len(g.ys)-1] }
func (g *pdfGrid) area() float64 {
	return (g.right() - g.left()) * (g.top() - g.bottom())
}

func (g *pdfGrid) overlaps(o *pdfGrid) bool {
	return g.left() < o.right() && o.left() < g.right() && g.bottom() < o.top() && o.bottom() < g.top()
}

func (g *pdfGrid) contains(x, y float64) bool {
	return x > g.left() && x < g.right() && y < g.top() && y > g.bottom()
}

// border reports whether one of the rules along an edge crosses at.
func border(rs []pdfRule, at float64) bool {
	for _, r := range rs {
		if at >= r.lo-ruleTol && at <= r.hi+ruleTol {
			return true
		}
	}
	return false
}

// read places the runs inside the grid into its cells. A cell whose border
// with its neighbour is not drawn is merged with it: its text is in the
// merged cell's first row and column, the others are left empty, so every
// row keeps the grid's column count. ok is false when the grid is not a
// table worth writing as one: text in fewer than two cells (a box around a
// paragraph, a chart's grid), or text inside it drawn sideways.
func (g *pdfGrid) read(runs []pdfRun, cand []int) (pdfTable, bool) {
	nr, nc := len(g.ys)-1, len(g.xs)-1
	type cellRef struct{ r, c int }
	owner := make([][]cellRef, nr)
	for r := range nr {
		owner[r] = make([]cellRef, nc)
		midY := (g.ys[r] + g.ys[r+1]) / 2
		for c := range nc {
			midX := (g.xs[c] + g.xs[c+1]) / 2
			switch {
			case r > 0 && !border(g.hAt[r], midX):
				owner[r][c] = owner[r-1][c]
			case c > 0 && !border(g.vAt[c], midY):
				owner[r][c] = owner[r][c-1]
			default:
				owner[r][c] = cellRef{r, c}
			}
		}
	}
	cellRuns := map[cellRef][]pdfRun{}
	var held []int
	for _, i := range cand {
		run := runs[i]
		cx, cy := runCentreX(run), runCentreY(run)
		if !g.contains(cx, cy) {
			continue
		}
		if !run.upright {
			return pdfTable{}, false
		}
		c := sort.Search(nc, func(k int) bool { return g.xs[k+1] > cx })
		r := sort.Search(nr, func(k int) bool { return g.ys[k+1] < cy })
		if c >= nc || r >= nr {
			continue
		}
		o := owner[r][c]
		cellRuns[o] = append(cellRuns[o], run)
		held = append(held, i)
	}
	if len(cellRuns) < 2 {
		return pdfTable{}, false
	}
	t := pdfTable{top: g.top(), bottom: g.bottom(), mid: (g.top() + g.bottom()) / 2, runs: held}
	rows := make([][]string, nr)
	for r := range nr {
		rows[r] = make([]string, nc)
		for c := range nc {
			if owner[r][c] != (cellRef{r, c}) {
				continue
			}
			text := assemble(cellRuns[cellRef{r, c}])
			t.nchars += nonSpace(text)
			rows[r][c] = cell(text)
		}
	}
	t.rows = dropHairlines(rows, g)
	return t, true
}

// dropHairlines removes rows and columns that hold no text and are no
// wider than two borders drawn side by side (a double border), so they do
// not show as empty cells.
func dropHairlines(rows [][]string, g *pdfGrid) [][]string {
	keepCol := make([]bool, len(g.xs)-1)
	for c := range keepCol {
		keepCol[c] = g.xs[c+1]-g.xs[c] > 2*ruleTol
		for _, row := range rows {
			if strings.TrimSpace(row[c]) != "" {
				keepCol[c] = true
			}
		}
	}
	var out [][]string
	for r, row := range rows {
		empty := true
		for _, s := range row {
			if strings.TrimSpace(s) != "" {
				empty = false
			}
		}
		if empty && g.ys[r]-g.ys[r+1] <= 2*ruleTol {
			continue
		}
		var kept []string
		for c, s := range row {
			if keepCol[c] {
				kept = append(kept, s)
			}
		}
		out = append(out, kept)
	}
	return out
}
