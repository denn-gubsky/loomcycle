package calc

import (
	"math/big"
	"strings"
)

// Units for convert. A unit is a scale factor to the coherent SI unit of its
// dimension times pi^pi, plus an exponent vector over the base dimensions.
// Angle and information are base dimensions of their own, so rad and deg
// never collapse into "dimensionless" next to a plain ratio, nor bit into a
// count. Two units convert iff their vectors are equal.
//
// The table is closed: no unit is added at run time. Every factor is an
// exact rational where its definition is exact, with the source of the
// definition beside it (NIST SP 811 2008 Appendix B, the SI brochure 9th
// edition, IAU 2012). A factor that needs pi keeps pi apart, so deg -> arcmin
// stays exact while deg -> rad becomes a float.

const (
	dimM = iota
	dimKg
	dimS
	dimA
	dimK
	dimMol
	dimCd
	dimRad
	dimBit
	nDims
)

type dims [nDims]int

// baseSymbols name each base dimension in a compound formula.
var baseSymbols = [nDims]string{"m", "kg", "s", "A", "K", "mol", "cd", "rad", "bit"}

type dimension struct {
	name string
	d    dims
}

// dimensions is the listing order of units(); names are what a refusal
// prints for a vector.
var dimensions = []dimension{
	{"length", dims{dimM: 1}},
	{"mass", dims{dimKg: 1}},
	{"time", dims{dimS: 1}},
	{"temperature", dims{dimK: 1}},
	{"area", dims{dimM: 2}},
	{"volume", dims{dimM: 3}},
	{"speed", dims{dimM: 1, dimS: -1}},
	{"acceleration", dims{dimM: 1, dimS: -2}},
	{"force", dims{dimKg: 1, dimM: 1, dimS: -2}},
	{"pressure", dims{dimKg: 1, dimM: -1, dimS: -2}},
	{"energy", dims{dimKg: 1, dimM: 2, dimS: -2}},
	{"power", dims{dimKg: 1, dimM: 2, dimS: -3}},
	{"angle", dims{dimRad: 1}},
	{"frequency", dims{dimS: -1}},
	{"data size", dims{dimBit: 1}},
	{"electric current", dims{dimA: 1}},
	{"amount of substance", dims{dimMol: 1}},
	{"luminous intensity", dims{dimCd: 1}},
}

// prefixKind says which prefixes a unit takes.
type prefixKind int

const (
	preNone prefixKind = iota
	// preSI takes every SI prefix, y to Y.
	preSI
	// preData takes the multiplying SI prefixes k to Y and the binary ones
	// Ki to Yi: a millibyte means nothing.
	preData
)

type unitDef struct {
	sym     string
	dim     string // a dimensions name
	k       *big.Rat
	pi      int      // the factor is k * pi^pi
	off     *big.Rat // temperature scales with an offset: base = (v + off) * k
	pre     prefixKind
	aliases []string // long names, taken without prefixes
}

// q is n/d * 10^e, exact.
func q(n, d int64, e int) *big.Rat {
	r := big.NewRat(n, d)
	return r.Mul(r, pow10Rat(e))
}

func mulR(a, b *big.Rat) *big.Rat { return new(big.Rat).Mul(a, b) }
func quoR(a, b *big.Rat) *big.Rat { return new(big.Rat).Quo(a, b) }

// Exact definitions other units are built from.
var (
	inch     = q(254, 1, -4)           // NIST SP 811 B.8: international inch = 0.0254 m exactly
	foot     = mulR(inch, q(12, 1, 0)) // 0.3048 m
	mile     = mulR(foot, q(5280, 1, 0))
	pound    = q(45359237, 1, -8)                               // NIST SP 811 B.8: avoirdupois pound = 0.45359237 kg exactly
	gStd     = q(980665, 1, -5)                                 // CGPM 1901: standard gravity 9.80665 m/s^2 exactly
	lbf      = mulR(pound, gStd)                                // pound-force = 4.4482216152605 N
	usGallon = mulR(q(231, 1, 0), mulR(inch, mulR(inch, inch))) // 231 in^3 = 3.785411784 L
	atm      = q(101325, 1, 0)                                  // NIST SP 811 B.8: standard atmosphere = 101325 Pa exactly
	day      = q(86400, 1, 0)
)

