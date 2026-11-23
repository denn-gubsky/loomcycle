package calc

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The higher-order functions accept the list and the function in either
// order, map(v, f) or map(f, v): models write both, and the types say
// which is which.
var sequenceFuncs = []builtin{
	{name: "range", sig: "range(a, b, step?)", section: SecSequences, min: 2, max: 3, fn: fnRange,
		summary: "the list a, a+step, ... up to b inclusive; same as a:b or a:step:b"},
	{name: "len", sig: "len(v)", section: SecSequences, min: 1, max: 1, fn: fnLen,
		summary: "number of elements of a list, or characters of a string"},
	{name: "map", sig: "map(v, f)", section: SecSequences, min: 2, max: 2, fn: fnMap,
		summary: "the list of f(x) for each element x"},
	{name: "filter", sig: "filter(v, pred)", section: SecSequences, min: 2, max: 2, fn: fnFilter,
		summary: "the elements x of v where pred(x) is true"},
	{name: "reduce", sig: "reduce(v, f, init?)", section: SecSequences, min: 2, max: 3, fn: fnReduce,
		summary: "fold v with f(acc, x), starting from init or the first element"},
	{name: "sort", sig: "sort(v, key?)", section: SecSequences, min: 1, max: 2, fn: fnSort,
		summary: "v ascending (numbers or strings), optionally by key(x); stable"},
	{name: "reverse", sig: "reverse(v)", section: SecSequences, min: 1, max: 1, fn: fnReverse,
		summary: "v in reverse order"},
	{name: "unique", sig: "unique(v)", section: SecSequences, min: 1, max: 1, fn: fnUnique,
		summary: "v without repeats, first occurrences kept in order"},
	{name: "count", sig: "count(v, pred_or_value?)", section: SecSequences, min: 1, max: 2, fn: fnCount,
		summary: "number of elements, of elements where pred(x) holds, or equal to a value"},
	{name: "any", sig: "any(v, pred?)", section: SecSequences, min: 1, max: 2, fn: anyAll(true),
		summary: "whether some element is true (or satisfies pred)"},
	{name: "all", sig: "all(v, pred?)", section: SecSequences, min: 1, max: 2, fn: anyAll(false),
		summary: "whether every element is true (or satisfies pred)"},
	{name: "zip", sig: "zip(a, b, ...)", section: SecSequences, min: 1, max: -1, fn: fnZip,
		summary: "the list of [a[i], b[i], ...], as long as the shortest list"},
	{name: "sum", sig: "sum(v) or sum(a, b, ...)", section: SecSequences, min: 1, max: -1, fn: fold("+", 0),
		summary: "sum of a list or of the arguments; exact on exact input"},
	{name: "product", sig: "product(v) or product(a, b, ...)", section: SecSequences, min: 1, max: -1, fn: fold(".*", 1),
		summary: "product of a list or of the arguments; exact on exact input"},
}

func fnRange(e *evaluator, a args) (val, *Error) {
	start, err := a.num(0)
	if err != nil {
		return nil, err
	}
	end, err := a.num(1)
	if err != nil {
		return nil, err
	}
	step := exactInt64(1)
	if len(a.v) == 3 {
		if step, err = a.num(2); err != nil {
			return nil, err
		}
	}
	return e.makeRange(start, step, end, a.pos)
}

func fnLen(_ *evaluator, a args) (val, *Error) {
	switch v := a.v[0].(type) {
	case []val:
		return exactInt64(int64(len(v))), nil
	case string:
		return exactInt64(int64(utf8.RuneCountInString(v))), nil
	}
	return nil, errf(codeType, a.pos, "len needs a list or a string, got a %s", typeName(a.v[0]))
}

// listAndFn finds the list and the function among the first two
// arguments, in either order.
func listAndFn(a args) ([]val, *fnVal, *Error) {
	if f, ok := a.v[0].(*fnVal); ok {
		l, err := a.list(1)
		return l, f, err
	}
	l, err := a.list(0)
	if err != nil {
		return nil, nil, err
	}
	f, err := a.fn(1)
	return l, f, err
}

