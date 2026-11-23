package calc

import (
	"context"
	"errors"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/semaphore"
)

type evaluator struct {
	ctx       context.Context
	steps     int
	nextCheck int
	// loadSteps is one step per cell read by reference: the load is charged,
	// and bounded by MaxRefCells, on top of MaxSteps, so it never takes the
	// expression's own budget.
	loadSteps int
	funcs     map[string]*builtin
	global    *scope
	assigned  []string
	rng       *rand.Rand
	calls     []*fnVal
	// sizes caches each list value's expanded size (see size).
	sizes map[listKey]int
	// now is the clock today() and now() read (Request.Now); nowAt is its
	// one reading for this evaluation (see clock).
	now   func() time.Time
	nowAt time.Time
	// zones caches tz()'s time.LoadLocation, which reads a file per call.
	zones map[string]*time.Location
	// gate bounds how many heavy matrix operations run at once across the
	// process (Request.Gate); nil means no gating.
	gate *semaphore.Weighted
}

// listKey identifies a list value. Lists are never mutated once built, so
// the first element's address and the length name one list.
type listKey struct {
	first *val
	n     int
}

// size is the number of values nested in v, every level counted and a
// shared list counted once per place it appears, saturating just past
// MaxListLen. Checking it whenever a list is produced bounds every later
// walk of the value (exactness, shape, rendering, keys, equality):
// otherwise `b = [a, a]` repeated doubles the expanded size per statement
// for a constant number of steps.
func (e *evaluator) size(v val) int {
	switch x := v.(type) {
	case *tableVal:
		return x.cells()
	case rowVal:
		return len(x.t.names)
	}
	l, ok := v.([]val)
	if !ok || len(l) == 0 {
		return 0
	}
	k := listKey{first: &l[0], n: len(l)}
	if s, ok := e.sizes[k]; ok {
		return s
	}
	s := 0
	for _, x := range l {
		s += 1 + e.size(x)
		if s > MaxListLen {
			s = MaxListLen + 1
			break
		}
	}
	e.sizes[k] = s
	return s
}

func (e *evaluator) checkSize(v val, pos int) *Error {
	if e.size(v) > MaxListLen {
		return errf(codeListTooLong, pos, "the list holds more than %d values, counting the elements of nested lists", MaxListLen).
			withHint("a list nested twice counts twice; build a smaller structure or summarise it first")
	}
	return nil
}

// charge counts n steps and, every ctxCheckEvery steps, polls ctx so a
// deadline or a cancel stops evaluation promptly.
func (e *evaluator) charge(n int, pos int) *Error {
	e.steps += n
	if e.steps > MaxSteps+e.loadSteps {
		return errf(codeStepLimit, pos, "evaluation took more than %d steps", MaxSteps).
			withHint("shrink the ranges or lists, or split the work into several calls")
	}
	if e.steps >= e.nextCheck {
		e.nextCheck = e.steps + ctxCheckEvery
		if err := e.ctx.Err(); err != nil {
			return ctxErr(err, pos)
		}
	}
	return nil
}

// poll checks ctx every ctxCheckEvery iterations of a loop whose steps
// were charged up front in bulk, so a long range or unique still stops
// promptly on a deadline.
func (e *evaluator) poll(i int, pos int) *Error {
	if i%ctxCheckEvery != ctxCheckEvery-1 {
		return nil
	}
	if err := e.ctx.Err(); err != nil {
		return ctxErr(err, pos)
	}
	return nil
}

func ctxErr(err error, pos int) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errf(codeTimeLimit, pos, "evaluation ran past its time limit (%s at most)", MaxWall)
	}
	return errf(codeCanceled, pos, "evaluation was canceled")
}

// aliases are names from other languages a model reaches for; the error
// for them names the function to use.
var aliases = map[string]string{
	"ln": "log", "arcsin": "asin", "arccos": "acos", "arctan": "atan",
	"choose": "binomial", "nCr": "binomial", "comb": "binomial", "fact": "factorial",
	"length": "len", "ptp": "spread", "average": "mean", "avg": "mean",
	"std": "stdev", "stddev": "stdev",
	"real": "re", "imag": "im", "conjugate": "conj", "angle": "arg", "phase": "arg",
	"roots": "polyroots",
	// Names a model brought from other tools in the Calc help measurement.
	"powmod": "modpow", "correl": "correlation", "corr": "correlation",
	"pearson": "correlation", "seq": "range",
	// Spreadsheet date functions under their Excel names.
	"networkdays": "workdays", "workday": "addworkdays", "edate": "addmonths",
}

