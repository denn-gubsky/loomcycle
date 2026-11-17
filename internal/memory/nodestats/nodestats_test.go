package nodestats

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"
)

const eps = 1e-9

func near(a, b float64) bool { return math.Abs(a-b) < eps }

func TestCentroidOf_MeanAndSpreadMatchHandValues(t *testing.T) {
	cases := []struct {
		name       string
		vecs       []Vector
		wantUnit   []float64
		wantSpread float64
	}{
		{"orthogonal pair", []Vector{{1, 0}, {0, 1}}, []float64{math.Sqrt2 / 2, math.Sqrt2 / 2}, 1 - math.Sqrt2/2},
		// Lengths do not matter: inputs are normalised first.
		{"orthogonal pair, scaled", []Vector{{2, 0}, {0, 3}}, []float64{math.Sqrt2 / 2, math.Sqrt2 / 2}, 1 - math.Sqrt2/2},
		{"identical", []Vector{{0, 0, 5}, {0, 0, 1}, {0, 0, 2}}, []float64{0, 0, 1}, 0},
		// mean (2/3, 1/3), |mean| = sqrt(5)/3.
		{"two and one", []Vector{{1, 0}, {1, 0}, {0, 1}}, []float64{2 / math.Sqrt(5), 1 / math.Sqrt(5)}, 1 - math.Sqrt(5)/3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := CentroidOf(tc.vecs)
			if err != nil {
				t.Fatal(err)
			}
			if c.Count != len(tc.vecs) {
				t.Errorf("count = %d, want %d", c.Count, len(tc.vecs))
			}
			for i := range tc.wantUnit {
				if !near(c.Unit[i], tc.wantUnit[i]) {
					t.Errorf("unit = %v, want %v", c.Unit, tc.wantUnit)
					break
				}
			}
			if !near(c.Spread(), tc.wantSpread) {
				t.Errorf("spread = %v, want %v", c.Spread(), tc.wantSpread)
			}
			// The closed form must equal the definition: mean of 1 - cos.
			var direct float64
			for _, v := range tc.vecs {
				u, _ := unit(v, 0)
				direct += 1 - dot(u, c.Unit)
			}
			direct /= float64(len(tc.vecs))
			if !near(c.Spread(), direct) {
				t.Errorf("spread = %v, direct mean cosine distance = %v", c.Spread(), direct)
			}
		})
	}
}

