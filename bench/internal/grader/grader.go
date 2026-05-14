// Package grader scores one (case, run) outcome across the three
// independent axes — structural, functional, semantic.
//
// Each grader is a standalone function. They share a small Result
// envelope so the reporter can write a uniform per-case row regardless
// of which axes were graded.
package grader

// Result is the per-axis outcome for one (case, run).
type Result struct {
	Structural AxisResult `json:"structural"`
	Functional AxisResult `json:"functional"`
	Semantic   AxisResult `json:"semantic"`
}

// AxisResult is one axis's pass/fail + diagnostics.
//
// For structural and functional, Pass is binary (true/false) and
// Score == 1.0 or 0.0.
//
// For semantic, Pass is (Score >= threshold/100) and Score is the
// judge's 0..100 rating divided by 100.
type AxisResult struct {
	Pass    bool     `json:"pass"`
	Score   float64  `json:"score"`              // 0..1
	Reasons []string `json:"reasons,omitempty"`   // human-readable diagnostics
}

// Passed reports whether the overall (case, run) is a success — all
// three axes must pass.
func (r Result) Passed() bool {
	return r.Structural.Pass && r.Functional.Pass && r.Semantic.Pass
}
