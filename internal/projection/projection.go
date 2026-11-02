// Package projection fits and applies the 3D principal-component layout of a set
// of embedding vectors (RFC FQ P2): the axes along which a scope's document
// chunks differ most, so a viewer can draw each chunk where its meaning puts it.
//
// It is plain arithmetic with no dependency. Every step is deterministic — a
// fixed-seed reservoir, fixed-seed start vectors, sequential sums, a sign rule —
// so two fits of the same sample give the same basis bit for bit, and a map
// drawn twice from unchanged data does not move.
package projection

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
)

// Components is the number of principal axes kept: a 3D map.
const Components = 3

const (
	// maxIterations bounds the power iteration per component. Convergence is
	// usually far sooner; two nearly equal eigenvalues are the slow case, and
	// there any vector in their plane is as good a map axis as the exact one.
	maxIterations = 300
	// convergedAt is how close successive iterates must be (1 - |cos|).
	convergedAt = 1e-12
	// reservoirSeed / startSeed are fixed so a pass is reproducible.
	reservoirSeed1, reservoirSeed2 = 0x6c6f6f6d, 0x70726f6a
	startSeed                      = 0x7871
)

// Basis is a fitted projection: the sample mean and the top three principal
// components, each a unit vector of Dimension values.
type Basis struct {
	Dimension  int
	Mean       []float32
	Components [Components][]float32
	// ExplainedVariance is each component's share of the sample's total
	// variance, in fit order (largest first). All zero for a sample with no
	// spread.
	ExplainedVariance [Components]float64
	SampleN           int
}

// Fit returns the basis of sample: its mean and top three principal components,
// found by power iteration with deflation on the covariance. The covariance is
// never materialised — each step computes Xᵀ(X·v) over the centred rows — so the
// cost is O(n·d) memory-free work per iteration, not a d×d matrix.
//
// prev, when it has the same dimension, fixes each component's sign to agree
// with prev's (a positive dot product) so a refit keeps the map's orientation.
// Without it the sign rule is: the component's largest-magnitude entry is
// positive. A component the sample has no variance along is still a unit vector
// orthogonal to the others, with zero explained variance.
func Fit(sample [][]float32, prev *Basis) (Basis, error) {
	n := len(sample)
	if n == 0 {
		return Basis{}, errors.New("projection: empty sample")
	}
	d := len(sample[0])
	if d == 0 {
		return Basis{}, errors.New("projection: zero-dimension vectors")
	}
	for i, row := range sample {
		if len(row) != d {
			return Basis{}, fmt.Errorf("projection: vector %d has dimension %d, want %d", i, len(row), d)
		}
	}
	mean := make([]float64, d)
	for _, row := range sample {
		for j, x := range row {
			mean[j] += float64(x)
		}
	}
	for j := range mean {
		mean[j] /= float64(n)
	}
	var total float64
	for _, row := range sample {
		for j, x := range row {
			c := float64(x) - mean[j]
			total += c * c
		}
	}
	total /= float64(n)

	// cov returns C·v = (1/n)·Xᵀ(X·v) on the centred rows.
	proj := make([]float64, n)
	cov := func(v, out []float64) {
		for i, row := range sample {
			var s float64
			for j, x := range row {
				s += (float64(x) - mean[j]) * v[j]
			}
			proj[i] = s
		}
		clear(out)
		for i, row := range sample {
			w := proj[i]
			if w == 0 {
				continue
			}
			for j, x := range row {
				out[j] += w * (float64(x) - mean[j])
			}
		}
		for j := range out {
			out[j] /= float64(n)
		}
	}

	b := Basis{Dimension: d, Mean: make([]float32, d), SampleN: n}
	for j, m := range mean {
		b.Mean[j] = float32(m)
	}
	rng := rand.New(rand.NewPCG(startSeed, uint64(d)))
	var found [][]float64
	w := make([]float64, d)
	for k := range Components {
		v := make([]float64, d)
		for j := range v {
			v[j] = rng.Float64() - 0.5
		}
		orthonormalize(v, found)
		var lambda float64
		for range maxIterations {
			cov(v, w)
			orthonormalize(w, found) // deflation: stay off the components already found
			if norm(w) == 0 {
				break // no variance left off the found components: keep v
			}
			delta := 1 - math.Abs(dot(w, v))
			copy(v, w)
			if delta < convergedAt {
				break
			}
		}
		if norm(v) == 0 {
			// The start vector lay in the span of the found components (only
			// possible when d < 3): any unit vector off them will do.
			for j := range v {
				v[j] = float64(j + 1)
			}
			orthonormalize(v, found)
		}
		cov(v, w)
		lambda = dot(v, w) // Rayleigh quotient: the variance along v
		alignSign(v, prev, k, d)
		found = append(found, v)
		comp := make([]float32, d)
		for j, x := range v {
			comp[j] = float32(x)
		}
		b.Components[k] = comp
		if total > 0 && lambda > 0 {
			b.ExplainedVariance[k] = lambda / total
		}
	}
	return b, nil
}

