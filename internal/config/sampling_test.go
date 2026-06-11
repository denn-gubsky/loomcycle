package config

import "testing"

func f64(v float64) *float64 { return &v }
func ip(v int) *int          { return &v }

// TestMergeSampling_PerFieldOverlay pins the per-field merge: `over` wins on
// the fields it sets, `base` is kept on the fields `over` leaves nil. This is
// what makes a fork that sets only temperature keep the parent's top_p, and a
// per-run sampling override win field-by-field over the agent's.
func TestMergeSampling_PerFieldOverlay(t *testing.T) {
	base := &Sampling{Temperature: f64(0.2), TopP: f64(0.9), Seed: ip(7)}
	over := &Sampling{Temperature: f64(0.9)} // override only temperature

	got := MergeSampling(base, over)
	if got == nil || got.Temperature == nil || *got.Temperature != 0.9 {
		t.Fatalf("temperature = %v, want 0.9 (over wins)", got.Temperature)
	}
	if got.TopP == nil || *got.TopP != 0.9 {
		t.Errorf("top_p = %v, want 0.9 (inherited from base — over left it nil)", got.TopP)
	}
	if got.Seed == nil || *got.Seed != 7 {
		t.Errorf("seed = %v, want 7 (inherited from base)", got.Seed)
	}
	// Must not alias either input.
	*got.Temperature = 1.5
	if *over.Temperature != 0.9 {
		t.Error("MergeSampling aliased over.Temperature")
	}
	if base.TopP != nil {
		*got.TopP = 0.1
		if *base.TopP != 0.9 {
			t.Error("MergeSampling aliased base.TopP")
		}
	}
}

func TestMergeSampling_NilCases(t *testing.T) {
	if got := MergeSampling(nil, nil); got != nil {
		t.Errorf("MergeSampling(nil,nil) = %v, want nil", got)
	}
	base := &Sampling{Temperature: f64(0.3)}
	if got := MergeSampling(base, nil); got == nil || *got.Temperature != 0.3 {
		t.Errorf("MergeSampling(base,nil) lost base")
	}
	over := &Sampling{TopP: f64(0.5)}
	got := MergeSampling(nil, over)
	if got == nil || got.TopP == nil || *got.TopP != 0.5 {
		t.Errorf("MergeSampling(nil,over) lost over")
	}
	if got.Temperature != nil {
		t.Errorf("MergeSampling(nil,over) invented a temperature")
	}
}

// TestSampling_Temperature0IsMeaningful guards the pointer semantics: a
// deterministic temperature 0.0 is a real value, distinct from unset.
func TestSampling_Temperature0IsMeaningful(t *testing.T) {
	s := &Sampling{Temperature: f64(0)}
	if s.IsZero() {
		t.Error("Sampling{temperature:0.0} reported IsZero — 0.0 is deterministic, not unset")
	}
	if (&Sampling{}).IsZero() != true {
		t.Error("empty Sampling should be IsZero")
	}
	var nilS *Sampling
	if !nilS.IsZero() {
		t.Error("nil Sampling should be IsZero")
	}
}

func TestSampling_Validate(t *testing.T) {
	ok := []*Sampling{
		nil,
		{Temperature: f64(0)},
		{Temperature: f64(2), TopP: f64(1), TopK: ip(1)},
		{FrequencyPenalty: f64(-2), PresencePenalty: f64(2), Seed: ip(0)},
	}
	for i, s := range ok {
		if err := s.Validate(); err != nil {
			t.Errorf("ok[%d]: unexpected error %v", i, err)
		}
	}
	bad := []*Sampling{
		{Temperature: f64(2.1)},
		{Temperature: f64(-0.1)},
		{TopP: f64(1.1)},
		{TopK: ip(0)},
		{FrequencyPenalty: f64(2.5)},
		{PresencePenalty: f64(-3)},
		{Stop: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("bad[%d]: want a validation error, got nil", i)
		}
	}
}
