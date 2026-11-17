package builtin

// document_measure.go — the taxonomy's built-in series (RFC GM 6A.1–6A.7, plan
// P2.2): a stage of the node_stats pass that keeps, per node and period, how many
// members the node had (count), what share of the scheme's things that was (share),
// how far the period's members moved from the earlier ones (shift), and per scheme
// how many of its things were placed nowhere (unplaced). Document op=measure reads
// them (document_measure_read.go).
//
// A THING'S DATE (6A.2, D28) is its root chunk's valid_at, else the source update
// time its import recorded (source_updated_at), else when the document was stored
// (documents.created_at). The last is a fallback: a corpus imported in one batch
// lands in one period. Every row says what share of its members only that
// fallback dated (fallback_share), so a reader can tell a series from an import.
//
// WHAT EACH SERIES IS, per node and period of the node's scheme (@period, month by
// default; a document class keeps months):
//
//   - count: the node's members dated in the period, rolled up as node_stats rolls
//     them (a document once per node).
//   - share: the members of the classes the scheme projects (@projects) dated in the
//     period, divided by every thing of those classes dated in it (its basis). A
//     member of another class counts in count and not in share, so share is never
//     over 1. Concepts only: a class has no scheme to be a share of.
//   - shift: 1 − cos between the centroid of the period's members and the centroid
//     of every earlier member, from the document annotation vectors node_stats
//     reads. Only where the store keeps vectors, and only when both sides hold
//     nodestats.MinMembers members with a vector.
//   - unplaced (per scheme, node_id the scheme's chunk): the things of the projected
//     classes dated in the period under none of the scheme's concepts; basis as
//     share's. A row is kept for every period with a basis, so the basis of a period
//     in which a concept had no member is still known.
//
// APPEND-ONLY (6A.5, D22). A row is never changed. The pass computes every row the
// current state yields and compares its inputs hash with the latest stored row of
// the same (measure, node, period, period start); only a missing or changed one is
// appended, with a later computed_at. A period whose members are all gone gets an
// EMPTY row (members 0, value 0), and so does a shift that can no longer be
// computed, so the latest row always says what holds now and a reader by shown_at
// reads what held then. Rows of a node no longer in force are left as they are
// (6A.7), and so are rows of another period length (a scheme whose @period changed)
// and, where the vector stage does not run, shift rows.
//
// THE INPUTS HASH of a row covers what that row is computed from, nothing else: the
// period's member ids (count); the projected members and the period's basis ids
// (share, unplaced); and for shift the ids, annotation hashes and annotation texts
// of the period's and every earlier member, and the embed model. A split, merge or
// move changes member sets and so marks exactly the affected periods; a rename
// changes no value and adds no row.
//
// members_sha256 is the hash of the documents a row is built from (count: the
// period's members; share and unplaced: the period's basis; shift: the members up
// to and including the period). A reader whose access rules limit it gets a row
// only when it may read every one of them and the row was built from that same set
// (document_measure_read.go): the rule node_stats applies to its statistics.
//
// SQLite has one connection per scope: every read is made before the write
// transaction opens.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The built-in series' reserved measure names (RFC GM 6A.5).
const (
	measureCount    = "count"
	measureShare    = "share"
	measureShift    = "shift"
	measureUnplaced = "unplaced"

	// measureHashVersion is the version of what a row's inputs hash covers; a change
	// to it marks every row stale on purpose.
	measureHashVersion = "measure/1"
	// seriesReportCap bounds the stale points a report lists; the counts are whole.
	seriesReportCap = 200
	// measurePointsPage is the rows read per query when loading stored points.
	measurePointsPage = 2000
)

