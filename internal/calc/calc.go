// Package calc evaluates arithmetic expressions written by a model: exact
// rational arithmetic by default, float64 where a function cannot be exact,
// lists with element-wise operators, user functions and lambdas, and a
// closed table of built-in functions (see Functions).
//
// It is deliberately not a programming language: there are no loops, no
// recursion, no I/O and no way to reach the host. Every evaluation is
// bounded by the constants in limits.go, which are checked before the work
// they bound wherever the size is known in advance.
package calc

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
)

// Request is one evaluation.
type Request struct {
	// Expr is one or more statements separated by newlines or `;`; the
	// last statement's value is the result.
	Expr string `json:"expr"`
	// Vars binds names before evaluation. A JSON number is exact (0.1 is
	// 1/10); a string that is a numeric literal ("0.1", "1_000") is a
	// number too, any other string stays a string; arrays become lists.
	Vars map[string]json.RawMessage `json:"vars,omitempty"`
	// Digits is the significant digits for a float or a non-terminating
	// exact result; 0 means DefaultDigits.
	Digits int `json:"digits,omitempty"`
	// Seed makes random functions reproducible.
	Seed *int64 `json:"seed,omitempty"`
}

// Response is a successful evaluation.
type Response struct {
	// Result is the value as JSON: numbers are json.Number carrying Text's
	// digits (an exact integer is never rounded through a float64), lists
	// are arrays, a function is its signature. Nil when Truncated cut the
	// result's text.
	Result any `json:"result"`
	// Type is number, string, boolean, list or function.
	Type string `json:"type"`
	// Exact is true when every number in the result is exact.
	Exact bool `json:"exact"`
	// Fraction is the exact value "n/d" of a non-terminating exact number,
	// whose Text is rounded.
	Fraction string `json:"fraction,omitempty"`
	// Assigned holds each name the expression assigned or defined, in the
	// same JSON form as Result.
	Assigned map[string]any `json:"assigned,omitempty"`
	// Text is the result rendered for reading.
	Text string `json:"text"`
	// Shape is the dimensions of a list result: [n], or [n, m] for n lists
	// of m.
	Shape []int `json:"shape,omitempty"`
	// Truncated is set when the rendered output passed MaxOutputBytes: Text
	// is cut there and the values that did not fit are null.
	Truncated bool `json:"truncated,omitempty"`
}

// Eval evaluates req. It returns within MaxWall, sooner if ctx ends.
func Eval(ctx context.Context, req Request) (Response, *Error) {
	return evalWithin(ctx, req, MaxWall)
}

// evalWithin is Eval with the wall bound as a parameter, so a test can
// reach the step limit under the race detector (several times slower)
// without the wall clock deciding first.
func evalWithin(ctx context.Context, req Request, wall time.Duration) (Response, *Error) {
	if len(req.Expr) > MaxExprBytes {
		return Response{}, errf(codeExprTooLong, -1, "the expression is %d bytes; the limit is %d", len(req.Expr), MaxExprBytes).
			withHint("pass large data in vars, or split the work into several calls")
	}
	size := 0
	for k, v := range req.Vars {
		size += len(k) + len(v)
	}
	if size > MaxVarsBytes {
		return Response{}, errf(codeVarsTooLarge, -1, "vars total %d bytes; the limit is %d", size, MaxVarsBytes)
	}
	digits := req.Digits
	if digits == 0 {
		digits = DefaultDigits
	}
	if digits < 1 || digits > MaxDigits {
		return Response{}, errf(codeBadRequest, -1, "digits must be between 1 and %d, got %d", MaxDigits, req.Digits)
	}
	ctx, cancel := context.WithTimeout(ctx, wall)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Response{}, ctxErr(err, -1)
	}
	stmts, perr := parse(req.Expr)
	if perr != nil {
		return Response{}, perr
	}
	e := &evaluator{
		ctx:    ctx,
		funcs:  builtinIndex,
		global: &scope{vars: map[string]val{}},
		rng:    newRNG(req.Seed),
	}
	if err := decodeVars(req.Vars, e.global.vars); err != nil {
		return Response{}, err
	}
	var last val
	for _, s := range stmts {
		v, err := e.eval(s, e.global)
		if err != nil {
			return Response{}, err
		}
		last = v
	}
	return e.respond(last, digits), nil
}

