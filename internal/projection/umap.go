package projection

// umap.go — UMAP, the second projection method (RFC FQ §11).
//
// PCA keeps the three directions of largest variance; on a real scope those hold
// a few percent of it, so a document's chunks scatter across the map. UMAP keeps
// neighbourhoods instead: who is near whom in the embedding space stays near on
// the map. The implementation follows umap-learn (McInnes et al.) step for step —
// an exact cosine k-NN graph, the fuzzy simplicial set, and the SGD layout with
// negative sampling — so its parameters mean what they mean there, with two
// differences that buy reproducibility: the SGD runs on one goroutine (umap-learn
// parallelises it without locks, which makes a seeded run differ between
// machines), and the layout starts from positions the caller gives (the PCA map,
// or the previous fit), never from a spectral embedding.

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
)

// UMAP defaults: umap-learn's.
const (
	DefaultNeighbors = 15
	DefaultMinDist   = 0.1
	// negativeSampleRate is the repulsive samples per attractive one.
	negativeSampleRate = 5
	// gradClip bounds one coordinate's step, as umap-learn's clip().
	gradClip = 4.0
	// initSpan is the box a cold start is rescaled into: the SGD's step sizes
	// assume coordinates of this order.
	initSpan = 10.0
	// initJitter separates points that start on the same spot (duplicate chunks
	// share an embedding and so a PCA position), which the attractive force
	// alone could never pull apart.
	initJitter = 1e-4
	// warmLearningRate and a fifth of the epochs (warmEpochDivisor) are how a
	// fit that starts from the previous map refines it (umap-learn's transform
	// uses a quarter of the rate). Refitting unchanged data from its own map
	// moved chunks, after alignment, 0.35 of the map's radius at the full rate
	// and epochs (300 points, 15 clusters). Over 12 random corpora each, a tenth
	// of the rate and a third of the epochs gave a worst case of 0.127 on 60
	// points in 4 clusters (0.081 on 300 in 15); a twentieth and a fifth gave
	// 0.070 (0.044). From a start where a fifth of 600 points (three whole new
	// clusters among them) were k-NN placed, the gentle refinement still brought
	// 10-NN agreement from 0.93 to 0.96–1.00.
	warmLearningRate = 0.05
	warmEpochDivisor = 5
	// initPCASampleMax bounds the points the cold start's PCA axes are fitted on.
	initPCASampleMax = 2000
)

// UMAPOptions steers a fit. Zero values are the defaults.
type UMAPOptions struct {
	// NNeighbors is the neighbourhood size, the point itself included (default 15).
	NNeighbors int
	// MinDist is how tightly neighbours may pack on the map (default 0.1).
	MinDist float64
	// Epochs overrides the SGD length; 0 = by sample size (see Epochs), a fifth
	// of that for a warm start.
	Epochs int
	// Seed fixes the negative sampling and the starting jitter.
	Seed uint64
	// Workers bounds the k-NN goroutines; ≤ 0 = GOMAXPROCS. The result does not
	// depend on it.
	Workers int
	// WarmStart takes init as a map already in UMAP's units (a previous fit's
	// output) and refines it as it is, instead of rescaling it and running a
	// full fit.
	WarmStart bool
	// LearningRate is the SGD's starting step; 0 = 1 (umap-learn's), or
	// warmLearningRate for a warm start.
	LearningRate float64
}

func (o UMAPOptions) withDefaults() UMAPOptions {
	if o.NNeighbors <= 0 {
		o.NNeighbors = DefaultNeighbors
	}
	if o.MinDist <= 0 {
		o.MinDist = DefaultMinDist
	}
	if o.LearningRate <= 0 {
		o.LearningRate = 1
		if o.WarmStart {
			o.LearningRate = warmLearningRate
		}
	}
	return o
}

// Epochs is the SGD length for n points: 200 at 10,000 points and above (the
// default sample cap), rising linearly to 500 at 2,000 and below. umap-learn
// steps from 500 to 200 at 10,000; a ramp avoids a map whose quality jumps when
// a scope crosses a threshold, and a small scope can afford the extra passes.
func Epochs(n int) int {
	const lo, hi, many, few = 200, 500, 10000, 2000
	switch {
	case n >= many:
		return lo
	case n <= few:
		return hi
	}
	return hi - (n-few)*(hi-lo)/(many-few)
}

