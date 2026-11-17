package main

import "sort"

// The margin question (RFC GM P0): does the gap between the nearest and the
// second nearest leave-one-out centroid predict that the nearest is right? A
// rule:centroid placement may go live only if the hit rate clearly rises with
// the margin, so the report shows it by quantile bin and above fixed
// thresholds.
const marginBins = 5

var marginThresholds = []float64{0, 0.01, 0.02, 0.03, 0.05, 0.08}

// MarginPoint is one evaluable unit: its margin and whether its nearest class
// was a declared one.
type MarginPoint struct {
	Margin float64
	Hit    bool
}

// MarginBin is one quantile bin: units with margins in [Lo, Hi].
type MarginBin struct {
	Lo      float64 `json:"lo"`
	Hi      float64 `json:"hi"`
	Units   int     `json:"units"`
	Hits    int     `json:"hits"`
	HitRate float64 `json:"hit_rate"`
}

// MarginThreshold is the units at or above one margin.
type MarginThreshold struct {
	Min      float64 `json:"min_margin"`
	Units    int     `json:"units"`
	Coverage float64 `json:"coverage"`
	Hits     int     `json:"hits"`
	HitRate  float64 `json:"hit_rate"`
}

// MarginReport is hit rate by margin over one pooled level.
type MarginReport struct {
	Units int `json:"units"`
	Hits  int `json:"hits"`
	// NoMargin counts evaluable units with a single candidate class: there is
	// no second nearest, so they are left out of every bin and threshold.
	NoMargin   int               `json:"no_margin"`
	Bins       []MarginBin       `json:"quantile_bins"`
	Thresholds []MarginThreshold `json:"thresholds"`
}

// MarginAgreement bins points into nbins equal-count bins by ascending margin
// (the first len%nbins bins are one larger; tied margins may straddle a bin
// edge) and counts, for each threshold, the points at or above it. Coverage is
// over all points with a margin.
func MarginAgreement(points []MarginPoint, nbins int, thresholds []float64) MarginReport {
	ps := append([]MarginPoint(nil), points...)
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Margin < ps[j].Margin })
	r := MarginReport{Units: len(ps)}
	for _, p := range ps {
		if p.Hit {
			r.Hits++
		}
	}
	if nbins > len(ps) {
		nbins = len(ps)
	}
	start := 0
	for b := 0; b < nbins; b++ {
		size := len(ps) / nbins
		if b < len(ps)%nbins {
			size++
		}
		bin := MarginBin{Lo: ps[start].Margin, Hi: ps[start+size-1].Margin, Units: size}
		for _, p := range ps[start : start+size] {
			if p.Hit {
				bin.Hits++
			}
		}
		bin.HitRate = float64(bin.Hits) / float64(size)
		r.Bins = append(r.Bins, bin)
		start += size
	}
	for _, t := range thresholds {
		th := MarginThreshold{Min: t}
		for _, p := range ps {
			if p.Margin >= t {
				th.Units++
				if p.Hit {
					th.Hits++
				}
			}
		}
		if len(ps) > 0 {
			th.Coverage = float64(th.Units) / float64(len(ps))
		}
		if th.Units > 0 {
			th.HitRate = float64(th.Hits) / float64(th.Units)
		}
		r.Thresholds = append(r.Thresholds, th)
	}
	return r
}

// marginPoints collects the evaluable units of one level; it returns how many
// evaluable units had no margin.
func marginPoints(ranks []UnitRank) ([]MarginPoint, int) {
	var ps []MarginPoint
	none := 0
	for _, u := range ranks {
		if !u.Evaluable {
			continue
		}
		if !u.HasMargin {
			none++
			continue
		}
		ps = append(ps, MarginPoint{Margin: u.Margin, Hit: u.Top1})
	}
	return ps, none
}
