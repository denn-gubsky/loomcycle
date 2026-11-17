package nodestats

import (
	"fmt"
	"math"
	"sort"
)

// Node is a node's id with its member set.
type Node struct {
	ID      string
	Members []Member
}

// Misfit is a member nearer another node's centroid than its own.
//
// OwnSimilarity is the cosine to its own node's centroid computed WITHOUT the
// member (leave-one-out); NearestSimilarity is the cosine to the other node's
// full centroid. The caller reads both: a member far outside its node is a
// move, one near both is an additional placement (RFC GM section 5.1).
type Misfit struct {
	NodeID            string
	MemberID          string
	OwnSimilarity     float64
	NearestNodeID     string
	NearestSimilarity float64
}

// Misfits lists, for the given nodes, every member whose cosine similarity to
// some other node's centroid exceeds its similarity to its own node's
// leave-one-out centroid by more than margin.
//
// Leave-one-out matters for small nodes: a member's own vector pulls its
// node's centroid towards it, so measured with the member in, a stray member
// of a three-member node can look at home.
//
// Rules, all deliberate:
//   - The nodes passed are the comparison set. Similarities to centroids at
//     different depths are not comparable (RFC GM section 3.3), so pass the
//     nodes of one level, typically one node's siblings and itself.
//   - A node with fewer than minMembers members (minMembers >= 2, so a
//     leave-one-out centroid exists) is neither judged nor compared against.
//   - A member that also belongs to another node (same id) is not compared
//     with that node: it is placed there, not a misfit of it.
//   - A member whose leave-one-out centroid is degenerate is skipped.
//   - Ties in the nearest other node break by node id ascending.
//
// The result is sorted by node id, then member id. Cost is O(members x nodes x
// dimension).
func Misfits(nodes []Node, margin float64, minMembers int) ([]Misfit, error) {
	if math.IsNaN(margin) || math.IsInf(margin, 0) || margin < 0 {
		return nil, fmt.Errorf("%w: margin = %v", ErrInvalidArgument, margin)
	}
	if minMembers < 2 {
		return nil, fmt.Errorf("%w: minMembers = %d, need >= 2", ErrInvalidArgument, minMembers)
	}

	type prepared struct {
		id      string
		members []Member
		units   [][]float64
		sum     Sum
		cen     Centroid
		ids     map[string]struct{}
	}
	var ps []prepared
	dim := 0
	seenNode := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		if _, dup := seenNode[n.ID]; dup {
			return nil, fmt.Errorf("%w: node %q", ErrDuplicateID, n.ID)
		}
		seenNode[n.ID] = struct{}{}
		if len(n.Members) < minMembers {
			continue
		}
		us, err := unitMembers(n.Members)
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", n.ID, err)
		}
		if dim == 0 {
			dim = len(us[0])
		} else if len(us[0]) != dim {
			return nil, fmt.Errorf("node %q: %w", n.ID, ErrDimensionMismatch)
		}
		p := prepared{id: n.ID, members: n.Members, units: us, ids: make(map[string]struct{}, len(us))}
		for i, u := range us {
			p.sum.addUnit(u)
			p.ids[n.Members[i].ID] = struct{}{}
		}
		c, err := p.sum.Centroid()
		if err != nil {
			// A node whose members cancel out has no direction to be near.
			continue
		}
		p.cen = c
		ps = append(ps, p)
	}

	var out []Misfit
	loo := make([]float64, dim)
	for _, p := range ps {
		n := float64(p.sum.n - 1)
		for i, u := range p.units {
			for d := range loo {
				loo[d] = (p.sum.vec[d] - u[d]) / n
			}
			r := norm(loo)
			if r < 1e-12 {
				continue
			}
			own := dot(u, loo) / r

			best, bestID := math.Inf(-1), ""
			for _, q := range ps {
				if q.id == p.id {
					continue
				}
				if _, also := q.ids[p.members[i].ID]; also {
					continue
				}
				s := dot(u, q.cen.Unit)
				if s > best || (s == best && q.id < bestID) {
					best, bestID = s, q.id
				}
			}
			if bestID != "" && best-own > margin {
				out = append(out, Misfit{
					NodeID: p.id, MemberID: p.members[i].ID, OwnSimilarity: own,
					NearestNodeID: bestID, NearestSimilarity: best,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeID != out[j].NodeID {
			return out[i].NodeID < out[j].NodeID
		}
		return out[i].MemberID < out[j].MemberID
	})
	return out, nil
}