// eval evaluates n and refuses a list result over the size bound. Every
// value reaches a later walk through here: arguments, function bodies,
// statements and looked-up vars.
func (e *evaluator) eval(n node, sc *scope) (val, *Error) {
	v, err := e.evalNode(n, sc)
	if err != nil {
		return nil, err
	}
	if _, isList := v.([]val); isList {
		if err := e.checkSize(v, n.position()); err != nil {
			return nil, err
		}
	}
	return v, nil
}

func (e *evaluator) evalNode(n node, sc *scope) (val, *Error) {
	if err := e.charge(1, n.position()); err != nil {
		return nil, err
	}
	switch n := n.(type) {
	case *numLit:
		if n.imag {
			return cnum(complex(0, n.v.float())), nil
		}
		return n.v, nil
	case *strLit:
		return n.s, nil
	case *boolLit:
		return n.v, nil
	case *identN:
		return e.lookup(n.name, n.p, sc)
	case *unaryN:
		return e.evalUnary(n, sc)
	case *binaryN:
		return e.evalBinary(n, sc)
	case *factN:
		x, err := e.eval(n.x, sc)
		if err != nil {
			return nil, err
		}
		return e.callBuiltin(e.funcs["factorial"], []val{x}, n.p)
	case *callN:
		return e.evalCall(n, sc)
	case *indexN:
		return e.evalIndex(n, sc)
	case *transN:
		x, err := e.eval(n.x, sc)
		if err != nil {
			return nil, err
		}
		return e.callBuiltin(e.funcs["transpose"], []val{x}, n.p)
	case *memberN:
		x, err := e.eval(n.x, sc)
		if err != nil {
			return nil, err
		}
		return e.member(x, n.name, n.p)
	case *allN:
		return nil, errf(codeSyntax, n.p, "a lone `:` means every row or column, and only inside brackets").
			withHint("A[i, :] is row i, A[:, j] is column j")
	case *listN:
		items := make([]val, 0, len(n.items))
		for _, it := range n.items {
			v, err := e.eval(it, sc)
			if err != nil {
				return nil, err
			}
			items = append(items, v)
		}
		return items, nil
	case *rangeN:
		return e.evalRange(n, sc)
	case *lambdaN:
		return &fnVal{params: n.params, body: n.body, env: sc}, nil
	case *assignN:
		v, err := e.eval(n.x, sc)
		if err != nil {
			return nil, err
		}
		if f, ok := v.(*fnVal); ok && f.builtin == nil && f.name == "" {
			// Name the lambda so errors and the rendered result say f(x).
			named := *f
			named.name = n.name
			v = &named
		}
		e.assign(n.name, v)
		return v, nil
	case *defineN:
		f := &fnVal{name: n.name, params: n.params, body: n.body, env: e.global}
		e.assign(n.name, f)
		return f, nil
	}
	return nil, errf(codeSyntax, n.position(), "unsupported expression")
}

