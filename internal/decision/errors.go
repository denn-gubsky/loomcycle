package decision

import "errors"

// The codes an *Error carries. A caller branches on the code, never on the text.
const (
	// CodeTooManyQuestions: more questions than one call takes. Limit is the most.
	CodeTooManyQuestions = "too_many_questions"
	// CodeBadOptions: a choice or score with too few or too many options. Min and
	// Max are the range.
	CodeBadOptions = "bad_options"
	// CodeBadQuestion: an unknown type, missing instructions, or criteria of the
	// wrong shape for the type.
	CodeBadQuestion = "bad_question"
	// CodePromptTooLarge: the state and questions exceed the model's context. The
	// model never truncates, and neither does this package. Tokens and Limit are
	// the prompt's size and the model's, when the refusal gave them (else 0).
	CodePromptTooLarge = "prompt_too_large"
	// CodeModelNotFound: the provider does not serve the model.
	CodeModelNotFound = "model_not_found"
	// CodeModelNotAllowed: the model is not one the operator's decision block lists.
	CodeModelNotAllowed = "model_not_allowed"
	// CodeTimeout: the call, or its wait for a slot, outlived the timeout. It
	// wraps context.DeadlineExceeded.
	CodeTimeout = "timeout"
	// CodeCallFailed: anything else: the transport, the key, an unreadable reply.
	CodeCallFailed = "call_failed"
)

// Error is every fault this package reports.
type Error struct {
	Code    string
	Message string
	// Question names the question at fault, when the fault is one question's.
	Question string
	// Limit is the most questions (CodeTooManyQuestions) or the model's context in
	// tokens (CodePromptTooLarge).
	Limit int
	// Min and Max are the option range (CodeBadOptions).
	Min, Max int
	// Tokens is the refused prompt's size (CodePromptTooLarge).
	Tokens int
	// Err is the cause, when there is one.
	Err error
}

func (e *Error) Error() string {
	msg := e.Message
	if e.Question != "" {
		msg = "question " + quote(e.Question) + ": " + msg
	}
	if e.Err != nil {
		if msg == "" {
			return "decision: " + e.Code + ": " + e.Err.Error()
		}
		return "decision: " + e.Code + ": " + msg + ": " + e.Err.Error()
	}
	return "decision: " + e.Code + ": " + msg
}

func (e *Error) Unwrap() error { return e.Err }

// CodeOf is err's decision code, or "" when err is not a decision fault.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func quote(s string) string { return `"` + s + `"` }