// FitUMAP lays vectors out in 3D. init gives each point's starting position, one
// per vector: for a cold start (the PCA layout) it is rescaled into the SGD's
// working box, so only its shape matters; for a warm start (a previous map) it is
// taken as it is. The result is in UMAP's own units, which a warm start keeps.
//
// The same vectors in the same order with the same init and options give the
// same layout, bit for bit.
func FitUMAP(vectors [][]float32, init [][3]float64, opts UMAPOptions) ([][3]float64, error) {
	n := len(vectors)
	if n == 0 {
		return nil, errors.New("projection: empty sample")
	}
	if len(init) != n {
		return nil, fmt.Errorf("projection: %d starting positions for %d vectors", len(init), n)
	}
	d := len(vectors[0])
	for i, v := range vectors {
		if len(v) != d {
			return nil, fmt.Errorf("projection: vector %d has dimension %d, want %d", i, len(v), d)
		}
	}
	opts = opts.withDefaults()
	rng := rand.New(rand.NewPCG(opts.Seed, 0x756d6170))
	emb := startLayout(init, opts.WarmStart, rng)
	if n < 3 {
		// Too few points for a neighbourhood: the starting layout is the answer.
		return emb, nil
	}
	k := min(opts.NNeighbors, n) // the point itself counts, as in umap-learn
	unit := normalized(vectors)
	graph := cosineKNN(unit, unit, k-1, true, opts.Workers)
	heads, tails, weights := fuzzySimplicialSet(graph, k)
	epochs := opts.Epochs
	if epochs <= 0 {
		epochs = Epochs(n)
		if opts.WarmStart {
			epochs = max(1, epochs/warmEpochDivisor)
		}
	}
	a, b := FitAB(opts.MinDist, 1)
	optimizeLayout(emb, heads, tails, weights, epochs, a, b, opts.LearningRate, rng)
	return emb, nil
}

// startLayout is the SGD's starting map: init plus a tiny jitter, rescaled first
// for a cold start so its widest axis spans [0, initSpan]. umap-learn rescales
// each axis on its own; a uniform scale keeps the PCA map's proportions.
func startLayout(init [][3]float64, warm bool, rng *rand.Rand) [][3]float64 {
	emb := slices.Clone(init)
	if !warm {
		var lo, hi [3]float64
		for ax := range 3 {
			lo[ax], hi[ax] = math.Inf(1), math.Inf(-1)
			for _, p := range init {
				lo[ax], hi[ax] = min(lo[ax], p[ax]), max(hi[ax], p[ax])
			}
		}
		span := max(hi[0]-lo[0], hi[1]-lo[1], hi[2]-lo[2])
		for i, p := range init {
			for ax := range 3 {
				emb[i][ax] = 0
				if span > 0 && !math.IsInf(span, 0) && !math.IsNaN(span) {
					emb[i][ax] = initSpan * (p[ax] - lo[ax]) / span
				}
			}
		}
	}
	for i := range emb {
		for ax := range 3 {
			emb[i][ax] += rng.NormFloat64() * initJitter
		}
	}
	return emb
}

// LayoutUMAP is a UMAP map of sample ready to serve. prev[i] is sample point i's
// position on the previous version of the map when hasPrev[i] (nil for a first
// fit).
//
// With enough points carried over from the previous map (at least NNeighbors),
// the fit starts warm: each carried point where it was, each new one at its k-NN
// placement among the carried ones; the result is then aligned to the previous
// map over the carried points (Procrustes with scale), so a refit keeps the
// arrangement a reader already knows. Otherwise it starts cold from the sample's
// PCA layout and is aligned to the previous map when at least three points carry
// over, else turned to the PCA layout's orientation without rescaling it — UMAP's
// own units, which a later warm start relies on.
func LayoutUMAP(sample [][]float32, prev [][3]float64, hasPrev []bool, opts UMAPOptions) ([][3]float64, error) {
	if len(sample) == 0 {
		return nil, errors.New("projection: empty sample")
	}
	if prev != nil && (len(prev) != len(sample) || len(hasPrev) != len(sample)) {
		return nil, fmt.Errorf("projection: %d previous positions for %d vectors", len(prev), len(sample))
	}
	nn := opts.withDefaults().NNeighbors
	var carried []int
	for i := range prev {
		if hasPrev[i] {
			carried = append(carried, i)
		}
	}
	carriedPrev := make([][3]float64, len(carried))
	for c, i := range carried {
		carriedPrev[c] = prev[i]
	}
	alignTo := func(pos [][3]float64, rigidRef [][3]float64) ([][3]float64, error) {
		var s Similarity
		var err error
		if len(carried) >= 3 {
			src := make([][3]float64, len(carried))
			for c, i := range carried {
				src[c] = pos[i]
			}
			s, err = Align(src, carriedPrev)
		} else {
			s, err = Align(pos, rigidRef)
			s.Scale = 1
		}
		if err != nil {
			return nil, err
		}
		out := make([][3]float64, len(pos))
		for i, p := range pos {
			out[i] = s.Apply(p)
		}
		return out, nil
	}

	if len(carried) >= nn {
		init := make([][3]float64, len(sample))
		var fresh []int
		for i := range sample {
			if prev != nil && hasPrev[i] {
				init[i] = prev[i]
			} else {
				fresh = append(fresh, i)
			}
		}
		if len(fresh) > 0 {
			carriedVecs := make([][]float32, len(carried))
			for c, i := range carried {
				carriedVecs[c] = sample[i]
			}
			anchors, err := NewAnchors(carriedVecs, carriedPrev)
			if err != nil {
				return nil, err
			}
			freshVecs := make([][]float32, len(fresh))
			for f, i := range fresh {
				freshVecs[f] = sample[i]
			}
			placed, _ := anchors.Place(freshVecs, nn, opts.Workers)
			for f, i := range fresh {
				init[i] = placed[f]
			}
		}
		warm := opts
		warm.WarmStart = true
		pos, err := FitUMAP(sample, init, warm)
		if err != nil {
			return nil, err
		}
		return alignTo(pos, nil)
	}

	// The start only needs the PCA map's rough shape, so its axes are fitted on
	// an evenly strided subset: the power iteration costs O(n·d) per step, and
	// on the full 10,000-chunk sample it took longer than the UMAP fit itself.
	pcaSample := sample
	if stride := (len(sample) + initPCASampleMax - 1) / initPCASampleMax; stride > 1 {
		pcaSample = nil
		for i := 0; i < len(sample); i += stride {
			pcaSample = append(pcaSample, sample[i])
		}
	}
	basis, err := Fit(pcaSample, nil)
	if err != nil {
		return nil, err
	}
	init := make([][3]float64, len(sample))
	for i, v := range sample {
		init[i], _ = basis.Project(v)
	}
	cold := opts
	cold.WarmStart = false
	pos, err := FitUMAP(sample, init, cold)
	if err != nil {
		return nil, err
	}
	return alignTo(pos, init)
}

