package calc

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestUnits_KnownValuesAreExact(t *testing.T) {
	cases := []struct{ expr, want string }{
		{`convert(5, "mi", "km")`, "8.04672"},
		{`convert(1, "lb", "g")`, "453.59237"},
		{`convert(1, "GiB", "B")`, "1073741824"},
		{`convert(90, "km/h", "m/s")`, "25"},
		{`convert(1, "in", "cm")`, "2.54"},
		{`convert(1, "gal", "L")`, "3.785411784"},
		{`convert(1, "impgal", "L")`, "4.54609"},
		{`convert(1, "kcal", "J")`, "4184"},
		{`convert(1, "kWh", "MJ")`, "3.6"},
		{`convert(1, "atm", "kPa")`, "101.325"},
		{`convert(1, "lbf", "N")`, "4.4482216152605"},
		{`convert(1, "hp", "W")`, "745.69987158227022"},
		{`convert(1, "kg*m/s^2", "N")`, "1"},
		{`convert(1, "m^3", "L")`, "1000"},
		{`convert(1, "m³", "L")`, "1000"},
		{`convert(1, "acre", "m^2")`, "4046.8564224"},
		{`convert(1, "deg", "arcmin")`, "60"},
		{`convert(100, "degC", "degF")`, "212"},
		{`convert(0, "degC", "K")`, "273.15"},
		{`convert(-40, "degF", "degC")`, "-40"},
		{`convert(10, "degC", "degR")`, "509.67"},
		{`convertdelta(10, "degC", "degF")`, "18"},
		{`convertdelta(1, "degC/min", "K/s")`, "0.0166666666666667"},
		{`convert(1, "kB", "b")`, "8000"},
		{`convert(1, "Mbit", "kB")`, "125"},
		{`convert(60, "rpm", "Hz")`, "1"},
		{`convert(1, "1/s", "Hz")`, "1"},
		{`convert(2, "(kg*m)/(s*s)", "N")`, "2"},
		{`convert(1, "km / h", "m/s")`, "0.277777777777778"},
		{`convert([1, 2, [3]], "ft", "in")`, "[12, 24, [36]]"},
		{`convert(1, "dam", "m")`, "10"},
		{`convert(1, "µs", "ns") + convert(1, "us", "ns")`, "2000"},
	}
	for _, c := range cases {
		r := mustEval(t, c.expr)
		if r.Text != c.want {
			t.Errorf("%s = %s, want %s", c.expr, r.Text, c.want)
		}
		if !r.Exact {
			t.Errorf("%s is not exact", c.expr)
		}
	}
	r := mustEval(t, `convert(72, "degF", "degC")`)
	if r.Fraction != "200/9" || !r.Exact {
		t.Errorf("72 degF in degC = %s (%s), want exactly 200/9", r.Text, r.Fraction)
	}
}

func TestUnits_PiFactorsGiveAFloat(t *testing.T) {
	r := mustEval(t, `convert(180, "deg", "rad")`)
	if r.Exact || r.Text != "3.14159265358979" {
		t.Errorf("180 deg in rad = %s exact=%v, want pi as a float", r.Text, r.Exact)
	}
	r = mustEval(t, `convert(sqrt(2), "m", "cm")`)
	if r.Exact || r.Text != "141.42135623731" {
		t.Errorf("float input = %s exact=%v", r.Text, r.Exact)
	}
}

// Every unit of the table, under every prefix it takes, converts to its
// dimension's coherent unit and back to exactly the value it started from.
func TestUnits_EveryUnitRoundTripsExactly(t *testing.T) {
	v := big.NewRat(123456789, 1000)
	for _, name := range unitNames() {
		u, ok := lookupUnit(name)
		if !ok {
			t.Errorf("%s is listed but does not resolve", name)
			continue
		}
		dim := dimName(u.d)
		base := coherentUnit(t, u.d)
		for _, pair := range [][2]string{{name, base}, {base, name}} {
			c, err := planConversion(pair[0], pair[1], false, args{name: "convert"})
			if err != nil {
				t.Errorf("%s (%s) -> %s: %v", pair[0], dim, pair[1], err)
				continue
			}
			if c.pi != 0 {
				continue // pi multiples are floats by design
			}
			back, err := planConversion(pair[1], pair[0], false, args{name: "convert"})
			if err != nil {
				t.Fatal(err)
			}
			mid := new(big.Rat).Add(new(big.Rat).Mul(v, c.a), c.b)
			got := new(big.Rat).Add(new(big.Rat).Mul(mid, back.a), back.b)
			if got.Cmp(v) != 0 {
				t.Errorf("%s -> %s -> back gives %s, want %s", pair[0], pair[1], got.RatString(), v.RatString())
			}
		}
	}
}

