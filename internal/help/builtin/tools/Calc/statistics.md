---
name: Calc/statistics
kind: section
description: "Calc statistics — mean, median, mode, spread, geometric, harmonic and weighted means, sample and population variance and standard deviation, z-scores, quantiles and percentiles, ranks, running totals and differences, covariance, correlation and the least-squares line."
---
Functions over a list of numbers. Exact data keeps an exact result wherever
the formula allows: the mean, median, variance, covariance, quantiles and the
regression line of exact numbers are exact fractions. A square root that does
not come out exact (`stdev`, `correlation`, `zscore`) and `geomean` give a
float. An empty list is refused (`domain`); two lists of different lengths
are refused (`shape`).

Of a matrix, `mean`, `median`, `variance` and `stdev` work column by column
and return one value per column (see `Calc/matrices`).

`range` is the list builder (`range(1, 5)`), not the statistical range: the
gap between the largest and smallest value is `spread`.

## Centre and spread

The one-list functions take a list or the values themselves: `mean(v)` and
`mean(3, 4, 5)` are the same call.

### mean(v) or mean(a, b, ...)
The arithmetic mean. Exact on exact input.

Example: `mean([12.5, 14, 9.75, 11])` gives `11.8125`.

### median(v) or median(a, b, ...)
The middle value of the sorted data, or the mean of the two middle values
for an even count.

Example: `median([7, 1, 4, 10])` gives `5.5`.

### mode(v) or mode(a, b, ...)
The most frequent value. When several values are tied for most frequent, the
**smallest** of them. Values that compare equal (an exact 2 and a float 2)
count as one value.

Example: `mode([3, 1, 3, 2, 2])` gives `2`.

### spread(v) or spread(a, b, ...)
The largest value minus the smallest (the statistical range; NumPy's `ptp`).

Example: `spread([18.5, 22, 19.25])` gives `3.5`.

### geomean(v) or geomean(a, b, ...)
The geometric mean, the n-th root of the product, for averaging growth
factors. Every value must be above zero. A float.

Example: `round(geomean([1.1, 1.25, 0.95]), 6)` gives `1.093139`.

### harmmean(v) or harmmean(a, b, ...)
The harmonic mean, n divided by the sum of reciprocals, for averaging rates
such as speeds over equal distances. Every value must be above zero. Exact on
exact input.

Example: `harmmean([60, 40])` gives `48`.

### wmean(v, w)
The mean of `v` weighted by `w`: `sum(v .* w) / sum(w)`. The lists must have
the same length; weights that sum to zero are refused.

Example: `wmean([90, 75, 60], [0.5, 0.3, 0.2])` gives `79.5`.

### variance(v, kind?)
The **sample** variance, dividing by n - 1, unless `kind` is `"population"`,
which divides by n. A sample of one value is refused. Exact on exact input.

Example: `variance([2, 4, 4, 4, 5, 5, 7, 9], "population")` gives `4`.

### stdev(v, kind?)
The square root of `variance(v, kind)`: sample by default, `"population"` to
divide by n. A float unless the variance is an exact square.

Example: `round(stdev([2, 4, 4, 4, 5, 5, 7, 9]), 4)` gives `2.1381`.

### zscore(v, kind?)
Each value's distance from the mean in standard deviations, `(x - mean) /
stdev`, with the same `kind`. Refused when every value is the same.

Example: `zscore([2, 4, 4, 4, 5, 5, 7, 9], "population")` gives `[-1.5, -0.5, -0.5, -0.5, 0, 0, 1, 2]`.

## Position

### quantile(v, p)
The `p` quantile, `p` from 0 to 1, by **linear interpolation**: the value at
position (n - 1)·p of the sorted data, counting from 0, interpolated between
its two neighbours. This is NumPy's default, Excel's `PERCENTILE.INC` and R's
type 7. `quantile(v, 0.5)` is the median.

Example: `quantile([1, 2, 3, 4], 0.25)` gives `1.75`.

### percentile(v, p)
`quantile(v, p / 100)`: `p` from 0 to 100, the same method. A `p`
strictly between 0 and 1 is refused, because it is almost always a 0–1 level
meant for `quantile`; `percentile(v, 1)` is the 1st percentile.

Example: `percentile([1, 2, 3, 4], 30)` gives `1.9`.
Example: `percentile([1, 2, 3, 4], 0.9)` gives `error domain`.

### rank(v)
Each value's rank, **1 for the smallest**, in the order of `v`. Tied values
share the average of the ranks they span (Excel's `RANK.AVG` ascending,
SciPy's `rankdata`). For 1 as the largest, rank `-v`. The rank of a matrix
is `matrank` (`Calc/matrices`).

Example: `rank([10, 20, 10, 30])` gives `[1.5, 3, 1.5, 4]`.

## Running values

### cumsum(v)
Running totals: element i is the sum of the first i values. An empty list
gives `[]`.

Example: `cumsum([120, 80, 45])` gives `[120, 200, 245]`.

### cumprod(v)
Running products, for compounding a list of growth factors.

Example: `cumprod([1.1, 1.2, 0.9])` gives `[1.1, 1.32, 1.188]`.

### diff(v)
The differences of neighbours, `v[i+1] - v[i]`: one fewer element than `v`.

Example: `diff([100, 112, 109, 125])` gives `[12, -3, 16]`.

## Two variables

### covariance(x, y, kind?)
The sample covariance, dividing by n - 1, or `"population"` to divide by n.
Exact on exact input.

Example: `covariance([2, 4, 8], [5, 11, 12])` gives `9.66666666666667`.

### correlation(x, y)
The Pearson correlation coefficient, from -1 to 1. Refused when every `x` or
every `y` is the same. A float unless the root comes out exact.

Example: `round(correlation([3, 2, 4, 5, 6], [9, 7, 12, 15, 17]), 4)` gives `0.9971`.

### linreg(x, y)
The least-squares line `y = slope * x + intercept`, returned as the list
`[slope, intercept]`. At least two points, and not every `x` the same. Exact
on exact input.

Example: `linreg([1, 2, 3, 4], [3, 5, 7, 9])` gives `[2, 1]`.

## Examples

Summary statistics of measurements passed in `vars`:

```json
{"expr": "[mean(t), median(t), round(stdev(t), 3)]", "vars": {"t": [21.5, 23, 22.25, 24, 21.75]}}
```

Result: [22.5, 22.25, 1.016]

The 90th percentile of response times in milliseconds:

```json
{"expr": "percentile(ms, 90)", "vars": {"ms": [120, 95, 210, 130, 180, 99, 140, 160, 105, 400]}}
```

Result: 229

A trend line and next month's forecast:

```json
{"expr": "fit = linreg(1:6, sales); round(fit[1] * 7 + fit[2], 1)", "vars": {"sales": [410, 425, 450, 445, 470, 490]}}
```

Result: 501.3

An empty list is refused:

```json
{"expr": "mean([])"}
```

Result: error domain
