package builtin

// document_node_growth.go — growth and quiet nodes (RFC GM 6A.3, §2C step 5, plan
// P5.3): Document op=node_stats mode growth reads the taxonomy's stored series
// (measure_points, the rows node_stats compute appends) and reports, writing nothing:
//
//   - candidates: the concepts with members dated in the recent window, ranked by
//     growth — the members of the last quiet_periods periods less those of the
//     quiet_periods before — then by recent members and size. These are the nodes
//     to grow narrower concepts under first (§2C step 5 ranks by growth as well as
//     by size).
//   - quiet: the concepts and document classes that have members but none dated in
//     the recent window, with how many periods since the last one. Reported only:
//     the pass never retires a node (D7).
//   - unplaced: per scheme, the periods of its unplaced series, those whose unplaced
//     share rose over the period before first, most recent first; a rising period
//     carries its unplaced documents, the queue grouping (P5.2) takes first.
//
// NOTHING IS PROPOSED. Growth reports; a new concept, a rename, a move or a
// retirement is an ontology change filed elsewhere. A concept of an @origin adopted
// scheme, or one @locked, is marked fixed: growth beneath it may only add local
// concepts, never rename or move it (RFC GM §2B).
//
// WHAT IS READ. The stored rows a reader of op=measure gets (servable): computed
// from documents every member of the tenant may read (C8), and none whose period
// now holds a restricted document unless it was computed from today's members. The
// answer says how many count and unplaced points are stale or missing, so a reader
// knows a node_stats compute is due; it ranks what is stored. A node with no stored
// count point is not judged. Windows are counted in each node's own period (its
// scheme's @period; a class keeps months), ending in the period holding now.
//
// It needs no vectors: it runs on every store.

