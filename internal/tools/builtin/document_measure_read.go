package builtin

// document_measure_read.go — reading the taxonomy's built-in series (RFC GM 6A.8,
// 6A.9, plan P2.2): Document op=measure, and the latest count, share and trend
// describe_ontology with_stats serves beside each node (D31: beside the node, never
// written into a summary).
//
// ACCESS (RFC GM C8, replacing D8 (i)). A stored series cannot be computed per
// caller, so it is computed from what every member of the tenant may read
// (document_measure.go) and served the same to every reader of the ontology. Rows
// stored before that rule are never read (loadMeasurePoints), shown_at included.
// Nor is a row whose members are no longer its point's current unrestricted members
// (seriesPlan.servable), shown_at included: a document restricted after a pass
// stops counting at once, not at the next pass.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	measureOp = "measure"
	// compareLogRatioCap bounds a comparison's log-ratio (RFC GM 6.3, 6A.3): e^3 is
	// a twenty-fold change, past which "it grew a lot" says all there is.
	compareLogRatioCap = 3.0
)

// measureRange is a range of periods by their start: from inclusive, to exclusive
// (unix nanos); an open end is unbounded.
type measureRange struct {
	From *int64 `json:"from,omitempty"`
	To   *int64 `json:"to,omitempty"`
}

func (r measureRange) holds(start int64) bool {
	return (r.From == nil || start >= *r.From) && (r.To == nil || start < *r.To)
}

// measureTarget is one node a read answers for.
type measureTarget struct {
	id, kind, name, tag, scheme, period string
	// retired: no node of the ontology in force has this id; its rows are read as
	// stored (RFC GM 6A.7).
	retired bool
}

// measurePoint is one period of a node's series.
type measurePoint struct {
	PeriodStart int64   `json:"period_start"`
	Label       string  `json:"label"`
	Value       float64 `json:"value"`
	Members     int     `json:"members"`
	// Basis (share, unplaced) is the number of things of the scheme's projected
	// classes dated in the period; Share (unplaced) is Value over it.
	Basis         *int     `json:"basis,omitempty"`
	Share         *float64 `json:"share,omitempty"`
	FallbackShare float64  `json:"fallback_share"`
	ComputedAt    int64    `json:"computed_at"`
}

// measureNode is one node's series.
type measureNode struct {
	NodeID  string         `json:"node_id"`
	Kind    string         `json:"kind,omitempty"`
	Name    string         `json:"name,omitempty"`
	Tag     string         `json:"tag,omitempty"`
	Scheme  string         `json:"scheme,omitempty"`
	Period  string         `json:"period,omitempty"`
	Retired bool           `json:"retired,omitempty"`
	Points  []measurePoint `json:"points"`
}

// measureCompared is one node of a comparison.
type measureCompared struct {
	NodeID        string   `json:"node_id"`
	Name          string   `json:"name"`
	Tag           string   `json:"tag,omitempty"`
	Scheme        string   `json:"scheme"`
	Count         int      `json:"count"`
	Basis         int      `json:"basis"`
	Share         *float64 `json:"share,omitempty"`
	BaselineCount int      `json:"baseline_count"`
	BaselineBasis int      `json:"baseline_basis"`
	BaselineShare *float64 `json:"baseline_share,omitempty"`
	LogRatio      float64  `json:"log_ratio"`
}

// measureComparison is compare's answer: the nodes ordered by how far their share
// moved from the baseline range to the main one.
type measureComparison struct {
	Range    measureRange      `json:"range"`
	Baseline measureRange      `json:"baseline"`
	Nodes    []measureCompared `json:"nodes"`
}

// measureAnswer is op=measure's answer.
type measureAnswer struct {
	Measure string `json:"measure"`
	Scheme  string `json:"scheme,omitempty"`
	From    *int64 `json:"from,omitempty"`
	To      *int64 `json:"to,omitempty"`
	ShownAt *int64 `json:"shown_at,omitempty"`
	Note    string `json:"note,omitempty"`
	// Recomputing: some stored points were left out because their point's members
	// changed since they were computed (seriesPlan.servable).
	Recomputing string             `json:"recomputing,omitempty"`
	Nodes       []measureNode      `json:"nodes"`
	Compare     *measureComparison `json:"compare,omitempty"`
}

// emptyPoint: a row that says the point holds no value now.
func emptyPoint(s storedPoint) bool { return s.members == 0 && s.basis == 0 }