// alignSign flips v to agree with prev's component k, or, without a comparable
// prev, to make its largest-magnitude entry positive.
func alignSign(v []float64, prev *Basis, k, d int) {
	if prev != nil && prev.Dimension == d && len(prev.Components[k]) == d {
		var s float64
		for j, x := range prev.Components[k] {
			s += float64(x) * v[j]
		}
		if s < 0 {
			negate(v)
		}
		if s != 0 {
			return
		}
	}
	big := 0
	for j := range v {
		if math.Abs(v[j]) > math.Abs(v[big]) {
			big = j
		}
	}
	if v[big] < 0 {
		negate(v)
	}
}

// Project places v on the basis: three coordinates, components · (v − mean). ok
// is false when v's dimension is not the basis's.
func (b *Basis) Project(v []float32) (xyz [Components]float64, ok bool) {
	if len(v) != b.Dimension {
		return xyz, false
	}
	for k := range Components {
		var s float64
		for j, x := range v {
			s += float64(b.Components[k][j]) * (float64(x) - float64(b.Mean[j]))
		}
		xyz[k] = s
	}
	return xyz, true
}

// Round4 rounds to 4 decimals for the wire, without a negative zero.
func Round4(x float64) float64 {
	r := math.Round(x*1e4) / 1e4
	if r == 0 {
		return 0
	}
	return r
}

// Reservoir is a uniform sample of at most Max vectors from a stream, with a
// fixed seed: the same stream gives the same sample.
type Reservoir struct {
	max  int
	seen int
	rows [][]float32
	keys []string // parallel to rows, filled by OfferKeyed
	rng  *rand.Rand
}

// NewReservoir returns an empty reservoir holding at most max vectors.
func NewReservoir(max int) *Reservoir {
	return &Reservoir{max: max, rng: rand.New(rand.NewPCG(reservoirSeed1, reservoirSeed2))}
}

// Offer considers v for the sample (Algorithm R). v is kept by reference; the
// caller must not modify it afterwards.
func (r *Reservoir) Offer(v []float32) { r.OfferKeyed("", v) }

// OfferKeyed is Offer for a vector with an identity the caller needs back with
// the sample (a UMAP fit stores its sample's chunk ids). The draws are Offer's,
// so a stream gives the same sample either way.
func (r *Reservoir) OfferKeyed(key string, v []float32) {
	r.seen++
	if r.max <= 0 {
		return
	}
	if len(r.rows) < r.max {
		r.rows = append(r.rows, v)
		r.keys = append(r.keys, key)
		return
	}
	if j := r.rng.IntN(r.seen); j < r.max {
		r.rows[j] = v
		r.keys[j] = key
	}
}

// Sample is the vectors kept; Keys their keys, index for index; Seen how many
// were offered.
func (r *Reservoir) Sample() [][]float32 { return r.rows }
func (r *Reservoir) Keys() []string      { return r.keys }
func (r *Reservoir) Seen() int           { return r.seen }

// EncodeFloat32s is v as float32 little-endian bytes.
func EncodeFloat32s(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(x))
	}
	return out
}

// DecodeFloat32s reverses EncodeFloat32s; want is the expected element count.
func DecodeFloat32s(b []byte, want int) ([]float32, error) {
	if len(b) != 4*want {
		return nil, fmt.Errorf("projection: %d bytes, want %d float32 values", len(b), want)
	}
	out := make([]float32, want)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out, nil
}

func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func norm(a []float64) float64 { return math.Sqrt(dot(a, a)) }

func negate(a []float64) {
	for i := range a {
		a[i] = -a[i]
	}
}

// orthonormalize removes v's components along each of basis (unit vectors) and
// scales the rest to unit length; a remainder too small to normalise is zeroed.
func orthonormalize(v []float64, basis [][]float64) {
	for range 2 { // twice: one Gram-Schmidt pass loses orthogonality in float64
		for _, u := range basis {
			s := dot(v, u)
			for j := range v {
				v[j] -= s * u[j]
			}
		}
	}
	n := norm(v)
	if n < 1e-300 {
		clear(v)
		return
	}
	for j := range v {
		v[j] /= n
	}
}
