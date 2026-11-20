package nodestats

import (
	"errors"
	"reflect"
	"testing"
)

// TestRecurringGroups_ObviousClustersRecurAndNoiseDoesNot — two well-separated
// clusters come back in every run, so both recur with full recurrence; uniform
// noise is partitioned differently by each seed, so none of its groups recurs.
func TestRecurringGroups_ObviousClustersRecurAndNoiseDoesNot(t *testing.T) {
	ms := twoClusters(12)
	res, err := RecurringGroups(ms, DefaultRecurOptions(1))
	if err != nil {
		t.Fatal(err)
	}
	if res.Runs != RecurRuns || len(res.Groups) != 2 {
		t.Fatalf("clusters: runs %d, recurring %d; want %d and 2 (%+v)", res.Runs, len(res.Groups), RecurRuns, res.Recurrence)
	}
	got := map[string]bool{}
	for _, g := range res.Groups {
		if g.Recurrence != RecurRuns {
			t.Errorf("group %v recurred %d times, want %d", g.Members, g.Recurrence, RecurRuns)
		}
		got[g.Members[0]] = true
	}
	if !got[idsWithPrefix(ms, 'x')[0]] || !got[idsWithPrefix(ms, 'y')[0]] {
		t.Errorf("recurring groups %+v; want the x and the y cluster", res.Groups)
	}

	// 200 Gaussian members in 16 dimensions: the case a seed-only check gets wrong
	// (2-means splits them along the sample's widest direction for most seeds).
	for _, n := range []int{60, 200} {
		for seed := uint64(0); seed < 10; seed++ {
			nz, err := RecurringGroups(noise(n, 16), DefaultRecurOptions(seed))
			if err != nil {
				t.Fatal(err)
			}
			if len(nz.Reference.Groups) == 0 {
				t.Fatalf("precondition: the reference split of noise keeps groups: %+v", nz.Reference)
			}
			if len(nz.Groups) != 0 {
				t.Errorf("noise n=%d seed %d: %d groups recur (%v); want none", n, seed, len(nz.Groups), nz.Recurrence)
			}
		}
	}
}

// TestRecurringGroups_SameSeedSameAnswer — the result is a function of the input
// and the seed.
func TestRecurringGroups_SameSeedSameAnswer(t *testing.T) {
	a, err := RecurringGroups(noise(40, 8), DefaultRecurOptions(3))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := RecurringGroups(noise(40, 8), DefaultRecurOptions(3))
	if !reflect.DeepEqual(a, b) {
		t.Errorf("two calls differ:\n%+v\n%+v", a, b)
	}
}

func TestRecurringGroups_RefusesBadOptions(t *testing.T) {
	ms := twoClusters(12)
	for _, mod := range []func(*RecurOptions){
		func(o *RecurOptions) { o.Runs = 0 },
		func(o *RecurOptions) { o.MinRecur = 0 },
		func(o *RecurOptions) { o.MinRecur = o.Runs + 1 },
		func(o *RecurOptions) { o.MinJaccard = 0 },
		func(o *RecurOptions) { o.MinJaccard = 1.5 },
	} {
		o := DefaultRecurOptions(1)
		mod(&o)
		if _, err := RecurringGroups(ms, o); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%+v: err %v, want ErrInvalidArgument", o, err)
		}
	}
	if _, err := RecurringGroups(ms[:3], DefaultRecurOptions(1)); !errors.Is(err, ErrTooFewMembers) {
		t.Errorf("three members: err %v, want ErrTooFewMembers", err)
	}
}

func TestNearest_PicksTheMostSimilarCentroid(t *testing.T) {
	x, _ := CentroidOf([]Vector{{1, 0, 0}})
	y, _ := CentroidOf([]Vector{{0, 1, 0}})
	i, s, err := Nearest(Vector{0.2, 1, 0}, []Centroid{x, y})
	if err != nil || i != 1 || s < 0.9 {
		t.Errorf("nearest = %d %v %v; want 1, ~0.98", i, s, err)
	}
	if i, _, _ := Nearest(Vector{1, 0, 0}, nil); i != -1 {
		t.Errorf("no centroids: %d, want -1", i)
	}
	if _, _, err := Nearest(Vector{1, 0}, []Centroid{x}); !errors.Is(err, ErrDimensionMismatch) {
		t.Errorf("dimension mismatch: %v", err)
	}
}