// unitTable is every unit convert reads. Order within a dimension is the
// order units() lists.
var unitTable = []unitDef{
	// length — base m.
	{sym: "m", dim: "length", k: q(1, 1, 0), pre: preSI, aliases: []string{"meter", "meters", "metre", "metres"}},
	{sym: "in", dim: "length", k: inch, aliases: []string{"inch", "inches"}},
	{sym: "ft", dim: "length", k: foot, aliases: []string{"foot", "feet"}},
	{sym: "yd", dim: "length", k: mulR(foot, q(3, 1, 0)), aliases: []string{"yard", "yards"}},
	{sym: "mi", dim: "length", k: mile, aliases: []string{"mile", "miles"}},                  // international mile, 5280 ft
	{sym: "nmi", dim: "length", k: q(1852, 1, 0), aliases: []string{"nauticalmile"}},         // NIST SP 811 B.8: 1852 m exactly
	{sym: "au", dim: "length", k: q(149597870700, 1, 0)},                                     // IAU 2012 Resolution B2, exact
	{sym: "ly", dim: "length", k: q(9460730472580800, 1, 0), aliases: []string{"lightyear"}}, // c * Julian year, exact
	{sym: "Å", dim: "length", k: q(1, 1, -10), aliases: []string{"angstrom"}},
	// mass — base kg; the gram takes the prefixes.
	{sym: "g", dim: "mass", k: q(1, 1, -3), pre: preSI, aliases: []string{"gram", "grams", "kilogram", "kilograms"}},
	{sym: "t", dim: "mass", k: q(1000, 1, 0), aliases: []string{"tonne", "tonnes"}},
	{sym: "lb", dim: "mass", k: pound, aliases: []string{"lbs", "pound", "pounds"}},
	{sym: "oz", dim: "mass", k: quoR(pound, q(16, 1, 0)), aliases: []string{"ounce", "ounces"}},
	{sym: "st", dim: "mass", k: mulR(pound, q(14, 1, 0)), aliases: []string{"stone"}},
	{sym: "ton", dim: "mass", k: mulR(pound, q(2000, 1, 0)), aliases: []string{"tons"}}, // US short ton
	{sym: "gr", dim: "mass", k: quoR(pound, q(7000, 1, 0)), aliases: []string{"grain", "grains"}},
	{sym: "ct", dim: "mass", k: q(2, 1, -4), aliases: []string{"carat", "carats"}}, // metric carat, 200 mg
	// time — base s.
	{sym: "s", dim: "time", k: q(1, 1, 0), pre: preSI, aliases: []string{"sec", "second", "seconds"}},
	{sym: "min", dim: "time", k: q(60, 1, 0), aliases: []string{"mins", "minute", "minutes"}},
	{sym: "h", dim: "time", k: q(3600, 1, 0), aliases: []string{"hr", "hrs", "hour", "hours"}},
	{sym: "d", dim: "time", k: day, aliases: []string{"day", "days"}},
	{sym: "wk", dim: "time", k: mulR(day, q(7, 1, 0)), aliases: []string{"week", "weeks"}},
	{sym: "yr", dim: "time", k: mulR(day, q(36525, 1, -2)), aliases: []string{"year", "years"}}, // Julian year, 365.25 d (IAU)
	// temperature — base K. degC and degF carry an offset (SI brochure 2.3.1;
	// NIST SP 811 B.9: T/K = (t/degF + 459.67) / 1.8).
	{sym: "K", dim: "temperature", k: q(1, 1, 0), pre: preSI, aliases: []string{"kelvin"}},
	{sym: "degC", dim: "temperature", k: q(1, 1, 0), off: q(27315, 1, -2), aliases: []string{"°C", "celsius"}},
	{sym: "degF", dim: "temperature", k: big.NewRat(5, 9), off: q(45967, 1, -2), aliases: []string{"°F", "fahrenheit"}},
	{sym: "degR", dim: "temperature", k: big.NewRat(5, 9), aliases: []string{"°R", "rankine"}},
	// area — base m^2.
	{sym: "ha", dim: "area", k: q(1, 1, 4), aliases: []string{"hectare", "hectares"}},
	{sym: "acre", dim: "area", k: mulR(q(43560, 1, 0), mulR(foot, foot)), aliases: []string{"acres"}}, // 43560 ft^2 (international foot)
	// volume — base m^3.
	{sym: "L", dim: "volume", k: q(1, 1, -3), pre: preSI, aliases: []string{"liter", "liters", "litre", "litres"}},
	{sym: "l", dim: "volume", k: q(1, 1, -3), pre: preSI},
	{sym: "gal", dim: "volume", k: usGallon, aliases: []string{"gallon", "gallons"}},                // US gallon, NIST SP 811 B.8
	{sym: "impgal", dim: "volume", k: q(454609, 1, -8)},                                             // imperial gallon = 4.54609 L exactly
	{sym: "qt", dim: "volume", k: quoR(usGallon, q(4, 1, 0)), aliases: []string{"quart", "quarts"}}, // US liquid
	{sym: "pt", dim: "volume", k: quoR(usGallon, q(8, 1, 0)), aliases: []string{"pint", "pints"}},   // US liquid
	{sym: "cup", dim: "volume", k: quoR(usGallon, q(16, 1, 0)), aliases: []string{"cups"}},          // US customary cup
	{sym: "floz", dim: "volume", k: quoR(usGallon, q(128, 1, 0))},                                   // US fluid ounce
	{sym: "tbsp", dim: "volume", k: quoR(usGallon, q(256, 1, 0))},
	{sym: "tsp", dim: "volume", k: quoR(usGallon, q(768, 1, 0))},
	// speed — base m/s; any length/time compound works too.
	{sym: "kph", dim: "speed", k: big.NewRat(1000, 3600)},
	{sym: "mph", dim: "speed", k: quoR(mile, q(3600, 1, 0))},
	{sym: "kn", dim: "speed", k: big.NewRat(1852, 3600), aliases: []string{"knot", "knots"}},
	// acceleration — base m/s^2.
	{sym: "gn", dim: "acceleration", k: gStd},
	// force — base N.
	{sym: "N", dim: "force", k: q(1, 1, 0), pre: preSI, aliases: []string{"newton", "newtons"}},
	{sym: "dyn", dim: "force", k: q(1, 1, -5)},
	{sym: "kgf", dim: "force", k: gStd},
	{sym: "lbf", dim: "force", k: lbf},
	// pressure — base Pa.
	{sym: "Pa", dim: "pressure", k: q(1, 1, 0), pre: preSI, aliases: []string{"pascal", "pascals"}},
	{sym: "bar", dim: "pressure", k: q(1, 1, 5), pre: preSI},
	{sym: "atm", dim: "pressure", k: atm},
	{sym: "Torr", dim: "pressure", k: quoR(atm, q(760, 1, 0))}, // NIST SP 811 B.8: 101325/760 Pa
	// conventional mmHg: 13.5951 g/cm^3 * 1 mm * 9.80665 m/s^2 (NIST SP 811 B.8 footnote)
	{sym: "mmHg", dim: "pressure", k: q(133322387415, 1, -9)},
	{sym: "psi", dim: "pressure", k: quoR(lbf, mulR(inch, inch))},
	// energy — base J.
	{sym: "J", dim: "energy", k: q(1, 1, 0), pre: preSI, aliases: []string{"joule", "joules"}},
	{sym: "cal", dim: "energy", k: q(4184, 1, -3), pre: preSI, aliases: []string{"calorie", "calories"}}, // thermochemical calorie, 4.184 J (NIST SP 811 B.8)
	{sym: "Wh", dim: "energy", k: q(3600, 1, 0), pre: preSI},
	{sym: "eV", dim: "energy", k: q(1602176634, 1, -28), pre: preSI}, // SI brochure 9th ed., exact since 2019
	{sym: "BTU", dim: "energy", k: q(105505585262, 1, -8)},           // International Table BTU (NIST SP 811 B.8)
	{sym: "erg", dim: "energy", k: q(1, 1, -7)},
	// power — base W.
	{sym: "W", dim: "power", k: q(1, 1, 0), pre: preSI, aliases: []string{"watt", "watts"}},
	{sym: "hp", dim: "power", k: mulR(q(550, 1, 0), mulR(foot, lbf)), aliases: []string{"horsepower"}}, // mechanical: 550 ft*lbf/s
	// angle — base rad; the others are rational multiples of pi.
	{sym: "rad", dim: "angle", k: q(1, 1, 0), pre: preSI, aliases: []string{"radian", "radians"}},
	{sym: "deg", dim: "angle", k: big.NewRat(1, 180), pi: 1, aliases: []string{"°", "degree", "degrees"}},
	{sym: "arcmin", dim: "angle", k: big.NewRat(1, 10800), pi: 1},
	{sym: "arcsec", dim: "angle", k: big.NewRat(1, 648000), pi: 1},
	{sym: "grad", dim: "angle", k: big.NewRat(1, 200), pi: 1, aliases: []string{"gon"}},
	{sym: "turn", dim: "angle", k: q(2, 1, 0), pi: 1, aliases: []string{"turns", "rev", "revolution", "revolutions"}},
	// frequency — base Hz.
	{sym: "Hz", dim: "frequency", k: q(1, 1, 0), pre: preSI, aliases: []string{"hertz"}},
	{sym: "rpm", dim: "frequency", k: big.NewRat(1, 60)}, // revolutions per minute as a frequency
	// data size — base bit.
	{sym: "bit", dim: "data size", k: q(1, 1, 0), pre: preData, aliases: []string{"bits"}},
	{sym: "b", dim: "data size", k: q(1, 1, 0), pre: preData},
	{sym: "B", dim: "data size", k: q(8, 1, 0), pre: preData, aliases: []string{"byte", "bytes"}},
	// the remaining base units, for compounds.
	{sym: "A", dim: "electric current", k: q(1, 1, 0), pre: preSI, aliases: []string{"ampere", "amperes"}},
	{sym: "mol", dim: "amount of substance", k: q(1, 1, 0), pre: preSI},
	{sym: "cd", dim: "luminous intensity", k: q(1, 1, 0), pre: preSI, aliases: []string{"candela"}},
}

