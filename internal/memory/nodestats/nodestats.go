// Package nodestats computes the statistics RFC GM (the classification graph)
// keeps for each concept of a taxonomy: count and centroid, spread, exemplars,
// nearest siblings, distinctive terms, misfits and split candidates.
//
// It is pure: vectors and texts in, numbers out. It reads no store and calls
// no model, so the runtime pass and the standalone measurement program share
// one implementation and must agree on a fixture.
//
// GEOMETRY. Every vector is L2-normalised on the way in and compared by cosine
// similarity; the work is done in float64 whatever the input precision. A zero
// vector, a NaN or an infinity, or a vector whose dimension differs from the
// others is refused with an error, never a panic.
//
// DETERMINISM. For the same input (in the same order) and the same seed every
// function returns the same answer: rankings break ties by id, and the only
// randomness (k-means initialisation, stability subsamples) comes from a
// math/rand/v2 PCG source seeded by the caller. Floating-point sums run in
// input order, so reordering the members may move a result in its last bits.
//
// SETS ARE THE CALLER'S. A broader node's members are the union of its
// narrower nodes' members and its own, a document counted once however many
// routes reach it. This package takes the member set it is given; Sum is an
// additive accumulator for callers whose sets are known to be disjoint.
package nodestats

import (
	"errors"
	"fmt"
	"math"
)

// Defaults named by RFC GM section 3.1 and 5.2.
const (
	// MinMembers is the fewest members a node needs before its statistics
	// are computed, and the smallest group a split may propose.
	MinMembers = 3
	// KExemplars is how many members nearest the centroid are reported.
	KExemplars = 3
	// KSiblings is how many sibling nodes nearest by centroid are reported.
	KSiblings = 3
	// TopTerms is how many distinctive terms are reported.
	TopTerms = 10
	// SplitMinMembers is the fewest members a node needs before it is
	// clustered for a split.
	SplitMinMembers = 20
	// MisfitMargin is how much nearer another node's centroid than its own a
	// member must be before it is a misfit (RFC GM section 5.1). Set by the P0
	// measurement of 2026-11-17: at 0.05 the development tenant's 206 placed
	// documents gave 4 top-level and 18 leaf misfits, against 29 / 33 at 0.02.
	// Specific to the embedding model and annotation prompt it was measured on.
	MisfitMargin = 0.05
)

// Errors. Each is returned wrapped with the offending id or index.
var (
	// ErrEmpty: a set that must hold at least one vector (or text) was empty.
	ErrEmpty = errors.New("nodestats: empty set")
	// ErrZeroVector: a vector has length zero and so no direction.
	ErrZeroVector = errors.New("nodestats: zero vector")
	// ErrNonFinite: a vector holds a NaN or an infinity.
	ErrNonFinite = errors.New("nodestats: non-finite component")
	// ErrDimensionMismatch: vectors of one call differ in dimension.
	ErrDimensionMismatch = errors.New("nodestats: dimension mismatch")
	// ErrDuplicateID: an id appears twice where ids must be unique.
	ErrDuplicateID = errors.New("nodestats: duplicate id")
	// ErrDegenerate: the members' unit vectors cancel out exactly, so the
	// centroid has no direction.
	ErrDegenerate = errors.New("nodestats: degenerate centroid")
	// ErrInvalidArgument: a parameter is out of range.
	ErrInvalidArgument = errors.New("nodestats: invalid argument")
	// ErrTooFewMembers: a node is below the size an operation needs.
	ErrTooFewMembers = errors.New("nodestats: too few members")
)

// Vector is an embedding as the store keeps it.
type Vector []float32

// Member is one member of a node: its id (a document id, say) and the
// embedding of its annotation.
type Member struct {
	ID  string
	Vec Vector
}

// Scored is an id with a cosine similarity.
type Scored struct {
	ID    string
	Score float64
}

