package calc

import "testing"

// The reference values are the worked examples of the Excel function docs
// (NORM.DIST, NORM.INV, NORM.S.DIST, T.DIST, T.INV, CHISQ.DIST,
// CHISQ.INV, F.DIST, F.INV, EXPON.DIST, BINOM.DIST, POISSON.DIST), each
// compared at the digits the docs print; and textbook table values
// (z 0.975 = 1.959964, t 0.975 with 10 df = 2.228139).
func TestDistributions_MatchPublishedReferenceValues(t *testing.T) {
	checkExact(t, false, []textCase{
		// NORM.DIST(42, 40, 1.5, TRUE) = 0.9087888; FALSE = 0.10934005.
		{"round(normcdf(42, 40, 1.5), 7)", "0.9087888"},
		{"round(normpdf(42, 40, 1.5), 8)", "0.10934005"},
		// NORM.INV(0.908789, 40, 1.5) = 42.000002.
		{"round(norminv(0.908789, 40, 1.5), 6)", "42.000002"},
		// NORM.S.DIST(1.333333, TRUE) = 0.908788726.
		{"round(normcdf(1.333333), 9)", "0.908788726"},
		{"round(norminv(0.975), 6)", "1.959964"},
		{"normcdf(0)", "0.5"},
		// T.DIST(60, 1, TRUE) = 0.99469533; T.DIST(8, 3, FALSE) = 0.00073691.
		{"round(tcdf(60, 1), 8)", "0.99469533"},
		{"round(tpdf(8, 3), 8)", "0.00073691"},
		// T.INV(0.75, 2) = 0.8164966.
		{"round(tinv(0.75, 2), 7)", "0.8164966"},
		{"round(tinv(0.975, 10), 6)", "2.228139"},
		// CHISQ.DIST(0.5, 1, TRUE) = 0.52049988; CHISQ.DIST(2, 3, FALSE) = 0.20755375.
		{"round(chi2cdf(0.5, 1), 8)", "0.52049988"},
		{"round(chi2pdf(2, 3), 8)", "0.20755375"},
		// CHISQ.INV(0.93, 1) = 3.283020287; CHISQ.INV(0.6, 2) = 1.832581464.
		{"round(chi2inv(0.93, 1), 9)", "3.283020287"},
		{"round(chi2inv(0.6, 2), 9)", "1.832581464"},
		// F.DIST(15.2069, 6, 4, TRUE) = 0.99; FALSE = 0.0012238.
		{"round(fcdf(15.2069, 6, 4), 2)", "0.99"},
		{"round(fpdf(15.2069, 6, 4), 7)", "0.0012238"},
		// F.INV(0.01, 6, 4) = 0.10930991.
		{"round(finv(0.01, 6, 4), 8)", "0.10930991"},
		// EXPON.DIST(0.2, 10, TRUE) = 0.86466472; FALSE = 1.35335283.
		{"round(expcdf(0.2, 10), 8)", "0.86466472"},
		{"round(exppdf(0.2, 10), 8)", "1.35335283"},
		{"round(expinv(expcdf(0.2, 10), 10), 12)", "0.2"},
		{"unifcdf(0.3, 0, 2)", "0.15"},
		{"unifpdf(1, 0, 2)", "0.5"},
		{"unifinv(0.25, 2, 6)", "3"},
		// BINOM.DIST(6, 10, 0.5, FALSE) = 0.2050781; cumulative 848/1024.
		{"binompdf(6, 10, 0.5)", "0.205078125"},
		{"binomcdf(6, 10, 0.5)", "0.828125"},
		// POISSON.DIST(2, 5, TRUE) = 0.124652; FALSE = 0.084224.
		{"round(poisscdf(2, 5), 6)", "0.124652"},
		{"round(poisspdf(2, 5), 6)", "0.084224"},
	})
}

func TestDistributions_MapOverAListOfPoints(t *testing.T) {
	checkTexts(t, []textCase{
		{"round(normcdf([-1, 0, 1]), 4)", "[0.1587, 0.5, 0.8413]"},
		{"binompdf(0:2, 2, 0.5)", "[0.25, 0.5, 0.25]"},
	})
}

// p = 0 and p = 1 put all the probability on one count; gonum computes
// 0 · log(0) there, so these are answered directly and must not be NaN.
func TestDistributions_BinomialAtCertainProbabilityIsAPointMass(t *testing.T) {
	checkTexts(t, []textCase{
		{"binompdf(0, 5, 0)", "1"},
		{"binompdf(1, 5, 0)", "0"},
		{"binompdf(5, 5, 1)", "1"},
		{"binomcdf(4, 5, 1)", "0"},
		{"binomcdf(5, 5, 1)", "1"},
		{"binomcdf(0, 5, 0)", "1"},
	})
}

func TestDistributions_OutsideTheSupportTheDensityIsZero(t *testing.T) {
	checkTexts(t, []textCase{
		{"chi2pdf(-1, 2)", "0"},
		{"exppdf(-1, 2)", "0"},
		{"unifpdf(3, 0, 2)", "0"},
		{"unifcdf(3, 0, 2)", "1"},
		{"chi2inv(0, 3)", "0"},
	})
}

func TestDistributions_RefuseParametersOutOfRange(t *testing.T) {
	for _, expr := range []string{
		"normpdf(0, 0, 0)", "normcdf(0, 0, -1)", // sigma <= 0
		"norminv(0)", "norminv(1)", "norminv(1.5)", "tinv(-0.1, 3)", // probability
		"chi2inv(1, 2)", "finv(1, 2, 3)", "expinv(1, 2)", "unifinv(1.1, 0, 1)",
		"tpdf(1, 0)", "chi2cdf(1, -2)", "fpdf(1, 0, 2)", "fpdf(1, 2, 0)", "exppdf(1, 0)",
		"unifpdf(1, 2, 2)", "unifpdf(1, 3, 2)", // a >= b
		"binompdf(1.5, 3, 0.5)", "binompdf(-1, 3, 0.5)", "binompdf(4, 3, 0.5)", // k
		"binompdf(1, 2.5, 0.5)", "binomcdf(1, 3, 1.2)", "binomcdf(1, 3, -0.1)", // n, p
		"poisspdf(1.5, 2)", "poisscdf(-1, 2)", "poisspdf(1, 0)",
		"chi2pdf(0, 1)", // the density is infinite at 0
	} {
		mustFail(t, expr, codeDomain)
	}
	mustFail(t, `normcdf("a")`, codeType)
	mustFail(t, "normcdf(10^400)", codeOverflow)
}