// measure is Document op=measure: a built-in series per node and period, or with
// compare the nodes ordered by share change. Always the tenant scope.
func (d *Document) measure(ctx context.Context, in docInput) (tools.Result, error) {
	if res := checkMeasureInput(in); res != nil {
		return *res, nil
	}
	key, _, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return docFail(measureOp, err), nil
	}
	if _, _, err := d.resolveScope(ctx, "tenant"); err != nil {
		return errPermission(measureOp+": permission: the series are kept in the tenant scope, which this caller is not granted",
			"Ask an operator to grant the tenant scope in memory_scopes and sql_scopes."), nil
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		return errBusiness(measureOp+": the tenant ontology could not be read", "Retry the call; a store fault is usually transient."), nil
	}
	acl := d.newDocACL(ctx, key)
	if read.DocumentID != "" && acl != nil {
		// A node is a part of the ontology document: asked as describe_ontology asks.
		if r, err := acl.document(ctx, measureOp, "read", read.DocumentID); err != nil {
			return errFrom(measureOp+": "+err.Error(), err), nil
		} else if r != nil {
			return *r, nil
		}
	}
	if in.Scheme != "" && !schemeInForce(read, in.Scheme) {
		return errValidation(fmt.Sprintf("%s: no scheme %q is in force", measureOp, in.Scheme),
			"Name a scheme from describe_ontology's schemes, or omit scheme."), nil
	}
	targets, res := d.measureTargets(ctx, key, read, in)
	if res != nil {
		return *res, nil
	}
	ans := measureAnswer{Measure: in.Measure, Scheme: in.Scheme, From: in.From, To: in.To, ShownAt: in.ShownAt, Nodes: []measureNode{}}
	if in.Measure == measureShift {
		if note := d.nodeStatsVectorsNote(); note != "" {
			ans.Note = "shift is kept only where the store keeps vectors and an embedder is configured; here " + note
		}
	}
	var shownAt int64
	if in.ShownAt != nil {
		shownAt = *in.ShownAt
	}
	sp, err := d.currentSeriesPlan(ctx, key, read)
	if err != nil {
		return errFrom(measureOp+": "+err.Error(), err), nil
	}
	if in.Compare != nil {
		cmp, dropped, err := d.measureCompare(ctx, key, read, sp, targets, shownAt, measureRange{From: in.From, To: in.To}, *in.Compare)
		if err != nil {
			return errFrom(measureOp+": "+err.Error(), err), nil
		}
		if dropped > 0 {
			ans.Recomputing = measureRecomputing
		}
		ans.Compare = cmp
		return jsonResult(ans)
	}
	ids := make([]string, len(targets))
	for i, t := range targets {
		ids[i] = t.id
	}
	stored, err := d.loadMeasurePointsBatched(ctx, key, shownAt, []string{in.Measure}, ids)
	if err != nil {
		return errFrom(measureOp+": "+err.Error(), err), nil
	}
	if sp.servable(stored) > 0 {
		ans.Recomputing = measureRecomputing
	}
	rng := measureRange{From: in.From, To: in.To}
	for _, t := range targets {
		ans.Nodes = append(ans.Nodes, seriesOf(t, in.Measure, stored, rng))
	}
	return jsonResult(ans)
}

// measureRecomputing is measureAnswer.Recomputing's text: one for every cause, so
// it does not say which.
const measureRecomputing = "some points are left out: their members changed since they were computed; a node_stats pass computes them again"

// currentSeriesPlan is the series plan of the tenant's current state, every scheme,
// which a read checks stored points against (servable).
func (d *Document) currentSeriesPlan(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead) (*seriesPlan, error) {
	p, err := d.buildNodeStatsPlan(ctx, key, read, "")
	if err != nil {
		return nil, err
	}
	return d.buildSeriesPlan(ctx, key, read, "", p)
}

// checkMeasureInput refuses what measure cannot answer, before anything is read.
func checkMeasureInput(in docInput) *tools.Result {
	fail := func(msg, hint string) *tools.Result {
		r := errValidation(measureOp+": "+msg, hint)
		return &r
	}
	switch in.Measure {
	case measureCount, measureShare, measureShift, measureUnplaced:
	case "":
		return fail("measure is required", "Pass measure: count, share, shift or unplaced.")
	default:
		return fail(fmt.Sprintf("measure %q is not a built-in series", in.Measure), "Pass measure: count, share, shift or unplaced.")
	}
	if in.Measure == measureUnplaced && in.Node != "" {
		return fail("unplaced is kept per scheme, not per node", "Name the scheme with scheme, or omit it for every scheme.")
	}
	if in.Compare != nil {
		if in.Measure != measureShare {
			return fail("compare orders nodes by share change; it takes measure share", "Pass measure: share with compare.")
		}
		if in.Compare.From == nil || in.Compare.To == nil {
			return fail("compare needs from and to: the baseline range", "Pass compare: {from, to} (unix nanoseconds).")
		}
	}
	for _, r := range []measureRange{{From: in.From, To: in.To}, derefRange(in.Compare)} {
		if r.From != nil && r.To != nil && *r.To <= *r.From {
			return fail("a range's to must be after its from", "Pass from before to; to is exclusive.")
		}
	}
	return nil
}

