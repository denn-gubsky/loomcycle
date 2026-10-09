package decision

import "github.com/denn-gubsky/loomcycle/internal/decisionq"

// The codes an *Error carries, and the fault type: internal/decisionq's, under
// this package's names. A caller branches on the code, never on the text.
const (
	CodeTooManyQuestions = decisionq.CodeTooManyQuestions
	CodeBadOptions       = decisionq.CodeBadOptions
	CodeBadQuestion      = decisionq.CodeBadQuestion
	CodePromptTooLarge   = decisionq.CodePromptTooLarge
	CodeModelNotFound    = decisionq.CodeModelNotFound
	CodeModelNotAllowed  = decisionq.CodeModelNotAllowed
	CodeTimeout          = decisionq.CodeTimeout
	CodeCallFailed       = decisionq.CodeCallFailed
)

// Error is every fault this package reports.
type Error = decisionq.Error

// CodeOf is err's decision code, or "" when err is not a decision fault.
func CodeOf(err error) string { return decisionq.CodeOf(err) }
