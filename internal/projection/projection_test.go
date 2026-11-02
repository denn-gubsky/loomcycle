package projection

import (
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
)

// syntheticSet draws n points in d dimensions that vary along three orthonormal
// directions with standard deviations 5, 3 and 1, plus a little isotropic noise,
// around an offset centre. It returns the points and the three directions.
func syntheticSet(n, d int, seed uint64) ([][]float32, [3][]float64) {
	rng := rand.New(rand.NewPCG(seed, seed+1))
	var dirs [3][]float64
	var found [][]float64
	for k := range dirs {
		v := make([]float64, d)
		for j := range v {
			v[j] = rng.NormFloat64()
		}
		orthonormalize(v, found)
		found = append(found, v)
		dirs[k] = v
	}
	sd := [3]float64{5, 3, 1}
	centre := make([]float64, d)
	for j := range centre {
		centre[j] = rng.NormFloat64() * 10
	}
	out := make([][]float32, n)
	for i := range out {
		row := make([]float32, d)
		var coef [3]float64
		for k := range coef {
			coef[k] = rng.NormFloat64() * sd[k]
		}
		for j := range row {
			x := centre[j] + rng.NormFloat64()*0.01
			for k := range coef {
				x += coef[k] * dirs[k][j]
			}
			row[j] = float32(x)
		}
		out[i] = row
	}
	return out, dirs
}

func cosine32(a []float32, b []float64) float64 {
	var s, na, nb float64
	for j := range a {
		s += float64(a[j]) * b[j]
		na += float64(a[j]) * float64(a[j])
		nb += b[j] * b[j]
	}
	return s / math.Sqrt(na*nb)
}