// fuzzySimplicialSet turns the k-NN graph into UMAP's weighted graph: each
// directed membership exp(−(d − rho)/sigma), symmetrised by fuzzy union
// (a + b − a·b), as a list of directed edges holding both directions of every
// pair, sorted by (head, tail). Edges too weak to be sampled in the run are
// dropped later, by the caller's epoch count.
func fuzzySimplicialSet(graph []neighbours, nNeighbors int) (heads, tails []int32, weights []float64) {
	n := len(graph)
	var total float64
	var count int
	for _, row := range graph {
		for _, d := range row.dist {
			total += float64(d)
			count++
		}
	}
	meanAll := 0.0
	if count > 0 {
		meanAll = total / float64(count)
	}
	// directed[i] = (j, w) for each of i's neighbours.
	type arc struct {
		j int32
		w float64
	}
	directed := make([][]arc, n)
	for i, row := range graph {
		sigma, rho := smoothKNN(row.dist, nNeighbors, meanAll)
		arcs := make([]arc, len(row.idx))
		for p, j := range row.idx {
			arcs[p] = arc{j: j, w: membership(row.dist[p], sigma, rho)}
		}
		slices.SortFunc(arcs, func(x, y arc) int { return int(x.j) - int(y.j) })
		directed[i] = arcs
	}
	// reverse is j's arc back to i, if j has i among its neighbours.
	reverse := func(i, j int32) (float64, bool) {
		arcs := directed[j]
		p, ok := slices.BinarySearchFunc(arcs, i, func(x arc, t int32) int { return int(x.j) - int(t) })
		if !ok {
			return 0, false
		}
		return arcs[p].w, true
	}
	// Each arc gives its own direction; an arc with no reverse twin also gives
	// the reverse direction (the twin would have given it otherwise): every
	// pair, once per direction.
	out := make([][]arc, n)
	for i := range directed {
		for _, a := range directed[i] {
			back, twin := reverse(int32(i), a.j)
			w := a.w + back - a.w*back
			out[i] = append(out[i], arc{j: a.j, w: w})
			if !twin {
				out[a.j] = append(out[a.j], arc{j: int32(i), w: w})
			}
		}
	}
	for i := range out {
		slices.SortFunc(out[i], func(x, y arc) int { return int(x.j) - int(y.j) })
		for _, a := range out[i] {
			if a.w <= 0 {
				continue
			}
			heads = append(heads, int32(i))
			tails = append(tails, a.j)
			weights = append(weights, a.w)
		}
	}
	return heads, tails, weights
}

