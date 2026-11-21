package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/denn-gubsky/loomcycle/internal/calc"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Calc evaluates an expression with internal/calc: exact rational arithmetic
// by default, a float only where a function cannot be exact. It is pure — no
// state, no I/O, no network — so it needs no grant beyond being in the
// agent's tools, and every refusal is about the expression, never the caller.
type Calc struct {
	stats calcStats
}

func (c *Calc) Name() string { return "Calc" }

// The description and the `expr` field together stay under 120 words
// (TestCalcDescription_StaysUnderTheWordBudget): they reach the model on every
// request. Name only what the evaluator implements; later functions add their
// words as they land.
func (c *Calc) Description() string {
	return strings.TrimSpace(`
Evaluate math exactly: arithmetic, rounding, powers and roots, logarithms, trigonometry, combinatorics, primes, lists and unit conversion. Use it for every calculation instead of working it out yourself. ` + "`expr`" + ` is one or more statements; the last one is the result. Pass data in ` + "`vars`" + `. Power is ` + "`^`" + `, ` + "`%`" + ` is modulo, indexing starts at 1, ` + "`.*`" + ` multiplies lists element by element. For the functions, read ` + "`Context op=help topic=Calc`" + ` first.`)
}

const calcExprDescription = "Statements separated by `;` or newlines; the last one's value is the result. Example: `subtotal = 1249.99 * 3; tax = round(subtotal * 0.2, 2); subtotal + tax`."

func (c *Calc) InputSchema() json.RawMessage {
	s, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"expr": map[string]any{"type": "string", "description": calcExprDescription},
			"vars": map[string]any{"type": "object",
				"description": "Names bound before evaluation: a number, a numeric string (exact), a string, a boolean or a list."},
			"digits": map[string]any{"type": "integer", "minimum": 1, "maximum": calc.MaxDigits,
				"description": "Significant digits for an approximate or non-terminating result (default 15)."},
			"seed": map[string]any{"type": "integer",
				"description": "Makes the random functions repeatable."},
		},
		"required":             []string{"expr"},
		"additionalProperties": false,
	})
	return s
}

type calcInput struct {
	Expr   *string                    `json:"expr"`
	Vars   map[string]json.RawMessage `json:"vars"`
	Digits int                        `json:"digits"`
	Seed   *int64                     `json:"seed"`
}

func (c *Calc) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var in calcInput
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		c.stats.count("bad_request")
		return calcRefusal(&calc.Error{Code: "bad_request", Detail: "the arguments are not the documented shape: " + err.Error(), Pos: -1,
			Hint: "pass `expr` (a string), and optionally `vars` (an object), `digits` and `seed` (integers)"}), nil
	}
	if in.Expr == nil {
		c.stats.count("bad_request")
		return calcRefusal(&calc.Error{Code: "bad_request", Detail: "expr is required", Pos: -1,
			Hint: "pass the expression as `expr`, e.g. {\"expr\": \"2 + 2\"}"}), nil
	}
	resp, cerr := calc.Eval(ctx, calc.Request{Expr: *in.Expr, Vars: in.Vars, Digits: in.Digits, Seed: in.Seed})
	if cerr != nil {
		c.stats.count(cerr.Code)
		// A canceled evaluation is the run ending, not a refusal of the
		// expression: hand the loop the ctx error, as other tools do.
		if cerr.Code == "canceled" && ctx.Err() != nil {
			return tools.Result{}, ctx.Err()
		}
		return calcRefusal(cerr), nil
	}
	body, err := json.Marshal(resp)
	if err != nil {
		// Unreachable: Response holds only JSON-safe values.
		return errBusiness("Calc: the result could not be encoded: "+err.Error(), "Simplify the expression."), nil
	}
	return tools.Result{Text: string(body)}, nil
}

// calcCategory decides, for each code calc.ErrorCodes() can return, whether
// the fix is in the call (validation: rewrite the expression or the vars) or
// the call is well formed and refused by the mathematics or a limit (business:
// resending it fails the same way). TestCalcCategory_MapsEveryErrorCode fails
// on a code this table does not decide.
var calcCategory = map[string]tools.ErrorCategory{
	"syntax":           tools.CategoryValidation,
	"arity":            tools.CategoryValidation,
	"type":             tools.CategoryValidation,
	"index":            tools.CategoryValidation,
	"unknown_function": tools.CategoryValidation,
	"unknown_name":     tools.CategoryValidation,
	"bad_var":          tools.CategoryValidation,
	"bad_request":      tools.CategoryValidation,
	"reserved_name":    tools.CategoryValidation,
	"recursion":        tools.CategoryValidation,
	"shape":            tools.CategoryValidation,
	"expr_too_long":    tools.CategoryValidation,
	"vars_too_large":   tools.CategoryValidation,
	"unknown_unit":     tools.CategoryValidation,
	"dimension":        tools.CategoryValidation,
	"domain":           tools.CategoryBusiness,
	"division_by_zero": tools.CategoryBusiness,
	"overflow":         tools.CategoryBusiness,
	"too_large":        tools.CategoryBusiness,
	"too_deep":         tools.CategoryBusiness,
	"list_too_long":    tools.CategoryBusiness,
	"step_limit":       tools.CategoryBusiness,
	"time_limit":       tools.CategoryBusiness,
	"empty":            tools.CategoryBusiness,
	// Reached only when the evaluation was canceled but the caller's ctx was
	// not (Execute hands the loop ctx.Err() otherwise): resending may work.
	"canceled": tools.CategoryTransient,
}

// calcRefusal renders a refusal as JSON the model reads field by field: the
// code to look up in Calc/errors, what went wrong, and what to write instead.
func calcRefusal(e *calc.Error) tools.Result {
	out := map[string]any{"error": e.Code, "detail": e.Detail}
	if e.Hint != "" {
		out["hint"] = e.Hint
	}
	if e.Pos >= 0 {
		out["pos"] = e.Pos
	}
	body, _ := json.Marshal(out)
	next := e.Hint
	if next == "" {
		next = "Read Context op=help topic=Calc/errors for what the code means and what to change."
	}
	switch calcCategory[e.Code] {
	case tools.CategoryValidation:
		return errValidation(string(body), next)
	case tools.CategoryTransient:
		return errTransient(string(body), next)
	default:
		return errBusiness(string(body), next)
	}
}

// calcStats counts refusals by error code for /metrics. The label set is
// bounded by calc.ErrorCodes(): the tool never puts model text in a label.
type calcStats struct {
	mu sync.Mutex
	m  map[string]*atomic.Uint64
}

func (s *calcStats) count(code string) {
	s.mu.Lock()
	if s.m == nil {
		s.m = map[string]*atomic.Uint64{}
	}
	ctr := s.m[code]
	if ctr == nil {
		ctr = &atomic.Uint64{}
		s.m[code] = ctr
	}
	s.mu.Unlock()
	ctr.Add(1)
}

// RefusalStats returns a snapshot of refusal counts keyed by error code.
func (c *Calc) RefusalStats() map[string]uint64 {
	c.stats.mu.Lock()
	defer c.stats.mu.Unlock()
	out := make(map[string]uint64, len(c.stats.m))
	for k, v := range c.stats.m {
		out[k] = v.Load()
	}
	return out
}

var _ tools.Tool = (*Calc)(nil)
