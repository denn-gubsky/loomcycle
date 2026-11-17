package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
)

// Unit is one measured vector (a document's, or one of its sections') and the
// group it belongs to. A group is the thing left out together: a document and
// all its sections, so a section is never judged against a centroid that still
// holds its own document's other sections.
type Unit struct {
	Group string
	Vec   []float64 // unit length
}

// AgreementInput is one level of one arm.
type AgreementInput struct {
	Classes []string
	Units   []Unit
	// Weights maps a group to the classes it is declared in and the weight its
	// units carry in each class's centroid (1, or 1/k for the weighted arm).
	Weights map[string]map[string]float64
}

// UnitRank is one unit's verdict: the classes ordered by cosine to their
// leave-group-out centroid, most similar first.
type UnitRank struct {
	Group     string
	Ranked    []string
	Evaluable bool
}

// Rates are hit counts over the evaluable units.
type Rates struct {
	Units     int     `json:"units"`
	Evaluable int     `json:"evaluable"`
	Top1      int     `json:"top1"`
	Top3      int     `json:"top3"`
	Top1Rate  float64 `json:"top1_rate"`
	Top3Rate  float64 `json:"top3_rate"`
}

// Add folds o into r and recomputes the rates.
func (r *Rates) Add(o Rates) {
	r.Units += o.Units
	r.Evaluable += o.Evaluable
	r.Top1 += o.Top1
	r.Top3 += o.Top3
	r.finish()
}

func (r *Rates) finish() {
	r.Top1Rate, r.Top3Rate = 0, 0
	if r.Evaluable > 0 {
		r.Top1Rate = float64(r.Top1) / float64(r.Evaluable)
		r.Top3Rate = float64(r.Top3) / float64(r.Evaluable)
	}
}

// Agreement ranks, for every unit, the classes by cosine to each class centroid
// computed WITHOUT the unit's group, and scores the ranking against the group's
// declared classes.
//
// nodestats has no subtraction, and the leave-out is exact algebra over sums,
// so it is done here: with S_c the weighted sum of class c's unit vectors and
// G_g the sum of group g's unit vectors, the centroid without g points along
// S_c - w G_g, and
//
//	cos(x, S_c - w G) = (x.S_c - w x.G) / sqrt(|S_c|^2 - 2w S_c.G + w^2 |G|^2).
//
// A class with no unit left after the leave-out is not a candidate; a unit none
// of whose declared classes is a candidate is not evaluable (its document is
// the only one placed there). Ties break by class id.
func Agreement(in AgreementInput) ([]UnitRank, Rates, error) {
	if len(in.Units) == 0 {
		return nil, Rates{}, nil
	}
	dim := len(in.Units[0].Vec)
	idx := map[string]int{}
	for i, c := range in.Classes {
		idx[c] = i
	}
	nc := len(in.Classes)
	S := make([][]float64, nc)
	count := make([]int, nc)
	for i := range S {
		S[i] = make([]float64, dim)
	}
	G := map[string][]float64{}
	gCount := map[string]int{}
	for i, u := range in.Units {
		if len(u.Vec) != dim {
			return nil, Rates{}, fmt.Errorf("unit %d: dimension %d, want %d", i, len(u.Vec), dim)
		}
		if G[u.Group] == nil {
			G[u.Group] = make([]float64, dim)
		}
		axpy(G[u.Group], 1, u.Vec)
		gCount[u.Group]++
		for c, w := range in.Weights[u.Group] {
			ci, ok := idx[c]
			if !ok {
				return nil, Rates{}, fmt.Errorf("group %q: class %q is not of this level", u.Group, c)
			}
			axpy(S[ci], w, u.Vec)
			count[ci]++
		}
	}
	SS := make([]float64, nc)
	for i := range S {
		SS[i] = dotf(S[i], S[i])
	}
	type gstat struct {
		SG []float64
		GG float64
	}
	gs := map[string]gstat{}
	for g, v := range G {
		st := gstat{SG: make([]float64, nc), GG: dotf(v, v)}
		for i := range S {
			st.SG[i] = dotf(S[i], v)
		}
		gs[g] = st
	}

	out := make([]UnitRank, 0, len(in.Units))
	var r Rates
	for _, u := range in.Units {
		st := gs[u.Group]
		xG := dotf(u.Vec, G[u.Group])
		var cands []scored
		for i, c := range in.Classes {
			w := in.Weights[u.Group][c]
			left := count[i]
			if w > 0 {
				left -= gCount[u.Group]
			}
			if left <= 0 {
				continue
			}
			den2 := SS[i] - 2*w*st.SG[i] + w*w*st.GG
			if den2 <= 0 {
				continue // exact cancellation: no direction left
			}
			num := dotf(u.Vec, S[i]) - w*xG
			cands = append(cands, scored{c, num / math.Sqrt(den2)})
		}
		ur := UnitRank{Group: u.Group, Ranked: rankIDs(cands)}
		for _, c := range ur.Ranked {
			if in.Weights[u.Group][c] > 0 {
				ur.Evaluable = true
			}
		}
		r.Units++
		if ur.Evaluable {
			r.Evaluable++
			if hitAt(ur.Ranked, in.Weights[u.Group], 1) {
				r.Top1++
			}
			if hitAt(ur.Ranked, in.Weights[u.Group], 3) {
				r.Top3++
			}
		}
		out = append(out, ur)
	}
	r.finish()
	return out, r, nil
}