// TestFit_RecoversAKnownThreeComponentStructure — on points that vary along three
// known directions with distinct spreads, the fit's components are those
// directions in order of spread, and the explained variance is ordered and
// accounts for nearly all of it.
func TestFit_RecoversAKnownThreeComponentStructure(t *testing.T) {
	sample, dirs := syntheticSet(800, 24, 7)
	b, err := Fit(sample, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k := range Components {
		if c := math.Abs(cosine32(b.Components[k], dirs[k])); c < 0.995 {
			t.Errorf("component %d: |cos| with the true direction = %.4f, want ≥ 0.995", k, c)
		}
		if math.Abs(float64(norm32(b.Components[k]))-1) > 1e-5 {
			t.Errorf("component %d is not a unit vector", k)
		}
	}
	ev := b.ExplainedVariance
	if !(ev[0] > ev[1] && ev[1] > ev[2] && ev[2] > 0) {
		t.Errorf("explained variance %v is not strictly decreasing", ev)
	}
	if sum := ev[0] + ev[1] + ev[2]; sum < 0.99 || sum > 1.0000001 {
		t.Errorf("explained variance sums to %.5f, want ≈1 for a three-component set", sum)
	}
	// 25 : 9 : 1 of the variance, give or take sampling.
	if math.Abs(ev[0]-25.0/35) > 0.05 || math.Abs(ev[2]-1.0/35) > 0.01 {
		t.Errorf("explained variance %v, want ≈ [%.3f %.3f %.3f]", ev, 25.0/35, 9.0/35, 1.0/35)
	}
	if b.SampleN != 800 || b.Dimension != 24 {
		t.Errorf("SampleN/Dimension = %d/%d", b.SampleN, b.Dimension)
	}
}

func norm32(a []float32) float32 {
	var s float64
	for _, x := range a {
		s += float64(x) * float64(x)
	}
	return float32(math.Sqrt(s))
}

// TestFit_IsDeterministic — the same sample fits to the same basis, bit for bit.
func TestFit_IsDeterministic(t *testing.T) {
	sample, _ := syntheticSet(300, 16, 3)
	a, err := Fit(sample, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Fit(sample, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two fits of one sample differ")
	}
}

// TestFit_AlignsSignsWithThePreviousBasis — each component of a refit points the
// way the previous basis's did, so the map keeps its orientation; without a
// previous basis the sign rule still yields a fixed orientation.
func TestFit_AlignsSignsWithThePreviousBasis(t *testing.T) {
	sample, _ := syntheticSet(300, 16, 11)
	b, err := Fit(sample, nil)
	if err != nil {
		t.Fatal(err)
	}
	flipped := b
	for k := range Components {
		neg := make([]float32, len(b.Components[k]))
		for j, x := range b.Components[k] {
			neg[j] = -x
		}
		flipped.Components[k] = neg
	}
	re, err := Fit(sample, &flipped)
	if err != nil {
		t.Fatal(err)
	}
	for k := range Components {
		if c := cosine32(re.Components[k], toF64(flipped.Components[k])); c < 0.999 {
			t.Errorf("component %d: cos with the previous basis = %.4f, want ≈ +1", k, c)
		}
	}
}

func toF64(a []float32) []float64 {
	out := make([]float64, len(a))
	for i, x := range a {
		out[i] = float64(x)
	}
	return out
}

// TestFit_DegenerateSampleStillGivesUnitAxes — a sample with no spread (or one
// point) has zero explained variance, and its axes are still orthonormal, so
// placing a chunk never divides by zero or yields NaN.
func TestFit_DegenerateSampleStillGivesUnitAxes(t *testing.T) {
	for _, sample := range [][][]float32{
		{{1, 2, 3, 4}},
		{{1, 1}, {1, 1}, {1, 1}},
	} {
		b, err := Fit(sample, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k := range Components {
			if n := norm32(b.Components[k]); math.Abs(float64(n)-1) > 1e-5 {
				t.Errorf("dim %d: component %d has norm %v", b.Dimension, k, n)
			}
			if b.ExplainedVariance[k] != 0 {
				t.Errorf("dim %d: explained variance %v, want 0", b.Dimension, b.ExplainedVariance)
			}
		}
		xyz, ok := b.Project(sample[0])
		if !ok {
			t.Fatal("Project refused a vector of the basis's dimension")
		}
		for _, x := range xyz {
			if math.IsNaN(x) {
				t.Fatalf("NaN coordinate %v", xyz)
			}
		}
	}
}

func TestFit_RefusesMixedDimensions(t *testing.T) {
	if _, err := Fit([][]float32{{1, 2}, {1, 2, 3}}, nil); err == nil {
		t.Fatal("mixed dimensions accepted")
	}
	if _, err := Fit(nil, nil); err == nil {
		t.Fatal("an empty sample accepted")
	}
}

// TestReservoir_IsUniformAndDeterministic — the same stream gives the same sample,
// and every position of a long stream is about equally likely to be kept.
func TestReservoir_IsUniformAndDeterministic(t *testing.T) {
	stream := make([][]float32, 2000)
	for i := range stream {
		stream[i] = []float32{float32(i)}
	}
	sample := func() [][]float32 {
		r := NewReservoir(200)
		for _, v := range stream {
			r.Offer(v)
		}
		if r.Seen() != len(stream) {
			t.Fatalf("Seen = %d", r.Seen())
		}
		return r.Sample()
	}
	a, b := sample(), sample()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two reservoirs over one stream differ")
	}
	if len(a) != 200 {
		t.Fatalf("kept %d, want 200", len(a))
	}
	// Each half of the stream should hold about half of the sample.
	first := 0
	for _, v := range a {
		if v[0] < 1000 {
			first++
		}
	}
	if first < 70 || first > 130 {
		t.Errorf("%d of 200 kept from the first half, want ≈100", first)
	}
}

func TestFloat32Encoding_RoundTrips(t *testing.T) {
	v := []float32{0, -1.5, float32(math.Pi), math.MaxFloat32}
	got, err := DecodeFloat32s(EncodeFloat32s(v), len(v))
	if err != nil || !reflect.DeepEqual(got, v) {
		t.Fatalf("round trip = %v, %v", got, err)
	}
	if _, err := DecodeFloat32s([]byte{1, 2, 3}, 1); err == nil {
		t.Fatal("a short buffer decoded")
	}
}

func TestRound4_NoNegativeZero(t *testing.T) {
	if r := Round4(-0.00001); math.Signbit(r) {
		t.Fatalf("Round4(-0.00001) = %v, a negative zero", r)
	}
	if r := Round4(1.23456); r != 1.2346 {
		t.Fatalf("Round4(1.23456) = %v", r)
	}
}