func derefRange(r *measureRange) measureRange {
	if r == nil {
		return measureRange{}
	}
	return *r
}

// measureTargets is the nodes a read answers for: node's (and, with compare, its
// narrower concepts), else every node the measure is kept for, narrowed to scheme.
func (d *Document) measureTargets(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, in docInput) ([]measureTarget, *tools.Result) {
	var concepts, classes, schemes []measureTarget
	if read.Confirmed {
		for _, s := range read.Schemes {
			if in.Scheme != "" && s.Name != in.Scheme {
				continue
			}
			per := schemePeriod(s)
			schemes = append(schemes, measureTarget{id: s.ChunkID, kind: "scheme", name: s.Name, scheme: s.Name, period: per})
			for _, c := range s.Concepts {
				if c.ChunkID != "" && c.Tag != "" {
					concepts = append(concepts, measureTarget{id: c.ChunkID, kind: nodeKindConcept, name: c.Name, tag: c.Tag, scheme: s.Name, period: per})
				}
			}
		}
	}
	if in.Scheme == "" && in.Measure != measureShare && in.Measure != measureUnplaced {
		for _, c := range describeOntologyRead(read).Classes {
			if c.Kind == "document" {
				classes = append(classes, measureTarget{id: c.Name, kind: nodeKindClass, name: c.Name, period: memrank.SchemePeriodMonth})
			}
		}
	}
	switch {
	case in.Measure == measureUnplaced:
		return schemes, nil
	case in.Node == "":
		return append(concepts, classes...), nil
	}
	for _, t := range append(append([]measureTarget{}, concepts...), classes...) {
		if t.id == in.Node || (t.tag != "" && t.tag == in.Node) {
			if in.Compare == nil || t.kind != nodeKindConcept {
				return []measureTarget{t}, nil
			}
			out := []measureTarget{t}
			for _, c := range concepts {
				if strings.HasPrefix(c.tag, t.tag+"/") {
					out = append(out, c)
				}
			}
			return out, nil
		}
	}
	// In force, but not in the scheme named: refused, not read as a retired node
	// (whose rows of every period length would be mixed).
	if in.Scheme != "" && nodeInForce(read, in.Node) {
		r := errValidation(fmt.Sprintf("%s: node %q is not in scheme %q", measureOp, in.Node, in.Scheme),
			"Name the node's own scheme, or omit scheme (a document class has no scheme).")
		return nil, &r
	}
	// No node in force by that name: a retired node's rows stay readable by its id.
	if in.Compare == nil {
		stored, err := d.loadMeasurePoints(ctx, key, 0, []string{in.Measure}, []string{in.Node})
		if err != nil {
			r := errFrom(measureOp+": "+err.Error(), err)
			return nil, &r
		}
		if len(stored) > 0 {
			return []measureTarget{{id: in.Node, retired: true}}, nil
		}
	}
	r := errValidation(fmt.Sprintf("%s: no node %q", measureOp, in.Node),
		"Name a concept by its chunk id or tag, or a document class by its name (describe_ontology lists them).")
	return nil, &r
}

// loadMeasurePointsBatched is loadMeasurePoints over nodes in batches, so a scope
// with hundreds of nodes keeps each IN list small.
func (d *Document) loadMeasurePointsBatched(ctx context.Context, key sqlmem.ScopeKey, shownAt int64, measures, nodes []string) (map[pointKey]storedPoint, error) {
	out := map[pointKey]storedPoint{}
	for start := 0; start < len(nodes); start += nodeStatsDocBatch {
		part, err := d.loadMeasurePoints(ctx, key, shownAt, measures, nodes[start:min(start+nodeStatsDocBatch, len(nodes))])
		if err != nil {
			return nil, err
		}
		for k, v := range part {
			out[k] = v
		}
	}
	return out, nil
}

