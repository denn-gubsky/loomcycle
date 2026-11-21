package calc

import (
	"math"
	"math/big"
	"strings"
	"unicode/utf8"
)

// SecUnits holds unit conversion.
const SecUnits = "units"

var unitFuncs = []builtin{
	{name: "convert", sig: "convert(value, from, to)", section: SecUnits, min: 3, max: 3, fn: convertFn(false),
		summary: "value (a number or a list) from one unit to another of the same dimension; exact where the definition is"},
	{name: "convertdelta", sig: "convertdelta(value, from, to)", section: SecUnits, min: 3, max: 3, fn: convertFn(true),
		summary: "a temperature difference (or any value) converted by scale only, with no offset"},
	{name: "units", sig: "units(dimension?)", section: SecUnits, min: 0, max: 1, fn: fnUnits,
		summary: "the dimension names, or the unit names of one dimension"},
}

const (
	// maxUnitBytes bounds a unit string; a real compound is under 20.
	maxUnitBytes = 64
	// maxUnitParens bounds parenthesis nesting in a unit string.
	maxUnitParens = 8
	// maxUnitExp bounds an exponent written after ^, and maxDimExp each
	// base-dimension exponent of the whole unit, so a nested power cannot
	// build a huge factor.
	maxUnitExp = 12
	maxDimExp  = 24
)

// conversion is value*a*pi^pi + b.
type conversion struct {
	a  *big.Rat
	pi int
	b  *big.Rat
	af float64 // a*pi^pi as a float, for float input or pi != 0
}

func convertFn(delta bool) func(*evaluator, args) (val, *Error) {
	return func(e *evaluator, a args) (val, *Error) {
		from, ok1 := a.v[1].(string)
		to, ok2 := a.v[2].(string)
		if !ok1 || !ok2 {
			i := 1
			if ok1 {
				i = 2
			}
			return nil, errf(codeType, a.pos, "%s: argument %d must be a unit string, got a %s", a.name, i+1, typeName(a.v[i])).
				withHint(`quote the units: ` + a.name + `(5, "mi", "km")`)
		}
		// The parse is charged by its length once, not per element.
		if err := e.charge(len(from)+len(to), a.pos); err != nil {
			return nil, err
		}
		c, err := planConversion(from, to, delta, a)
		if err != nil {
			return nil, err
		}
		return e.applyConversion(c, a.v[0], a)
	}
}

func planConversion(from, to string, delta bool, a args) (conversion, *Error) {
	fu, err := parseUnit(from, a)
	if err != nil {
		return conversion{}, err
	}
	tu, err := parseUnit(to, a)
	if err != nil {
		return conversion{}, err
	}
	if fu.d != tu.d {
		e := errf(codeDimension, a.pos, "%s: %q is %s but %q is %s", a.name, from, dimName(fu.d), to, dimName(tu.d))
		return conversion{}, e.withHint(dimensionHint(fu.d, tu.d))
	}
	if !delta {
		for _, u := range []struct {
			s string
			v unitVal
		}{{from, fu}, {to, tu}} {
			if u.v.offsetInCompound {
				return conversion{}, errf(codeDimension, a.pos, "%s: %q puts a temperature scale with an offset (degC or degF) inside a compound, where it could mean a point or a difference", a.name, u.s).
					withHint("write K or degR inside a compound, or use convertdelta to convert a rate or a difference")
			}
		}
	}
	c := conversion{a: quoR(fu.k, tu.k), pi: fu.pi - tu.pi, b: new(big.Rat)}
	if !delta && (fu.off != nil || tu.off != nil) {
		// base = (v + offFrom)*kFrom; out = base/kTo - offTo.
		if fu.off != nil {
			c.b.Mul(fu.off, c.a)
		}
		if tu.off != nil {
			c.b.Sub(c.b, tu.off)
		}
	}
	af, _ := c.a.Float64()
	c.af = af * math.Pow(math.Pi, float64(c.pi))
	return c, nil
}