type prefixDef struct {
	sym string
	k   *big.Rat
}

// siPrefixes are tried longest first, so "dam" is deca-metre.
var siPrefixes = []prefixDef{
	{"da", q(1, 1, 1)},
	{"y", q(1, 1, -24)}, {"z", q(1, 1, -21)}, {"a", q(1, 1, -18)}, {"f", q(1, 1, -15)},
	{"p", q(1, 1, -12)}, {"n", q(1, 1, -9)}, {"µ", q(1, 1, -6)}, {"μ", q(1, 1, -6)}, {"u", q(1, 1, -6)},
	{"m", q(1, 1, -3)}, {"c", q(1, 1, -2)}, {"d", q(1, 1, -1)}, {"h", q(1, 1, 2)},
	{"k", q(1, 1, 3)}, {"M", q(1, 1, 6)}, {"G", q(1, 1, 9)}, {"T", q(1, 1, 12)},
	{"P", q(1, 1, 15)}, {"E", q(1, 1, 18)}, {"Z", q(1, 1, 21)}, {"Y", q(1, 1, 24)},
}

// binaryPrefixes are IEC 80000-13: Ki = 2^10 and so on.
var binaryPrefixes = []prefixDef{
	{"Ki", ratPow2(10)}, {"Mi", ratPow2(20)}, {"Gi", ratPow2(30)}, {"Ti", ratPow2(40)},
	{"Pi", ratPow2(50)}, {"Ei", ratPow2(60)}, {"Zi", ratPow2(70)}, {"Yi", ratPow2(80)},
}

