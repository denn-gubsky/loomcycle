package calc

import "time"

// The limits are fixed constants, not configuration: the input is written
// by a model, and every bound here is what keeps one call from costing more
// than a few seconds of CPU or a few tens of megabytes of memory. Each one
// is checked before the work it bounds wherever the size can be known in
// advance (expression length, var size, result bit size of a power or a
// factorial, range length).
const (
	// MaxExprBytes bounds the expression text.
	MaxExprBytes = 16 << 10
	// MaxVarsBytes bounds the summed size of the raw JSON var values plus
	// their names.
	MaxVarsBytes = 1 << 20
	// MaxSteps bounds evaluation work. A step is one expression node, one
	// list element touched by a broadcast, map, filter, reduce, sort, range,
	// or one function call; big exact numbers charge extra (see bitStepCost).
	MaxSteps = 1_000_000
	// MaxExactBits bounds an exact number: numerator plus denominator bits.
	MaxExactBits = 16384
	// MaxWall bounds wall time; the caller's ctx can only shorten it.
	MaxWall = 2 * time.Second
	// MaxOutputBytes bounds the rendered text; past it the text is cut and
	// Response.Truncated is set.
	MaxOutputBytes = 64 << 10
	// MaxListLen bounds one list. A million elements is far beyond any list
	// a model writes by hand, and still under the step bound for one pass.
	MaxListLen = 1_000_000
	// MaxDepth bounds syntactic nesting (parentheses, lists, unary chains,
	// lambda bodies) and, with MaxCallDepth, the evaluator's recursion.
	MaxDepth = 200
	// MaxCallDepth bounds nested calls of user functions. Recursion is
	// refused outright; this only bounds a chain of distinct functions.
	MaxCallDepth = 64
	// MaxDigits bounds Request.Digits.
	MaxDigits = 1000
	// DefaultDigits is used when Request.Digits is zero.
	DefaultDigits = 15
	// maxVarDepth bounds the nesting of a JSON var value.
	maxVarDepth = 32
	// ctxCheckEvery is how many steps pass between ctx checks: often enough
	// that a deadline stops evaluation within microseconds, rare enough that
	// the check does not show in the arithmetic benchmark.
	ctxCheckEvery = 256
	// bitStepCost charges one extra step per this many bits of an exact
	// result, so a million-element list of 16,384-bit numbers cannot be
	// built inside the step bound (it would hold about 2 GB).
	bitStepCost = 512
	// maxPrimeBits bounds isprime/nextprime arguments. Measured on an
	// M-series laptop: nextprime(2^2046) took 1.5 s (20 Miller-Rabin rounds
	// on the prime that ends a ~1,400-wide gap); 1,024 bits costs about an
	// eighth of that.
	maxPrimeBits = 1024
	// maxModBits bounds modpow's modulus and exponent for the same reason.
	maxModBits = 4096
	// maxKeyBytes bounds the comparison keys one unique call builds: a
	// 900 KB string repeated twenty times costs few steps but 18 MB of keys.
	maxKeyBytes = 16 << 20
	// maxRoundPlaces bounds the n of round(x, n) and friends.
	maxRoundPlaces = 1000
	// MaxMatrixElems bounds a matrix a matrix function reads or builds:
	// 500 × 500.
	MaxMatrixElems = 250_000
	// MaxMatrixWork bounds a matrix product, solve, inverse or
	// decomposition by rows × columns × the inner (or smaller) dimension:
	// the work of one 500 × 500 matrix. It is checked from the shapes
	// before any work, because a gonum routine cannot be interrupted: once
	// it starts, neither the step count nor the clock stops it, so this cap
	// is what bounds its time. Measured CPU time at 500 × 500 on an M-series
	// laptop: det 22 ms, inv 71 ms, the product 90 ms, qr 135 ms, the
	// symmetric eigen decomposition 300 ms, svd with its factors 570 ms.
	MaxMatrixWork = 500 * 500 * 500
	// matrixWorkPerStep charges one step per this many multiply-adds of a
	// matrix routine, before it runs.
	matrixWorkPerStep = 1000
	// gateElems and gateWork: a matrix operation reading more elements, or
	// doing more work, takes a slot of Request.Gate first.
	gateElems = 10_000
	gateWork  = 1_000_000
)