func newRNG(seed *int64) *rand.Rand {
	var s1, s2 uint64
	if seed != nil {
		s1, s2 = uint64(*seed), 0x9e3779b97f4a7c15
	} else {
		var b [16]byte
		_, _ = crand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
		s1, s2 = binary.LittleEndian.Uint64(b[:8]), binary.LittleEndian.Uint64(b[8:])
	}
	return rand.New(rand.NewPCG(s1, s2))
}

// respond renders the result and the assigned names under one shared
// MaxOutputBytes budget.
func (e *evaluator) respond(last val, digits int) Response {
	w := &capWriter{limit: MaxOutputBytes}
	renderText(w, last, digits, true)
	resp := Response{
		Type:      typeName(last),
		Exact:     allExact(last),
		Text:      w.b.String(),
		Shape:     shape(last),
		Truncated: w.over,
	}
	if !w.over {
		resp.Result = toJSON(last, digits)
	}
	if n, ok := last.(num); ok && n.exact() {
		if _, frac := formatNum(n, digits); frac != "" {
			resp.Fraction = frac
		}
	}
	room := MaxOutputBytes - len(resp.Text)
	if len(e.assigned) > 0 {
		resp.Assigned = make(map[string]any, len(e.assigned))
	}
	for _, name := range e.assigned {
		v := e.global.vars[name]
		aw := &capWriter{limit: max(room, 0)}
		renderText(aw, v, digits, true)
		room -= aw.b.Len()
		if aw.over {
			resp.Assigned[name] = nil
			resp.Truncated = true
			continue
		}
		resp.Assigned[name] = toJSON(v, digits)
	}
	return resp
}

// decodeVars binds the request vars into dst, in name order so an error
// names the same var on every call.
func decodeVars(vars map[string]json.RawMessage, dst map[string]val) *Error {
	names := make([]string, 0, len(vars))
	for k := range vars {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		if !isIdentifier(name) {
			return errf(codeBadVar, -1, "var name %q is not a valid name", name).withHint("names are letters, digits and _, starting with a letter or _")
		}
		if err := checkAssignable(name, -1); err != nil {
			return err
		}
		dec := json.NewDecoder(bytes.NewReader(vars[name]))
		dec.UseNumber()
		var raw any
		if err := dec.Decode(&raw); err != nil {
			return errf(codeBadVar, -1, "var %s is not valid JSON: %v", name, err)
		}
		if dec.More() {
			return errf(codeBadVar, -1, "var %s holds more than one JSON value", name)
		}
		v, err := fromJSON(name, raw, 0)
		if err != nil {
			return err
		}
		dst[name] = v
	}
	return nil
}

func isIdentifier(s string) bool {
	if s == "" || !isLetter(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isLetter(s[i]) && !isDigit(s[i]) {
			return false
		}
	}
	return true
}

func fromJSON(name string, x any, depth int) (val, *Error) {
	if depth > maxVarDepth {
		return nil, errf(codeBadVar, -1, "var %s nests deeper than %d levels", name, maxVarDepth)
	}
	switch x := x.(type) {
	case json.Number:
		n, err := parseNumberLiteral(string(x), false)
		if err != nil {
			err.Detail = "var " + name + ": " + err.Detail
			return nil, err
		}
		return n, nil
	case string:
		if n, ok := numericString(x); ok {
			return n, nil
		}
		return x, nil
	case bool:
		return x, nil
	case []any:
		if len(x) > MaxListLen {
			return nil, errf(codeListTooLong, -1, "var %s has %d elements; the limit is %d", name, len(x), MaxListLen)
		}
		out := make([]val, len(x))
		for i, it := range x {
			v, err := fromJSON(name, it, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case nil:
		return nil, errf(codeBadVar, -1, "var %s is null", name).withHint("pass a number, string, boolean or list")
	}
	return nil, errf(codeBadVar, -1, "var %s is a JSON object, which has no meaning here", name).
		withHint("pass a number, string, boolean or list; send each field as its own var")
}

// numericString reads a var string as a number when the whole string is a
// numeric literal, optionally signed: "0.10", "-1e3", "1_000", "0xff".
func numericString(s string) (num, bool) {
	t := strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(t, "-") || strings.HasPrefix(t, "+") {
		neg = t[0] == '-'
		t = t[1:]
	}
	if t == "" || !(isDigit(t[0]) || t[0] == '.' && len(t) > 1 && isDigit(t[1])) {
		return num{}, false
	}
	tok, err := lexNumber(t, 0)
	if err != nil || len(tok.s) != len(t) {
		return num{}, false
	}
	n := tok.num
	if neg {
		n = exactNum(new(big.Rat).Neg(n.r))
	}
	return n, true
}
