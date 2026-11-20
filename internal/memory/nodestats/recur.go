package nodestats

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
)

// Recurrence defaults (RFC GM checks: "the split and unplaced groupings repeated
// over seeds and parameters; a proposal is filed only for a group that recurs").
// Not set by the P0 measurement, whose item (5) ran no grid: chosen on synthetic
// data (2026-11-20) where seeds alone were not enough. 2-means on Gaussian noise
// of a few hundred members splits along the sample's widest direction for most
// seeds, so re-running the full set with new seeds (4 of 5 runs, Jaccard 0.6)
// called 17 of 240 noise sets structured. Re-clustering a seeded 80% subsample
// per run and asking the group to keep three quarters of its members in every
// run called 3 of 450 (n 20..500, dimension 8..64) and missed none of 100
// planted pairs of clusters. A group that passes is a candidate, not a topic:
// RFC GM 2C step 5 also asks a model grouping to find it.
const (
	// RecurRuns is how many clusterings there are: the reference, then RecurRuns-1
	// re-clusterings of subsamples.
	RecurRuns = 5
	// RecurMin is how many of those runs (the reference included) must hold a
	// matching group: all of them.
	RecurMin = 5
	// RecurJaccard is the Jaccard index two member sets need to match, taken over
	// the members the run clustered.
	RecurJaccard = 0.75
)

// RecurOptions configures RecurringGroups.
type RecurOptions struct {
	// Split is the reference run's options. Run r (r >= 1) clusters a subsample of
	// Split.SubsampleFraction of the members, drawn and clustered with seed
	// Split.Seed + r; only the reference must reach MinNodeMembers. StabilityRuns
	// is not used: this recurrence replaces it.
	Split SplitOptions
	// Runs is the number of clusterings (the reference is the first).
	Runs int
	// MinRecur is how many runs, the reference included, must hold a group
	// matching a reference group for it to recur.
	MinRecur int
	// MinJaccard is the Jaccard index two member sets need to match.
	MinJaccard float64
}

// DefaultRecurOptions returns the defaults above with the given seed.
func DefaultRecurOptions(seed uint64) RecurOptions {
	return RecurOptions{Split: DefaultSplitOptions(seed), Runs: RecurRuns, MinRecur: RecurMin, MinJaccard: RecurJaccard}
}

func (o RecurOptions) validate() error {
	switch {
	case o.Runs < 1:
		return fmt.Errorf("%w: Runs = %d", ErrInvalidArgument, o.Runs)
	case o.MinRecur < 1 || o.MinRecur > o.Runs:
		return fmt.Errorf("%w: MinRecur = %d, need 1..Runs", ErrInvalidArgument, o.MinRecur)
	case !(o.MinJaccard > 0 && o.MinJaccard <= 1):
		return fmt.Errorf("%w: MinJaccard = %v", ErrInvalidArgument, o.MinJaccard)
	}
	return o.Split.validate()
}

// RecurringGroup is a group of the reference clustering with the number of runs
// (the reference included) holding a group that matches it.
type RecurringGroup struct {
	Group
	Recurrence int
}

// RecurResult is the outcome of RecurringGroups. Reference is the first run's
// split (its K, silhouette and every group it kept); Groups are those of its
// groups that recur, in the reference's order; Recurrence is every reference
// group's count, index-aligned with Reference.Groups. Runs counts the
// clusterings tried (a run whose split failed holds no group).
type RecurResult struct {
	Reference  SplitResult
	Groups     []RecurringGroup
	Recurrence []int
	Runs       int
}

// RecurringGroups splits members (Split) and keeps each group of that reference
// split which comes back when the grouping is redone: run r re-clusters a random
// subsample of the members with its own seed, and holds the group when one of
// its groups has a Jaccard index of at least MinJaccard with the group's members
// that the run saw. k-means imposes a partition on any data; on structureless
// members the partition moves with the seed and the sample, so its groups do not
// recur.
//
// Each run chooses its own k: a group recurs whatever the partition around it,
// which is the question a proposal asks ("is this one topic?"), not whether the
// whole partition is stable.
func RecurringGroups(members []Member, opts RecurOptions) (RecurResult, error) {
	if err := opts.validate(); err != nil {
		return RecurResult{}, err
	}
	so := opts.Split
	so.StabilityRuns = 0
	ref, err := Split(members, so)
	if err != nil {
		return RecurResult{}, err
	}
	res := RecurResult{Reference: ref, Runs: 1, Recurrence: make([]int, len(ref.Groups))}
	for i := range res.Recurrence {
		res.Recurrence[i] = 1
	}
	m := int(math.Round(so.SubsampleFraction * float64(len(members))))
	for r := 1; r < opts.Runs; r++ {
		res.Runs++
		seed := opts.Split.Seed + uint64(r)
		rng := rand.New(rand.NewPCG(seed, stabilityStream))
		pick := rng.Perm(len(members))[:m]
		sort.Ints(pick)
		sub := make([]Member, len(pick))
		seen := make(map[string]bool, len(pick))
		for j, p := range pick {
			sub[j] = members[p]
			seen[members[p].ID] = true
		}
		rs := so
		rs.Seed, rs.MinNodeMembers = seed, rs.KMin
		sr, err := Split(sub, rs)
		if err != nil {
			continue
		}
		for i, g := range ref.Groups {
			set := make(map[string]bool, len(g.Members))
			for _, id := range g.Members {
				if seen[id] {
					set[id] = true
				}
			}
			for _, h := range sr.Groups {
				if jaccard(set, h.Members) >= opts.MinJaccard {
					res.Recurrence[i]++
					break
				}
			}
		}
	}
	for i, g := range ref.Groups {
		if res.Recurrence[i] >= opts.MinRecur {
			res.Groups = append(res.Groups, RecurringGroup{Group: g, Recurrence: res.Recurrence[i]})
		}
	}
	return res, nil
}

// jaccard is |a ∩ b| / |a ∪ b| for a set a and a duplicate-free list b.
func jaccard(a map[string]bool, b []string) float64 {
	inter := 0
	for _, id := range b {
		if a[id] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// Nearest returns, for vector v, the index of the most similar centroid among cs
// and its cosine similarity, ties to the lower index; -1 when cs is empty.
func Nearest(v Vector, cs []Centroid) (int, float64, error) {
	if len(cs) == 0 {
		return -1, 0, nil
	}
	u, err := unit(v, len(cs[0].Unit))
	if err != nil {
		return -1, 0, err
	}
	best, bestSim := -1, 0.0
	for i, c := range cs {
		if err := checkCentroid(c, "centroid"); err != nil {
			return -1, 0, err
		}
		if len(c.Unit) != len(u) {
			return -1, 0, fmt.Errorf("%w: centroid %d has %d, want %d", ErrDimensionMismatch, i, len(c.Unit), len(u))
		}
		if s := dot(u, c.Unit); best < 0 || s > bestSim {
			best, bestSim = i, s
		}
	}
	return best, bestSim, nil
}

// Similarity is the cosine similarity of two centroids' directions.
func Similarity(a, b Centroid) (float64, error) {
	s, err := Shift(a, b)
	return 1 - s, err
}