// seriesOf is t's series of measure from stored, within rng.
func seriesOf(t measureTarget, measure string, stored map[pointKey]storedPoint, rng measureRange) measureNode {
	n := measureNode{NodeID: t.id, Kind: t.kind, Name: t.name, Tag: t.tag, Scheme: t.scheme, Period: t.period,
		Retired: t.retired, Points: []measurePoint{}}
	var keys []pointKey
	for k, s := range stored {
		if k.measure != measure || k.node != t.id || emptyPoint(s) || !rng.holds(k.start) {
			continue
		}
		if !t.retired && k.period != t.period {
			continue // rows of an earlier @period
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return pointLess(keys[i], keys[j]) })
	for _, k := range keys {
		s := stored[k]
		pt := measurePoint{PeriodStart: k.start, Label: periodLabel(k.start, k.period), Value: s.value, Members: s.members,
			FallbackShare: s.fallback, ComputedAt: s.computedAt}
		if measure == measureShare || measure == measureUnplaced {
			b := s.basis
			pt.Basis = &b
		}
		if measure == measureUnplaced && s.basis > 0 {
			sh := s.value / float64(s.basis)
			pt.Share = &sh
		}
		if t.retired && n.Period == "" {
			n.Period = k.period
		}
		n.Points = append(n.Points, pt)
	}
	return n
}

// measureCompare sums each concept's share over the main range and the baseline,
// the basis from its scheme's unplaced rows (kept for every period with a basis),
// and orders the concepts by the capped log-ratio of the two shares, largest move
// first. Only servable rows are summed; dropped is how many were not.
func (d *Document) measureCompare(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, sp *seriesPlan, targets []measureTarget,
	shownAt int64, main, base measureRange) (_ *measureComparison, dropped int, _ error) {
	if main.From == nil && main.To == nil {
		// The main range defaults to everything after the baseline.
		main.From = base.To
	}
	schemeID := map[string]string{}
	for _, s := range read.Schemes {
		schemeID[s.Name] = s.ChunkID
	}
	var ids []string
	for _, t := range targets {
		ids = append(ids, t.id)
	}
	for _, id := range schemeID {
		ids = append(ids, id)
	}
	stored, err := d.loadMeasurePointsBatched(ctx, key, shownAt, []string{measureShare, measureUnplaced}, ids)
	if err != nil {
		return nil, 0, err
	}
	dropped = sp.servable(stored)
	out := &measureComparison{Range: main, Baseline: base, Nodes: []measureCompared{}}
	type sums struct{ count, basis int }
	for _, t := range targets {
		var m, b sums
		for k, s := range stored {
			if k.period != t.period || emptyPoint(s) {
				continue
			}
			inMain, inBase := main.holds(k.start), base.holds(k.start)
			share := k.measure == measureShare && k.node == t.id
			basis := k.measure == measureUnplaced && k.node == schemeID[t.scheme]
			if (!inMain && !inBase) || (!share && !basis) {
				continue
			}
			// A share row's members are the node's projected members; an unplaced row
			// carries the period's basis.
			for _, r := range []struct {
				in bool
				s  *sums
			}{{inMain, &m}, {inBase, &b}} {
				switch {
				case !r.in:
				case share:
					r.s.count += s.members
				default:
					r.s.basis += s.basis
				}
			}
		}
		c := measureCompared{NodeID: t.id, Name: t.name, Tag: t.tag, Scheme: t.scheme,
			Count: m.count, Basis: m.basis, BaselineCount: b.count, BaselineBasis: b.basis}
		if m.basis > 0 {
			sh := float64(m.count) / float64(m.basis)
			c.Share = &sh
		}
		if b.basis > 0 {
			sh := float64(b.count) / float64(b.basis)
			c.BaselineShare = &sh
		}
		c.LogRatio = cappedLogRatio(m.count, m.basis, b.count, b.basis)
		out.Nodes = append(out.Nodes, c)
	}
	sort.SliceStable(out.Nodes, func(i, j int) bool {
		a, b := out.Nodes[i], out.Nodes[j]
		if math.Abs(a.LogRatio) != math.Abs(b.LogRatio) {
			return math.Abs(a.LogRatio) > math.Abs(b.LogRatio)
		}
		if a.LogRatio != b.LogRatio {
			return a.LogRatio > b.LogRatio
		}
		return a.Tag < b.Tag
	})
	return out, dropped, nil
}

// cappedLogRatio is ln(share now / share then), each share smoothed by half a member
// (Haldane's correction) so that a node absent from one range has a finite ratio,
// capped at ±compareLogRatioCap. 0 when neither range has a basis.
func cappedLogRatio(count, basis, baseCount, baseBasis int) float64 {
	if basis == 0 && baseBasis == 0 {
		return 0
	}
	now := (float64(count) + 0.5) / (float64(basis) + 1)
	then := (float64(baseCount) + 0.5) / (float64(baseBasis) + 1)
	return math.Max(-compareLogRatioCap, math.Min(compareLogRatioCap, math.Log(now/then)))
}

