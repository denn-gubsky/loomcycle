package calc

import (
	"math/big"
)

// The generator is per evaluation (evaluator.rng), seeded from
// Request.Seed when set, so a seeded call is reproducible.
var randomFuncs = []builtin{
	{name: "random", sig: "random()", section: SecRandom, min: 0, max: 0, fn: fnRandom,
		summary: "a uniform float in [0, 1)"},
	{name: "randint", sig: "randint(a, b)", section: SecRandom, min: 2, max: 2, fn: fnRandInt,
		summary: "a uniform integer in [a, b], both ends included"},
	{name: "randnormal", sig: "randnormal(mu?, sigma?)", section: SecRandom, min: 0, max: 2, fn: fnRandNormal,
		summary: "a normal sample, mean mu (default 0) and deviation sigma (default 1)"},
	{name: "shuffle", sig: "shuffle(v)", section: SecRandom, min: 1, max: 1, fn: fnShuffle,
		summary: "v in a uniformly random order"},
	{name: "sample", sig: "sample(v, k)", section: SecRandom, min: 2, max: 2, fn: fnSample,
		summary: "k distinct elements of v, chosen uniformly without replacement"},
}

func fnRandom(e *evaluator, _ args) (val, *Error) {
	return floatNum(e.rng.Float64()), nil
}

func fnRandInt(e *evaluator, a args) (val, *Error) {
	lo, err := a.integer(0)
	if err != nil {
		return nil, err
	}
	hi, err := a.integer(1)
	if err != nil {
		return nil, err
	}
	if lo.Cmp(hi) > 0 {
		return nil, errf(codeDomain, a.pos, "randint: a (%s) is above b (%s)", lo, hi)
	}
	span := new(big.Int).Sub(hi, lo)
	if !span.IsUint64() || span.Uint64() == ^uint64(0) {
		return nil, errf(codeTooLarge, a.pos, "randint: the interval is limited to 2^64 - 1 values")
	}
	off := e.rng.Uint64N(span.Uint64() + 1)
	return exactInt(new(big.Int).Add(lo, new(big.Int).SetUint64(off))), nil
}

func fnRandNormal(e *evaluator, a args) (val, *Error) {
	mu, sigma := 0.0, 1.0
	if len(a.v) >= 1 {
		m, err := a.num(0)
		if err != nil {
			return nil, err
		}
		mu = m.float()
	}
	if len(a.v) == 2 {
		s, err := a.num(1)
		if err != nil {
			return nil, err
		}
		if s.sign() < 0 {
			return nil, errf(codeDomain, a.pos, "randnormal: sigma must not be negative")
		}
		sigma = s.float()
	}
	return checkFloat(mu+sigma*e.rng.NormFloat64(), a.pos, "randnormal")
}

func fnShuffle(e *evaluator, a args) (val, *Error) {
	l, err := a.list(0)
	if err != nil {
		return nil, err
	}
	if err := e.charge(len(l), a.pos); err != nil {
		return nil, err
	}
	out := append([]val(nil), l...)
	e.rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, nil
}

func fnSample(e *evaluator, a args) (val, *Error) {
	l, err := a.list(0)
	if err != nil {
		return nil, err
	}
	k, err := a.small(1, MaxListLen)
	if err != nil {
		return nil, err
	}
	if k < 0 || int(k) > len(l) {
		return nil, errf(codeDomain, a.pos, "sample: k must be between 0 and len(v) = %d, got %d", len(l), k)
	}
	if err := e.charge(len(l), a.pos); err != nil {
		return nil, err
	}
	// A partial Fisher–Yates over a copy: the first k slots are the sample.
	tmp := append([]val(nil), l...)
	for i := 0; i < int(k); i++ {
		j := i + e.rng.IntN(len(tmp)-i)
		tmp[i], tmp[j] = tmp[j], tmp[i]
	}
	return tmp[:k], nil
}