// measurePointsDDL is RFC GM 6A.5's table, portable across both tiers. Beside the
// RFC's columns: basis (the things a share is of), fallback_share (6A.2) and
// members_sha256 (the read check). value_sum is DOUBLE PRECISION, not REAL: a
// Postgres REAL is 4 bytes and would round a share. groups and sources are the
// declared measures' (P8); a built-in row writes 0.
const measurePointsDDL = `CREATE TABLE IF NOT EXISTS measure_points (
		measure TEXT NOT NULL, node_id TEXT NOT NULL, subject_id TEXT NOT NULL DEFAULT '',
		period_start BIGINT NOT NULL, period TEXT NOT NULL,
		value_sum DOUBLE PRECISION NOT NULL,
		members INTEGER NOT NULL, groups INTEGER NOT NULL, sources INTEGER NOT NULL,
		basis INTEGER NOT NULL, fallback_share DOUBLE PRECISION NOT NULL,
		ontology_version INTEGER NOT NULL, inputs_sha256 TEXT NOT NULL, members_sha256 TEXT NOT NULL,
		computed_at BIGINT NOT NULL,
		PRIMARY KEY (measure, node_id, subject_id, period_start, computed_at))`

// measurePointsIndexDDL serves the latest-row lookup per (measure, node, period).
const measurePointsIndexDDL = `CREATE INDEX IF NOT EXISTS measure_points_latest
		ON measure_points(measure, node_id, subject_id, period, period_start, computed_at)`

// pointKey names one series point, across its rows.
type pointKey struct {
	measure, node, period string
	start                 int64
}

// seriesRow is one row the current state yields.
type seriesRow struct {
	pointKey
	value          float64
	members, basis int
	fallback       float64
	inputs, readSH string
	empty          bool
	// shift only: the ids of the period's members and of every earlier member.
	periodIDs, earlierIDs []string
}

// storedPoint is one stored row, without its key.
type storedPoint struct {
	value          float64
	members, basis int
	fallback       float64
	inputs, readSH string
	computedAt     int64
}

// seriesDoc is what the stage knows of one document of the scope.
type seriesDoc struct {
	typ, root string
	created   int64
	validAt   *int64
}

// seriesDate is the date a document counts under, and whether the last fallback
// (created_at) gave it.
type seriesDate struct {
	at       int64
	fallback bool
}

// seriesPlan is every row the current state yields, and the nodes it covers.
type seriesPlan struct {
	rows []seriesRow
	// period is the period length of each node of the pass (concept and class ids,
	// and each scheme's chunk id for unplaced): stored rows of these nodes at this
	// length are the ones the pass answers for.
	period map[string]string
	// shiftOff says why shift is not computed here, "" when it is.
	shiftOff string
	// read is, per point, the documents its row is built from (for the read check).
	read map[pointKey][]string
}

// SeriesReport is the series stage's part of node_stats' answer.
type SeriesReport struct {
	// Shift says why shift is not kept here; absent when it is.
	Shift    string `json:"shift,omitempty"`
	Fresh    int    `json:"fresh"`
	Stale    int    `json:"stale"`
	Missing  int    `json:"missing"`
	Computed int    `json:"computed"`
	// Points lists the stale and missing points, at most seriesReportCap of them.
	Points    []seriesPointStatus `json:"points,omitempty"`
	Truncated bool                `json:"truncated,omitempty"`
}

