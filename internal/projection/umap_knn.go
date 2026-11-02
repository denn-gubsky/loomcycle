package projection

import (
	"math"
	"runtime"
	"sync"
)

// normalized returns unit-length copies of vs, so a dot product is the cosine. A
// zero vector stays zero: it is at cosine distance 1 from everything.
func normalized(vs [][]float32) [][]float32 {
	out := make([][]float32, len(vs))
	for i, v := range vs {
		var s float64
		for _, x := range v {
			s += float64(x) * float64(x)
		}
		u := make([]float32, len(v))
		if s > 0 {
			inv := 1 / math.Sqrt(s)
			for j, x := range v {
				u[j] = float32(float64(x) * inv)
			}
		}
		out[i] = u
	}
	return out
}

// dot32 is a·b with eight accumulators over bounds-checked-once windows: the loop
// is the whole cost of a k-NN graph, and independent sums let the CPU overlap
// the multiply-adds (about 2.5× four accumulators on an M1). The order is fixed,
// so the result is the same on every run on one architecture (arm64 fuses the
// multiply-add, amd64 does not, so the last bits can differ between them).
func dot32(a, b []float32) float32 {
	var s0, s1, s2, s3, s4, s5, s6, s7 float32
	n := len(a)
	b = b[:n]
	j := 0
	for ; j+8 <= n; j += 8 {
		x := a[j : j+8 : j+8]
		y := b[j : j+8 : j+8]
		s0 += x[0] * y[0]
		s1 += x[1] * y[1]
		s2 += x[2] * y[2]
		s3 += x[3] * y[3]
		s4 += x[4] * y[4]
		s5 += x[5] * y[5]
		s6 += x[6] * y[6]
		s7 += x[7] * y[7]
	}
	for ; j < n; j++ {
		s0 += a[j] * b[j]
	}
	return ((s0 + s1) + (s2 + s3)) + ((s4 + s5) + (s6 + s7))
}

// neighbours is one row of a k-NN graph: indexes into the reference set and their
// cosine distances, nearest first.
type neighbours struct {
	idx  []int32
	dist []float32
}

// knnBlock is the query rows a worker scores together. Each reference row is read
// once per block and dotted with every row of it, so a 1024-dimension reference
// row (4 KB) is fetched from memory once per 32 queries instead of once per
// query: at 10,000 rows the reference set is 40 MB, far past any cache.
const knnBlock = 32

// cosineKNN finds, for every row of q, its k nearest rows of ref by cosine
// distance (1 − cos), nearest first; both are unit vectors. With self set, q is
// ref and a row is never its own neighbour. Exact brute force: every pair is
// scored, so the graph does not depend on an index's luck.
//
// Blocks of rows are split across workers goroutines (≤ 0 = GOMAXPROCS). Each
// row is computed by one goroutine scanning ref in order, and ties keep the
// lower index, so the graph is the same for any worker count.
func cosineKNN(q, ref [][]float32, k int, self bool, workers int) []neighbours {
	out := make([]neighbours, len(q))
	if k <= 0 || len(ref) == 0 {
		return out
	}
	k = min(k, len(ref))
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	workers = max(1, min(workers, (len(q)+knnBlock-1)/knnBlock))
	next := make(chan int)
	go func() {
		for i := 0; i < len(q); i += knnBlock {
			next <- i
		}
		close(next)
	}()
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var top [knnBlock]neighbours
			for start := range next {
				end := min(start+knnBlock, len(q))
				for b := range end - start {
					top[b] = neighbours{idx: make([]int32, 0, k), dist: make([]float32, 0, k)}
				}
				for j, r := range ref {
					for b := range end - start {
						i := start + b
						if self && j == i {
							continue
						}
						d := 1 - dot32(q[i], r)
						if d < 0 {
							d = 0
						}
						top[b].insert(int32(j), d, k)
					}
				}
				for b := range end - start {
					out[start+b] = top[b]
				}
			}
		}()
	}
	wg.Wait()
	return out
}

// insert adds neighbour j at distance d to a nearest-first list of at most k,
// when it is near enough. An equal distance goes after, so the lower index (the
// one scanned first) wins a tie.
func (nb *neighbours) insert(j int32, d float32, k int) {
	n := len(nb.idx)
	if n == k && d >= nb.dist[k-1] {
		return
	}
	p := n
	for p > 0 && nb.dist[p-1] > d {
		p--
	}
	if n < k {
		nb.idx = append(nb.idx, 0)
		nb.dist = append(nb.dist, 0)
	}
	copy(nb.idx[p+1:], nb.idx[p:len(nb.idx)-1])
	copy(nb.dist[p+1:], nb.dist[p:len(nb.dist)-1])
	nb.idx[p], nb.dist[p] = j, d
}

// smoothKNN is UMAP's per-point scale: rho, the distance to the nearest neighbour
// that is not a duplicate, and sigma, chosen by binary search so the memberships
// exp(−(d − rho)/sigma) over the neighbours sum to log2(nNeighbors). It is what
// makes a sparse region and a dense one each give their points the same total
// neighbourhood weight. meanAll is the mean distance over the whole graph, the
// floor for a point whose neighbours are all duplicates.
func smoothKNN(dist []float32, nNeighbors int, meanAll float64) (sigma, rho float64) {
	const (
		iterations  = 64
		tolerance   = 1e-5
		minDistance = 1e-3 // umap-learn's MIN_K_DIST_SCALE
	)
	target := math.Log2(float64(nNeighbors))
	for _, d := range dist {
		if d > 0 {
			rho = float64(d)
			break
		}
	}
	lo, hi, mid := 0.0, math.Inf(1), 1.0
	for range iterations {
		var psum float64
		for _, d := range dist {
			if dd := float64(d) - rho; dd > 0 {
				psum += math.Exp(-dd / mid)
			} else {
				psum++
			}
		}
		if math.Abs(psum-target) < tolerance {
			break
		}
		if psum > target {
			hi = mid
			mid = (lo + hi) / 2
		} else {
			lo = mid
			if math.IsInf(hi, 1) {
				mid *= 2
			} else {
				mid = (lo + hi) / 2
			}
		}
	}
	sigma = mid
	if rho > 0 {
		var mean float64
		for _, d := range dist {
			mean += float64(d)
		}
		if len(dist) > 0 {
			mean /= float64(len(dist))
		}
		sigma = max(sigma, minDistance*mean)
	} else {
		sigma = max(sigma, minDistance*meanAll)
	}
	return sigma, rho
}

// membership is the fuzzy weight of a neighbour at distance d from a point with
// scale (sigma, rho): 1 inside rho, decaying past it.
func membership(d float32, sigma, rho float64) float64 {
	dd := float64(d) - rho
	if dd <= 0 || sigma == 0 {
		return 1
	}
	return math.Exp(-dd / sigma)
}