// optimizeLayout is umap-learn's optimize_layout_euclidean on one goroutine:
// each edge is sampled in proportion to its weight, pulls its two ends together,
// and pushes the head away from negativeSampleRate random points; the learning
// rate falls linearly to zero over the epochs.
func optimizeLayout(emb [][3]float64, heads, tails []int32, weights []float64, epochs int, a, b, rate float64, rng *rand.Rand) {
	n := len(emb)
	wmax := 0.0
	for _, w := range weights {
		wmax = max(wmax, w)
	}
	if wmax == 0 {
		return
	}
	// An edge sampled less than once in the whole run is noise: umap-learn
	// zeroes it.
	floor := wmax / float64(epochs)
	perSample := make([]float64, len(weights))
	for e, w := range weights {
		if w < floor {
			perSample[e] = -1
			continue
		}
		perSample[e] = wmax / w
	}
	nextSample := slices.Clone(perSample)
	perNegative := make([]float64, len(weights))
	nextNegative := make([]float64, len(weights))
	for e, p := range perSample {
		perNegative[e] = p / negativeSampleRate
		nextNegative[e] = perNegative[e]
	}
	clip := func(x float64) float64 { return max(-gradClip, min(gradClip, x)) }
	alpha := rate
	for epoch := range epochs {
		ep := float64(epoch)
		for e := range heads {
			if perSample[e] < 0 || nextSample[e] > ep {
				continue
			}
			j, k := heads[e], tails[e]
			cur, other := &emb[j], &emb[k]
			dsq := sqDist(cur, other)
			var coeff float64
			if dsq > 0 {
				pb := pow(dsq, b)
				coeff = -2 * a * b * (pb / dsq) / (a*pb + 1)
			}
			for ax := range 3 {
				g := clip(coeff * (cur[ax] - other[ax]))
				cur[ax] += g * alpha
				other[ax] -= g * alpha
			}
			nextSample[e] += perSample[e]

			negs := int((ep - nextNegative[e]) / perNegative[e])
			for range negs {
				kk := rng.IntN(n)
				if kk == int(j) {
					continue
				}
				other := &emb[kk]
				dsq := sqDist(cur, other)
				if dsq <= 0 {
					continue
				}
				coeff := 2 * b / ((0.001 + dsq) * (a*pow(dsq, b) + 1))
				for ax := range 3 {
					cur[ax] += clip(coeff*(cur[ax]-other[ax])) * alpha
				}
			}
			nextNegative[e] += float64(negs) * perNegative[e]
		}
		alpha = rate * (1 - (ep+1)/float64(epochs))
	}
}

// pow is x^y for x > 0. math.Pow spends its time on special cases (negative,
// infinite, integral y) a squared distance never is, and it is the SGD's inner
// cost.
func pow(x, y float64) float64 { return math.Exp(y * math.Log(x)) }

func sqDist(p, q *[3]float64) float64 {
	dx, dy, dz := p[0]-q[0], p[1]-q[1], p[2]-q[2]
	return dx*dx + dy*dy + dz*dz
}

// FitAB fits UMAP's low-dimensional similarity 1/(1 + a·x^(2b)) to the target
// curve set by minDist and spread (1 inside minDist, exp(−(x − minDist)/spread)
// past it) over 300 points of [0, 3·spread], by Levenberg–Marquardt from (1, 1) —
// what umap-learn's find_ab_params does with scipy's curve_fit.
func FitAB(minDist, spread float64) (a, b float64) {
	const points = 300
	xs := make([]float64, points)
	ys := make([]float64, points)
	for i := range xs {
		x := 3 * spread * float64(i) / float64(points-1)
		xs[i] = x
		if x < minDist {
			ys[i] = 1
		} else {
			ys[i] = math.Exp(-(x - minDist) / spread)
		}
	}
	cost := func(a, b float64) float64 {
		var s float64
		for i, x := range xs {
			r := 1/(1+a*math.Pow(x, 2*b)) - ys[i]
			s += r * r
		}
		return s
	}
	a, b = 1, 1
	lambda := 1e-3
	c := cost(a, b)
	for range 500 {
		// Normal equations of the 2-parameter least squares.
		var jaa, jab, jbb, ga, gb float64
		for i, x := range xs {
			if x == 0 {
				continue // f = 1 regardless of a and b: no gradient
			}
			p := math.Pow(x, 2*b)
			den := 1 + a*p
			r := 1/den - ys[i]
			da := -p / (den * den)
			db := -a * p * 2 * math.Log(x) / (den * den)
			jaa += da * da
			jab += da * db
			jbb += db * db
			ga += da * r
			gb += db * r
		}
		for {
			m00, m11 := jaa*(1+lambda), jbb*(1+lambda)
			det := m00*m11 - jab*jab
			if det == 0 {
				return a, b
			}
			sa := -(m11*ga - jab*gb) / det
			sb := -(-jab*ga + m00*gb) / det
			na, nb := a+sa, b+sb
			if na > 0 && nb > 0 {
				if nc := cost(na, nb); nc < c {
					converged := c-nc < 1e-15
					a, b, c = na, nb, nc
					lambda = max(lambda/10, 1e-12)
					if converged {
						return a, b
					}
					break
				}
			}
			lambda *= 10
			if lambda > 1e12 {
				return a, b
			}
		}
	}
	return a, b
}
