package projection

import (
	"errors"
	"fmt"
	"math"
)

// Anchors are a UMAP fit's sample: each point's vector and its place on the map.
// They are the UMAP basis — a chunk outside the sample is placed among them.
type Anchors struct {
	unit [][]float32
	pos  [][3]float64
	dim  int
}

// NewAnchors pairs the sample's vectors with their fitted positions.
func NewAnchors(vectors [][]float32, positions [][3]float64) (*Anchors, error) {
	if len(vectors) == 0 {
		return nil, errors.New("projection: no anchors")
	}
	if len(vectors) != len(positions) {
		return nil, fmt.Errorf("projection: %d anchor vectors for %d positions", len(vectors), len(positions))
	}
	d := len(vectors[0])
	for i, v := range vectors {
		if len(v) != d {
			return nil, fmt.Errorf("projection: anchor %d has dimension %d, want %d", i, len(v), d)
		}
	}
	return &Anchors{unit: normalized(vectors), pos: positions, dim: d}, nil
}

// Dimension is the anchors' vector dimension.
func (a *Anchors) Dimension() int { return a.dim }

// Len is the number of anchors.
func (a *Anchors) Len() int { return len(a.pos) }

// Place puts each vector at the membership-weighted mean of its nNeighbors
// nearest anchors' positions, the weights UMAP's fuzzy set would give those
// neighbours. This is umap-learn's transform without its few epochs of
// refinement (the RFC's measurement put the difference at about 0.03 of k-NN
// agreement): exact, cheap, and it moves no anchor, so the map does not shift
// when a chunk is added. ok[i] is false for a vector of another dimension.
func (a *Anchors) Place(vectors [][]float32, nNeighbors, workers int) (xyz [][3]float64, ok []bool) {
	if nNeighbors <= 0 {
		nNeighbors = DefaultNeighbors
	}
	xyz = make([][3]float64, len(vectors))
	ok = make([]bool, len(vectors))
	var q [][]float32
	var at []int
	for i, v := range vectors {
		if len(v) == a.dim {
			q = append(q, v)
			at = append(at, i)
		}
	}
	if len(q) == 0 {
		return xyz, ok
	}
	k := min(nNeighbors, len(a.pos))
	graph := cosineKNN(normalized(q), a.unit, k, false, workers)
	pos := make([][3]float64, k)
	for r, row := range graph {
		pos = pos[:len(row.idx)]
		for c, j := range row.idx {
			pos[c] = a.pos[j]
		}
		xyz[at[r]], ok[at[r]] = PlaceAmong(row.dist, pos), true
	}
	return xyz, ok
}

// PlaceAmong is the membership-weighted mean of pos: the places of a point's
// nearest neighbours, at cosine distances dist (nearest first). It is Place for
// a caller that found the neighbours itself. With no neighbours it is the origin.
func PlaceAmong(dist []float32, pos [][3]float64) [3]float64 {
	// The point is not among its neighbours, so all of them count toward the
	// membership target, where the fit counts the point itself as one.
	sigma, rho := smoothKNN(dist, len(dist), 0)
	var sw float64
	var p [3]float64
	for c := range dist {
		w := membership(dist[c], sigma, rho)
		sw += w
		for ax := range 3 {
			p[ax] += w * pos[c][ax]
		}
	}
	if sw > 0 {
		for ax := range 3 {
			p[ax] /= sw
		}
	}
	return p
}

// Similarity is x ↦ Scale·(x − From)·R + To: a rotation (or reflection),
// a uniform scale and a translation.
type Similarity struct {
	R     [3][3]float64
	Scale float64
	From  [3]float64
	To    [3]float64
}

// Apply maps p.
func (s Similarity) Apply(p [3]float64) [3]float64 {
	var c, out [3]float64
	for ax := range 3 {
		c[ax] = p[ax] - s.From[ax]
	}
	for col := range 3 {
		var v float64
		for row := range 3 {
			v += c[row] * s.R[row][col]
		}
		out[col] = s.Scale*v + s.To[col]
	}
	return out
}