func fnMap(e *evaluator, a args) (val, *Error) {
	l, f, err := listAndFn(a)
	if err != nil {
		return nil, err
	}
	out := make([]val, len(l))
	for i, x := range l {
		v, err := e.call(f, []val{x}, a.pos)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (e *evaluator) predicate(f *fnVal, x val, a args) (bool, *Error) {
	v, err := e.call(f, []val{x}, a.pos)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, errf(codeType, a.pos, "%s: the predicate must return a boolean, got a %s", a.name, typeName(v)).
			withHint("compare inside it, e.g. `x -> x > 0`")
	}
	return b, nil
}

func fnFilter(e *evaluator, a args) (val, *Error) {
	l, f, err := listAndFn(a)
	if err != nil {
		return nil, err
	}
	out := []val{}
	for _, x := range l {
		keep, err := e.predicate(f, x, a)
		if err != nil {
			return nil, err
		}
		if keep {
			out = append(out, x)
		}
	}
	return out, nil
}

func fnReduce(e *evaluator, a args) (val, *Error) {
	l, f, err := listAndFn(a)
	if err != nil {
		return nil, err
	}
	var acc val
	switch {
	case len(a.v) == 3:
		acc = a.v[2]
	case len(l) == 0:
		return nil, errf(codeDomain, a.pos, "reduce of an empty list needs an initial value").withHint("reduce(v, f, init)")
	default:
		acc, l = l[0], l[1:]
	}
	for _, x := range l {
		v, err := e.call(f, []val{acc, x}, a.pos)
		if err != nil {
			return nil, err
		}
		acc = v
	}
	return acc, nil
}

// sortCost is the step charge for sorting n items: n log2 n comparisons.
func sortCost(n int) int {
	if n < 2 {
		return n
	}
	return n * int(math.Ceil(math.Log2(float64(n))))
}

func fnSort(e *evaluator, a args) (val, *Error) {
	l, err := a.list(0)
	if err != nil {
		return nil, err
	}
	keys := l
	if len(a.v) == 2 {
		f, err := a.fn(1)
		if err != nil {
			return nil, err
		}
		keys = make([]val, len(l))
		for i, x := range l {
			if keys[i], err = e.call(f, []val{x}, a.pos); err != nil {
				return nil, err
			}
		}
	}
	if err := e.charge(sortCost(len(l)), a.pos); err != nil {
		return nil, err
	}
	// Check the key types up front: the comparator cannot return an error.
	allNum, allStr, allDate, allTime := true, true, true, true
	for _, k := range keys {
		_, isNum := k.(num)
		_, isStr := k.(string)
		_, isDate := k.(dateVal)
		_, isTime := k.(timeVal)
		allNum = allNum && isNum
		allStr = allStr && isStr
		allDate = allDate && isDate
		allTime = allTime && isTime
	}
	for _, k := range keys {
		if isComplex(k) {
			return nil, errf(codeType, a.pos, "sort: complex numbers have no order").
				withHint("sort by a real key, e.g. sort(v, z -> abs(z)) or sort(v, z -> re(z))")
		}
	}
	if !allNum && !allStr && !allDate && !allTime {
		return nil, errf(codeType, a.pos, "sort needs all numbers, all strings, all dates or all datetimes").
			withHint("to sort lists or by a field, pass a key, e.g. sort(v, p -> p[2])")
	}
	idx := make([]int, len(l))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool {
		ki, kj := keys[idx[i]], keys[idx[j]]
		switch {
		case allNum:
			return cmpNum(ki.(num), kj.(num)) < 0
		case allDate:
			return ki.(dateVal).days < kj.(dateVal).days
		case allTime:
			return ki.(timeVal).t.Before(kj.(timeVal).t)
		}
		return ki.(string) < kj.(string)
	})
	out := make([]val, len(l))
	for i, k := range idx {
		out[i] = l[k]
	}
	return out, nil
}

func fnReverse(e *evaluator, a args) (val, *Error) {
	l, err := a.list(0)
	if err != nil {
		return nil, err
	}
	if err := e.charge(len(l), a.pos); err != nil {
		return nil, err
	}
	out := make([]val, len(l))
	for i, x := range l {
		out[len(l)-1-i] = x
	}
	return out, nil
}

// keyBuf builds one key at a time and counts every byte written across
// all the keys of one call, so the call can be refused at maxKeyBytes
// however few steps those bytes cost.
type keyBuf struct {
	b    strings.Builder
	used int
}

func (k *keyBuf) write(s string, a args) *Error {
	k.used += len(s)
	if k.used > maxKeyBytes {
		return errf(codeTooLarge, a.pos, "%s: comparing these values would build more than %d bytes of keys", a.name, maxKeyBytes).
			withHint("compare smaller values, or fewer of them")
	}
	k.b.WriteString(s)
	return nil
}

// ceilDiv is ceil(n/d) for n >= 0.
func ceilDiv(n, d int) int { return (n + d - 1) / d }