// unit returns v normalised to length 1 in float64. dim, when positive, is
// the dimension v must have.
func unit(v Vector, dim int) ([]float64, error) {
	if len(v) == 0 {
		return nil, ErrEmpty
	}
	if dim > 0 && len(v) != dim {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrDimensionMismatch, len(v), dim)
	}
	out := make([]float64, len(v))
	var ss float64
	for i, x := range v {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, ErrNonFinite
		}
		out[i] = f
		ss += f * f
	}
	if ss == 0 {
		return nil, ErrZeroVector
	}
	n := math.Sqrt(ss)
	for i := range out {
		out[i] /= n
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

// Sum is an additive accumulator of unit vectors: their vector sum and their
// count. Two Sums over disjoint member sets merge into the Sum of the union.
// The zero value is an empty Sum whose dimension is fixed by the first Add.
//
// A Sum holds a slice: assigning one Sum to another shares it, and an Add to
// either changes both. Start a roll-up from a zero Sum and Merge into it, or
// Clone first.
type Sum struct {
	vec []float64
	n   int
}

// Add normalises v and adds it.
func (s *Sum) Add(v Vector) error {
	u, err := unit(v, len(s.vec))
	if err != nil {
		return err
	}
	s.addUnit(u)
	return nil
}

func (s *Sum) addUnit(u []float64) {
	if s.vec == nil {
		s.vec = make([]float64, len(u))
	}
	for i, x := range u {
		s.vec[i] += x
	}
	s.n++
}

// Merge adds o into s. Merging an empty Sum is a no-op.
func (s *Sum) Merge(o Sum) error {
	if o.n == 0 {
		return nil
	}
	if s.vec != nil && len(s.vec) != len(o.vec) {
		return fmt.Errorf("%w: got %d, want %d", ErrDimensionMismatch, len(o.vec), len(s.vec))
	}
	if s.vec == nil {
		s.vec = make([]float64, len(o.vec))
	}
	for i, x := range o.vec {
		s.vec[i] += x
	}
	s.n += o.n
	return nil
}

// Clone returns an independent copy of s.
func (s Sum) Clone() Sum {
	if s.vec == nil {
		return Sum{}
	}
	return Sum{vec: append([]float64(nil), s.vec...), n: s.n}
}

// Count is the number of vectors added.
func (s Sum) Count() int { return s.n }

// Centroid is the centroid of the vectors added.
func (s Sum) Centroid() (Centroid, error) {
	if s.n == 0 {
		return Centroid{}, ErrEmpty
	}
	mean := make([]float64, len(s.vec))
	for i, x := range s.vec {
		mean[i] = x / float64(s.n)
	}
	r := norm(mean)
	// The mean of unit vectors has length in [0,1]; a length this small is
	// cancellation, and its direction is rounding noise.
	if r < 1e-12 {
		return Centroid{}, ErrDegenerate
	}
	u := make([]float64, len(mean))
	for i, x := range mean {
		u[i] = x / r
	}
	return Centroid{Count: s.n, Mean: mean, Unit: u}, nil
}

// Centroid of a member set.
//
// Mean is the arithmetic mean of the members' unit vectors; its length R (the
// mean resultant length, in (0,1]) measures how tightly they point one way.
// Unit is Mean normalised: the direction every similarity is taken against.
type Centroid struct {
	Count int
	Mean  []float64
	Unit  []float64
}

// Spread is the mean cosine distance of the members to the centroid:
//
//	spread = (1/n) * sum_i (1 - cos(x_i, Unit)) = 1 - |Mean|
//
// (the identity holds because cos(x_i, Unit) = x_i . Mean/|Mean| for unit
// x_i). It is 0 when every member points the same way and approaches 1 as the
// members scatter evenly. It needs no second pass over the members.
//
// When every member points one way, the normalised mean can round to a length
// just above 1; the clamp keeps that rounding from reporting a negative spread.
func (c Centroid) Spread() float64 {
	return math.Max(0, 1-norm(c.Mean))
}

// CentroidOf is the centroid of vecs.
func CentroidOf(vecs []Vector) (Centroid, error) {
	if len(vecs) == 0 {
		return Centroid{}, ErrEmpty
	}
	var s Sum
	for i, v := range vecs {
		if err := s.Add(v); err != nil {
			return Centroid{}, fmt.Errorf("vector %d: %w", i, err)
		}
	}
	return s.Centroid()
}

// unitMembers normalises members, checking one dimension and unique ids.
func unitMembers(members []Member) ([][]float64, error) {
	out := make([][]float64, len(members))
	seen := make(map[string]struct{}, len(members))
	dim := 0
	for i, m := range members {
		if _, dup := seen[m.ID]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateID, m.ID)
		}
		seen[m.ID] = struct{}{}
		u, err := unit(m.Vec, dim)
		if err != nil {
			return nil, fmt.Errorf("member %q: %w", m.ID, err)
		}
		if i == 0 {
			dim = len(u)
		}
		out[i] = u
	}
	return out, nil
}

// checkCentroid refuses a zero-value or malformed Centroid.
func checkCentroid(c Centroid, what string) error {
	if len(c.Unit) == 0 || c.Count == 0 {
		return fmt.Errorf("%s: %w", what, ErrEmpty)
	}
	return nil
}