// nodeSeriesView is a node's series as describe_ontology with_stats serves it: the
// latest period with a member, its count and (a concept) share, and the trend from
// the period before; the same to every reader.
type nodeSeriesView struct {
	Period        string     `json:"period,omitempty"`
	Latest        string     `json:"latest,omitempty"`
	Count         *int       `json:"count,omitempty"`
	Share         *float64   `json:"share,omitempty"`
	FallbackShare *float64   `json:"fallback_share,omitempty"`
	Trend         *nodeTrend `json:"trend,omitempty"`
}

// nodeTrend is the move from the period before the latest one: Change is the share
// change for a concept whose share is known in both, else the count change.
type nodeTrend struct {
	Previous      string   `json:"previous"`
	PreviousCount int      `json:"previous_count"`
	PreviousShare *float64 `json:"previous_share,omitempty"`
	Change        float64  `json:"change"`
}

// describeSeries adds each concept's and document class's series view (with_stats),
// from the servable rows only; p is describeStats' plan (nil: it could not be
// built, and has said so). A store fault marks the answer partial, as the
// statistics do.
func (d *Document) describeSeries(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, p *nodeStatsPlan, out *ontologyDescription) {
	if p == nil {
		return
	}
	fail := func() {
		out.Partial = true
		out.Issues = append(out.Issues, "with_stats: the series could not be read; read again")
	}
	sp, err := d.buildSeriesPlan(ctx, key, read, "", p)
	if err != nil {
		fail()
		return
	}
	stored, err := d.loadMeasurePoints(ctx, key, 0, []string{measureCount, measureShare, measureUnplaced}, nil)
	if err != nil {
		fail()
		return
	}
	sp.servable(stored)
	if len(stored) == 0 {
		return
	}
	for i := range out.Schemes {
		s := &out.Schemes[i]
		per := schemePeriod(s.OntologyScheme)
		for j := range s.Concepts {
			c := &s.Concepts[j]
			c.Series = seriesView(c.ChunkID, s.ChunkID, per, stored)
		}
	}
	for i := range out.Classes {
		if out.Classes[i].Kind == "document" {
			out.Classes[i].Series = seriesView(out.Classes[i].Name, "", memrank.SchemePeriodMonth, stored)
		}
	}
}

// seriesView is node's view from stored; schemeID names its scheme's unplaced rows
// ("" for a class). nil when the node has no count row at its period.
func seriesView(node, schemeID, period string, stored map[pointKey]storedPoint) *nodeSeriesView {
	latest, found := int64(0), false
	for k, s := range stored {
		if k.measure == measureCount && k.node == node && k.period == period && !emptyPoint(s) && (!found || k.start > latest) {
			latest, found = k.start, true
		}
	}
	if !found {
		return nil
	}
	prev := shiftPeriod(latest, period, -1)
	get := func(measure, id string, start int64) (storedPoint, bool) {
		s, ok := stored[pointKey{measure: measure, node: id, period: period, start: start}]
		return s, ok && !emptyPoint(s)
	}
	v := &nodeSeriesView{Period: period, Latest: periodLabel(latest, period)}
	cnt, _ := get(measureCount, node, latest)
	shr, hasShare := get(measureShare, node, latest)
	pc, hasPC := get(measureCount, node, prev)
	ps, hasPS := get(measureShare, node, prev)
	_, hasPU := get(measureUnplaced, schemeID, prev)
	n, fb := cnt.members, cnt.fallback
	v.Count, v.FallbackShare = &n, &fb
	t := &nodeTrend{Previous: periodLabel(prev, period)}
	if hasPC {
		t.PreviousCount = pc.members
	}
	switch {
	case hasPS:
		t.PreviousShare = &ps.value
	case hasPU && schemeID != "":
		// The period before had things to place and none under this node.
		zero := 0.0
		t.PreviousShare = &zero
	}
	t.Change = float64(n - t.PreviousCount)
	if hasShare {
		sv := shr.value
		v.Share = &sv
		if t.PreviousShare != nil {
			t.Change = sv - *t.PreviousShare
		}
	}
	v.Trend = t
	return v
}

// nodeInForce: node names a concept of any scheme (chunk id or tag) or a document
// class in force.
func nodeInForce(read OntologyRead, node string) bool {
	if read.Confirmed {
		for _, s := range read.Schemes {
			for _, c := range s.Concepts {
				if c.ChunkID != "" && c.Tag != "" && (c.ChunkID == node || c.Tag == node) {
					return true
				}
			}
		}
	}
	for _, c := range describeOntologyRead(read).Classes {
		if c.Kind == "document" && c.Name == node {
			return true
		}
	}
	return false
}
