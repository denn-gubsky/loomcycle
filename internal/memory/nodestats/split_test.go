package nodestats

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"sort"
	"testing"
)

// twoClusters is n members near the x axis ("x00".."x(n-1)") and n near the
// y axis ("y00"...), each tilted a little by a fixed perturbation.
func twoClusters(n int) []Member {
	var ms []Member
	for i := 0; i < n; i++ {
		e := float32(i%5) * 0.03
		f := float32(i%3) * 0.02
		ms = append(ms,
			Member{fmt.Sprintf("x%02d", i), Vector{1, e, f}},
			Member{fmt.Sprintf("y%02d", i), Vector{f, 1, e}},
		)
	}
	return ms
}

func idsWithPrefix(ms []Member, p byte) []string {
	var out []string
	for _, m := range ms {
		if m.ID[0] == p {
			out = append(out, m.ID)
		}
	}
	sort.Strings(out)
	return out
}

// noise is n unit-ish vectors drawn from a fixed source in dim dimensions:
// no structure for k-means to find.
func noise(n, dim int) []Member {
	rng := rand.New(rand.NewPCG(7, 7))
	ms := make([]Member, n)
	for i := range ms {
		v := make(Vector, dim)
		for d := range v {
			v[d] = float32(rng.NormFloat64())
		}
		ms[i] = Member{fmt.Sprintf("n%03d", i), v}
	}
	return ms
}

func TestSplit_RecoversTwoObviousClustersForEverySeed(t *testing.T) {
	ms := twoClusters(12)
	wantX, wantY := idsWithPrefix(ms, 'x'), idsWithPrefix(ms, 'y')
	for seed := uint64(0); seed < 25; seed++ {
		res, err := Split(ms, DefaultSplitOptions(seed))
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if res.K != 2 || len(res.Groups) != 2 || res.Dropped != 0 {
			t.Fatalf("seed %d: K=%d groups=%d dropped=%d tried=%v", seed, res.K, len(res.Groups), res.Dropped, res.Tried)
		}
		got := [][]string{res.Groups[0].Members, res.Groups[1].Members}
		if !(reflect.DeepEqual(got, [][]string{wantX, wantY}) || reflect.DeepEqual(got, [][]string{wantY, wantX})) {
			t.Fatalf("seed %d: groups %v", seed, got)
		}
		if res.Stability < 0.95 || res.StabilityRuns != SplitStabilityRuns {
			t.Fatalf("seed %d: stability %v over %d runs, want >= 0.95 over %d",
				seed, res.Stability, res.StabilityRuns, SplitStabilityRuns)
		}
	}
}

func TestSplit_StabilityIsLowOnUniformNoise(t *testing.T) {
	ms := noise(60, 8)
	for seed := uint64(0); seed < 5; seed++ {
		res, err := Split(ms, DefaultSplitOptions(seed))
		if err != nil {
			t.Fatal(err)
		}
		if res.Stability > 0.5 {
			t.Fatalf("seed %d: stability %v on noise (K=%d), want <= 0.5", seed, res.Stability, res.K)
		}
	}
}

func TestSplit_SameSeedGivesSameResult(t *testing.T) {
	ms := noise(40, 6)
	a, err := Split(ms, DefaultSplitOptions(42))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Split(ms, DefaultSplitOptions(42))
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("two runs with one seed differ:\n%+v\n%+v", a, b)
	}
}

func TestSplit_DropsGroupsBelowMinGroup(t *testing.T) {
	ms := twoClusters(10)
	ms = append(ms, Member{"z0", Vector{0, 0, 1}}, Member{"z1", Vector{0.02, 0, 1}})
	res, err := Split(ms, DefaultSplitOptions(1))
	if err != nil {
		t.Fatal(err)
	}
	if res.K != 3 || len(res.Groups) != 2 || res.Dropped != 2 {
		t.Fatalf("K=%d groups=%d dropped=%d tried=%v", res.K, len(res.Groups), res.Dropped, res.Tried)
	}
	for _, g := range res.Groups {
		if len(g.Members) != 10 {
			t.Fatalf("group %v", g.Members)
		}
	}
}

func TestSplit_IdenticalMembersGiveNoCandidate(t *testing.T) {
	ms := make([]Member, SplitMinMembers)
	for i := range ms {
		ms[i] = Member{fmt.Sprintf("m%02d", i), Vector{1, 1}}
	}
	res, err := Split(ms, DefaultSplitOptions(1))
	if err != nil {
		t.Fatal(err)
	}
	if res.K != 0 || len(res.Groups) != 0 {
		t.Fatalf("got %+v, want no candidate", res)
	}
}