func dimensionHint(from, to dims) string {
	mass, force := unitIdx.dimOf["mass"], unitIdx.dimOf["force"]
	if from == mass && to == force || from == force && to == mass {
		return "mass and force differ: lbf and kgf are forces, lb and kg masses; multiply by gn to go from one to the other"
	}
	if name := dimName(from); !strings.HasPrefix(name, "the dimension") && name != "dimensionless" {
		return `both units must measure the same thing; units("` + name + `") lists the ` + name + ` units`
	}
	return "both units must have the same base dimensions"
}

// applyConversion converts a number, or each number of a (nested) list.
func (e *evaluator) applyConversion(c conversion, v val, a args) (val, *Error) {
	if err := e.charge(1, a.pos); err != nil {
		return nil, err
	}
	switch x := v.(type) {
	case num:
		if x.exact() && c.pi == 0 {
			r := new(big.Rat).Mul(x.r, c.a)
			return e.exactResult(r.Add(r, c.b), a.pos)
		}
		bf, _ := c.b.Float64()
		return checkFloat(x.float()*c.af+bf, a.pos, a.name)
	case []val:
		out := make([]val, len(x))
		for i, el := range x {
			y, err := e.applyConversion(c, el, a)
			if err != nil {
				return nil, err
			}
			out[i] = y
		}
		return out, nil
	}
	return nil, errf(codeType, a.pos, "%s: the value must be a number or a list of numbers, got a %s", a.name, typeName(v))
}

func fnUnits(e *evaluator, a args) (val, *Error) {
	if len(a.v) == 0 {
		out := make([]val, len(dimensions))
		for i, d := range dimensions {
			out[i] = d.name
		}
		return out, e.charge(len(out), a.pos)
	}
	name, ok := a.v[0].(string)
	if !ok {
		return nil, errf(codeType, a.pos, "units: the dimension must be a string, got a %s", typeName(a.v[0])).
			withHint(`units("length"); units() lists the dimensions`)
	}
	if _, ok := unitIdx.dimOf[name]; !ok {
		return nil, errf(codeUnknownUnit, a.pos, "units: no dimension named %q", name).
			withHint("units() lists the dimensions: " + strings.Join(dimensionNames(), ", "))
	}
	var out []val
	for _, u := range unitTable {
		if u.dim == name {
			out = append(out, u.sym)
		}
	}
	return out, e.charge(len(out), a.pos)
}

func dimensionNames() []string {
	out := make([]string, len(dimensions))
	for i, d := range dimensions {
		out[i] = d.name
	}
	return out
}

// --- the unit-string parser ---
//
//	unit   = term { ("*" | "·" | "/") term }    left to right: a/b*c is (a/b)*c
//	term   = factor [ "^" int | "²" | "³" ]     int is -12..12, not 0
//	factor = name | "1" | "(" unit ")"
//
// Spaces between tokens are ignored.

type unitParser struct {
	src   string
	i     int
	depth int
	a     args
}

func parseUnit(s string, a args) (unitVal, *Error) {
	if len(s) > maxUnitBytes {
		return unitVal{}, errf(codeUnknownUnit, a.pos, "%s: the unit string is %d bytes; the limit is %d", a.name, len(s), maxUnitBytes)
	}
	p := &unitParser{src: s, a: a}
	u, err := p.unit()
	if err != nil {
		return unitVal{}, err
	}
	p.space()
	if p.i < len(p.src) {
		return unitVal{}, p.malformed("unexpected %q", p.rest())
	}
	return u, nil
}

func (p *unitParser) malformed(format string, args ...any) *Error {
	e := errf(codeUnknownUnit, p.a.pos, "%s: the unit %q is not well formed: "+format, append([]any{p.a.name, p.src}, args...)...)
	return e.withHint(`combine units with *, / and ^, e.g. "kg*m/s^2", "km/h", "m^3"`)
}

