package connector

import (
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// MaxInterruptOtherAnswerChars caps an answer outside a question's options
// (RFC HG §3.2). The agent reads it as the person's free text; an option is
// bounded by the agent itself, an "other" answer only by this.
const MaxInterruptOtherAnswerChars = 4000

// CheckInterruptAnswer is the one answer check every resolve path applies —
// HTTP, gRPC, MCP interruption_resolve and A2A resume (RFC HG §3.2). A decline
// carries no answer and is not checked here.
//
//   - no options: a non-empty answer;
//   - options without allow_other: one of them, exactly;
//   - options with allow_other: any non-empty answer of at most
//     MaxInterruptOtherAnswerChars characters.
//
// A refusal wraps ErrInterruptInvalidAnswer (HTTP 422, gRPC InvalidArgument,
// an MCP tool error), with the same message on every path. Options that
// cannot be decoded refuse every answer: the row cannot say what it allows.
func CheckInterruptAnswer(row store.InterruptRow, answer string) error {
	opts, err := row.OptionList()
	if err != nil {
		return WithMessage(ErrInterruptInvalidAnswer, "the interrupt's options cannot be read")
	}
	if len(opts) == 0 {
		if answer == "" {
			return WithMessage(ErrInterruptInvalidAnswer, "answer is required for free-text interrupts")
		}
		return nil
	}
	if slices.Contains(opts, answer) {
		return nil
	}
	if !row.AllowOther {
		return WithMessage(ErrInterruptInvalidAnswer,
			fmt.Sprintf("answer %q is not one of the declared options: %v", answer, opts))
	}
	if answer == "" {
		return WithMessage(ErrInterruptInvalidAnswer, "answer is required")
	}
	if n := utf8.RuneCountInString(answer); n > MaxInterruptOtherAnswerChars {
		return WithMessage(ErrInterruptInvalidAnswer,
			fmt.Sprintf("an answer outside the options is %d characters, more than %d", n, MaxInterruptOtherAnswerChars))
	}
	return nil
}
