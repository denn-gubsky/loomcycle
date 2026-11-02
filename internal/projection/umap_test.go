package projection

import (
	"cmp"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"time"
)

// clusteredCorpus draws `docs` documents of `per` chunks each in d dimensions:
// every document has a random direction and its chunks scatter around it. With
// many documents in random directions, PCA's top three components hold a small
// share of the variance — the shape of a real scope, where a 3D PCA map mixes
// documents together. label[i] is chunk i's document.
func clusteredCorpus(docs, per, d int, spread float64, seed uint64) (vecs [][]float32, label []int) {
	rng := rand.New(rand.NewPCG(seed, 17))
	for c := range docs {
		centre := make([]float64, d)
		for j := range centre {
			centre[j] = rng.NormFloat64()
		}
		for range per {
			v := make([]float32, d)
			for j := range v {
				v[j] = float32(centre[j] + spread*rng.NormFloat64())
			}
			vecs = append(vecs, v)
			label = append(label, c)
		}
	}
	return vecs, label
}

// pcaLayout is the product's PCA map of vecs.
func pcaLayout(t *testing.T, vecs [][]float32) ([][3]float64, Basis) {
	t.Helper()
	b, err := Fit(vecs, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := make([][3]float64, len(vecs))
	for i, v := range vecs {
		p, ok := b.Project(v)
		if !ok {
			t.Fatal("project")
		}
		out[i] = p
	}
	return out, b
}

// knnAgreement is the share of each point's k nearest 3D neighbours (Euclidean,
// self excluded) that carry its label, averaged over the points — the RFC's
// cluster-agreement metric.
func knnAgreement(pos [][3]float64, label []int, k int) float64 {
	var total float64
	for i := range pos {
		type nd struct {
			j int
			d float64
		}
		ds := make([]nd, 0, len(pos)-1)
		for j := range pos {
			if j != i {
				ds = append(ds, nd{j, sqDist(&pos[i], &pos[j])})
			}
		}
		slices.SortFunc(ds, func(a, b nd) int {
			if a.d < b.d {
				return -1
			}
			if a.d > b.d {
				return 1
			}
			return a.j - b.j
		})
		same := 0
		for _, n := range ds[:k] {
			if label[n.j] == label[i] {
				same++
			}
		}
		total += float64(same) / float64(k)
	}
	return total / float64(len(pos))
}

// rmsRadius is the RMS distance of pos from its centroid.
func rmsRadius(pos [][3]float64) float64 {
	var c [3]float64
	for _, p := range pos {
		for ax := range 3 {
			c[ax] += p[ax] / float64(len(pos))
		}
	}
	var s float64
	for _, p := range pos {
		s += sqDist(&p, &c)
	}
	return math.Sqrt(s / float64(len(pos)))
}

// TestUMAP_KeepsDocumentsTogetherOnASyntheticCorpus — the RFC's build gate: on a
// corpus whose top three principal components hold little of the variance, the
// UMAP map keeps a chunk's 3D neighbours in its own document far more often than
// the PCA map does (by at least 0.3 of 10-NN agreement).
func TestUMAP_KeepsDocumentsTogetherOnASyntheticCorpus(t *testing.T) {
	vecs, label := clusteredCorpus(60, 10, 128, 1.0, 1)
	pca, basis := pcaLayout(t, vecs)
	ev := basis.ExplainedVariance[0] + basis.ExplainedVariance[1] + basis.ExplainedVariance[2]
	if ev > 0.15 {
		t.Fatalf("the corpus is too easy for PCA: its top 3 components hold %.3f of the variance", ev)
	}
	start := time.Now()
	umap, err := LayoutUMAP(vecs, nil, nil, UMAPOptions{Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	p, u := knnAgreement(pca, label, 10), knnAgreement(umap, label, 10)
	t.Logf("n=%d d=%d: PCA explained %.3f; 10-NN document agreement PCA %.3f, UMAP %.3f (Δ %.3f); fit %v",
		len(vecs), len(vecs[0]), ev, p, u, u-p, took)
	if u-p < 0.3 {
		t.Fatalf("UMAP agreement %.3f is not above PCA's %.3f by 0.3", u, p)
	}
}

// TestUMAP_StableAcrossTwoPassesOnUnchangedData — two fits of the same vectors
// give the same map bit for bit, whatever the number of k-NN workers; the seed
// does reach the fit.
func TestUMAP_StableAcrossTwoPassesOnUnchangedData(t *testing.T) {
	vecs, _ := clusteredCorpus(8, 15, 32, 0.5, 2)
	a, err := LayoutUMAP(vecs, nil, nil, UMAPOptions{Seed: 3, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := LayoutUMAP(vecs, nil, nil, UMAPOptions{Seed: 3, Workers: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two fits of unchanged data differ")
	}
	c, _ := LayoutUMAP(vecs, nil, nil, UMAPOptions{Seed: 4})
	if reflect.DeepEqual(a, c) {
		t.Fatal("the seed does not reach the fit: a different seed gave the same map")
	}
}

// TestUMAP_NewChunkPlacedWithoutRefit — a chunk outside the sample is placed among
// its own document's anchors, by k-NN against them, and placing it moves no
// anchor.
func TestUMAP_NewChunkPlacedWithoutRefit(t *testing.T) {
	vecs, label := clusteredCorpus(12, 21, 48, 0.6, 3)
	// Every 7th chunk is held out: written after the fit.
	var sample, late [][]float32
	var sampleLabel, lateLabel []int
	for i, v := range vecs {
		if i%7 == 0 {
			late, lateLabel = append(late, v), append(lateLabel, label[i])
		} else {
			sample, sampleLabel = append(sample, v), append(sampleLabel, label[i])
		}
	}
	pos, err := LayoutUMAP(sample, nil, nil, UMAPOptions{Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	frozen := slices.Clone(pos)
	anchors, err := NewAnchors(sample, pos)
	if err != nil {
		t.Fatal(err)
	}
	placed, ok := anchors.Place(late, 0, 0)
	if !reflect.DeepEqual(pos, frozen) {
		t.Fatal("placing new chunks moved an anchor")
	}
	var agree float64
	for i, p := range placed {
		if !ok[i] {
			t.Fatalf("chunk %d not placed", i)
		}
		// The share of its 10 nearest anchors on the map that are its document's.
		type nd struct {
			j int
			d float64
		}
		var ds []nd
		for j := range pos {
			ds = append(ds, nd{j, sqDist(&p, &pos[j])})
		}
		slices.SortFunc(ds, func(a, b nd) int { return cmp.Compare(a.d, b.d) })
		same := 0
		for _, n := range ds[:10] {
			if sampleLabel[n.j] == lateLabel[i] {
				same++
			}
		}
		agree += float64(same) / 10
	}
	agree /= float64(len(placed))
	t.Logf("placed %d late chunks; 10-NN agreement with their document's anchors %.3f", len(placed), agree)
	if agree < 0.9 {
		t.Fatalf("late chunks sit among their document's anchors %.3f of the time, want ≥ 0.9", agree)
	}
	// A vector of another dimension is refused, not placed at the origin.
	if _, ok := anchors.Place([][]float32{{1, 2}}, 0, 0); ok[0] {
		t.Fatal("a vector of another dimension was placed")
	}
}

// meanDisplacement is the mean distance between next and ref point for point,
// over ref's RMS radius.
func meanDisplacement(next, ref [][3]float64) float64 {
	var sum float64
	for i := range next {
		sum += math.Sqrt(sqDist(&next[i], &ref[i]))
	}
	return sum / float64(len(next)) / rmsRadius(ref)
}

// TestUMAP_RefitKeepsTheArrangement — a refit of unchanged data, started from the
// previous map and aligned to it, keeps chunks within a tenth of the map's RMS
// radius of where they were on average. A cold fit with another seed, aligned
// the same way, is logged for contrast: that drift is what the warm start
// removes.
func TestUMAP_RefitKeepsTheArrangement(t *testing.T) {
	vecs, _ := clusteredCorpus(15, 20, 64, 0.6, 4)
	v1, err := LayoutUMAP(vecs, nil, nil, UMAPOptions{Seed: 9})
	if err != nil {
		t.Fatal(err)
	}
	all := make([]bool, len(vecs))
	for i := range all {
		all[i] = true
	}
	v2, err := LayoutUMAP(vecs, v1, all, UMAPOptions{Seed: 9})
	if err != nil {
		t.Fatal(err)
	}
	warm := meanDisplacement(v2, v1)

	cold, _ := LayoutUMAP(vecs, nil, nil, UMAPOptions{Seed: 10})
	s, _ := Align(cold, v1)
	for i := range cold {
		cold[i] = s.Apply(cold[i])
	}
	t.Logf("aligned displacement / RMS radius: warm refit %.3f, cold fit with another seed %.3f", warm, meanDisplacement(cold, v1))
	if warm >= 0.1 {
		t.Fatalf("a refit of unchanged data moved chunks %.3f of the radius on average, want < 0.1", warm)
	}
	// The map keeps its scale: a refit is not a zoom.
	if r := rmsRadius(v2) / rmsRadius(v1); r < 0.9 || r > 1.1 {
		t.Fatalf("a refit changed the map's radius by a factor %.3f", r)
	}
}

// TestUMAP_RefitWithNewChunksKeepsTheCarriedOnesInPlace — when a fifth of the
// sample is new (whole new documents among it), the chunks carried over stay
// near their previous places and the new documents still come out together.
func TestUMAP_RefitWithNewChunksKeepsTheCarriedOnesInPlace(t *testing.T) {
	vecs, label := clusteredCorpus(20, 15, 64, 0.8, 5)
	var oldIdx []int
	for i := range vecs {
		if label[i] < 17 && i%5 != 0 {
			oldIdx = append(oldIdx, i)
		}
	}
	oldVecs := make([][]float32, len(oldIdx))
	for c, i := range oldIdx {
		oldVecs[c] = vecs[i]
	}
	v1, err := LayoutUMAP(oldVecs, nil, nil, UMAPOptions{Seed: 2})
	if err != nil {
		t.Fatal(err)
	}
	prev := make([][3]float64, len(vecs))
	has := make([]bool, len(vecs))
	for c, i := range oldIdx {
		prev[i], has[i] = v1[c], true
	}
	v2, err := LayoutUMAP(vecs, prev, has, UMAPOptions{Seed: 2})
	if err != nil {
		t.Fatal(err)
	}
	carried := make([][3]float64, len(oldIdx))
	for c, i := range oldIdx {
		carried[c] = v2[i]
	}
	disp := meanDisplacement(carried, v1)
	agree := knnAgreement(v2, label, 10)
	t.Logf("carried %d of %d: displacement %.3f of the radius; 10-NN document agreement %.3f", len(oldIdx), len(vecs), disp, agree)
	if disp >= 0.15 {
		t.Errorf("carried chunks moved %.3f of the radius, want < 0.15", disp)
	}
	if agree < 0.9 {
		t.Errorf("10-NN document agreement after the refit %.3f, want ≥ 0.9", agree)
	}
}

// TestAlign_RecoversAKnownSimilarity — a cloud rotated (with a reflection),
// scaled and shifted is mapped back onto the original exactly.
func TestAlign_RecoversAKnownSimilarity(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	ref := make([][3]float64, 50)
	for i := range ref {
		ref[i] = [3]float64{rng.NormFloat64() * 3, rng.NormFloat64() * 2, rng.NormFloat64()}
	}
	// An orthogonal matrix with determinant −1: a rotation about z, then a
	// reflection through the x–y plane.
	th := 0.7
	q := [3][3]float64{{math.Cos(th), -math.Sin(th), 0}, {math.Sin(th), math.Cos(th), 0}, {0, 0, -1}}
	src := make([][3]float64, len(ref))
	for i, p := range ref {
		for c := range 3 {
			var v float64
			for r := range 3 {
				v += p[r] * q[r][c]
			}
			src[i][c] = 2.5*v + []float64{4, -1, 7}[c]
		}
	}
	s, err := Align(src, ref)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range src {
		got := s.Apply(p)
		if d := math.Sqrt(sqDist(&got, &ref[i])); d > 1e-9 {
			t.Fatalf("point %d lands %.3g from its original", i, d)
		}
	}
	// Degenerate input aligns to the identity rather than failing.
	flat := [][3]float64{{1, 1, 1}, {1, 1, 1}}
	if _, err := Align(flat, flat); err != nil {
		t.Fatal(err)
	}
}

// TestFitAB_MatchesUMAPLearn — the curve parameters for min_dist 0.1, spread 1 are
// umap-learn's (a ≈ 1.577, b ≈ 0.895).
func TestFitAB_MatchesUMAPLearn(t *testing.T) {
	a, b := FitAB(0.1, 1)
	if math.Abs(a-1.5769) > 2e-3 || math.Abs(b-0.8951) > 2e-3 {
		t.Fatalf("FitAB(0.1, 1) = (%.4f, %.4f), want ≈ (1.5769, 0.8951)", a, b)
	}
}

// TestCosineKNN_MatchesBruteForceForAnyWorkerCount — the k-NN graph is the exact
// one, nearest first, independent of the worker split.
func TestCosineKNN_MatchesBruteForceForAnyWorkerCount(t *testing.T) {
	vecs, _ := clusteredCorpus(5, 9, 16, 0.8, 6)
	unit := normalized(vecs)
	one := cosineKNN(unit, unit, 6, true, 1)
	many := cosineKNN(unit, unit, 6, true, 7)
	if !reflect.DeepEqual(one, many) {
		t.Fatal("the graph depends on the worker count")
	}
	for i, row := range one {
		type nd struct {
			j int32
			d float32
		}
		var all []nd
		for j := range unit {
			if j != i {
				all = append(all, nd{int32(j), max(0, 1-dot32(unit[i], unit[j]))})
			}
		}
		slices.SortStableFunc(all, func(a, b nd) int {
			if a.d < b.d {
				return -1
			}
			if a.d > b.d {
				return 1
			}
			return 0
		})
		for p := range row.idx {
			if row.idx[p] != all[p].j || row.dist[p] != all[p].d {
				t.Fatalf("row %d neighbour %d = (%d, %g), want (%d, %g)", i, p, row.idx[p], row.dist[p], all[p].j, all[p].d)
			}
		}
	}
}

// TestFitUMAP_TinySamples — one or two points have no neighbourhood: the start
// layout comes back, without an error.
func TestFitUMAP_TinySamples(t *testing.T) {
	for _, n := range []int{1, 2, 3} {
		vecs, _ := clusteredCorpus(1, n, 4, 0.5, 8)
		init := make([][3]float64, n)
		for i := range init {
			init[i] = [3]float64{float64(i), 0, 0}
		}
		pos, err := FitUMAP(vecs, init, UMAPOptions{})
		if err != nil || len(pos) != n {
			t.Fatalf("n=%d: %v %v", n, pos, err)
		}
		for _, p := range pos {
			for _, x := range p {
				if math.IsNaN(x) || math.IsInf(x, 0) {
					t.Fatalf("n=%d: non-finite coordinate %v", n, pos)
				}
			}
		}
	}
}

// BenchmarkLayoutUMAP_10KBy1024 is a fit at the default sample cap and a
// common embedding width: 10,000 chunks of 1,024 dimensions in 400 documents.
func BenchmarkLayoutUMAP_10KBy1024(b *testing.B) {
	vecs, _ := clusteredCorpus(400, 25, 1024, 1.0, 11)
	b.ResetTimer()
	for range b.N {
		if _, err := LayoutUMAP(vecs, nil, nil, UMAPOptions{Seed: 1}); err != nil {
			b.Fatal(err)
		}
	}
}
