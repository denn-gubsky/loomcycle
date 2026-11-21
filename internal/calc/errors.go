package calc

import (
	"fmt"
	"sort"
)

// Error is a refusal. Code is one of ErrorCodes(); Detail says what went
// wrong in words a model can act on; Hint, when set, says what to write
// instead; Pos is the byte offset into Request.Expr, or -1 when the error is
// not tied to a place in the expression (a var, a limit).
type Error struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
	Pos    int    `json:"pos"`
}

func (e *Error) Error() string {
	s := e.Code + ": " + e.Detail
	if e.Hint != "" {
		s += " (" + e.Hint + ")"
	}
	return s
}

// errCode is unexported so that an Error can only carry one of the declared
// codes below; TestErrorCodes_EachDeclaredCodeIsReachable keeps the list and
// the producers in step.
type errCode string

const (
	codeSyntax         errCode = "syntax"
	codeUnknownName    errCode = "unknown_name"
	codeUnknownFunc    errCode = "unknown_function"
	codeArity          errCode = "arity"
	codeType           errCode = "type"
	codeDomain         errCode = "domain"
	codeDivZero        errCode = "division_by_zero"
	codeOverflow       errCode = "overflow"
	codeTooLarge       errCode = "too_large"
	codeIndex          errCode = "index"
	codeShape          errCode = "shape"
	codeRecursion      errCode = "recursion"
	codeReservedName   errCode = "reserved_name"
	codeBadRequest     errCode = "bad_request"
	codeBadVar         errCode = "bad_var"
	codeExprTooLong    errCode = "expr_too_long"
	codeVarsTooLarge   errCode = "vars_too_large"
	codeStepLimit      errCode = "step_limit"
	codeTimeLimit      errCode = "time_limit"
	codeCanceled       errCode = "canceled"
	codeListTooLong    errCode = "list_too_long"
	codeTooDeep        errCode = "too_deep"
	codeEmptyStatement errCode = "empty"
	codeBadDate        errCode = "bad_date"
	codeUnknownUnit    errCode = "unknown_unit"
	codeDimension      errCode = "dimension"
)

var allCodes = [...]errCode{
	codeSyntax, codeUnknownName, codeUnknownFunc, codeArity, codeType,
	codeDomain, codeDivZero, codeOverflow, codeTooLarge, codeIndex,
	codeShape, codeRecursion, codeReservedName, codeBadRequest, codeBadVar,
	codeExprTooLong, codeVarsTooLarge, codeStepLimit, codeTimeLimit,
	codeCanceled, codeListTooLong, codeTooDeep, codeEmptyStatement,
	codeBadDate,
	codeUnknownUnit, codeDimension,
}

// ErrorCodes returns every code an Error may carry, sorted.
func ErrorCodes() []string {
	out := make([]string, len(allCodes))
	for i, c := range allCodes {
		out[i] = string(c)
	}
	sort.Strings(out)
	return out
}

func errf(code errCode, pos int, format string, args ...any) *Error {
	return &Error{Code: string(code), Detail: fmt.Sprintf(format, args...), Pos: pos}
}

func (e *Error) withHint(h string) *Error {
	e.Hint = h
	return e
}