// coherentUnit writes a vector as a compound of base symbols.
func coherentUnit(t *testing.T, d dims) string {
	t.Helper()
	var parts []string
	for i, n := range d {
		if n != 0 {
			parts = append(parts, baseSymbols[i]+"^"+itoa(n))
		}
	}
	if len(parts) == 0 {
		t.Fatal("dimensionless unit in the table")
	}
	return strings.Join(parts, "*")
}

// Every name a unit string may use resolves one way: no symbol is also a
// prefix on another symbol, and no two prefixed forms coincide.
func TestUnits_EveryNameIsUnambiguous(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range unitNames() {
		if seen[n] {
			t.Errorf("%s names two units", n)
		}
		seen[n] = true
	}
	for _, d := range dimensions {
		found := false
		for _, u := range unitTable {
			found = found || u.dim == d.name
		}
		if !found {
			t.Errorf("dimension %s has no unit", d.name)
		}
	}
	for _, u := range unitTable {
		if _, ok := unitIdx.dimOf[u.dim]; !ok {
			t.Errorf("%s names dimension %q, which is not listed", u.sym, u.dim)
		}
	}
}

func TestUnits_PrefixesFollowTheTable(t *testing.T) {
	ok := []string{"mm", "Mm", "km", "kg", "mg", "µg", "ns", "MHz", "kPa", "hPa", "mbar", "kcal", "MWh", "keV",
		"mL", "ml", "kB", "MB", "KiB", "TiB", "Gb", "Gbit", "mK", "mrad", "dam"}
	for _, n := range ok {
		if _, found := lookupUnit(n); !found {
			t.Errorf("%s does not resolve", n)
		}
	}
	bad := []string{"kmi", "kft", "mdegF", "mB", "cB", "Kim", "kkg", "kmile", "Km", "KB"}
	for _, n := range bad {
		if _, found := lookupUnit(n); found {
			t.Errorf("%s resolves, want it refused", n)
		}
	}
	// Case matters: mm and Mm, b and B.
	r := mustEval(t, `[convert(1, "Mm", "mm"), convert(1, "B", "b")]`)
	if r.Text != "[1000000000, 8]" {
		t.Errorf("case-sensitive units = %s", r.Text)
	}
}

func TestUnits_MalformedStringsAreRefused(t *testing.T) {
	for _, u := range []string{"", "km//h", "m^", "m^0", "m^13", "m2", "(m", "m)", "*m", "m*", "km h", "m^x",
		"((((((((((m))))))))))", "(m^12)^12", strings.Repeat("m*", 40) + "m"} {
		e := mustFail(t, `convert(1, "`+u+`", "m")`, codeUnknownUnit)
		if !strings.Contains(e.Detail, "not well formed") && !strings.Contains(e.Detail, "bytes") {
			t.Errorf("%q: detail %q", u, e.Detail)
		}
	}
}

func TestUnits_DimensionMismatchNamesBoth(t *testing.T) {
	e := mustFail(t, `convert(1, "kg", "m")`, codeDimension)
	if !strings.Contains(e.Detail, "mass") || !strings.Contains(e.Detail, "length") {
		t.Errorf("detail %q does not name both dimensions", e.Detail)
	}
	e = mustFail(t, `convert(1, "kg*m", "N")`, codeDimension)
	if !strings.Contains(e.Detail, "kg*m") || !strings.Contains(e.Detail, "force") {
		t.Errorf("compound detail %q", e.Detail)
	}
	e = mustFail(t, `convert(1, "lb", "lbf")`, codeDimension)
	if !strings.Contains(e.Hint, "gn") {
		t.Errorf("mass/force hint %q", e.Hint)
	}
	mustFail(t, `convert(1, "rad", "1")`, codeDimension)
	mustFail(t, `convert(1, "bit", "1")`, codeDimension)
}

