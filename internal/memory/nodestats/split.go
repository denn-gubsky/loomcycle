package nodestats

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
)

// Split defaults (RFC GM section 5.2).
const (
	SplitKMin              = 2
	SplitKMax              = 4
	SplitMaxIterations     = 100
	SplitStabilityRuns     = 10
	SplitSubsampleFraction = 0.8
)

// stabilityStream offsets the PCG stream of the stability runs from the
// streams of the k selection (which use k itself), so no run replays the
// reference clustering's random draws.
const stabilityStream = 1 << 32

// SplitOptions configures Split. Start from DefaultSplitOptions; every field
// is validated and none has a zero-means-default reading.
type SplitOptions struct {
	// Seed seeds every random draw. The same seed on the same input gives
	// the same result.
	Seed uint64
	// KMin and KMax bound the number of clusters tried (2..4 by default).
	KMin, KMax int
	// MinGroup is the smallest group kept; smaller ones are dropped.
	MinGroup int
	// MinNodeMembers is the fewest members a node needs to be clustered.
	MinNodeMembers int
	// MaxIterations bounds each k-means run.
	MaxIterations int
	// StabilityRuns is how many reruns the stability score averages; 0
	// skips it.
	StabilityRuns int
	// SubsampleFraction is the share of members each stability run clusters,
	// in (0, 1].
	SubsampleFraction float64
}

// DefaultSplitOptions returns the RFC GM defaults with the given seed.
func DefaultSplitOptions(seed uint64) SplitOptions {
	return SplitOptions{
		Seed:              seed,
		KMin:              SplitKMin,
		KMax:              SplitKMax,
		MinGroup:          MinMembers,
		MinNodeMembers:    SplitMinMembers,
		MaxIterations:     SplitMaxIterations,
		StabilityRuns:     SplitStabilityRuns,
		SubsampleFraction: SplitSubsampleFraction,
	}
}

func (o SplitOptions) validate() error {
	switch {
	case o.KMin < 2:
		return fmt.Errorf("%w: KMin = %d, need >= 2", ErrInvalidArgument, o.KMin)
	case o.KMax < o.KMin:
		return fmt.Errorf("%w: KMax = %d < KMin = %d", ErrInvalidArgument, o.KMax, o.KMin)
	case o.MinGroup < 1:
		return fmt.Errorf("%w: MinGroup = %d", ErrInvalidArgument, o.MinGroup)
	case o.MinNodeMembers < o.KMin:
		return fmt.Errorf("%w: MinNodeMembers = %d < KMin", ErrInvalidArgument, o.MinNodeMembers)
	case o.MaxIterations < 1:
		return fmt.Errorf("%w: MaxIterations = %d", ErrInvalidArgument, o.MaxIterations)
	case o.StabilityRuns < 0:
		return fmt.Errorf("%w: StabilityRuns = %d", ErrInvalidArgument, o.StabilityRuns)
	case !(o.SubsampleFraction > 0 && o.SubsampleFraction <= 1):
		return fmt.Errorf("%w: SubsampleFraction = %v", ErrInvalidArgument, o.SubsampleFraction)
	}
	return nil
}

// KScore is the separation one k reached.
type KScore struct {
	K          int
	Silhouette float64
}

// Group is one proposed narrower group: its member ids (sorted) and centroid.
type Group struct {
	Members  []string
	Centroid Centroid
}

// SplitResult is the outcome of Split.
//
// K is the chosen number of clusters, 0 when no k could be clustered (the
// members point in fewer than KMin distinct directions): then there is no
// candidate. Groups are the clusters of at least MinGroup members, largest
// first (ties by first member id); fewer than two groups means no split.
// Dropped counts the members of the dropped small clusters.
//
// Stability is the mean adjusted Rand index between the chosen clustering
// and StabilityRuns re-clusterings, each with its own seed on a random
// SubsampleFraction of the members (compared on the members both saw). It is
// about 1 when the grouping recurs whatever the seed and sample, and about 0
// when the reruns agree no better than chance (k-means imposing a partition
// on structureless data). Each rerun uses the chosen K. StabilityRuns counts
// the reruns that completed; when it is 0 the score was not measured and
// Stability is 0 by default, not by finding.
//
// Silhouette is not a split signal on its own: the simplified silhouette
// reads about 0.5 on structureless data. Read it with Stability.
type SplitResult struct {
	K             int
	Silhouette    float64
	Tried         []KScore
	Groups        []Group
	Dropped       int
	Stability     float64
	StabilityRuns int
}

