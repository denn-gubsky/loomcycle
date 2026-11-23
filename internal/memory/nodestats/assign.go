package nodestats

import (
	"fmt"
	"sort"
)

// Ranker ranks a thing's vector against the centroids of one comparison set of
// nodes (RFC GM P3.3: the siblings of one level), so the nearest and the second
// nearest, and the margin between them, can be read off.
//
// Leave-one-out, as Misfits: a member's own vector pulls its node's centroid
// towards it, so a node is measured without the thing when the thing is one of
// its members. Without it every placed document would agree with its own
// placement, and an agreement figure read for a held-out check would be inflated
// by construction.
type Ranker struct {
	nodes []rankNode
	dim   int
}

type rankNode struct {
	id    string
	sum   Sum
	units map[string][]float64 // member id → its unit vector
}

// NewRanker prepares the nodes. A node with fewer than minMembers members
// (minMembers >= 2, so a leave-one-out centroid exists) is left out, as Misfits
// leaves it out. Ids must be unique, members within a node too, and every vector
// of one dimension.
func NewRanker(nodes []Node, minMembers int) (*Ranker, error) {
	if minMembers < 2 {
		return nil, fmt.Errorf("%w: minMembers = %d, need >= 2", ErrInvalidArgument, minMembers)
	}
	r := &Ranker{}
	seen := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		if _, dup := seen[n.ID]; dup {
			return nil, fmt.Errorf("%w: node %q", ErrDuplicateID, n.ID)
		}
		seen[n.ID] = struct{}{}
		if len(n.Members) < minMembers {
			continue
		}
		us, err := unitMembers(n.Members)
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", n.ID, err)
		}
		if r.dim == 0 {
			r.dim = len(us[0])
		} else if len(us[0]) != r.dim {
			return nil, fmt.Errorf("node %q: %w", n.ID, ErrDimensionMismatch)
		}
		rn := rankNode{id: n.ID, units: make(map[string][]float64, len(us))}
		for i, u := range us {
			rn.sum.addUnit(u)
			rn.units[n.Members[i].ID] = u
		}
		r.nodes = append(r.nodes, rn)
	}
	return r, nil
}

// Len is the number of nodes the ranker compares.
func (r *Ranker) Len() int { return len(r.nodes) }

// Rank returns every node's cosine similarity to v, most similar first, ties by
// node id ascending. A node of which id is a member is measured without it. A
// node whose centroid is degenerate (its members cancel out, with or without the
// thing) is left out.
func (r *Ranker) Rank(id string, v Vector) ([]Scored, error) {
	if len(r.nodes) == 0 {
		return nil, nil
	}
	u, err := unit(v, r.dim)
	if err != nil {
		return nil, err
	}
	mean := make([]float64, r.dim)
	out := make([]Scored, 0, len(r.nodes))
	for _, n := range r.nodes {
		count := float64(n.sum.n)
		own, member := n.units[id]
		for d := range mean {
			mean[d] = n.sum.vec[d]
			if member {
				mean[d] -= own[d]
			}
		}
		if member {
			count--
		}
		if count < 1 {
			continue
		}
		// mean holds the members' SUM; its length over count is the mean resultant
		// length Sum.Centroid checks for cancellation.
		length := norm(mean)
		if length/count < 1e-12 {
			continue
		}
		out = append(out, Scored{ID: n.id, Score: dot(u, mean) / length})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