func TestUnits_OffsetTemperatureInACompoundIsRefusedButDeltaTakesIt(t *testing.T) {
	e := mustFail(t, `convert(1, "degC/s", "K/s")`, codeDimension)
	if !strings.Contains(e.Hint, "convertdelta") {
		t.Errorf("hint %q", e.Hint)
	}
	mustFail(t, `convert(1, "degF^2", "K^2")`, codeDimension)
	if got, _ := evalText(t, `convert(1, "J/K", "J/degR")`); got != "0.555555555555556" {
		t.Errorf("J/K in J/degR = %s, want 5/9 (K and degR are absolute)", got)
	}
}

func TestUnits_UnknownUnitSuggestsNearNames(t *testing.T) {
	cases := map[string]string{"Km": "km", "kgs": "kg", "furlongs": "", "C": "degC", "KB": "KiB", "kilometers": "km"}
	for name, want := range cases {
		e := mustFail(t, `convert(1, "`+name+`", "m")`, codeUnknownUnit)
		if want != "" && !strings.Contains(e.Hint, want) {
			t.Errorf("%s: hint %q, want it to mention %s", name, e.Hint, want)
		}
		if e.Hint == "" {
			t.Errorf("%s: no hint", name)
		}
	}
}

func TestUnits_ListsDimensionsAndUnits(t *testing.T) {
	r := mustEval(t, `units()`)
	if !strings.HasPrefix(r.Text, `["length", "mass", "time", "temperature"`) || r.Shape[0] != len(dimensions) {
		t.Errorf("units() = %s", r.Text)
	}
	if got, _ := evalText(t, `units("temperature")`); got != `["K", "degC", "degF", "degR"]` {
		t.Errorf("units(temperature) = %s", got)
	}
	mustFail(t, `units("colour")`, codeUnknownUnit)
	mustFail(t, `units(1)`, codeType)
	mustFail(t, `convert("1", "m", "km")`, codeType)
	mustFail(t, `convert(1, m, "km")`, codeUnknownName)
	mustFail(t, `convert(1, 2, "km")`, codeType)
}

// A huge list charges a step per element, so it stays inside the step bound
// instead of running unbounded.
func TestUnits_HugeListsChargeAndStayBounded(t *testing.T) {
	// evalWithin with a minute of wall: under -race the 2 s wall would decide
	// before the step bound this test is about.
	r, err := evalWithin(context.Background(), Request{Expr: `len(convert(1:200000, "mi", "km"))`}, time.Minute)
	if err != nil || r.Text != "200000" {
		t.Fatalf("200k elements: %v %s", err, r.Text)
	}
	e := &evaluator{ctx: context.Background(), funcs: builtinIndex, global: &scope{vars: map[string]val{}}, sizes: map[listKey]int{}}
	before := e.steps
	l := make([]val, 1000)
	for i := range l {
		l[i] = exactInt64(int64(i))
	}
	if _, err := fnCall(e, "convert", l, "m", "km"); err != nil {
		t.Fatal(err)
	}
	if e.steps-before < len(l) {
		t.Errorf("converting %d elements charged %d steps", len(l), e.steps-before)
	}
	// The range alone is 900,000 steps, under the bound; the conversion's
	// step per element is what passes it.
	_, err = evalWithin(context.Background(), Request{Expr: `x = 1:900000; convert(x, "m", "km")`}, time.Minute)
	if err == nil || err.Code != string(codeStepLimit) {
		t.Errorf("900k elements: err = %v, want step_limit", err)
	}
}

func fnCall(e *evaluator, name string, argv ...val) (val, *Error) {
	return e.callBuiltin(builtinIndex[name], argv, 0)
}