func (p *unitParser) space() {
	for p.i < len(p.src) && p.src[p.i] == ' ' {
		p.i++
	}
}

func (p *unitParser) rest() string {
	r, _ := utf8.DecodeRuneInString(p.src[p.i:])
	return string(r)
}

func (p *unitParser) unit() (unitVal, *Error) {
	u, err := p.term()
	if err != nil {
		return unitVal{}, err
	}
	for {
		p.space()
		div := false
		switch {
		case strings.HasPrefix(p.src[p.i:], "*"):
			p.i++
		case strings.HasPrefix(p.src[p.i:], "·"):
			p.i += len("·")
		case strings.HasPrefix(p.src[p.i:], "/"):
			p.i++
			div = true
		default:
			return u, nil
		}
		v, err := p.term()
		if err != nil {
			return unitVal{}, err
		}
		if div {
			v = powUnit(v, -1)
		}
		if u, err = p.check(mulUnit(u, v)); err != nil {
			return unitVal{}, err
		}
	}
}

func (p *unitParser) term() (unitVal, *Error) {
	u, err := p.factor()
	if err != nil {
		return unitVal{}, err
	}
	p.space()
	n := 0
	switch {
	case strings.HasPrefix(p.src[p.i:], "²"):
		p.i += len("²")
		n = 2
	case strings.HasPrefix(p.src[p.i:], "³"):
		p.i += len("³")
		n = 3
	case strings.HasPrefix(p.src[p.i:], "^"):
		p.i++
		if n, err = p.exponent(); err != nil {
			return unitVal{}, err
		}
	default:
		return u, nil
	}
	return p.check(powUnit(u, n))
}

func (p *unitParser) exponent() (int, *Error) {
	p.space()
	neg := false
	if p.i < len(p.src) && p.src[p.i] == '-' {
		neg = true
		p.i++
	}
	start := p.i
	for p.i < len(p.src) && isDigit(p.src[p.i]) && p.i-start < 3 {
		p.i++
	}
	if p.i == start {
		return 0, p.malformed("^ needs a whole number after it")
	}
	n := 0
	for _, c := range p.src[start:p.i] {
		n = n*10 + int(c-'0')
	}
	if n == 0 || n > maxUnitExp {
		return 0, p.malformed("the exponent must be between -%d and %d, and not 0", maxUnitExp, maxUnitExp)
	}
	if neg {
		n = -n
	}
	return n, nil
}

func (p *unitParser) factor() (unitVal, *Error) {
	p.space()
	if p.i >= len(p.src) {
		if p.src == "" {
			return unitVal{}, p.malformed("it is empty")
		}
		return unitVal{}, p.malformed("a unit is missing at the end")
	}
	switch c := p.src[p.i]; {
	case c == '(':
		if p.depth >= maxUnitParens {
			return unitVal{}, p.malformed("parentheses nest deeper than %d", maxUnitParens)
		}
		p.i++
		p.depth++
		u, err := p.unit()
		if err != nil {
			return unitVal{}, err
		}
		p.space()
		if p.i >= len(p.src) || p.src[p.i] != ')' {
			return unitVal{}, p.malformed("a ) is missing")
		}
		p.i++
		p.depth--
		return u, nil
	case c == '1':
		p.i++
		return unitVal{k: q(1, 1, 0)}, nil
	case isDigit(c):
		return unitVal{}, p.malformed("a number cannot be part of a unit; write m^2, not m2")
	}
	start := p.i
	for p.i < len(p.src) && !p.atSeparator() {
		p.i++
	}
	name := p.src[start:p.i]
	if name == "" {
		return unitVal{}, p.malformed("a unit is missing before %q", p.rest())
	}
	if p.i < len(p.src) && isDigit(p.src[p.i]) {
		return unitVal{}, p.malformed("%q is followed by a number; write %s^%c", name, name, p.src[p.i])
	}
	u, ok := lookupUnit(name)
	if !ok {
		return unitVal{}, unknownUnit(name, p.a)
	}
	return u, nil
}

