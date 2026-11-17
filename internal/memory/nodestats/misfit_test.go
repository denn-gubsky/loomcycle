package nodestats

import (
	"errors"
	"math"
	"testing"
)

// looFixture: node A holds two members along x and a stray along y; node B
// sits mostly along z, tilted towards y. Measured against A's FULL centroid
// the stray is nearer A (1/sqrt5 = 0.447) than B (0.3/sqrt1.09 = 0.287), so
// it only shows as a misfit with itself left out of A (own = 0).
func looFixture() []Node {
	b := Vector{0, 0.3, 1}
	return []Node{
		{ID: "A", Members: []Member{{"a1", Vector{1, 0, 0}}, {"a2", Vector{1, 0, 0}}, {"x", Vector{0, 1, 0}}}},
		{ID: "B", Members: []Member{{"b1", b}, {"b2", b}, {"b3", b}}},
	}
}

func TestMisfits_LeaveOneOutRevealsStrayMember(t *testing.T) {
	nodes := looFixture()
	got, err := Misfits(nodes, 0.1, MinMembers)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v, want exactly the stray", got)
	}
	m := got[0]
	// float32 input: 0.3 is stored to about 1e-8.
	wantNear := 0.3 / math.Sqrt(1.09)
	if m.NodeID != "A" || m.MemberID != "x" || m.NearestNodeID != "B" ||
		!near(m.OwnSimilarity, 0) || math.Abs(m.NearestSimilarity-wantNear) > 1e-6 {
		t.Fatalf("got %+v, want A/x own 0 nearest B %v", m, wantNear)
	}

	// The premise: without leave-one-out x would not be a misfit.
	full, err := CentroidOf([]Vector{{1, 0, 0}, {1, 0, 0}, {0, 1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if withIn := full.Unit[1]; withIn <= wantNear {
		t.Fatalf("fixture broken: full-centroid similarity %v should exceed %v", withIn, wantNear)
	}
}

func TestMisfits_MarginSuppressesSmallGap(t *testing.T) {
	got, err := Misfits(looFixture(), 0.3, MinMembers)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("gap 0.287 is under margin 0.3, got %+v", got)
	}
}

func TestMisfits_MemberAlsoPlacedInOtherNodeIsNotItsMisfit(t *testing.T) {
	nodes := looFixture()
	nodes[1].Members = append(nodes[1].Members, Member{"x", Vector{0, 1, 0}})
	got, err := Misfits(nodes, 0.1, MinMembers)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("x belongs to both nodes, got %+v", got)
	}
}

func TestMisfits_NodeBelowMinMembersIsNeitherJudgedNorTarget(t *testing.T) {
	nodes := looFixture()
	nodes[1].Members = nodes[1].Members[:2]
	got, err := Misfits(nodes, 0.1, MinMembers)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("B has 2 < 3 members and must not be a target, got %+v", got)
	}
}

func TestMisfits_RefusesBadInput(t *testing.T) {
	cases := []struct {
		name   string
		nodes  []Node
		margin float64
		min    int
		want   error
	}{
		{"negative margin", looFixture(), -0.1, 3, ErrInvalidArgument},
		{"NaN margin", looFixture(), math.NaN(), 3, ErrInvalidArgument},
		{"min one", looFixture(), 0.1, 1, ErrInvalidArgument},
		{"duplicate node", append(looFixture(), looFixture()[0]), 0.1, 3, ErrDuplicateID},
		{"dimension across nodes", append(looFixture(), Node{ID: "C", Members: []Member{
			{"c1", Vector{1, 0}}, {"c2", Vector{1, 0}}, {"c3", Vector{1, 0}}}}), 0.1, 3, ErrDimensionMismatch},
		{"zero member", []Node{{ID: "A", Members: []Member{
			{"a", Vector{1, 0}}, {"b", Vector{0, 0}}, {"c", Vector{1, 0}}}}}, 0.1, 3, ErrZeroVector},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Misfits(tc.nodes, tc.margin, tc.min); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
