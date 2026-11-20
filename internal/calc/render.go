package calc

import (
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// formatNum renders n. An exact integer or terminating fraction prints in
// full; an exact non-terminating value prints to digits significant digits
// and also returns its fraction ("1/3"); a float prints the shortest text
// that round-trips after rounding to digits significant digits (at most
// 17, all a float64 holds).
func formatNum(n num, digits int) (text, fraction string) {
	if !n.exact() {
		return formatFloat(n.f, min(digits, 17)), ""
	}
	r := n.r
	if r.IsInt() {
		return r.Num().String(), ""
	}
	if places, ok := terminatingPlaces(r.Denom()); ok {
		return strings.TrimRight(r.FloatString(places), "0"), ""
	}
	return formatRatSig(r, digits), r.RatString()
}

func formatFloat(f float64, digits int) string {
	if f == 0 {
		return "0"
	}
	g, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'e', digits-1, 64), 64)
	if a := math.Abs(g); a >= 1e-6 && a < 1e21 {
		return strconv.FormatFloat(g, 'f', -1, 64)
	}
	// Go writes e-09; drop the padding so floats and exact numbers share
	// one exponent style.
	s := strconv.FormatFloat(g, 'e', -1, 64)
	return strings.Replace(strings.Replace(s, "e-0", "e-", 1), "e+0", "e+", 1)
}

// terminatingPlaces reports whether 1/d has a finite decimal expansion
// (d = 2^a 5^b) and how many places it needs: max(a, b).
func terminatingPlaces(d *big.Int) (int, bool) {
	x := new(big.Int).Set(d)
	a := int(x.TrailingZeroBits())
	x.Rsh(x, uint(a))
	five := big.NewInt(5)
	b := 0
	m := new(big.Int)
	for x.Cmp(big.NewInt(1)) > 0 {
		q, rem := new(big.Int).QuoRem(x, five, m)
		if rem.Sign() != 0 {
			return 0, false
		}
		x = q
		b++
	}
	return max(a, b), true
}

var bigTen = big.NewInt(10)

// formatRatSig rounds r half away from zero to digits significant digits.
func formatRatSig(r *big.Rat, digits int) string {
	neg := r.Sign() < 0
	a := new(big.Rat).Abs(r)
	// e = floor(log10 a): estimate from the decimal lengths, then correct.
	e := len(a.Num().String()) - len(a.Denom().String())
	if a.Cmp(pow10Rat(e)) < 0 {
		e--
	}
	scaled := new(big.Rat).Mul(a, pow10Rat(digits-1-e))
	m := roundHalfAway(scaled)
	if len(m.String()) > digits { // rounded up to 10^digits
		m.Quo(m, bigTen)
		e++
	}
	s := m.String()
	s = strings.TrimRight(s, "0")
	if s == "" {
		s = "0"
	}
	var out string
	switch {
	case e >= 21 || e < -7:
		out = s[:1]
		if len(s) > 1 {
			out += "." + s[1:]
		}
		out += "e" + signedExp(e)
	case e < 0:
		out = "0." + strings.Repeat("0", -e-1) + s
	case len(s) > e+1:
		out = s[:e+1] + "." + s[e+1:]
	default:
		out = s + strings.Repeat("0", e+1-len(s))
	}
	if neg {
		out = "-" + out
	}
	return out
}

func signedExp(e int) string {
	if e < 0 {
		return strconv.Itoa(e)
	}
	return "+" + strconv.Itoa(e)
}

func pow10Rat(e int) *big.Rat {
	p := new(big.Int).Exp(bigTen, big.NewInt(int64(abs(e))), nil)
	if e < 0 {
		return new(big.Rat).SetFrac(big.NewInt(1), p)
	}
	return new(big.Rat).SetInt(p)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// roundHalfAway rounds a non-negative or negative rational to the nearest
// integer, ties away from zero.
func roundHalfAway(r *big.Rat) *big.Int {
	twice := new(big.Int).Mul(r.Num(), big.NewInt(2))
	twice.Add(twice, new(big.Int).Mul(r.Denom(), big.NewInt(int64(r.Sign()))))
	den := new(big.Int).Mul(r.Denom(), big.NewInt(2))
	return new(big.Int).Quo(twice, den) // Quo truncates toward zero
}

// capWriter stops writing at limit bytes, cutting on a rune boundary.
type capWriter struct {
	b     strings.Builder
	limit int
	over  bool
}

func (w *capWriter) write(s string) {
	if w.over {
		return
	}
	room := w.limit - w.b.Len()
	if len(s) <= room {
		w.b.WriteString(s)
		return
	}
	cut := room
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	w.b.WriteString(s[:cut])
	w.over = true
}

// renderText writes v; a string at the top level prints bare, inside a
// list quoted.
func renderText(w *capWriter, v val, digits int, top bool) {
	if w.over {
		return
	}
	switch v := v.(type) {
	case num:
		s, _ := formatNum(v, digits)
		w.write(s)
	case string:
		if top {
			w.write(v)
			return
		}
		q, _ := json.Marshal(v)
		w.write(string(q))
	case bool:
		w.write(strconv.FormatBool(v))
	case []val:
		w.write("[")
		for i, x := range v {
			if i > 0 {
				w.write(", ")
			}
			renderText(w, x, digits, false)
			if w.over {
				return
			}
		}
		w.write("]")
	case *fnVal:
		w.write(fnDescribe(v))
	}
}

func fnDescribe(f *fnVal) string {
	if f.builtin != nil {
		return "function " + f.builtin.sig
	}
	return "function " + fnSignature(f)
}

// toJSON converts v for Response.Result. Numbers become json.Number with
// the rendered text, so an exact 300-digit integer is not rounded through a
// float64 by the encoder.
func toJSON(v val, digits int) any {
	switch v := v.(type) {
	case num:
		s, _ := formatNum(v, digits)
		return json.Number(s)
	case string, bool:
		return v
	case []val:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = toJSON(x, digits)
		}
		return out
	case *fnVal:
		return fnDescribe(v)
	}
	return nil
}

// allExact reports whether v holds at least one number and every number
// in it is exact.
func allExact(v val) bool {
	seen, ok := exactWalk(v)
	return seen && ok
}

func exactWalk(v val) (seen, ok bool) {
	switch v := v.(type) {
	case num:
		return true, v.exact()
	case []val:
		ok = true
		for _, x := range v {
			s, o := exactWalk(x)
			seen = seen || s
			ok = ok && o
		}
		return seen, ok
	}
	return false, true
}

// shape returns the dimensions of a rectangular nested list: [n] for a flat
// list, [n, m] for a list of n lists of m, and so on; ragged nesting stops
// at the last regular level.
func shape(v val) []int {
	l, ok := v.([]val)
	if !ok {
		return nil
	}
	dims := []int{len(l)}
	if len(l) == 0 {
		return dims
	}
	first, ok := l[0].([]val)
	if !ok {
		return dims
	}
	for _, x := range l {
		xl, ok := x.([]val)
		if !ok || len(xl) != len(first) {
			return dims
		}
	}
	sub := shape(first)
	for _, x := range l[1:] {
		if !equalInts(shape(x), sub) {
			return append(dims, len(first))
		}
	}
	return append(dims, sub...)
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