// atSeparator reports whether the parser stands on an operator, a digit or
// a space: everything else belongs to a unit name.
func (p *unitParser) atSeparator() bool {
	c := p.src[p.i]
	if strings.IndexByte("*/^() -", c) >= 0 || isDigit(c) {
		return true
	}
	rest := p.src[p.i:]
	return strings.HasPrefix(rest, "·") || strings.HasPrefix(rest, "²") || strings.HasPrefix(rest, "³")
}

// check refuses a unit whose exponents or factor grew past the bounds.
func (p *unitParser) check(u unitVal) (unitVal, *Error) {
	for _, n := range u.d {
		if n > maxDimExp || n < -maxDimExp {
			return unitVal{}, p.malformed("a base unit's exponent passes %d", maxDimExp)
		}
	}
	if u.pi > maxDimExp || u.pi < -maxDimExp || ratBits(u.k) > MaxExactBits {
		return unitVal{}, p.malformed("its factor is too large")
	}
	return u, nil
}

func mulUnit(a, b unitVal) unitVal {
	out := unitVal{k: mulR(a.k, b.k), pi: a.pi + b.pi}
	for i := range out.d {
		out.d[i] = a.d[i] + b.d[i]
	}
	out.offsetInCompound = a.off != nil || b.off != nil || a.offsetInCompound || b.offsetInCompound
	return out
}

func powUnit(u unitVal, n int) unitVal {
	k := new(big.Rat).SetInt(new(big.Int).Exp(u.k.Num(), big.NewInt(int64(abs(n))), nil))
	k.Quo(k, new(big.Rat).SetInt(new(big.Int).Exp(u.k.Denom(), big.NewInt(int64(abs(n))), nil)))
	if n < 0 {
		k.Inv(k)
	}
	out := unitVal{k: k, pi: u.pi * n, offsetInCompound: u.off != nil || u.offsetInCompound}
	for i := range out.d {
		out.d[i] = u.d[i] * n
	}
	return out
}

func unknownUnit(name string, a args) *Error {
	e := errf(codeUnknownUnit, a.pos, "%s: unknown unit %q", a.name, name)
	if h, ok := unitHints[name]; ok {
		return e.withHint(h)
	}
	names := unitNames()
	var near []string
	for _, n := range names {
		if strings.EqualFold(n, name) && n != name {
			near = append(near, n)
		}
	}
	for _, s := range suggest(name, names) {
		if !contains(near, s) && len(near) < 3 {
			near = append(near, s)
		}
	}
	if len(near) > 0 {
		return e.withHint("did you mean " + strings.Join(near, ", ") + "? Unit names are case-sensitive")
	}
	lower := strings.ToLower(name)
	for _, long := range []string{"kilo", "mega", "giga", "milli", "micro", "nano", "centi"} {
		if strings.HasPrefix(lower, long) {
			return e.withHint("prefixes go on symbols: km, MHz, ms, µs")
		}
	}
	return e.withHint(`units() lists the dimensions and units("length") the units of one`)
}

// unitHints answer names a model reaches for that are not units here.
var unitHints = map[string]string{
	"C":    "Celsius is degC; the coulomb is not in the table",
	"F":    "Fahrenheit is degF; the farad is not in the table",
	"R":    "Rankine is degR",
	"kmh":  "write km/h",
	"mps":  "write m/s",
	"cc":   "write cm^3",
	"sqm":  "write m^2",
	"sqft": "write ft^2",
	"KB":   "kB is 1000 bytes and KiB 1024 bytes",
	"Kb":   "kb is 1000 bits and Kib 1024 bits",
	"usd":  "currency is not a unit here",
	"USD":  "currency is not a unit here",
	"EUR":  "currency is not a unit here",
}