func TestSplit_RefusesBadInput(t *testing.T) {
	ok := twoClusters(12)
	mod := func(f func(*SplitOptions)) SplitOptions {
		o := DefaultSplitOptions(1)
		f(&o)
		return o
	}
	cases := []struct {
		name    string
		members []Member
		opts    SplitOptions
		want    error
	}{
		{"too few members", ok[:SplitMinMembers-1], DefaultSplitOptions(1), ErrTooFewMembers},
		{"zero options", ok, SplitOptions{}, ErrInvalidArgument},
		{"KMin one", ok, mod(func(o *SplitOptions) { o.KMin = 1 }), ErrInvalidArgument},
		{"KMax below KMin", ok, mod(func(o *SplitOptions) { o.KMax = 1 }), ErrInvalidArgument},
		{"subsample zero", ok, mod(func(o *SplitOptions) { o.SubsampleFraction = 0 }), ErrInvalidArgument},
		{"subsample above one", ok, mod(func(o *SplitOptions) { o.SubsampleFraction = 1.5 }), ErrInvalidArgument},
		{"negative runs", ok, mod(func(o *SplitOptions) { o.StabilityRuns = -1 }), ErrInvalidArgument},
		{"zero vector", append(append([]Member(nil), ok...), Member{"z", Vector{0, 0, 0}}), DefaultSplitOptions(1), ErrZeroVector},
		{"dimension", append(append([]Member(nil), ok...), Member{"z", Vector{1, 0}}), DefaultSplitOptions(1), ErrDimensionMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Split(tc.members, tc.opts); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAdjustedRand_MatchesHandValues(t *testing.T) {
	cases := []struct {
		name string
		a, b []int
		want float64
	}{
		{"relabelled identical", []int{0, 0, 1, 1}, []int{1, 1, 0, 0}, 1},
		// index 2, expected 6*3/15 = 1.2, max 4.5: 0.8/3.3.
		{"refinement", []int{0, 0, 0, 1, 1, 1}, []int{0, 0, 1, 1, 2, 2}, 0.8 / 3.3},
		{"both one cluster", []int{0, 0, 0}, []int{5, 5, 5}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := adjustedRand(tc.a, tc.b); !near(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// A cancelling cluster and two empty ones each take the member least similar
// to its own centre, drawn only from a cluster that still has more than one
// member after the reseeds before it, so no cluster is left empty.
func TestRecenter_EmptyAndCancellingClustersTakeTheFarthestMember(t *testing.T) {
	us := [][]float64{
		{1, 0, 0},     // 0: cluster 0, the least similar of all
		{0.8, 0.6, 0}, // 1: cluster 0, next least similar
		{0, 1, 0},     // 2: cluster 1
		{0, 0.8, 0.6}, // 3: cluster 1
		{0.6, 0.8, 0}, // 4: cluster 1
		{0, 0, 1},     // 5: cluster 2 (cancels with 6)
		{0, 0, -1},    // 6: cluster 2
	}
	labels := []int{0, 0, 1, 1, 1, 2, 2}
	sims := []float64{0.3, 0.4, 0.9, 0.8, 0.7, 0.95, 0.96}
	centers := make([][]float64, 5)
	for j := range centers {
		centers[j] = make([]float64, 3)
	}
	recenter(us, labels, sims, centers)

	// Cluster 2 cancels and takes member 0. Cluster 0 is then a singleton, so
	// empty cluster 3 must pass over member 1 and take member 4; empty
	// cluster 4 then takes member 3.
	want := []int{2, 0, 1, 4, 3, 2, 2}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels %v, want %v", labels, want)
	}
	for j, m := range map[int]int{2: 0, 3: 4, 4: 3} {
		if !reflect.DeepEqual(centers[j], us[m]) {
			t.Errorf("centre %d = %v, want member %d %v", j, centers[j], m, us[m])
		}
		if sims[m] != 1 {
			t.Errorf("member %d sim %g, want 1", m, sims[m])
		}
	}
	size := make([]int, len(centers))
	for _, l := range labels {
		size[l]++
	}
	for j, n := range size {
		if n == 0 {
			t.Errorf("cluster %d left empty: %v", j, labels)
		}
	}
}