import (
	"context"
	"fmt"
	"sort"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

const (
	nodeStatsModeGrowth = "growth"

	// growthQuietDefault is the recent window when quiet_periods is not given: half
	// a year of the default monthly period. growthQuietMax bounds it.
	growthQuietDefault = 6
	growthQuietMax     = 120
	// growthListCap bounds each list of the answer; growthDocsCap the documents a
	// rising unplaced period carries.
	growthListCap = 200
	growthDocsCap = 50
)

// growthNode is one growth candidate.
type growthNode struct {
	NodeID  string `json:"node_id"`
	Name    string `json:"name"`
	Tag     string `json:"tag"`
	Scheme  string `json:"scheme"`
	Period  string `json:"period"`
	Members int    `json:"members"`
	// Recent and Previous are the members dated in the recent window and in the
	// window before it; Growth is Recent - Previous.
	Recent   int `json:"recent"`
	Previous int `json:"previous"`
	Growth   int `json:"growth"`
	// LastPeriod is the latest period holding a member.
	LastPeriod string `json:"last_period"`
	// Fixed: adopted (its scheme is @origin adopted) or locked (@locked): growth
	// beneath it may add local concepts, never rename or move it.
	Fixed string `json:"fixed,omitempty"`
}

// quietNode is a node with members and none dated in the recent window.
type quietNode struct {
	NodeID     string `json:"node_id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Tag        string `json:"tag,omitempty"`
	Scheme     string `json:"scheme,omitempty"`
	Period     string `json:"period"`
	Members    int    `json:"members"`
	LastPeriod string `json:"last_period"`
	// PeriodsSince is how many periods lie between the last one with a member and
	// the period holding now.
	PeriodsSince int    `json:"periods_since"`
	Fixed        string `json:"fixed,omitempty"`
}

// unplacedPeriod is one period of a scheme's unplaced series.
type unplacedPeriod struct {
	Label         string   `json:"label"`
	PeriodStart   int64    `json:"period_start"`
	Unplaced      int      `json:"unplaced"`
	Basis         int      `json:"basis"`
	Share         float64  `json:"share"`
	PreviousShare *float64 `json:"previous_share,omitempty"`
	// Rising: the share is above the previous period's (the one before with a basis).
	Rising bool `json:"rising"`
	// Documents (rising periods only): the unplaced documents the current state
	// dates in the period, at most growthDocsCap.
	Documents          []string `json:"documents,omitempty"`
	DocumentsTruncated bool     `json:"documents_truncated,omitempty"`
}

// unplacedScheme is one scheme's unplaced periods, rising first.
type unplacedScheme struct {
	Scheme   string           `json:"scheme"`
	SchemeID string           `json:"scheme_id"`
	Period   string           `json:"period"`
	Periods  []unplacedPeriod `json:"periods"`
}

// NodeGrowthReport is node_stats mode growth's answer.
type NodeGrowthReport struct {
	Mode         string `json:"mode"`
	Scheme       string `json:"scheme,omitempty"`
	QuietPeriods int    `json:"quiet_periods"`
	// SeriesStale / SeriesMissing: count and unplaced points the stored series lack
	// or hold out of date; Note says a compute is due when either is non-zero.
	SeriesStale   int              `json:"series_stale"`
	SeriesMissing int              `json:"series_missing"`
	Note          string           `json:"note,omitempty"`
	Candidates    []growthNode     `json:"candidates"`
	Quiet         []quietNode      `json:"quiet"`
	Unplaced      []unplacedScheme `json:"unplaced"`
	Truncated     bool             `json:"truncated,omitempty"`
}

// growthWindow is quiet_periods as a call asks for it, or a refusal reason.
func growthWindow(n int) (int, string) {
	switch {
	case n == 0:
		return growthQuietDefault, ""
	case n < 0 || n > growthQuietMax:
		return 0, fmt.Sprintf("quiet_periods must be 1 to %d", growthQuietMax)
	}
	return n, ""
}

// NodeGrowth reports growth, quiet nodes and unplaced periods of the tenant scope
// key names (only scheme's concepts when scheme is set), as of now (unix nanos),
// with a recent window of window periods. The caller has checked who may run it.
func (d *Document) NodeGrowth(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, scheme string, window int, now int64) (NodeGrowthReport, error) {
	ans := NodeGrowthReport{Mode: nodeStatsModeGrowth, Scheme: scheme, QuietPeriods: window,
		Candidates: []growthNode{}, Quiet: []quietNode{}, Unplaced: []unplacedScheme{}}
	p, err := d.buildNodeStatsPlanOpts(ctx, key, read, scheme, true) // counts only: no vector is read
	if err != nil {
		return ans, err
	}
	sp, err := d.buildSeriesPlan(ctx, key, read, scheme, p)
	if err != nil {
		return ans, err
	}
	stored, err := d.loadMeasurePoints(ctx, key, 0, []string{measureCount, measureUnplaced}, nil)
	if err != nil {
		return ans, err
	}
	// What a compute would append, as seriesDiff decides it: a point the state
	// yields and the store lacks is missing; one it holds otherwise is stale, and so
	// is a stored point the state no longer yields (its members are gone or dated
	// elsewhere now), which compute overwrites with an empty row. stored holds
	// count and unplaced only, so the diff's other measures are left out here.
	_, due := sp.seriesDiff(stored)
	for _, r := range due {
		if r.measure != measureCount && r.measure != measureUnplaced {
			continue
		}
		if _, ok := stored[r.pointKey]; ok {
			ans.SeriesStale++
		} else {
			ans.SeriesMissing++
		}
	}
	if ans.SeriesStale+ans.SeriesMissing > 0 {
		ans.Note = "the stored series are behind the documents; run node_stats mode compute for a current ranking"
	}
	// A gone point says what held before its members left; none of it holds now,
	// so it is not ranked from.
	yielded := make(map[pointKey]bool, len(sp.rows))
	for _, r := range sp.rows {
		yielded[r.pointKey] = true
	}
	for k := range stored {
		if !yielded[k] {
			delete(stored, k)
		}
	}
	sp.servable(stored)

	// Each node's servable count points, by start.
	counts := map[string]map[int64]int{}
	for k, s := range stored {
		if k.measure != measureCount || emptyPoint(s) || sp.period[k.node] != k.period {
			continue
		}
		if counts[k.node] == nil {
			counts[k.node] = map[int64]int{}
		}
		counts[k.node][k.start] = int(s.value)
	}
	info := growthConceptInfo(read)
	for _, n := range p.nodes {
		pts := counts[n.ID]
		if len(pts) == 0 {
			continue // never computed, or nothing dated: not judged
		}
		period := sp.period[n.ID]
		cur := periodStart(now, period)
		recentFrom := shiftPeriod(cur, period, -(window - 1))
		prevFrom := shiftPeriod(cur, period, -(2*window - 1))
		recent, previous := 0, 0
		last := int64(0)
		hasLast := false
		for start, v := range pts {
			if start > cur {
				continue // dated after the period holding now: not recent, not a last period
			}
			switch {
			case start >= recentFrom:
				recent += v
			case start >= prevFrom:
				previous += v
			}
			if v > 0 && (!hasLast || start > last) {
				last, hasLast = start, true
			}
		}
		if !hasLast || len(n.members) == 0 {
			continue // nothing dated up to now, or no member now: not judged
		}
		ci := info[n.ID]
		switch {
		case n.Kind == nodeKindConcept && recent > 0:
			ans.Candidates = append(ans.Candidates, growthNode{NodeID: n.ID, Name: n.Name, Tag: n.Tag, Scheme: ci.scheme,
				Period: period, Members: len(n.members), Recent: recent, Previous: previous, Growth: recent - previous,
				LastPeriod: periodLabel(last, period), Fixed: ci.fixed})
		case recent == 0:
			ans.Quiet = append(ans.Quiet, quietNode{NodeID: n.ID, Kind: n.Kind, Name: n.Name, Tag: n.Tag, Scheme: ci.scheme,
				Period: period, Members: len(n.members), LastPeriod: periodLabel(last, period),
				PeriodsSince: periodsBetween(last, cur, period), Fixed: ci.fixed})
		}
	}
	sort.Slice(ans.Candidates, func(i, j int) bool {
		a, b := ans.Candidates[i], ans.Candidates[j]
		if a.Growth != b.Growth {
			return a.Growth > b.Growth
		}
		if a.Recent != b.Recent {
			return a.Recent > b.Recent
		}
		if a.Members != b.Members {
			return a.Members > b.Members
		}
		return a.Tag < b.Tag
	})
	sort.Slice(ans.Quiet, func(i, j int) bool {
		a, b := ans.Quiet[i], ans.Quiet[j]
		if a.PeriodsSince != b.PeriodsSince {
			return a.PeriodsSince > b.PeriodsSince
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name+a.Tag < b.Name+b.Tag
	})
	ans.Unplaced = growthUnplaced(read, scheme, sp, stored)
	if len(ans.Candidates) > growthListCap {
		ans.Candidates, ans.Truncated = ans.Candidates[:growthListCap], true
	}
	if len(ans.Quiet) > growthListCap {
		ans.Quiet, ans.Truncated = ans.Quiet[:growthListCap], true
	}
	return ans, nil
}

// growthUnplaced is each scheme's unplaced periods from the servable stored rows,
// rising first (most recent first), then the rest (most recent first).
func growthUnplaced(read OntologyRead, scheme string, sp *seriesPlan, stored map[pointKey]storedPoint) []unplacedScheme {
	out := []unplacedScheme{}
	if !read.Confirmed {
		return out
	}
	for _, s := range read.Schemes {
		if scheme != "" && s.Name != scheme {
			continue
		}
		period := sp.period[s.ChunkID]
		var starts []int64
		for k, v := range stored {
			if k.measure == measureUnplaced && k.node == s.ChunkID && k.period == period && v.basis > 0 {
				starts = append(starts, k.start)
			}
		}
		if len(starts) == 0 {
			continue
		}
		sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
		us := unplacedScheme{Scheme: s.Name, SchemeID: s.ChunkID, Period: period}
		var prev *float64
		for _, start := range starts {
			v := stored[pointKey{measure: measureUnplaced, node: s.ChunkID, period: period, start: start}]
			share := v.value / float64(v.basis)
			up := unplacedPeriod{Label: periodLabel(start, period), PeriodStart: start, Unplaced: int(v.value),
				Basis: v.basis, Share: share, PreviousShare: prev, Rising: prev != nil && share > *prev}
			if up.Rising {
				docs := sp.unplaced[s.ChunkID][start]
				if len(docs) > growthDocsCap {
					docs, up.DocumentsTruncated = docs[:growthDocsCap], true
				}
				up.Documents = append([]string{}, docs...)
			}
			us.Periods = append(us.Periods, up)
			sh := share
			prev = &sh
		}
		sort.SliceStable(us.Periods, func(i, j int) bool {
			a, b := us.Periods[i], us.Periods[j]
			if a.Rising != b.Rising {
				return a.Rising
			}
			return a.PeriodStart > b.PeriodStart
		})
		if len(us.Periods) > growthListCap {
			us.Periods = us.Periods[:growthListCap]
		}
		out = append(out, us)
	}
	return out
}

// growthConcept is what growth reports of a concept beside its counts.
type growthConcept struct{ scheme, fixed string }

// growthConceptInfo is each in-force concept's scheme and whether it is fixed: of
// an @origin adopted scheme, or @locked (itself or its scheme).
func growthConceptInfo(read OntologyRead) map[string]growthConcept {
	out := map[string]growthConcept{}
	if !read.Confirmed {
		return out
	}
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			gc := growthConcept{scheme: s.Name}
			switch {
			case s.Origin == memrank.SchemeOriginAdopted || c.Origin == memrank.SchemeOriginAdopted:
				gc.fixed = "adopted"
			case s.Locked || c.Locked:
				gc.fixed = "locked"
			}
			out[c.ChunkID] = gc
		}
	}
	return out
}

// periodsBetween is how many periods of the given length lie from the period
// starting at from to the one starting at to (0 when to is not after from). Bounded
// so a far past date cannot loop for long: past ten thousand it stops counting.
func periodsBetween(from, to int64, period string) int {
	n := 0
	for s := from; s < to && n < 10000; s = shiftPeriod(s, period, 1) {
		n++
	}
	return n
}

// growthNow is the instant growth windows end in.
func growthNow() int64 { return time.Now().UnixNano() }