func TestCentroidOf_RefusesBadInput(t *testing.T) {
	cases := []struct {
		name string
		vecs []Vector
		want error
	}{
		{"empty set", nil, ErrEmpty},
		{"empty vector", []Vector{{}}, ErrEmpty},
		{"zero vector", []Vector{{1, 0}, {0, 0}}, ErrZeroVector},
		{"dimension mismatch", []Vector{{1, 0}, {1, 0, 0}}, ErrDimensionMismatch},
		{"NaN", []Vector{{float32(math.NaN()), 1}}, ErrNonFinite},
		{"Inf", []Vector{{float32(math.Inf(1)), 1}}, ErrNonFinite},
		{"cancelling", []Vector{{1, 0}, {-1, 0}}, ErrDegenerate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CentroidOf(tc.vecs); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSum_MergeOfDisjointSumsEqualsSumOfUnion(t *testing.T) {
	a, b, c := Vector{1, 0, 0}, Vector{0, 2, 0}, Vector{1, 1, 1}
	var left, right, root Sum
	for _, v := range []Vector{a, b} {
		if err := left.Add(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := right.Add(c); err != nil {
		t.Fatal(err)
	}
	for _, s := range []Sum{left, right, {}} {
		if err := root.Merge(s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := root.Centroid()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := CentroidOf([]Vector{a, b, c})
	if got.Count != 3 || root.Count() != 3 {
		t.Fatalf("count = %d", got.Count)
	}
	for i := range want.Mean {
		if !near(got.Mean[i], want.Mean[i]) {
			t.Fatalf("mean = %v, want %v", got.Mean, want.Mean)
		}
	}
	// Merging into root must not have written into left's storage.
	if lc, _ := left.Centroid(); lc.Count != 2 {
		t.Fatalf("left changed: count %d", lc.Count)
	}
}

func TestSum_CloneIsIndependent(t *testing.T) {
	var s Sum
	_ = s.Add(Vector{1, 0})
	c := s.Clone()
	_ = c.Add(Vector{0, 1})
	if s.Count() != 1 || s.vec[1] != 0 {
		t.Fatalf("original changed: %+v", s)
	}
}

func TestSum_MergeRefusesDimensionMismatch(t *testing.T) {
	var a, b Sum
	_ = a.Add(Vector{1, 0})
	_ = b.Add(Vector{1, 0, 0})
	if err := a.Merge(b); !errors.Is(err, ErrDimensionMismatch) {
		t.Fatalf("err = %v", err)
	}
	if err := a.Add(Vector{1, 0, 0}); !errors.Is(err, ErrDimensionMismatch) {
		t.Fatalf("add err = %v", err)
	}
}

func TestExemplars_NearestFirstTiesById(t *testing.T) {
	c, _ := CentroidOf([]Vector{{1, 0}, {0, 1}})
	members := []Member{
		{"d", Vector{-1, 0}},
		{"c", Vector{0, 1}},
		{"b", Vector{1, 1}},
		{"a", Vector{1, 0}},
	}
	got, err := Exemplars(c, members, KExemplars)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"b", "a", "c"}
	wantScores := []float64{1, math.Sqrt2 / 2, math.Sqrt2 / 2}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for i := range got {
		if got[i].ID != wantIDs[i] || !near(got[i].Score, wantScores[i]) {
			t.Fatalf("got %v, want ids %v scores %v", got, wantIDs, wantScores)
		}
	}
}

func TestExemplars_RefusesBadInput(t *testing.T) {
	c, _ := CentroidOf([]Vector{{1, 0}})
	cases := []struct {
		name    string
		c       Centroid
		members []Member
		k       int
		want    error
	}{
		{"k zero", c, []Member{{"a", Vector{1, 0}}}, 0, ErrInvalidArgument},
		{"zero centroid", Centroid{}, []Member{{"a", Vector{1, 0}}}, 1, ErrEmpty},
		{"no members", c, nil, 1, ErrEmpty},
		{"member dimension", c, []Member{{"a", Vector{1, 0, 0}}}, 1, ErrDimensionMismatch},
		{"zero member", c, []Member{{"a", Vector{0, 0}}}, 1, ErrZeroVector},
		{"duplicate id", c, []Member{{"a", Vector{1, 0}}, {"a", Vector{0, 1}}}, 1, ErrDuplicateID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Exemplars(tc.c, tc.members, tc.k); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNearestSiblings_OrderedByCentroidSimilarity(t *testing.T) {
	mk := func(id string, v Vector) NodeCentroid {
		c, err := CentroidOf([]Vector{v})
		if err != nil {
			t.Fatal(err)
		}
		return NodeCentroid{ID: id, Centroid: c}
	}
	self := mk("self", Vector{1, 0})
	sibs := []NodeCentroid{
		mk("s4", Vector{-1, 0}),
		mk("s2", Vector{0, 1}),
		mk("s1", Vector{1, 1}),
		mk("s3", Vector{1, 0.1}),
	}
	got, err := NearestSiblings(self, sibs, KSiblings)
	if err != nil {
		t.Fatal(err)
	}
	want := []Scored{{"s3", 1 / math.Sqrt(1.01)}, {"s1", math.Sqrt2 / 2}, {"s2", 0}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].ID != want[i].ID || math.Abs(got[i].Score-want[i].Score) > 1e-6 {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestNearestSiblings_RefusesSelfAndDuplicates(t *testing.T) {
	c, _ := CentroidOf([]Vector{{1, 0}})
	self := NodeCentroid{"x", c}
	if _, err := NearestSiblings(self, []NodeCentroid{{"x", c}}, 1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("self as sibling: err = %v", err)
	}
	if _, err := NearestSiblings(self, []NodeCentroid{{"y", c}, {"y", c}}, 1); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate: err = %v", err)
	}
	c3, _ := CentroidOf([]Vector{{1, 0, 0}})
	if _, err := NearestSiblings(self, []NodeCentroid{{"y", c3}}, 1); !errors.Is(err, ErrDimensionMismatch) {
		t.Fatalf("dimension: err = %v", err)
	}
}

// Members that all point one way have spread 0, but the normalised mean can
// round to a length just above 1; the spread must not then come out negative.
func TestCentroid_SpreadOfIdenticalMembersIsNeverNegative(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 5))
	for trial := 0; trial < 2000; trial++ {
		v := make(Vector, 1+rng.IntN(64))
		for d := range v {
			v[d] = float32(rng.Float64()*2 - 1)
		}
		vs := make([]Vector, 1+rng.IntN(10))
		for i := range vs {
			vs[i] = v
		}
		c, err := CentroidOf(vs)
		if err != nil {
			continue
		}
		if s := c.Spread(); s < 0 || s >= 1 {
			t.Fatalf("trial %d (dim %d, n %d): spread %g outside [0,1)", trial, len(v), len(vs), s)
		}
	}
}