func (e *evaluator) assign(name string, v val) {
	if !contains(e.assigned, name) {
		e.assigned = append(e.assigned, name)
	}
	e.global.vars[name] = v
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func (e *evaluator) lookup(name string, pos int, sc *scope) (val, *Error) {
	if v, ok := sc.lookup(name); ok {
		return v, nil
	}
	if c, ok := constants[name]; ok {
		return floatNum(c), nil
	}
	if b, ok := e.funcs[name]; ok {
		return &fnVal{name: name, builtin: b}, nil
	}
	err := errf(codeUnknownName, pos, "unknown name %s", name)
	if s := suggest(name, e.knownNames(sc)); len(s) > 0 {
		err.Hint = "did you mean " + strings.Join(s, ", ") + "?"
	} else {
		err.Hint = "assign it first (`" + name + " = ...`) or pass it in vars"
	}
	return nil, err
}

func (e *evaluator) knownNames(sc *scope) []string {
	var names []string
	for s := sc; s != nil; s = s.parent {
		for k := range s.vars {
			names = append(names, k)
		}
	}
	for k := range constants {
		names = append(names, k)
	}
	for k := range e.funcs {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func (e *evaluator) evalUnary(n *unaryN, sc *scope) (val, *Error) {
	x, err := e.eval(n.x, sc)
	if err != nil {
		return nil, err
	}
	if n.op == "not" {
		b, ok := x.(bool)
		if !ok {
			return nil, errf(codeType, n.p, "not needs a boolean, got a %s", typeName(x))
		}
		return !b, nil
	}
	if n.op == "+" {
		switch x.(type) {
		case num, cnum:
			return x, nil
		}
		if _, ok := x.([]val); ok {
			return x, nil
		}
		return nil, errf(codeType, n.p, "unary + needs a number, got a %s", typeName(x))
	}
	return e.arith("-", exactInt64(0), x, n.p)
}

func (e *evaluator) evalBinary(n *binaryN, sc *scope) (val, *Error) {
	l, err := e.eval(n.l, sc)
	if err != nil {
		return nil, err
	}
	if n.op == "and" || n.op == "or" {
		lb, ok := l.(bool)
		if !ok {
			return nil, errf(codeType, n.p, "%s needs booleans, got a %s on the left", n.op, typeName(l)).
				withHint("compare first, e.g. `x > 0 and y > 0`")
		}
		// Short-circuit: the right side is not evaluated when the left
		// decides, so `x != 0 and 1/x > 2` is safe.
		if n.op == "and" && !lb || n.op == "or" && lb {
			return lb, nil
		}
		r, err := e.eval(n.r, sc)
		if err != nil {
			return nil, err
		}
		rb, ok := r.(bool)
		if !ok {
			return nil, errf(codeType, n.p, "%s needs booleans, got a %s on the right", n.op, typeName(r))
		}
		return rb, nil
	}
	r, err := e.eval(n.r, sc)
	if err != nil {
		return nil, err
	}
	if cmpOps[n.op] {
		return e.compare(n.op, l, r, n.p)
	}
	return e.arith(n.op, l, r, n.p)
}

func (e *evaluator) evalCall(n *callN, sc *scope) (val, *Error) {
	var f *fnVal
	if id, ok := n.fn.(*identN); ok {
		if id.name == "if" {
			return e.evalIf(n, sc)
		}
		// Variables and functions are separate namespaces: a call takes
		// the user's function of that name, else the built-in, so a
		// variable `sum = 10` leaves sum([1, 2]) working.
		v, found := sc.lookup(id.name)
		fv, isFn := v.(*fnVal)
		switch {
		case found && isFn:
			f = fv
		case e.funcs[id.name] != nil:
			f = &fnVal{name: id.name, builtin: e.funcs[id.name]}
		case found:
			return nil, errf(codeType, n.p, "%s is a %s, not a function", id.name, typeName(v)).
				withHint("no implicit multiplication: write `" + id.name + "*(...)`")
		default:
			return nil, e.unknownFunc(id.name, id.p, sc)
		}
	} else {
		v, err := e.eval(n.fn, sc)
		if err != nil {
			return nil, err
		}
		fv, ok := v.(*fnVal)
		if !ok {
			return nil, errf(codeType, n.p, "a %s cannot be called", typeName(v)).
				withHint("no implicit multiplication: write `*` before `(`")
		}
		f = fv
	}
	argv := make([]val, 0, len(n.args))
	for _, a := range n.args {
		v, err := e.eval(a, sc)
		if err != nil {
			return nil, err
		}
		argv = append(argv, v)
	}
	return e.call(f, argv, n.p)
}

func (e *evaluator) unknownFunc(name string, pos int, sc *scope) *Error {
	err := errf(codeUnknownFunc, pos, "unknown function %s", name)
	if a, ok := aliases[name]; ok {
		return err.withHint("use " + a)
	}
	var names []string
	for k := range e.funcs {
		names = append(names, k)
	}
	for s := sc; s != nil; s = s.parent {
		for k, v := range s.vars {
			if _, ok := v.(*fnVal); ok {
				names = append(names, k)
			}
		}
	}
	if s := suggest(name, names); len(s) > 0 {
		err.Hint = "did you mean " + strings.Join(s, ", ") + "?"
	} else {
		err.Hint = "define it first, e.g. `" + name + "(x) = ...`"
	}
	return err
}

func (e *evaluator) evalIf(n *callN, sc *scope) (val, *Error) {
	if len(n.args) != 3 {
		return nil, errf(codeArity, n.p, "if takes 3 arguments, got %d", len(n.args)).withHint("if(cond, a, b)")
	}
	c, err := e.eval(n.args[0], sc)
	if err != nil {
		return nil, err
	}
	b, ok := c.(bool)
	if !ok {
		return nil, errf(codeType, n.p, "the condition of if must be a boolean, got a %s", typeName(c)).
			withHint("compare, e.g. `if(x > 0, x, -x)`")
	}
	if b {
		return e.eval(n.args[1], sc)
	}
	return e.eval(n.args[2], sc)
}

// call applies f to already-evaluated arguments.
func (e *evaluator) call(f *fnVal, argv []val, pos int) (val, *Error) {
	if err := e.charge(1, pos); err != nil {
		return nil, err
	}
	if f.builtin != nil {
		return e.callBuiltin(f.builtin, argv, pos)
	}
	if len(argv) != len(f.params) {
		return nil, errf(codeArity, pos, "%s takes %d argument(s), got %d", fnLabel(f), len(f.params), len(argv)).
			withHint(fnSignature(f))
	}
	for _, c := range e.calls {
		if c == f {
			return nil, recursionErr(fnLabel(f), pos)
		}
	}
	if len(e.calls) >= MaxCallDepth {
		return nil, errf(codeTooDeep, pos, "functions call each other more than %d deep", MaxCallDepth)
	}
	local := &scope{vars: make(map[string]val, len(argv)), parent: f.env}
	for i, p := range f.params {
		local.vars[p] = argv[i]
	}
	e.calls = append(e.calls, f)
	v, err := e.eval(f.body, local)
	e.calls = e.calls[:len(e.calls)-1]
	return v, err
}

func fnLabel(f *fnVal) string {
	if f.name != "" {
		return f.name
	}
	return "the lambda"
}

func fnSignature(f *fnVal) string {
	if f.builtin != nil {
		return f.builtin.sig
	}
	name := f.name
	if name == "" {
		return "(" + strings.Join(f.params, ", ") + ") -> ..."
	}
	return name + "(" + strings.Join(f.params, ", ") + ")"
}

func (e *evaluator) callBuiltin(b *builtin, argv []val, pos int) (val, *Error) {
	if len(argv) < b.min || b.max >= 0 && len(argv) > b.max {
		return nil, errf(codeArity, pos, "%s takes %s, got %d", b.name, arityText(b), len(argv)).withHint(b.sig)
	}
	if b.name == "if" {
		// Reached only when `if` is passed as a value; its arguments are
		// already evaluated, so it is eager here.
		c, ok := argv[0].(bool)
		if !ok {
			return nil, errf(codeType, pos, "the condition of if must be a boolean, got a %s", typeName(argv[0]))
		}
		if c {
			return argv[1], nil
		}
		return argv[2], nil
	}
	if skipsEmpty[b.name] && !(b.name == "count" && len(argv) > 1) {
		argv = dropEmpty(argv, b.max < 0)
	}
	if b.vec && len(argv) > 0 {
		if isEmpty(argv[0]) {
			return empty, nil
		}
		if l, ok := argv[0].([]val); ok {
			out := make([]val, 0, len(l))
			for _, x := range l {
				if err := e.charge(1, pos); err != nil {
					return nil, err
				}
				if isEmpty(x) {
					out = append(out, empty)
					continue
				}
				sub := append([]val{x}, argv[1:]...)
				v, err := e.callBuiltin(b, sub, pos)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			return out, nil
		}
	}
	v, handled, err := e.complexCall(b, argv, pos)
	if !handled {
		v, err = b.fn(e, args{v: argv, pos: pos, name: b.name})
	}
	if err != nil {
		complexHint(err)
	}
	return v, err
}

func arityText(b *builtin) string {
	switch {
	case b.max < 0:
		return pluralArgs(b.min, "at least ")
	case b.min == b.max:
		return pluralArgs(b.min, "")
	default:
		return pluralArgs(b.min, "") + " to " + pluralArgs(b.max, "")
	}
}

func pluralArgs(n int, prefix string) string {
	s := prefix + strconv.Itoa(n) + " argument"
	if n != 1 {
		s += "s"
	}
	return s
}

func (e *evaluator) evalIndex(n *indexN, sc *scope) (val, *Error) {
	x, err := e.eval(n.x, sc)
	if err != nil {
		return nil, err
	}
	rows, rowAll, err := e.evalSelector(n.idx, sc)
	if err != nil {
		return nil, err
	}
	switch x.(type) {
	case *tableVal, rowVal:
		if rowAll || n.col != nil {
			return nil, errf(codeIndex, n.p, "a %s takes one index", typeName(x)).
				withHint("t[i] is row i, t[\"name\"] a column, t[i].name one cell")
		}
		return e.indexTable(x, rows, n.p)
	}
	l, ok := x.([]val)
	if !ok {
		return nil, errf(codeType, n.p, "only a list can be indexed, got a %s", typeName(x))
	}
	picked, many, err := e.pickSel(l, rows, rowAll, n.p)
	if err != nil || n.col == nil {
		return picked, err
	}
	cols, colAll, err := e.evalSelector(n.col, sc)
	if err != nil {
		return nil, err
	}
	if !many {
		return e.pickRow(picked, cols, colAll, n.p)
	}
	in := picked.([]val)
	out := make([]val, len(in))
	for i, r := range in {
		v, err := e.pickRow(r, cols, colAll, n.p)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// evalSelector evaluates one index; all reports a lone `:`.
func (e *evaluator) evalSelector(n node, sc *scope) (v val, all bool, err *Error) {
	if _, ok := n.(*allN); ok {
		return nil, true, nil
	}
	v, err = e.eval(n, sc)
	return v, false, err
}

// pickRow applies the column index of A[i, j] to one row.
func (e *evaluator) pickRow(row val, cols val, all bool, pos int) (val, *Error) {
	l, ok := row.([]val)
	if !ok {
		return nil, errf(codeType, pos, "A[i, j] needs a list of lists (a matrix); a row here is a %s", typeName(row)).
			withHint("index a plain list with one index, v[i]")
	}
	v, _, err := e.pickSel(l, cols, all, pos)
	return v, err
}

// pickSel applies one index to l: a number picks an element, a list of
// numbers picks several, and `:` takes them all. many reports a list of
// picks rather than one element.
func (e *evaluator) pickSel(l []val, idx val, all bool, pos int) (v val, many bool, err *Error) {
	if all {
		if err := e.charge(len(l), pos); err != nil {
			return nil, false, err
		}
		return l, true, nil
	}
	il, ok := idx.([]val)
	if !ok {
		v, err := pick(l, idx, pos)
		return v, false, err
	}
	if err := e.charge(len(il), pos); err != nil {
		return nil, false, err
	}
	out := make([]val, 0, len(il))
	for _, iv := range il {
		v, err := pick(l, iv, pos)
		if err != nil {
			return nil, false, err
		}
		out = append(out, v)
	}
	return out, true, nil
}

func pick(l []val, idx val, pos int) (val, *Error) {
	in, ok := idx.(num)
	if !ok {
		return nil, errf(codeType, pos, "an index must be a number, got a %s", typeName(idx))
	}
	i, ok := smallInt(in, int64(MaxListLen)+1)
	if !ok {
		return nil, errf(codeIndex, pos, "index %s is not a whole number in range", fmtArg(in))
	}
	switch {
	case i == 0:
		return nil, errf(codeIndex, pos, "index 0 does not exist").withHint("indexing starts at 1: v[1] is the first element")
	case i < 0 || int(i) > len(l):
		return nil, errf(codeIndex, pos, "index %d is out of range for a list of %d", i, len(l)).
			withHint("valid indexes are 1 to len(v)")
	}
	return l[i-1], nil
}

// skipsEmpty names the aggregates, which skip the empty cells of a table
// column (and empty arguments): sum(t.price) adds the prices there are.
// Functions that pair or order elements (wmean, covariance, rank, cumsum)
// refuse an empty cell instead, since dropping one would misalign the rest.
var skipsEmpty = map[string]bool{
	"sum": true, "product": true, "mean": true, "median": true, "mode": true,
	"spread": true, "geomean": true, "harmmean": true, "variance": true,
	"stdev": true, "quantile": true, "percentile": true, "min": true,
	"max": true, "count": true,
}

// dropEmpty removes the empty cells of a list first argument and, for a
// variadic aggregate, empty arguments; arguments left with no data become
// one empty list, so mean(empty) says "of an empty list".
func dropEmpty(argv []val, variadic bool) []val {
	if len(argv) == 0 {
		return argv
	}
	out := argv
	if l, ok := argv[0].([]val); ok && hasEmpty(l) {
		kept := make([]val, 0, len(l))
		for _, x := range l {
			if !isEmpty(x) {
				kept = append(kept, x)
			}
		}
		out = append([]val{kept}, argv[1:]...)
	}
	if variadic && hasEmpty(out) {
		kept := make([]val, 0, len(out))
		for _, x := range out {
			if !isEmpty(x) {
				kept = append(kept, x)
			}
		}
		if len(kept) == 0 {
			kept = []val{[]val{}}
		}
		out = kept
	}
	return out
}

func hasEmpty(l []val) bool {
	for _, x := range l {
		if isEmpty(x) {
			return true
		}
	}
	return false
}