// Split clusters one node's members with spherical k-means for each k in
// [KMin, KMax] and keeps the k with the highest silhouette (ties to the
// smaller k).
//
// Clustering: k-means++ initialisation over cosine distance from a PCG source
// seeded (Seed, k), then Lloyd iterations assigning each member to the most
// similar centre (ties to the lower index) and re-centring on the normalised
// mean, until no assignment changes or MaxIterations. A cluster left empty is
// re-seeded with the member farthest from its own centre.
//
// Silhouette is the simplified (centroid) silhouette in cosine distance: for
// each member, a = 1 - cos to its own centre, b = 1 - cos to the nearest
// other centre, s = (b - a) / max(a, b), and s = 0 for a member alone in its
// cluster; the score is the mean s. It costs O(n x k x dimension) where the
// pairwise silhouette costs O(n^2 x dimension) — a node may hold thousands of
// members.
func Split(members []Member, opts SplitOptions) (SplitResult, error) {
	if err := opts.validate(); err != nil {
		return SplitResult{}, err
	}
	if len(members) < opts.MinNodeMembers {
		return SplitResult{}, fmt.Errorf("%w: %d < %d", ErrTooFewMembers, len(members), opts.MinNodeMembers)
	}
	us, err := unitMembers(members)
	if err != nil {
		return SplitResult{}, err
	}

	var res SplitResult
	var bestLabels []int
	var bestCenters [][]float64
	for k := opts.KMin; k <= opts.KMax && k <= len(us); k++ {
		rng := rand.New(rand.NewPCG(opts.Seed, uint64(k)))
		labels, centers, err := kmeans(us, k, rng, opts.MaxIterations)
		if err != nil {
			continue
		}
		s := silhouette(us, labels, centers)
		res.Tried = append(res.Tried, KScore{K: k, Silhouette: s})
		if bestLabels == nil || s > res.Silhouette {
			res.K, res.Silhouette, bestLabels, bestCenters = k, s, labels, centers
		}
	}
	if bestLabels == nil {
		return res, nil
	}

	clusters := make([][]int, len(bestCenters))
	for i, l := range bestLabels {
		clusters[l] = append(clusters[l], i)
	}
	for _, idx := range clusters {
		if len(idx) < opts.MinGroup {
			res.Dropped += len(idx)
			continue
		}
		var s Sum
		ids := make([]string, len(idx))
		for j, i := range idx {
			s.addUnit(us[i])
			ids[j] = members[i].ID
		}
		c, err := s.Centroid()
		if err != nil {
			res.Dropped += len(idx)
			continue
		}
		sort.Strings(ids)
		res.Groups = append(res.Groups, Group{Members: ids, Centroid: c})
	}
	sort.Slice(res.Groups, func(i, j int) bool {
		a, b := res.Groups[i], res.Groups[j]
		if len(a.Members) != len(b.Members) {
			return len(a.Members) > len(b.Members)
		}
		return a.Members[0] < b.Members[0]
	})

	res.Stability, res.StabilityRuns = stability(us, bestLabels, res.K, opts)
	return res, nil
}

// stability reruns k-means with K fixed on seeded subsamples and averages the
// adjusted Rand index against the reference labels.
func stability(us [][]float64, ref []int, k int, opts SplitOptions) (float64, int) {
	m := int(math.Round(opts.SubsampleFraction * float64(len(us))))
	if m < k {
		return 0, 0
	}
	var total float64
	runs := 0
	for r := 0; r < opts.StabilityRuns; r++ {
		rng := rand.New(rand.NewPCG(opts.Seed, stabilityStream+uint64(r)))
		pick := rng.Perm(len(us))[:m]
		sort.Ints(pick)
		sub := make([][]float64, m)
		refSub := make([]int, m)
		for i, p := range pick {
			sub[i] = us[p]
			refSub[i] = ref[p]
		}
		labels, _, err := kmeans(sub, k, rng, opts.MaxIterations)
		if err != nil {
			continue
		}
		total += adjustedRand(refSub, labels)
		runs++
	}
	if runs == 0 {
		return 0, 0
	}
	return total / float64(runs), runs
}