// Align finds the similarity that best lays src onto ref, point i onto point i:
// the orthogonal Procrustes rotation (reflections allowed — a UMAP layout has no
// handedness) between the centred clouds, and a scale.
//
// The scale makes src's RMS radius equal ref's, rather than being the
// least-squares scale. The least-squares scale is the radius ratio times how well
// the two clouds agree, so it shrinks a map that rearranged: aligned that way, a
// map refitted ten times would come out smaller each time. When the clouds agree
// the two are the same.
func Align(src, ref [][3]float64) (Similarity, error) {
	n := len(src)
	if n != len(ref) {
		return Similarity{}, fmt.Errorf("projection: aligning %d points onto %d", n, len(ref))
	}
	if n == 0 {
		return Similarity{}, errors.New("projection: nothing to align")
	}
	s := Similarity{Scale: 1}
	for i := range n {
		for ax := range 3 {
			s.From[ax] += src[i][ax]
			s.To[ax] += ref[i][ax]
		}
	}
	for ax := range 3 {
		s.From[ax] /= float64(n)
		s.To[ax] /= float64(n)
	}
	// M = Xcᵀ·Yc; with the SVD M = U·Σ·Vᵀ the rotation is U·Vᵀ.
	var m [3][3]float64
	var srcSS, refSS float64
	for i := range n {
		var x, y [3]float64
		for ax := range 3 {
			x[ax] = src[i][ax] - s.From[ax]
			y[ax] = ref[i][ax] - s.To[ax]
			srcSS += x[ax] * x[ax]
			refSS += y[ax] * y[ax]
		}
		for r := range 3 {
			for c := range 3 {
				m[r][c] += x[r] * y[c]
			}
		}
	}
	if srcSS == 0 || refSS == 0 {
		s.R = identity3()
		return s, nil
	}
	s.Scale = math.Sqrt(refSS / srcSS)
	s.R = polarRotation(m)
	return s, nil
}

func identity3() [3][3]float64 { return [3][3]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}} }

// polarRotation is U·Vᵀ for the SVD of m: the orthogonal matrix nearest m. V and
// Σ² come from the eigendecomposition of mᵀm by Jacobi rotations; U's columns are
// m·vᵢ/σᵢ, and a column whose σ vanishes (a flat cloud) is completed to an
// orthonormal basis.
func polarRotation(m [3][3]float64) [3][3]float64 {
	var ata [3][3]float64
	for r := range 3 {
		for c := range 3 {
			for k := range 3 {
				ata[r][c] += m[k][r] * m[k][c]
			}
		}
	}
	eig, v := jacobiEigen3(ata)
	// Order by eigenvalue, largest first, so the completion below fills the
	// weakest directions.
	order := [3]int{0, 1, 2}
	for i := range 3 {
		for j := i + 1; j < 3; j++ {
			if eig[order[j]] > eig[order[i]] {
				order[i], order[j] = order[j], order[i]
			}
		}
	}
	var u, vs [3][3]float64 // columns
	top := math.Sqrt(max(eig[order[0]], 0))
	var have [][3]float64
	for c, o := range order {
		var vc [3]float64
		for r := range 3 {
			vc[r] = v[r][o]
		}
		for r := range 3 {
			vs[r][c] = vc[r]
		}
		sigma := math.Sqrt(max(eig[o], 0))
		var uc [3]float64
		if sigma > 1e-12*top && sigma > 0 {
			for r := range 3 {
				for k := range 3 {
					uc[r] += m[r][k] * vc[k]
				}
				uc[r] /= sigma
			}
		} else {
			uc = completeBasis(have)
		}
		have = append(have, uc)
		for r := range 3 {
			u[r][c] = uc[r]
		}
	}
	var out [3][3]float64
	for r := range 3 {
		for c := range 3 {
			for k := range 3 {
				out[r][c] += u[r][k] * vs[c][k]
			}
		}
	}
	return out
}

// completeBasis is a unit vector orthogonal to have.
func completeBasis(have [][3]float64) [3]float64 {
	for _, e := range [][3]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}} {
		v := e
		for range 2 {
			for _, h := range have {
				d := v[0]*h[0] + v[1]*h[1] + v[2]*h[2]
				for ax := range 3 {
					v[ax] -= d * h[ax]
				}
			}
		}
		if n := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2]); n > 1e-6 {
			for ax := range 3 {
				v[ax] /= n
			}
			return v
		}
	}
	return [3]float64{}
}

// jacobiEigen3 diagonalises a symmetric 3×3 matrix: the eigenvalues and the
// eigenvectors as v's columns.
func jacobiEigen3(a [3][3]float64) ([3]float64, [3][3]float64) {
	v := identity3()
	for range 100 {
		off := a[0][1]*a[0][1] + a[0][2]*a[0][2] + a[1][2]*a[1][2]
		if off < 1e-30 {
			break
		}
		for p := range 2 {
			for q := p + 1; q < 3; q++ {
				if a[p][q] == 0 {
					continue
				}
				theta := (a[q][q] - a[p][p]) / (2 * a[p][q])
				t := math.Copysign(1, theta) / (math.Abs(theta) + math.Sqrt(theta*theta+1))
				c := 1 / math.Sqrt(t*t+1)
				s := t * c
				for k := range 3 {
					akp, akq := a[k][p], a[k][q]
					a[k][p] = c*akp - s*akq
					a[k][q] = s*akp + c*akq
				}
				for k := range 3 {
					apk, aqk := a[p][k], a[q][k]
					a[p][k] = c*apk - s*aqk
					a[q][k] = s*apk + c*aqk
				}
				for k := range 3 {
					vkp, vkq := v[k][p], v[k][q]
					v[k][p] = c*vkp - s*vkq
					v[k][q] = s*vkp + c*vkq
				}
			}
		}
	}
	return [3]float64{a[0][0], a[1][1], a[2][2]}, v
}