// FixedAgreement ranks every unit against fixed class vectors (the definition
// baseline): no leave-out, since no unit contributed to them. A class without a
// vector is not a candidate.
func FixedAgreement(in AgreementInput, classVec map[string][]float64) Rates {
	var r Rates
	for _, u := range in.Units {
		var cands []scored
		for _, c := range in.Classes {
			if v, ok := classVec[c]; ok {
				cands = append(cands, scored{c, dotf(u.Vec, v)})
			}
		}
		ranked := rankIDs(cands)
		r.Units++
		ok := false
		for _, c := range ranked {
			ok = ok || in.Weights[u.Group][c] > 0
		}
		if !ok {
			continue
		}
		r.Evaluable++
		if hitAt(ranked, in.Weights[u.Group], 1) {
			r.Top1++
		}
		if hitAt(ranked, in.Weights[u.Group], 3) {
			r.Top3++
		}
	}
	r.finish()
	return r
}

// PermutationSummary is the labels-permuted baseline over n permutations.
type PermutationSummary struct {
	N        int     `json:"n"`
	Top1Mean float64 `json:"top1_mean"`
	Top1P95  float64 `json:"top1_p95"`
	Top3Mean float64 `json:"top3_mean"`
	Top3P95  float64 `json:"top3_p95"`
}

// Permute shuffles the groups' declared-class sets among the groups (so every
// class keeps its size) with a PCG source seeded by seed.
func Permute(weights map[string]map[string]float64, seed uint64) map[string]map[string]float64 {
	groups := make([]string, 0, len(weights))
	for g := range weights {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	rng := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
	perm := rng.Perm(len(groups))
	out := make(map[string]map[string]float64, len(groups))
	for i, g := range groups {
		out[g] = weights[groups[perm[i]]]
	}
	return out
}

// Percentile is the nearest-rank p-th percentile (0 < p <= 100) of xs.
func Percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	k := int(math.Ceil(p/100*float64(len(s)))) - 1
	if k < 0 {
		k = 0
	}
	return s[k]
}

type scored struct {
	id string
	s  float64
}

func rankIDs(c []scored) []string {
	sort.Slice(c, func(i, j int) bool {
		if c[i].s != c[j].s {
			return c[i].s > c[j].s
		}
		return c[i].id < c[j].id
	})
	out := make([]string, len(c))
	for i, x := range c {
		out[i] = x.id
	}
	return out
}

func hitAt(ranked []string, declared map[string]float64, k int) bool {
	for i := 0; i < k && i < len(ranked); i++ {
		if declared[ranked[i]] > 0 {
			return true
		}
	}
	return false
}

func axpy(dst []float64, a float64, x []float64) {
	for i, v := range x {
		dst[i] += a * v
	}
}

func dotf(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// unitVec normalises a stored vector, refusing a zero, non-finite one.
func unitVec(v []float32) ([]float64, error) {
	out := make([]float64, len(v))
	var n float64
	for i, x := range v {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("non-finite component %d", i)
		}
		out[i] = f
		n += f * f
	}
	if n == 0 {
		return nil, fmt.Errorf("zero vector")
	}
	n = math.Sqrt(n)
	for i := range out {
		out[i] /= n
	}
	return out, nil
}