// kmeans is spherical k-means over unit vectors. It fails with ErrDegenerate
// when the points hold fewer than k distinct directions.
func kmeans(us [][]float64, k int, rng *rand.Rand, maxIter int) ([]int, [][]float64, error) {
	n := len(us)
	if n < k {
		return nil, nil, ErrTooFewMembers
	}
	centers := make([][]float64, 0, k)
	centers = append(centers, append([]float64(nil), us[rng.IntN(n)]...))
	w := make([]float64, n)
	for len(centers) < k {
		var total float64
		for i, u := range us {
			best := math.Inf(-1)
			for _, c := range centers {
				best = math.Max(best, dot(u, c))
			}
			d := math.Max(0, 1-best)
			w[i] = d * d
			total += w[i]
		}
		if total <= 1e-18 {
			return nil, nil, ErrDegenerate
		}
		r := rng.Float64() * total
		pick := -1
		for i, x := range w {
			if x == 0 {
				continue
			}
			pick = i
			r -= x
			if r < 0 {
				break
			}
		}
		centers = append(centers, append([]float64(nil), us[pick]...))
	}

	labels := make([]int, n)
	for i := range labels {
		labels[i] = -1
	}
	sims := make([]float64, n)
	// The loop ends on an assignment pass, so the labels returned are each
	// member's nearest of the centres returned.
	for it := 0; ; it++ {
		changed := false
		for i, u := range us {
			bl, bs := 0, math.Inf(-1)
			for j, c := range centers {
				if s := dot(u, c); s > bs {
					bl, bs = j, s
				}
			}
			if labels[i] != bl {
				labels[i], changed = bl, true
			}
			sims[i] = bs
		}
		if !changed || it == maxIter-1 {
			break
		}
		recenter(us, labels, sims, centers)
	}
	return labels, centers, nil
}

// recenter moves each centre to its cluster's normalised mean. An empty (or
// cancelling) cluster takes the member least similar to its own centre, which
// changes that member's label so the next assignment pass runs.
func recenter(us [][]float64, labels []int, sims []float64, centers [][]float64) {
	dim := len(us[0])
	sums := make([][]float64, len(centers))
	counts := make([]int, len(centers))
	for j := range sums {
		sums[j] = make([]float64, dim)
	}
	for i, u := range us {
		l := labels[i]
		counts[l]++
		for d, x := range u {
			sums[l][d] += x
		}
	}
	for j := range centers {
		r := norm(sums[j])
		if counts[j] > 0 && r > 1e-12 {
			for d := range sums[j] {
				centers[j][d] = sums[j][d] / r
			}
			continue
		}
		far, farSim := -1, math.Inf(1)
		for i, s := range sims {
			if counts[labels[i]] > 1 && s < farSim {
				far, farSim = i, s
			}
		}
		if far < 0 {
			continue
		}
		copy(centers[j], us[far])
		counts[labels[far]]--
		counts[j]++
		labels[far] = j
		sims[far] = 1
	}
}

// silhouette is the simplified (centroid) silhouette in cosine distance; see
// Split.
func silhouette(us [][]float64, labels []int, centers [][]float64) float64 {
	size := make([]int, len(centers))
	for _, l := range labels {
		size[l]++
	}
	var total float64
	for i, u := range us {
		if size[labels[i]] < 2 {
			continue
		}
		a := 1 - dot(u, centers[labels[i]])
		b := math.Inf(1)
		for j, c := range centers {
			if j != labels[i] {
				b = math.Min(b, 1-dot(u, c))
			}
		}
		if m := math.Max(a, b); m > 0 {
			total += (b - a) / m
		}
	}
	return total / float64(len(us))
}

// adjustedRand is the adjusted Rand index of two labelings of the same items
// (Hubert and Arabie 1985): 1 for identical partitions, about 0 for chance
// agreement. When both partitions are trivial in the same way (the index
// equals its maximum and its expectation) it is 1.
func adjustedRand(a, b []int) float64 {
	n := len(a)
	pair := func(x int) float64 { return float64(x) * float64(x-1) / 2 }
	type cell struct{ i, j int }
	cont := map[cell]int{}
	ra, rb := map[int]int{}, map[int]int{}
	for i := range a {
		cont[cell{a[i], b[i]}]++
		ra[a[i]]++
		rb[b[i]]++
	}
	// Map iteration order varies; the sums are of integers-as-floats small
	// enough to be exact, so the order cannot change the result.
	var index, sa, sb float64
	for _, c := range cont {
		index += pair(c)
	}
	for _, c := range ra {
		sa += pair(c)
	}
	for _, c := range rb {
		sb += pair(c)
	}
	expected := sa * sb / pair(n)
	maxIndex := (sa + sb) / 2
	if maxIndex == expected {
		return 1
	}
	return (index - expected) / (maxIndex - expected)
}
