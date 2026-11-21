package calc

import (
	"math"
	"math/big"
	"sort"
)

// FuncInfo describes one built-in function. Functions() is the single
// source the help text and its tests are checked against.
type FuncInfo struct {
	Name      string `json:"name"`
	Signature string `json:"signature"`
	Section   string `json:"section"`
	Summary   string `json:"summary"`
}

// Sections, in the order help lists them.
const (
	SecControl    = "control"
	SecElementary = "elementary"
	SecTrig       = "trigonometric"
	SecSpecial    = "special"
	SecIntegers   = "integers"
	SecBits       = "bits and bases"
	SecSequences  = "sequences"
	SecRandom     = "random"
	SecDates      = "dates"
)

// args is one call's evaluated arguments plus where it was written.
type args struct {
	v    []val
	pos  int
	name string
}

type builtin struct {
	name, sig, summary string
	section            string
	min, max           int // max < 0: variadic
	// vec maps the function over a list first argument (recursively), with
	// the other arguments held, so round(prices*1.2, 2) works on a list.
	vec bool
	fn  func(e *evaluator, a args) (val, *Error)
}

// sections lists every table; a new section is one more slice here. The
// tables hold only immutable data and function pointers. Implementations
// reach the table through evaluator.funcs, never through builtinIndex, so
// the package initialises without a cycle.
var sections = [][]builtin{
	controlFuncs, elementaryFuncs, trigFuncs, specialFuncs,
	integerFuncs, bitFuncs, sequenceFuncs, randomFuncs,
	dateFuncs,
}

var builtinIndex = indexBuiltins()

func indexBuiltins() map[string]*builtin {
	m := map[string]*builtin{}
	for _, sec := range sections {
		for i := range sec {
			m[sec[i].name] = &sec[i]
		}
	}
	return m
}

// controlFuncs holds the special forms. `if` is evaluated lazily by the
// evaluator; the entry is here for help, arity and the reserved-name check.
var controlFuncs = []builtin{
	{name: "if", sig: "if(cond, a, b)", section: SecControl, min: 3, max: 3,
		summary: "a when cond is true, else b; only the chosen branch is evaluated"},
}

// Functions lists every built-in, grouped by section in help order, then
// by name.
func Functions() []FuncInfo {
	order := map[string]int{}
	for i, s := range []string{SecControl, SecElementary, SecTrig, SecSpecial, SecIntegers, SecBits, SecSequences, SecRandom, SecDates} {
		order[s] = i
	}
	var out []FuncInfo
	for _, sec := range sections {
		for _, b := range sec {
			out = append(out, FuncInfo{Name: b.name, Signature: b.sig, Section: b.section, Summary: b.summary})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if order[out[i].Section] != order[out[j].Section] {
			return order[out[i].Section] < order[out[j].Section]
		}
		return out[i].Name < out[j].Name
	})
	return out
}

var constants = map[string]float64{"pi": math.Pi, "e": math.E, "tau": 2 * math.Pi, "phi": math.Phi}

func isConstant(name string) bool { _, ok := constants[name]; return ok }

// --- argument helpers shared by every section ---

func (a args) num(i int) (num, *Error) {
	n, ok := a.v[i].(num)
	if !ok {
		return num{}, errf(codeType, a.pos, "%s: argument %d must be a number, got a %s", a.name, i+1, typeName(a.v[i]))
	}
	return n, nil
}

func (a args) list(i int) ([]val, *Error) {
	l, ok := a.v[i].([]val)
	if !ok {
		return nil, errf(codeType, a.pos, "%s: argument %d must be a list, got a %s", a.name, i+1, typeName(a.v[i]))
	}
	return l, nil
}

// integer returns argument i as an exact integer. Floats are refused, not
// rounded: a float result never silently becomes exact again.
func (a args) integer(i int) (*big.Int, *Error) {
	n, err := a.num(i)
	if err != nil {
		return nil, err
	}
	v, ok := intOf(n)
	if !ok {
		e := errf(codeType, a.pos, "%s: argument %d must be an exact integer", a.name, i+1)
		if !n.exact() {
			e.Hint = "it is a float; floats never become exact again — round() gives a float too, so compute the argument exactly"
		} else {
			e.Hint = "use floor(), ceil() or round() to make it an integer"
		}
		return nil, e
	}
	return v, nil
}

// small returns argument i as an integer within ±lim (exact or integral
// float), for counts, places and radixes.
func (a args) small(i int, lim int64) (int64, *Error) {
	n, err := a.num(i)
	if err != nil {
		return 0, err
	}
	v, ok := smallInt(n, lim)
	if !ok {
		return 0, errf(codeDomain, a.pos, "%s: argument %d must be an integer between %d and %d", a.name, i+1, -lim, lim)
	}
	return v, nil
}

func (a args) fn(i int) (*fnVal, *Error) {
	f, ok := a.v[i].(*fnVal)
	if !ok {
		return nil, errf(codeType, a.pos, "%s: argument %d must be a function, got a %s", a.name, i+1, typeName(a.v[i])).
			withHint("write a lambda such as `x -> x^2`")
	}
	return f, nil
}

// float1 wraps a float function of one number.
func float1(f func(float64) float64) func(*evaluator, args) (val, *Error) {
	return func(_ *evaluator, a args) (val, *Error) {
		x, err := a.num(0)
		if err != nil {
			return nil, err
		}
		return checkFloat(f(x.float()), a.pos, a.name+"("+fmtArg(x)+")")
	}
}

// fmtArg renders a number for an error message.
func fmtArg(n num) string {
	s, _ := formatNum(n, 6)
	return s
}

// suggest returns up to three names near name, nearest first: within edit
// distance 1 for a name of up to three letters (two edits would turn "xx"
// into "e" or "if"), 2 otherwise.
func suggest(name string, names []string) []string {
	type cand struct {
		s string
		d int
	}
	limit := 2
	if len(name) <= 3 {
		limit = 1
	}
	var cs []cand
	for _, n := range names {
		if d := editDistance(name, n); d <= limit {
			cs = append(cs, cand{n, d})
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].d != cs[j].d {
			return cs[i].d < cs[j].d
		}
		return cs[i].s < cs[j].s
	})
	var out []string
	for i := 0; i < len(cs) && i < 3; i++ {
		out = append(out, cs[i].s)
	}
	return out
}

func editDistance(a, b string) int {
	if len(a) > 64 || len(b) > 64 {
		return 99
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			c := 1
			if a[i-1] == b[j-1] {
				c = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+c)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
