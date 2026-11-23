package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/semaphore"

	"github.com/denn-gubsky/loomcycle/internal/calc"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Calc evaluates an expression with internal/calc: exact rational arithmetic
// by default, a float only where a function cannot be exact. Without `from`
// and `to` it is pure — no state, no I/O, no network — so it needs no grant
// beyond being in the agent's tools. With them it reads and writes the
// agent's own stores under the agent's own grants (calc_refs.go).
type Calc struct {
	// Gate bounds how many heavy matrix operations and data loads run at
	// once across every caller of this tool (calc.NewGate, built once at
	// boot); nil means no gating, as in tests.
	Gate *semaphore.Weighted
	// Memory and Docs are the stores a memory: or document: reference reads
	// and writes, through their own scope checks. Nil: such a reference is
	// refused. Read at call time, as the tools' stores are bound after
	// construction.
	Memory *Memory
	Docs   *Document
	stats  calcStats
}

func (c *Calc) Name() string { return "Calc" }

// The description and the `expr` field together stay under 120 words
// (TestCalcDescription_StaysUnderTheWordBudget): they reach the model on every
// request. Name only what the evaluator implements; later functions add their
// words as they land.
func (c *Calc) Description() string {
	return strings.TrimSpace(`
Evaluate math exactly: arithmetic, rounding, powers and roots, logarithms, trigonometry, combinatorics, primes, lists, unit conversion, statistics and probability, integrals and roots, matrices, complex numbers, finance, dates and tables. Use it for every calculation instead of working it out yourself. ` + "`expr`" + ` is one or more statements; the last one is the result. Pass data in ` + "`vars`" + `, ` + CalcDataByReference + `. Power is ` + "`^`" + `, ` + "`%`" + ` is modulo, indexing starts at 1, ` + "`.*`" + ` multiplies lists element by element. For the functions, read ` + "`Context op=help topic=Calc`" + ` first.`)
}

// CalcDataByReference is the description's sentence on `from` and `to`. A
// transport with no run, where they are refused, drops it from the
// description it advertises.
const CalcDataByReference = "or name stored tables in `from`; `to` writes the result"

const calcExprDescription = "Statements separated by `;` or newlines; the last one's value is the result. Example: `subtotal = 1249.99 * 3; tax = round(subtotal * 0.2, 2); subtotal + tax`."

func (c *Calc) InputSchema() json.RawMessage { return calcSchema(true) }

// CalcOffRunSchema is the input schema of Calc outside a run: the in-run one
// without `from` and `to`, which are refused there.
func CalcOffRunSchema() json.RawMessage { return calcSchema(false) }

func calcSchema(byReference bool) json.RawMessage {
	props := map[string]any{
		"expr": map[string]any{"type": "string", "description": calcExprDescription},
		"vars": map[string]any{"type": "object",
			"description": "Names bound before evaluation: a number, a numeric string (exact), a string, a boolean, a list, or a table ({\"columns\": [...], \"rows\": [[...]]} or an array of objects)."},
		"format": map[string]any{"type": "string", "enum": []string{calc.FormatJSON, calc.FormatMarkdown, calc.FormatCSV},
			"description": "How a table or matrix result is written in `text` (default json)."},
		"digits": map[string]any{"type": "integer", "minimum": 1, "maximum": calc.MaxDigits,
			"description": "Significant digits for an approximate or non-terminating result (default 15)."},
		"seed": map[string]any{"type": "integer",
			"description": "Makes the random functions repeatable."},
	}
	if byReference {
		props["from"] = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
			"description": "Names bound to stored data: volume:<path>, volume:<name>:<path>, memory:<scope>/<key> or document:<scope>/<chunk_id>; CSV, TSV, JSON or the first Markdown table. Needs Read, Memory or Document."}
		props["to"] = map[string]any{"type": "string",
			"description": "Write the result there instead of returning it (csv, json or markdown by format or extension). Needs Write, Memory or Document."}
	}
	s, _ := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           props,
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
	Format string                     `json:"format"`
	From   map[string]string          `json:"from"`
	To     *string                    `json:"to"`
}

func (c *Calc) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var in calcInput
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		c.stats.count("bad_request")
		return calcRefusal(&calc.Error{Code: "bad_request", Detail: "the arguments are not the documented shape: " + err.Error(), Pos: -1,
			Hint: "pass `expr` (a string), and optionally `vars` and `from` (objects), `to` (a reference), `digits` and `seed` (integers) and `format` (json, markdown or csv)"}), nil
	}
	if in.Expr == nil {
		c.stats.count("bad_request")
		return calcRefusal(&calc.Error{Code: "bad_request", Detail: "expr is required", Pos: -1,
			Hint: "pass the expression as `expr`, e.g. {\"expr\": \"2 + 2\"}"}), nil
	}
	var refs calcRefs
	if in.From != nil || in.To != nil {
		var res tools.Result
		var ok bool
		if refs, res, ok = c.prepareRefs(ctx, in); !ok {
			return res, nil
		}
	}
	resp, cerr := calc.Eval(ctx, calc.Request{Expr: *in.Expr, Vars: in.Vars, Digits: in.Digits, Seed: in.Seed, Format: in.Format, Gate: c.Gate,
		Data: refs.data, Write: refs.writeFormat})
	if cerr != nil {
		c.stats.count(cerr.Code)
		// A canceled evaluation is the run ending, not a refusal of the
		// expression: hand the loop the ctx error, as other tools do.
		if cerr.Code == "canceled" && ctx.Err() != nil {
			return tools.Result{}, ctx.Err()
		}
		return calcRefusal(cerr), nil
	}
	if refs.to != nil {
		return c.writeResult(ctx, refs, resp), nil
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
	"bad_date":         tools.CategoryValidation,
	"unknown_unit":     tools.CategoryValidation,
	"dimension":        tools.CategoryValidation,
	"column":           tools.CategoryValidation,
	"bad_data":         tools.CategoryValidation,
	"domain":           tools.CategoryBusiness,
	"division_by_zero": tools.CategoryBusiness,
	"overflow":         tools.CategoryBusiness,
	"too_large":        tools.CategoryBusiness,
	"too_deep":         tools.CategoryBusiness,
	"list_too_long":    tools.CategoryBusiness,
	"step_limit":       tools.CategoryBusiness,
	"time_limit":       tools.CategoryBusiness,
	"empty":            tools.CategoryBusiness,
	"no_convergence":   tools.CategoryBusiness,
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