// valueKey writes a canonical text for equality under ==: a float and an
// exact number with the same value share a key, as they compare equal. A
// key serialises nested lists and every digit, so each value written costs
// one step plus one per bitStepCost bits of a number (at least one) or
// bytes of a quoted string. A function has no value to compare and is
// refused.
func (e *evaluator) valueKey(k *keyBuf, v val, a args) *Error {
	if err := e.charge(1, a.pos); err != nil {
		return err
	}
	switch v := v.(type) {
	case num:
		r := v.rat()
		if err := e.charge(max(1, ceilDiv(ratBits(r), bitStepCost)), a.pos); err != nil {
			return err
		}
		return k.write("n"+r.RatString(), a)
	case cnum:
		return k.write(complexKey(complex128(v)), a)
	case string:
		q := strconv.Quote(v)
		if err := e.charge(1+ceilDiv(len(q), bitStepCost), a.pos); err != nil {
			return err
		}
		return k.write("s"+q, a)
	case bool:
		return k.write("b"+strconv.FormatBool(v), a)
	case dateVal:
		return k.write("d"+strconv.FormatInt(v.days, 10), a)
	case timeVal:
		// The instant, not the zone: tz() changes how it reads, not what it is.
		return k.write("t"+strconv.FormatInt(v.t.Unix(), 10)+"."+strconv.Itoa(v.t.Nanosecond()), a)
	case []val:
		if err := k.write("[", a); err != nil {
			return err
		}
		for _, x := range v {
			if err := e.valueKey(k, x, a); err != nil {
				return err
			}
			if err := k.write(",", a); err != nil {
				return err
			}
		}
		return k.write("]", a)
	}
	return errf(codeType, a.pos, "%s: functions cannot be compared", a.name)
}

func fnUnique(e *evaluator, a args) (val, *Error) {
	l, err := a.list(0)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(l))
	out := []val{}
	var k keyBuf
	for _, x := range l {
		k.b.Reset()
		if err := e.valueKey(&k, x, a); err != nil {
			return nil, err
		}
		key := k.b.String()
		if !seen[key] {
			seen[key] = true
			out = append(out, x)
		}
	}
	return out, nil
}

func fnCount(e *evaluator, a args) (val, *Error) {
	l, err := a.list(0)
	if err != nil {
		return nil, err
	}
	if len(a.v) == 1 {
		return exactInt64(int64(len(l))), nil
	}
	n := int64(0)
	f, isFn := a.v[1].(*fnVal)
	for _, x := range l {
		var hit bool
		if isFn {
			if hit, err = e.predicate(f, x, a); err != nil {
				return nil, err
			}
		} else if hit, err = e.equal(x, a.v[1], a.pos); err != nil {
			return nil, err
		}
		if hit {
			n++
		}
	}
	return exactInt64(n), nil
}

func anyAll(isAny bool) func(*evaluator, args) (val, *Error) {
	return func(e *evaluator, a args) (val, *Error) {
		l, err := a.list(0)
		if err != nil {
			return nil, err
		}
		var f *fnVal
		if len(a.v) == 2 {
			if f, err = a.fn(1); err != nil {
				return nil, err
			}
		}
		for _, x := range l {
			if err := e.charge(1, a.pos); err != nil {
				return nil, err
			}
			var b bool
			if f != nil {
				if b, err = e.predicate(f, x, a); err != nil {
					return nil, err
				}
			} else {
				bv, ok := x.(bool)
				if !ok {
					return nil, errf(codeType, a.pos, "%s needs a list of booleans, got a %s element", a.name, typeName(x)).
						withHint("pass a predicate: " + a.name + "(v, x -> x > 0)")
				}
				b = bv
			}
			if b == isAny {
				return isAny, nil
			}
		}
		return !isAny, nil
	}
}

func fnZip(e *evaluator, a args) (val, *Error) {
	lists := make([][]val, len(a.v))
	n := math.MaxInt
	for i := range a.v {
		l, err := a.list(i)
		if err != nil {
			return nil, err
		}
		lists[i] = l
		n = min(n, len(l))
	}
	if err := e.charge(n*len(lists), a.pos); err != nil {
		return nil, err
	}
	out := make([]val, n)
	for i := range out {
		row := make([]val, len(lists))
		for j, l := range lists {
			row[j] = l[i]
		}
		out[i] = row
	}
	return out, nil
}

// fold sums or multiplies a list (or the arguments) with the evaluator's
// own operators, so exactness and list broadcasting behave as with + and .*.
func fold(op string, unit int64) func(*evaluator, args) (val, *Error) {
	return func(e *evaluator, a args) (val, *Error) {
		items := a.v
		if len(a.v) == 1 {
			l, err := a.list(0)
			if err != nil {
				return nil, err
			}
			items = l
		}
		var acc val = exactInt64(unit)
		for i, x := range items {
			if err := e.charge(1, a.pos); err != nil {
				return nil, err
			}
			if i == 0 {
				if _, ok := asComplex(x); !ok {
					if _, ok := x.([]val); !ok {
						return nil, errf(codeType, a.pos, "%s needs numbers, got a %s", a.name, typeName(x))
					}
				}
				acc = x
				continue
			}
			v, err := e.arith(op, acc, x, a.pos)
			if err != nil {
				return nil, err
			}
			acc = v
		}
		return acc, nil
	}
}