func ratPow2(n uint) *big.Rat {
	return new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), n))
}

// prefixesFor lists the prefixes unit u accepts.
func prefixesFor(u *unitDef) []prefixDef {
	switch u.pre {
	case preSI:
		return siPrefixes
	case preData:
		var out []prefixDef
		for _, p := range siPrefixes {
			if p.k.Cmp(q(1, 1, 3)) >= 0 {
				out = append(out, p)
			}
		}
		return append(out, binaryPrefixes...)
	}
	return nil
}

type unitIndex struct {
	byName map[string]*unitDef
	dimOf  map[string]dims
}

var unitIdx = indexUnits()

func indexUnits() unitIndex {
	ix := unitIndex{byName: map[string]*unitDef{}, dimOf: map[string]dims{}}
	for _, d := range dimensions {
		ix.dimOf[d.name] = d.d
	}
	for i := range unitTable {
		u := &unitTable[i]
		ix.byName[u.sym] = u
		for _, a := range u.aliases {
			ix.byName[a] = u
		}
	}
	return ix
}

// unitVal is a parsed unit string.
type unitVal struct {
	d  dims
	k  *big.Rat
	pi int
	// off is the offset of a bare temperature scale (degC, degF).
	off *big.Rat
	// offsetInCompound is set when degC or degF sits inside a compound,
	// where "a degree Celsius" could mean a point or a difference.
	offsetInCompound bool
}