// seriesPointStatus is one point that is not fresh.
type seriesPointStatus struct {
	Measure string `json:"measure"`
	NodeID  string `json:"node_id"`
	Period  string `json:"period"`
	Label   string `json:"label"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
}

// loadSeriesDocs reads every document of the scope with its type, creation time and
// root valid_at, keyset-paged so the row cap never cuts it.
func (d *Document) loadSeriesDocs(ctx context.Context, key sqlmem.ScopeKey) (map[string]seriesDoc, error) {
	out := map[string]seriesDoc{}
	after := ""
	for {
		res, err := d.query(ctx, key, `SELECT d.id, d.type, d.created_at, d.root_chunk_id, c.valid_at
			FROM documents d LEFT JOIN chunks c ON c.id = d.root_chunk_id
			WHERE d.id > ? ORDER BY d.id LIMIT ?`, after, searchFilterPage)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Rows {
			sd := seriesDoc{typ: asStr(r[1]), created: asInt64Val(r[2]), root: asStr(r[3])}
			if v, ok := asInt64(r[4]); ok {
				sd.validAt = &v
			}
			out[asStr(r[0])] = sd
		}
		if len(res.Rows) == 0 || (!res.Truncated && len(res.Rows) < searchFilterPage) {
			return out, nil
		}
		after = asStr(res.Rows[len(res.Rows)-1][0])
	}
}

// seriesDates dates each of ids by RFC GM 6A.2. The source date is read from the
// root's import provenance only for a document without a root valid_at. That field is
// the server's on every write path (document_import_source.go), so no tool input
// can date a document through it. A store fault fails the pass: a wrong date would
// append a row to the wrong period for good.
func (d *Document) seriesDates(ctx context.Context, key sqlmem.ScopeKey, docs map[string]seriesDoc, ids []string) (map[string]seriesDate, error) {
	out := make(map[string]seriesDate, len(ids))
	for _, id := range ids {
		sd, ok := docs[id]
		if !ok {
			continue
		}
		if sd.validAt != nil {
			out[id] = seriesDate{at: *sd.validAt}
			continue
		}
		cb, err := d.readBody(ctx, store.MemoryScopeTenant, key, sd.root)
		if err != nil {
			return nil, fmt.Errorf("the provenance of document %s: %w", id, err)
		}
		if p := provenanceView(cb.Fields); p != nil {
			if s, _ := p[importFieldSourceUpdatedAt].(string); s != "" {
				if t, err := time.Parse(time.RFC3339, s); err == nil {
					out[id] = seriesDate{at: t.UnixNano()}
					continue
				}
			}
		}
		out[id] = seriesDate{at: sd.created, fallback: true}
	}
	return out, nil
}

// seriesHash hashes lines under a row's measure and period.
func seriesHash(k pointKey, extra string, lines []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s %d\n%s\n", measureHashVersion, k.measure, k.period, k.start, extra)
	for _, l := range lines {
		fmt.Fprintf(h, "%s\n", l)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fallbackShare is the share of ids the last fallback dated.
func fallbackShare(ids []string, dates map[string]seriesDate) float64 {
	if len(ids) == 0 {
		return 0
	}
	n := 0
	for _, id := range ids {
		if dates[id].fallback {
			n++
		}
	}
	return float64(n) / float64(len(ids))
}

// bucketByPeriod groups ids (sorted) by the period their date falls in; an id with
// no date (a document gone since the plan was read) is left out.
func bucketByPeriod(ids []string, dates map[string]seriesDate, period string) map[int64][]string {
	out := map[int64][]string{}
	for _, id := range ids {
		if dt, ok := dates[id]; ok {
			s := periodStart(dt.at, period)
			out[s] = append(out[s], id)
		}
	}
	return out
}

func sortedStarts(m map[int64][]string) []int64 {
	out := make([]int64, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// seriesScheme is one scheme as the stage sees it.
type seriesScheme struct {
	s      memrank.OntologyScheme
	period string
	// basis: period start → the projected things dated in it, sorted.
	basis     map[int64][]string
	basisHash map[int64]string
	projected map[string]bool
}

// buildSeriesPlan computes every row the current state yields for the nodes of p
// (scheme narrows it as it narrows p). Reads only.
func (d *Document) buildSeriesPlan(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, scheme string, p *nodeStatsPlan) (*seriesPlan, error) {
	docs, err := d.loadSeriesDocs(ctx, key)
	if err != nil {
		return nil, err
	}
	sp := &seriesPlan{period: map[string]string{}, read: map[pointKey][]string{}}
	if p.model == "" {
		sp.shiftOff = "shift needs the vector stage, which does not run here: " + p.note
	}
	classes := describeOntologyRead(read).Classes
	var schemes []*seriesScheme
	if read.Confirmed {
		for _, s := range read.Schemes {
			if scheme != "" && s.Name != scheme {
				continue
			}
			ss := &seriesScheme{s: s, period: schemePeriod(s), projected: map[string]bool{}}
			proj := projectedClasses(s.Projects, classes)
			for id, sd := range docs {
				if proj[sd.typ] && id != read.DocumentID {
					ss.projected[id] = true
				}
			}
			schemes = append(schemes, ss)
		}
	}
	need := map[string]bool{}
	for _, n := range p.nodes {
		for _, id := range n.members {
			need[id] = true
		}
	}
	for _, ss := range schemes {
		for id := range ss.projected {
			need[id] = true
		}
	}
	dates, err := d.seriesDates(ctx, key, docs, sortedSet(need))
	if err != nil {
		return nil, err
	}
	schemeOf := map[string]*seriesScheme{}
	for _, ss := range schemes {
		sp.schemeRows(ss, p, dates)
		for _, c := range ss.s.Concepts {
			schemeOf[c.ChunkID] = ss
		}
	}
	for _, n := range p.nodes {
		ss := schemeOf[n.ID]
		if n.Kind == nodeKindConcept && ss == nil {
			continue
		}
		period := memrank.SchemePeriodMonth
		if ss != nil {
			period = ss.period
		}
		sp.period[n.ID] = period
		sp.nodeRows(n, ss, period, p, dates)
	}
	return sp, nil
}

// schemeRows fills ss's basis per period and adds its unplaced rows.
func (sp *seriesPlan) schemeRows(ss *seriesScheme, p *nodeStatsPlan, dates map[string]seriesDate) {
	placed := map[string]bool{}
	for _, n := range p.nodes {
		if n.Kind != nodeKindConcept {
			continue
		}
		for _, c := range ss.s.Concepts {
			if c.ChunkID == n.ID {
				for _, id := range n.members {
					placed[id] = true
				}
			}
		}
	}
	ss.basis = bucketByPeriod(sortedSet(ss.projected), dates, ss.period)
	ss.basisHash = map[int64]string{}
	sp.period[ss.s.ChunkID] = ss.period
	for _, start := range sortedStarts(ss.basis) {
		ids := ss.basis[start]
		ss.basisHash[start] = membersHash(ids)
		var unplaced []string
		for _, id := range ids {
			if !placed[id] {
				unplaced = append(unplaced, id)
			}
		}
		k := pointKey{measure: measureUnplaced, node: ss.s.ChunkID, period: ss.period, start: start}
		sp.add(seriesRow{pointKey: k, value: float64(len(unplaced)), members: len(unplaced), basis: len(ids),
			fallback: fallbackShare(unplaced, dates), inputs: seriesHash(k, "basis "+ss.basisHash[start], unplaced),
			readSH: ss.basisHash[start]}, ids)
	}
}

// nodeRows adds n's count, share (a concept of ss) and shift candidates.
func (sp *seriesPlan) nodeRows(n statNode, ss *seriesScheme, period string, p *nodeStatsPlan, dates map[string]seriesDate) {
	buckets := bucketByPeriod(n.members, dates, period)
	starts := sortedStarts(buckets)
	var earlier []string
	for _, start := range starts {
		ids := buckets[start]
		k := pointKey{measure: measureCount, node: n.ID, period: period, start: start}
		sp.add(seriesRow{pointKey: k, value: float64(len(ids)), members: len(ids), fallback: fallbackShare(ids, dates),
			inputs: seriesHash(k, "", ids), readSH: membersHash(ids)}, ids)
		if ss != nil && len(ss.basis[start]) > 0 {
			var in []string
			for _, id := range ids {
				if ss.projected[id] {
					in = append(in, id)
				}
			}
			basis := len(ss.basis[start])
			k := pointKey{measure: measureShare, node: n.ID, period: period, start: start}
			sp.add(seriesRow{pointKey: k, value: float64(len(in)) / float64(basis), members: len(in), basis: basis,
				fallback: fallbackShare(in, dates), inputs: seriesHash(k, "basis "+ss.basisHash[start], in),
				readSH: ss.basisHash[start]}, ss.basis[start])
		}
		if p.model != "" && len(ids) >= nodestats.MinMembers && len(earlier) >= nodestats.MinMembers {
			k := pointKey{measure: measureShift, node: n.ID, period: period, start: start}
			lines := make([]string, 0, len(ids)+len(earlier))
			for _, group := range []struct {
				tag string
				ids []string
			}{{"p", ids}, {"e", earlier}} {
				for _, id := range group.ids {
					lines = append(lines, group.tag+" "+p.memberLine(id))
				}
			}
			all := append(append([]string{}, earlier...), ids...)
			sort.Strings(all)
			sp.add(seriesRow{pointKey: k, members: len(ids), fallback: fallbackShare(ids, dates),
				inputs: seriesHash(k, "model "+p.model, lines), readSH: membersHash(all),
				periodIDs: ids, earlierIDs: append([]string{}, earlier...)}, all)
		}
		earlier = append(earlier, ids...)
	}
}

func (sp *seriesPlan) add(r seriesRow, readIDs []string) {
	sp.rows = append(sp.rows, r)
	sp.read[r.pointKey] = readIDs
}

// loadMeasurePoints reads, per point, the latest stored row computed at or before
// shownAt (0: the latest), of measures and nodes (nil: all). Keyset-paged.
func (d *Document) loadMeasurePoints(ctx context.Context, key sqlmem.ScopeKey, shownAt int64, measures, nodes []string) (map[pointKey]storedPoint, error) {
	if shownAt <= 0 {
		shownAt = 1<<63 - 1
	}
	filter, fargs := "", []any{}
	if len(measures) > 0 {
		ph, args := inPlaceholders(measures)
		filter += ` AND m.measure IN (` + ph + `)`
		fargs = append(fargs, args...)
	}
	if len(nodes) > 0 {
		ph, args := inPlaceholders(nodes)
		filter += ` AND m.node_id IN (` + ph + `)`
		fargs = append(fargs, args...)
	}
	stmt := `SELECT m.measure, m.node_id, m.period, m.period_start, m.value_sum, m.members, m.basis,
			m.fallback_share, m.inputs_sha256, m.members_sha256, m.computed_at
		FROM measure_points m
		WHERE m.subject_id = '' AND m.computed_at <= ?` + filter + `
		  AND (m.measure, m.node_id, m.period, m.period_start) > (?, ?, ?, ?)
		  AND m.computed_at = (SELECT max(x.computed_at) FROM measure_points x
		        WHERE x.measure = m.measure AND x.node_id = m.node_id AND x.subject_id = m.subject_id
		          AND x.period = m.period AND x.period_start = m.period_start AND x.computed_at <= ?)
		ORDER BY m.measure, m.node_id, m.period, m.period_start LIMIT ?`
	out := map[pointKey]storedPoint{}
	var after pointKey
	after.start = -1 << 63
	for {
		args := append(append([]any{shownAt}, fargs...), after.measure, after.node, after.period, after.start, shownAt, measurePointsPage)
		res, err := d.query(ctx, key, stmt, args...)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Rows {
			k := pointKey{measure: asStr(r[0]), node: asStr(r[1]), period: asStr(r[2]), start: asInt64Val(r[3])}
			v := 0.0
			if f := asFloat64Ptr(r[4]); f != nil {
				v = *f
			}
			fb := 0.0
			if f := asFloat64Ptr(r[7]); f != nil {
				fb = *f
			}
			out[k] = storedPoint{value: v, members: asInt(r[5]), basis: asInt(r[6]), fallback: fb,
				inputs: asStr(r[8]), readSH: asStr(r[9]), computedAt: asInt64Val(r[10])}
			after = k
		}
		if len(res.Rows) == 0 || (!res.Truncated && len(res.Rows) < measurePointsPage) {
			return out, nil
		}
	}
}

// seriesDiff compares the rows the state yields with the stored ones: the report,
// and the rows a compute appends (shift values still to fill).
func (sp *seriesPlan) seriesDiff(stored map[pointKey]storedPoint) (SeriesReport, []seriesRow) {
	rep := SeriesReport{Shift: sp.shiftOff}
	var out []seriesRow
	note := func(r seriesRow, status, reason string) {
		if len(rep.Points) >= seriesReportCap {
			rep.Truncated = true
			return
		}
		rep.Points = append(rep.Points, seriesPointStatus{Measure: r.measure, NodeID: r.node, Period: r.period,
			Label: periodLabel(r.start, r.period), Status: status, Reason: reason})
	}
	yielded := map[pointKey]bool{}
	for _, r := range sp.rows {
		yielded[r.pointKey] = true
		s, ok := stored[r.pointKey]
		switch {
		case !ok:
			rep.Missing++
			note(r, "missing", "never computed")
			out = append(out, r)
		case s.inputs != r.inputs:
			rep.Stale++
			note(r, "stale", "its members or their inputs changed")
			out = append(out, r)
		default:
			rep.Fresh++
		}
	}
	// A stored point the state no longer yields, of a node of this pass at its
	// current period length, whose latest row still holds a value: its members are
	// gone, or its shift can no longer be computed. Shift rows are left alone where
	// the vector stage does not run.
	var gone []pointKey
	for k, s := range stored {
		if yielded[k] || (s.members == 0 && s.basis == 0) || sp.period[k.node] != k.period {
			continue
		}
		if k.measure == measureShift && sp.shiftOff != "" {
			continue
		}
		gone = append(gone, k)
	}
	sort.Slice(gone, func(i, j int) bool { return pointLess(gone[i], gone[j]) })
	for _, k := range gone {
		r := seriesRow{pointKey: k, empty: true, inputs: seriesHash(k, "empty", nil), readSH: membersHash(nil)}
		rep.Stale++
		note(r, "stale", "no member is dated in this period now")
		out = append(out, r)
	}
	return rep, out
}

func pointLess(a, b pointKey) bool {
	if a.measure != b.measure {
		return a.measure < b.measure
	}
	if a.node != b.node {
		return a.node < b.node
	}
	if a.period != b.period {
		return a.period < b.period
	}
	return a.start < b.start
}

func hasShift(rows []seriesRow) bool {
	for _, r := range rows {
		if r.measure == measureShift {
			return true
		}
	}
	return false
}

func withoutShift(rows []seriesRow) []seriesRow {
	out := rows[:0:0]
	for _, r := range rows {
		if r.measure != measureShift {
			out = append(out, r)
		}
	}
	return out
}

// fillShift computes the value of each shift row in rows from vecs; one with too
// few members with a vector on either side becomes an empty row.
func fillShift(rows []seriesRow, vecs map[string]nodestats.Vector) {
	centroid := func(ids []string) (nodestats.Centroid, int, error) {
		var vs []nodestats.Vector
		for _, id := range ids {
			if v, ok := vecs[id]; ok {
				vs = append(vs, v)
			}
		}
		if len(vs) < nodestats.MinMembers {
			return nodestats.Centroid{}, len(vs), nodestats.ErrTooFewMembers
		}
		c, err := nodestats.CentroidOf(vs)
		return c, len(vs), err
	}
	for i := range rows {
		r := &rows[i]
		if r.measure != measureShift || r.empty {
			continue
		}
		cp, n, err := centroid(r.periodIDs)
		var ce nodestats.Centroid
		if err == nil {
			ce, _, err = centroid(r.earlierIDs)
		}
		var shift float64
		if err == nil {
			shift, err = nodestats.Shift(ce, cp)
		}
		if err != nil {
			r.empty, r.value, r.members = true, 0, 0
			continue
		}
		r.value, r.members = shift, n
	}
}

// writeMeasurePoints appends rows in one transaction. ON CONFLICT DO NOTHING: two
// passes over one scope (the sweeper on another replica and an operator call) may
// both append the same row; only an identical key (to the nanosecond) collides.
func (d *Document) writeMeasurePoints(ctx context.Context, key sqlmem.ScopeKey, rows []seriesRow, ontologyVersion int, now int64) error {
	if len(rows) == 0 {
		return nil
	}
	return d.withSqlTxn(ctx, key, func(txnID string) error {
		for _, r := range rows {
			value, members, basis, fb := r.value, r.members, r.basis, r.fallback
			if r.empty {
				value, members, basis, fb = 0, 0, 0, 0
			}
			if err := d.execTxn(ctx, txnID, `INSERT INTO measure_points (measure, node_id, subject_id, period_start, period,
				value_sum, members, groups, sources, basis, fallback_share, ontology_version, inputs_sha256, members_sha256, computed_at)
				VALUES (?, ?, '', ?, ?, ?, ?, 0, 0, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (measure, node_id, subject_id, period_start, computed_at) DO NOTHING`,
				r.measure, r.node, r.start, r.period, value, members, basis, fb, ontologyVersion, r.inputs, r.readSH, now); err != nil {
				return fmt.Errorf("measure_points %s %s %d: %w", r.measure, r.node, r.start, err)
			}
		}
		return nil
	})
}
