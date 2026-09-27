package builtin

import (
	"github.com/denn-gubsky/loomcycle/internal/errclassify"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Classified failures. A builtin's failure used to be errResult(msg): the
// model got the message and nothing about what KIND of failure it was, so it
// could not tell "fix the input" from "this will never work" from "try again
// later". Each helper below makes that decision at the call site, where the
// code knows the reason — never inferred from the message text later.
//
// msg stays the result's text, exactly as errResult produced it. The second
// argument is the NEXT STEP, model-visible: say what to do, not restate what
// broke; no internal design-doc references, host names or secrets. Leave it
// empty only when msg already says what to do.
//
// ⚠️ A row that exists but belongs to another tenant, user or agent is NOT a
// permission failure. It must read exactly like a row that does not exist
// (errNotFound, same text): a different category would tell the caller the
// row is there.

// errValidation is a call the caller can fix alone and resend: a missing,
// invalid or unknown argument, a bad value, an unknown op.
func errValidation(msg, fix string) tools.Result {
	return classified(msg, tools.CategoryValidation, false, fix)
}

// errNotFound is a named id or path that does not exist — in the caller's
// scope. It is validation: the caller fixes the id and sends a new call. Use it
// for another scope's row too, with the same text (see the warning above).
func errNotFound(msg, fix string) tools.Result {
	return classified(msg, tools.CategoryValidation, false, fix)
}

// errBusiness is a well-formed call refused by a rule or by state: a quota, a
// revision conflict, a retired definition, already exists, a cap reached, a
// capability this deployment does not have. Resending it unchanged fails.
func errBusiness(msg, next string) tools.Result {
	return classified(msg, tools.CategoryBusiness, false, next)
}

// errPermission is the caller's own grant lacking something an operator could
// grant — a scope not in memory_scopes, a tool not granted. Never for another
// principal's row (see the warning above).
func errPermission(msg, next string) tools.Result {
	return classified(msg, tools.CategoryPermission, false, next)
}

// errTransient is a failure that resending the same call may clear: a
// timeout, a backend briefly unavailable, a lock held by a concurrent writer.
func errTransient(msg, next string) tools.Result {
	return classified(msg, tools.CategoryTransient, true, next)
}

// errFrom wraps an error a store or backend returned: msg is the text (as
// errResult's was, usually a prefix plus err.Error()), and the category comes
// from err's TYPE, through the same classifier the transports use. An error of
// no known type stays unclassified rather than guessed.
func errFrom(msg string, err error) tools.Result {
	res := errResult(msg)
	if info, ok := errclassify.CategoryOf(err); ok {
		res.Error = &info
	}
	return res
}

func classified(msg string, cat tools.ErrorCategory, retryable bool, next string) tools.Result {
	return tools.Result{
		IsError: true,
		Text:    msg,
		Error:   &tools.ErrorInfo{Category: cat, Retryable: retryable, Description: next},
	}
}