// lookupUnit resolves one name: a symbol or a long name exactly, else an SI
// (or, for data, binary) prefix on a symbol that takes one.
func lookupUnit(name string) (unitVal, bool) {
	if u, ok := unitIdx.byName[name]; ok {
		return unitOf(u, q(1, 1, 0)), true
	}
	for _, list := range [][]prefixDef{binaryPrefixes, siPrefixes} {
		for _, p := range list {
			rest, ok := strings.CutPrefix(name, p.sym)
			if !ok || rest == "" {
				continue
			}
			u, ok := unitIdx.byName[rest]
			if !ok || u.sym != rest {
				continue // long names take no prefix
			}
			for _, allowed := range prefixesFor(u) {
				if allowed.sym == p.sym {
					return unitOf(u, p.k), true
				}
			}
		}
	}
	return unitVal{}, false
}

func unitOf(u *unitDef, scale *big.Rat) unitVal {
	return unitVal{d: unitIdx.dimOf[u.dim], k: mulR(u.k, scale), pi: u.pi, off: u.off}
}

// unitNames is every name a unit string may use, prefixed forms included,
// for the suggestions of an unknown_unit refusal.
func unitNames() []string {
	var out []string
	for i := range unitTable {
		u := &unitTable[i]
		out = append(out, u.sym)
		out = append(out, u.aliases...)
		for _, p := range prefixesFor(u) {
			out = append(out, p.sym+u.sym)
		}
	}
	return out
}

// dimName names a vector in words when it is a listed dimension, otherwise
// as a formula over the base units.
func dimName(d dims) string {
	for _, dm := range dimensions {
		if dm.d == d {
			return dm.name
		}
	}
	var num, den []string
	for i, n := range d {
		switch {
		case n == 1:
			num = append(num, baseSymbols[i])
		case n > 1:
			num = append(num, baseSymbols[i]+"^"+itoa(n))
		case n == -1:
			den = append(den, baseSymbols[i])
		case n < -1:
			den = append(den, baseSymbols[i]+"^"+itoa(-n))
		}
	}
	s := strings.Join(num, "*")
	if s == "" {
		if len(den) == 0 {
			return "dimensionless"
		}
		s = "1"
	}
	if len(den) == 1 {
		s += "/" + den[0]
	} else if len(den) > 1 {
		s += "/(" + strings.Join(den, "*") + ")"
	}
	return "the dimension " + s
}

func itoa(n int) string { return big.NewInt(int64(n)).String() }
