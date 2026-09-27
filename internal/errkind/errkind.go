// Package errkind is the vocabulary for describing WHAT KIND of failure
// something is: the coarse category, whether resending can succeed, and an
// optional backoff hint.
//
// # Why this is its own package
//
// Every layer that has to describe a failure sits above it — internal/tools
// (a tool's result), internal/providers (a run event), internal/connector, and
// the MCP, HTTP and gRPC transports. internal/tools cannot host it: tools
// imports providers, so providers could never import back, and providers is
// exactly where a run event has to carry a category.
//
// # Keep it a leaf
//
// This package imports NOTHING from this module, and that is load-bearing
// rather than tidy. internal/providers sits near the floor of the import
// graph; the moment errkind grows a dependency it stops being importable from
// there, and the reason it exists is gone.
//
// So: types and constants only. A classifier belongs in internal/errclassify,
// which maps the runtime's typed errors onto this vocabulary and imports half
// the tree to do it. A renderer belongs with the transport that renders. If
// you are about to add a helper here that needs an import, it belongs
// somewhere else.
package errkind

import (
	"encoding/json"
	"time"
)

// Category says what KIND of failure was hit, because that is the only thing
// the caller actually has to decide from: resend this call, change it, or
// stop. The four values are deliberately coarse — a finer taxonomy would not
// change the decision, and a caller that has to reason about fifteen buckets
// reasons about none of them.
type Category string

const (
	// CategoryTransient — the request was valid and the system was
	// temporarily unable to serve it. The same call may succeed later.
	CategoryTransient Category = "transient"

	// CategoryValidation — the request is malformed. The caller can fix it
	// alone, which is what separates this from Business: a validation
	// failure is recoverable without anyone else's involvement.
	CategoryValidation Category = "validation"

	// CategoryBusiness — the request is well-formed and was refused by a
	// rule. Retrying is pointless and so is rewording; the caller needs a
	// different path. A token budget lands here rather than in Transient
	// even though the HTTP surface renders it 429, because a budget does
	// not refill by waiting.
	CategoryBusiness Category = "business"

	// CategoryPermission — the caller lacks authority it could be granted.
	// NEVER used for "this row belongs to another tenant": that stays an
	// opaque not-found, because a permission category on a cross-tenant
	// read confirms the row exists and turns the error into an existence
	// oracle.
	CategoryPermission Category = "permission"
)

// Info is the optional structured half of a failure. It exists so a caller
// does not have to infer control flow by reading English prose.
//
// A nil *Info means "not classified" — which is the honest state for most
// failures today and keeps their behaviour byte-identical. Nil is NOT a
// synonym for "unknown category"; there is deliberately no such category,
// because a bucket that carries no decision is worse than an absent field.
type Info struct {
	Category Category

	// Retryable answers only "will resending this exact call fail?". It is
	// not "should the caller give up" — a false here still leaves the
	// alternative paths in Description open.
	Retryable bool

	// Description says what went wrong AND what to do next. Model-visible
	// prompt text: no internal design-doc citations, no DSNs, no host names
	// from operator config, no token suffixes.
	Description string

	// RetryAfter is an optional backoff hint, meaningful only when Retryable.
	// A pointer because an absent hint and a zero hint are opposite
	// instructions: "wait as you see fit" versus "retry immediately".
	RetryAfter *time.Duration

	// CallFormat is the correct shape of the call that failed, when the tool is
	// documented and the call's shape may be the cause: a validation failure,
	// or one nobody has classified yet. Nil otherwise — for a transient,
	// business or permission failure a correct example would read as if the
	// shape were the problem.
	//
	// It can be set on an Info with no Category: that is an unclassified
	// failure that still carries the call format, not a classification.
	CallFormat *CallFormat
}

// infoWire is Info's JSON shape: the SSE `error_info` object, and a persisted
// event's. Its keys are the ones @loomcycle/client's ErrorInfo type has always
// declared, and the gRPC ErrorInfo message uses.
//
// ⚠️ Info had no JSON tags, so it serialized as its Go field names
// ({"Category", "Retryable", "RetryAfter" in nanoseconds}), and a TS consumer
// reading `error_info.category` got undefined. UnmarshalJSON still reads that
// legacy shape, because events persisted before the fix carry it.
type infoWire struct {
	Category          Category    `json:"category,omitempty"`
	IsRetryable       bool        `json:"is_retryable"`
	Description       string      `json:"description,omitempty"`
	RetryAfterMS      *int64      `json:"retry_after_ms,omitempty"`
	CorrectCallFormat *CallFormat `json:"correct_call_format,omitempty"`
}

// MarshalJSON writes Info in its wire shape. A backoff is sent only on a
// retryable failure with a real hint: a zero reads as "retry now", and a hint
// on a non-retryable failure invites waiting for something that cannot clear.
func (i Info) MarshalJSON() ([]byte, error) {
	w := infoWire{
		Category:          i.Category,
		IsRetryable:       i.Retryable,
		Description:       i.Description,
		CorrectCallFormat: i.CallFormat,
	}
	if i.Retryable && i.RetryAfter != nil && *i.RetryAfter > 0 {
		ms := i.RetryAfter.Milliseconds()
		if ms < 1 {
			ms = 1
		}
		w.RetryAfterMS = &ms
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads the wire shape, and the legacy Go-field-name shape that
// events persisted before MarshalJSON existed carry.
func (i *Info) UnmarshalJSON(b []byte) error {
	var w struct {
		infoWire
		// Legacy keys.
		LegacyCategory    Category       `json:"Category"`
		LegacyRetryable   bool           `json:"Retryable"`
		LegacyDescription string         `json:"Description"`
		LegacyRetryAfter  *time.Duration `json:"RetryAfter"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*i = Info{
		Category:    w.Category,
		Retryable:   w.IsRetryable,
		Description: w.Description,
		CallFormat:  w.CorrectCallFormat,
	}
	if w.RetryAfterMS != nil {
		d := time.Duration(*w.RetryAfterMS) * time.Millisecond
		i.RetryAfter = &d
	}
	if i.Category == "" && w.LegacyCategory != "" {
		i.Category = w.LegacyCategory
		i.Retryable = w.LegacyRetryable
		i.Description = w.LegacyDescription
		i.RetryAfter = w.LegacyRetryAfter
	}
	return nil
}

// CallFormat is what a correct call to a documented tool looks like, taken from
// the operation's own help article. Measured on local models: a model rarely
// follows a pointer to a manual, but it does copy a correct call it is shown.
type CallFormat struct {
	Tool string `json:"tool"`
	// Op is the operation the example is for; empty when the call named no
	// valid op, in which case Operations says which exist.
	Op string `json:"op,omitempty"`
	// Example is the argument object of one correct call.
	Example json.RawMessage `json:"example,omitempty"`
	// Operations lists the tool's valid operations — only when the op itself
	// was missing or unknown, the one case where it is the fix.
	Operations []string `json:"operations,omitempty"`
	// Reference is the call that returns the full article.
	Reference *CallRef `json:"reference,omitempty"`
}

// CallRef is one tool call, as its tool name and argument object.
type CallRef struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
}
