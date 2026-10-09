// Package decisionq is the shape of a decision model's questions and the check
// that a set of them is well formed.
//
// It is apart from internal/decision, which calls the models, because a team
// definition carries questions too and checks them when it is saved: the
// definition's model must not import the provider packages a call needs. Both
// read the one validator here, so a question a team stores is a question the
// call accepts.
package decisionq

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The question types.
const (
	TypeChoice = "choice"
	TypeNoul   = "noul"
	TypeScore  = "score"
)

// Limits bound one request. They are the endpoint's own, known ahead of a call.
// How much state and criteria fit is NOT among them: it differs per model and no
// endpoint reports it, so an over-long request is learned from the model's
// refusal (CodePromptTooLarge), which carries the numbers.
type Limits struct {
	MaxQuestions int `json:"max_questions"`
	// MinOptions and MaxOptions bound a choice's options and a score's levels.
	MinOptions int `json:"min_options"`
	MaxOptions int `json:"max_options"`
}

// AnyModel is the least every decision model asks of a question: a choice or a
// score has two options or more. It is what can be checked with no model in
// hand; a model's own Limits add how many questions and options it takes.
var AnyModel = Limits{MinOptions: 2}

// Question is one typed question.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	// Criteria depends on Type:
	//   choice: an object mapping each option to a description (a string or null).
	//           The keys are the caller's own words and come back as the answer.
	//   noul:   optional; an object describing "true" and/or "false".
	//   score:  an array of descriptions, lowest level first.
	Criteria json.RawMessage `json:"criteria,omitempty"`
}

// Options are a choice question's options, sorted; nil for another type or
// criteria that are not a choice's.
func (q Question) Options() []string {
	if q.Type != TypeChoice {
		return nil
	}
	var opts map[string]json.RawMessage
	if json.Unmarshal(q.Criteria, &opts) != nil {
		return nil
	}
	out := make([]string, 0, len(opts))
	for name := range opts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

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

// Error is every fault a decision call or its questions report.
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

// Validate checks questions against lim and returns every fault, each an
// *Error. Questions are checked in name order, so the same bad set always
// reports the same faults in the same order. A fault in the set as a whole
// (none, or too many) comes first.
func Validate(questions map[string]Question, lim Limits) []*Error {
	var out []*Error
	if len(questions) == 0 {
		return []*Error{{Code: CodeBadQuestion, Message: "at least one question is required"}}
	}
	if lim.MaxQuestions > 0 && len(questions) > lim.MaxQuestions {
		out = append(out, &Error{Code: CodeTooManyQuestions, Limit: lim.MaxQuestions,
			Message: fmt.Sprintf("%d questions; one call takes at most %d", len(questions), lim.MaxQuestions)})
	}
	names := make([]string, 0, len(questions))
	for name := range questions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validateQuestion(name, questions[name], lim); err != nil {
			out = append(out, err)
		}
	}
	return out
}

func validateQuestion(name string, q Question, lim Limits) *Error {
	bad := func(format string, a ...any) *Error {
		return &Error{Code: CodeBadQuestion, Question: name, Message: fmt.Sprintf(format, a...)}
	}
	if name == "" {
		return bad("a question needs a name")
	}
	if strings.TrimSpace(q.Instructions) == "" {
		return bad("instructions are required")
	}
	criteria := bytes.TrimSpace(q.Criteria)
	absent := len(criteria) == 0 || bytes.Equal(criteria, []byte("null"))
	options := func(n int, what string) *Error {
		if n >= lim.MinOptions && (lim.MaxOptions <= 0 || n <= lim.MaxOptions) {
			return nil
		}
		takes := fmt.Sprintf("%d to %d", lim.MinOptions, lim.MaxOptions)
		if lim.MaxOptions <= 0 {
			takes = fmt.Sprintf("at least %d", lim.MinOptions)
		}
		return &Error{Code: CodeBadOptions, Question: name, Min: lim.MinOptions, Max: lim.MaxOptions,
			Message: fmt.Sprintf("%d %s; a %s takes %s", n, what, q.Type, takes)}
	}
	switch q.Type {
	case TypeChoice:
		var opts map[string]json.RawMessage
		if absent || json.Unmarshal(criteria, &opts) != nil {
			return bad("choice criteria must be an object mapping each option to a description or null")
		}
		for key, v := range opts {
			if key == "" || !isStringOrNull(v) {
				return bad("choice criteria must be an object mapping each option to a description or null")
			}
		}
		return options(len(opts), "options")
	case TypeNoul:
		if absent {
			return nil
		}
		var sides map[string]json.RawMessage
		if json.Unmarshal(criteria, &sides) != nil {
			return bad(`noul criteria must be an object describing "true" and "false"`)
		}
		for key, v := range sides {
			if (key != "true" && key != "false") || !isString(v) {
				return bad(`noul criteria must be an object describing "true" and "false"`)
			}
		}
		return nil
	case TypeScore:
		var levels []json.RawMessage
		if absent || json.Unmarshal(criteria, &levels) != nil {
			return bad("score criteria must be an array of descriptions, lowest level first")
		}
		for _, v := range levels {
			if !isString(v) {
				return bad("score criteria must be an array of descriptions, lowest level first")
			}
		}
		return options(len(levels), "levels")
	default:
		return bad("type %q is not one of choice, noul, score", q.Type)
	}
}

func isString(v json.RawMessage) bool {
	v = bytes.TrimSpace(v)
	return len(v) > 0 && v[0] == '"'
}

func isStringOrNull(v json.RawMessage) bool {
	return isString(v) || bytes.Equal(bytes.TrimSpace(v), []byte("null"))
}
