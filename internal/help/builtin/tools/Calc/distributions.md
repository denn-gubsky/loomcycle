---
name: Calc/distributions
kind: section
description: "Calc probability distributions — density, cumulative probability and inverse (quantile) of the normal, Student's t, chi-squared, F, exponential and uniform distributions, and the binomial and Poisson probabilities; critical values, p-values and confidence intervals."
---
Each continuous distribution has three functions: `…pdf` is the density at
`x`, `…cdf` the probability of a value **at most** `x`, and `…inv` the
inverse of the cdf: the `x` below which the probability is `p` (a quantile, or
a critical value). The discrete ones have `…pdf` (exactly `k`) and `…cdf` (at
most `k`).

The point comes first, then the parameters, so a list of points gives a list:
`normcdf([-1, 0, 1])`. Every result is a float, even for exact input; round it
for display.

A parameter out of range is refused (`domain`): a standard deviation, degrees
of freedom or rate at or below zero, `a` not below `b`, a probability outside
0 to 1, a count `k` or `n` that is not a whole number, or `k` above `n`. An
inverse at `p = 1`, or at `p = 0` for the normal and t distributions, is
infinite and refused too.

For the upper tail, take `1 - …cdf(x)`; for a two-sided p-value of a
statistic `z`, `2 * (1 - normcdf(abs(z)))`.

## Normal

### normpdf(x, mu?, sigma?)
The normal density with mean `mu` (default 0) and standard deviation `sigma`
(default 1).

Example: `round(normpdf(42, 40, 1.5), 8)` gives `0.10934005`.

### normcdf(x, mu?, sigma?)
The probability of a value at most `x` (Excel's `NORM.DIST(x, mu, sigma,
TRUE)`).

Example: `round(normcdf(42, 40, 1.5), 7)` gives `0.9087888`.

### norminv(p, mu?, sigma?)
The value with `normcdf` equal to `p`, for `0 < p < 1`. The two-sided 95%
critical value is `norminv(0.975)`.

Example: `round(norminv(0.975), 4)` gives `1.96`.

## Student's t

`df` is the degrees of freedom, above zero; it need not be whole.

### tpdf(x, df)
The t density.

Example: `round(tpdf(8, 3), 8)` gives `0.00073691`.

### tcdf(x, df)
The probability of a value at most `x`.

Example: `round(tcdf(60, 1), 8)` gives `0.99469533`.

### tinv(p, df)
The value with `tcdf` equal to `p`, for `0 < p < 1`; `tinv(0.975, n - 1)` is
the critical value of a 95% confidence interval from `n` observations.

Example: `round(tinv(0.975, 10), 4)` gives `2.2281`.

## Chi-squared

### chi2pdf(x, df)
The chi-squared density; 0 below 0. At `x = 0` with `df` 1 it is infinite
and refused.

Example: `round(chi2pdf(2, 3), 8)` gives `0.20755375`.

### chi2cdf(x, df)
The probability of a value at most `x`. A test's p-value is
`1 - chi2cdf(statistic, df)`.

Example: `round(chi2cdf(0.5, 1), 8)` gives `0.52049988`.

### chi2inv(p, df)
The value with `chi2cdf` equal to `p`, for `0 <= p < 1`.

Example: `round(chi2inv(0.95, 2), 4)` gives `5.9915`.

## F

`d1` and `d2` are the numerator and denominator degrees of freedom.

### fpdf(x, d1, d2)
The F density.

Example: `round(fpdf(15.2069, 6, 4), 7)` gives `0.0012238`.

### fcdf(x, d1, d2)
The probability of a value at most `x`.

Example: `round(fcdf(15.2069, 6, 4), 2)` gives `0.99`.

### finv(p, d1, d2)
The value with `fcdf` equal to `p`, for `0 <= p < 1`.

Example: `round(finv(0.01, 6, 4), 8)` gives `0.10930991`.

## Exponential

`lambda` is the **rate**, the events per unit of time; the mean is
`1 / lambda` (Excel's `EXPON.DIST` convention).

### exppdf(x, lambda)
The exponential density; 0 below 0.

Example: `round(exppdf(0.2, 10), 8)` gives `1.35335283`.

### expcdf(x, lambda)
The probability of a value at most `x`, `1 - exp(-lambda * x)`.

Example: `round(expcdf(0.2, 10), 8)` gives `0.86466472`.

### expinv(p, lambda)
The value with `expcdf` equal to `p`, for `0 <= p < 1`: the median waiting
time is `expinv(0.5, lambda)`.

Example: `round(expinv(0.5, 2), 6)` gives `0.346574`.

## Uniform

### unifpdf(x, a, b)
The uniform density on `[a, b]`: `1 / (b - a)` inside, 0 outside.

Example: `unifpdf(1, 0, 4)` gives `0.25`.

### unifcdf(x, a, b)
The probability of a value at most `x`.

Example: `unifcdf(0.3, 0, 2)` gives `0.15`.

### unifinv(p, a, b)
The value with `unifcdf` equal to `p`, for `0 <= p <= 1`.

Example: `unifinv(0.25, 2, 6)` gives `3`.

## Binomial and Poisson

### binompdf(k, n, p)
The probability of exactly `k` successes in `n` independent trials, each
succeeding with probability `p`. `k` and `n` are whole numbers, `0 <= k <= n`.

Example: `binompdf(6, 10, 0.5)` gives `0.205078125`.

### binomcdf(k, n, p)
The probability of **at most** `k` successes; at least `k` is
`1 - binomcdf(k - 1, n, p)`.

Example: `binomcdf(6, 10, 0.5)` gives `0.828125`.

### poisspdf(k, lambda)
The probability of exactly `k` events when `lambda` are expected.

Example: `round(poisspdf(2, 5), 6)` gives `0.084224`.

### poisscdf(k, lambda)
The probability of at most `k` events.

Example: `round(poisscdf(2, 5), 6)` gives `0.124652`.

## Examples

A 95% confidence interval for a mean from a sample in `vars`:

```json
{"expr": "n = len(x); h = tinv(0.975, n - 1) * stdev(x) / sqrt(n); round([mean(x) - h, mean(x) + h], 3)", "vars": {"x": [9.8, 10.2, 10.4, 9.9, 10.1, 10.0]}}
```

Result: [9.84, 10.293]

A two-sided p-value for a z statistic of 2.1:

```json
{"expr": "round(2 * (1 - normcdf(2.1)), 4)"}
```

Result: 0.0357

The chance of at least 3 defects in 50 parts at a 2% defect rate:

```json
{"expr": "round(1 - binomcdf(2, 50, 0.02), 4)"}
```

Result: 0.0784

A probability outside 0 to 1 is refused:

```json
{"expr": "norminv(1.2)"}
```

Result: error domain
