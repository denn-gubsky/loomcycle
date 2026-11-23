package nodestats

import (
	"errors"
	"math"
	"testing"
)

// TestRanker_MemberIsMeasuredWithoutItself — the stray x of looFixture's node A
// ranks B first only with itself left out of A; a thing that is no member of A
// is measured against A's full centroid.
func TestRanker_MemberIsMeasuredWithoutItself(t *testing.T) {
	r, err := NewRanker(looFixture(), MinMembers)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Rank("x", Vector{0, 1, 0})
	if err != nil {
		t.Fatal(err)
	}
	wantB := 0.3 / math.Sqrt(1.09)
	if len(got) != 2 || got[0].ID != "B" || math.Abs(got[0].Score-wantB) > 1e-6 || !near(got[1].Score, 0) {
		t.Fatalf("x ranks %+v; want B %v then A 0 (A without x)", got, wantB)
	}
	// The same vector, not a member: A's full centroid holds x, so A leads.
	got, err = r.Rank("outsider", Vector{0, 1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ID != "A" || math.Abs(got[0].Score-1/math.Sqrt(5)) > 1e-6 {
		t.Fatalf("outsider ranks %+v; want A at 1/sqrt5 first", got)
	}
}

// TestRanker_SmallNodesAreLeftOutAndTiesBreakById
func TestRanker_SmallNodesAreLeftOutAndTiesBreakById(t *testing.T) {
	e := Vector{1, 0}
	r, err := NewRanker([]Node{
		{ID: "b", Members: []Member{{"1", e}, {"2", e}, {"3", e}}},
		{ID: "a", Members: []Member{{"4", e}, {"5", e}, {"6", e}}},
		{ID: "small", Members: []Member{{"7", e}, {"8", e}}},
	}, MinMembers)
	if err != nil {
		t.Fatal(err)
	}
	if r.Len() != 2 {
		t.Fatalf("Len = %d; want the two nodes of MinMembers", r.Len())
	}
	got, _ := r.Rank("q", e)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("got %+v; want a, b (equal scores, by id)", got)
	}
}

func TestRanker_RefusesBadInput(t *testing.T) {
	if _, err := NewRanker(nil, 1); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("minMembers 1: %v", err)
	}
	dup := []Node{{ID: "a"}, {ID: "a"}}
	if _, err := NewRanker(dup, MinMembers); !errors.Is(err, ErrDuplicateID) {
		t.Errorf("duplicate node: %v", err)
	}
	r, err := NewRanker(looFixture(), MinMembers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Rank("q", Vector{1, 0}); !errors.Is(err, ErrDimensionMismatch) {
		t.Errorf("wrong dimension: %v", err)
	}
	empty, _ := NewRanker(nil, MinMembers)
	if got, err := empty.Rank("q", Vector{1}); got != nil || err != nil {
		t.Errorf("empty ranker = %v, %v", got, err)
	}
}
