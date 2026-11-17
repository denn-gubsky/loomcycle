package nodestats

import (
	"fmt"
	"sort"
)

// NodeCentroid is a node's id with its centroid.
type NodeCentroid struct {
	ID       string
	Centroid Centroid
}

// Exemplars returns the k members nearest the centroid c, most similar first,
// ties broken by id ascending. Fewer than k members return them all.
func Exemplars(c Centroid, members []Member, k int) ([]Scored, error) {
	if k < 1 {
		return nil, fmt.Errorf("%w: k = %d", ErrInvalidArgument, k)
	}
	if err := checkCentroid(c, "centroid"); err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, ErrEmpty
	}
	us, err := unitMembers(members)
	if err != nil {
		return nil, err
	}
	if len(us[0]) != len(c.Unit) {
		return nil, fmt.Errorf("%w: members %d, centroid %d", ErrDimensionMismatch, len(us[0]), len(c.Unit))
	}
	out := make([]Scored, len(members))
	for i, m := range members {
		out[i] = Scored{ID: m.ID, Score: dot(us[i], c.Unit)}
	}
	return topK(out, k), nil
}

// NearestSiblings orders the sibling nodes by the cosine similarity of their
// centroid to self's, most similar first, ties broken by id ascending, and
// returns the first k. A sibling with self's own id is refused: the caller
// passes the node's siblings, not the node.
func NearestSiblings(self NodeCentroid, siblings []NodeCentroid, k int) ([]Scored, error) {
	if k < 1 {
		return nil, fmt.Errorf("%w: k = %d", ErrInvalidArgument, k)
	}
	if err := checkCentroid(self.Centroid, "node "+self.ID); err != nil {
		return nil, err
	}
	out := make([]Scored, 0, len(siblings))
	seen := make(map[string]struct{}, len(siblings))
	for _, s := range siblings {
		if s.ID == self.ID {
			return nil, fmt.Errorf("%w: sibling %q is the node itself", ErrInvalidArgument, s.ID)
		}
		if _, dup := seen[s.ID]; dup {
			return nil, fmt.Errorf("%w: sibling %q", ErrDuplicateID, s.ID)
		}
		seen[s.ID] = struct{}{}
		if err := checkCentroid(s.Centroid, "sibling "+s.ID); err != nil {
			return nil, err
		}
		if len(s.Centroid.Unit) != len(self.Centroid.Unit) {
			return nil, fmt.Errorf("%w: sibling %q", ErrDimensionMismatch, s.ID)
		}
		out = append(out, Scored{ID: s.ID, Score: dot(s.Centroid.Unit, self.Centroid.Unit)})
	}
	return topK(out, k), nil
}

// topK sorts by score descending, id ascending, and keeps the first k.
func topK(s []Scored, k int) []Scored {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Score != s[j].Score {
			return s[i].Score > s[j].Score
		}
		return s[i].ID < s[j].ID
	})
	if len(s) > k {
		s = s[:k]
	}
	return s
}
