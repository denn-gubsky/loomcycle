// Package decision calls decision models: models that answer typed questions
// about a piece of state with probabilities instead of text.
//
// A decision model is asked a set of named questions, each a choice among
// options, a yes/no ("noul"), or a score on a described scale, and returns for
// each the answer and how sure it is. There is no reply to parse and nothing to
// repair, which is what makes it usable for routing, gating and ranking inside
// the runtime.
//
// WHY A PACKAGE OF ITS OWN: the first user was the memory reranker, which spoke
// the protocol privately. A second user would have copied the endpoint, the key
// rule and the refusal parsing; this is the one copy, below the tool layer, that
// every user shares. It knows nothing of config: a caller resolves its endpoint
// and key the way it resolves any provider's and hands them in as Options.
package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/decisionq"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// The question types, the limits, a question and the faults are declared in
// internal/decisionq, which a team definition reads too; these are the same
// types under this package's names.
const (
	TypeChoice = decisionq.TypeChoice
	TypeNoul   = decisionq.TypeNoul
	TypeScore  = decisionq.TypeScore
)

type (
	Limits   = decisionq.Limits
	Question = decisionq.Question
)

// Driver is one provider's decision endpoint.
type Driver interface {
	// Decide asks req's questions of req.Model. The request is checked against
	// Limits first and refused, never trimmed, when it does not fit.
	Decide(ctx context.Context, req Request) (*Response, error)
	// Limits are the bounds a request to model must respect.
	Limits(model string) Limits
}

// Request is one call: every question is answered about the same state.
type Request struct {
	// Model is the name the provider serves, never a models: alias.
	Model string
	// State is what the questions are about: any JSON object.
	State map[string]any
	// Questions are keyed by the caller's own names, which key the answers.
	Questions map[string]Question
}

// Response is the model's answers, one per question, under the question's name.
type Response struct {
	// Provider is the provider id the call was made to; Model is the model the
	// provider says it served.
	Provider string
	Model    string
	Answers  map[string]Answer
	// Usage is the call's tokens, stamped with the provider, the requested model
	// and whose key paid, ready for a usage ledger.
	Usage providers.Usage
}

// Answer is one question's answer, as the model gave it. The numeric fields are
// pointers because zero is an answer: a noul of 0 is a certain "no", and must not
// read as a missing field.
type Answer struct {
	Type string `json:"type"`
	// Choice is the chosen option, for a choice.
	Choice *string `json:"choice,omitempty"`
	// Noul is the probability of yes, for a noul.
	Noul *float64 `json:"noul,omitempty"`
	// Score is the expected level, for a score; Legend names the levels.
	Score  *float64          `json:"score,omitempty"`
	Legend map[string]string `json:"legend,omitempty"`
	// Probabilities is the probability per option (choice) or per level (score).
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	// Raw is the answer exactly as received, so a field a later model version
	// adds is not lost to a caller that passes answers on.
	Raw json.RawMessage `json:"-"`
}

// Options is what a driver needs to reach one provider. The caller resolves the
// endpoint and the key as it does for the provider's chat calls.
type Options struct {
	// ProviderID is the provider's id in the operator's config, for usage records.
	ProviderID string
	BaseURL    string
	// APIKey is the operator's key ("" for a keyless endpoint). KeyEnvName is the
	// credential name a tenant's own key is stored under; "" means the endpoint
	// takes no key at all, and then no run is restricted from it.
	APIKey     string
	KeyEnvName string
	// Timeout bounds one call, including its wait for a slot. 0 = 30s.
	Timeout time.Duration
	// MaxConcurrent bounds this driver's calls in flight. 0 = 4. It is the driver's
	// own bound, not a provider run slot: a decision is asked from inside a run
	// that already holds one, and would wait on itself where the provider's cap is 1.
	MaxConcurrent int
}

const (
	defaultTimeout       = 30 * time.Second
	defaultMaxConcurrent = 4
)

// Factory builds a driver for one provider endpoint.
type Factory func(Options) (Driver, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register records the decision driver for a provider DRIVER name ("ollama"),
// the name a providers: entry carries in `driver:`. Called from init.
func Register(driver string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[driver] = f
}

// New builds the decision driver registered for a provider driver name.
func New(driver string, opts Options) (Driver, error) {
	registryMu.RLock()
	f, ok := registry[driver]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("the %q driver serves no decision models (drivers that do: %v)", driver, Registered())
	}
	return f(opts)
}

// Registered lists the provider driver names that serve decision models, sorted.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Validate checks req against lim and returns the first fault as an *Error.
// Questions are checked in name order, so the same bad request always reports
// the same fault.
func Validate(req Request, lim Limits) error {
	if strings.TrimSpace(req.Model) == "" {
		return &Error{Code: CodeModelNotFound, Message: "no model named"}
	}
	if faults := decisionq.Validate(req.Questions, lim); len(faults) > 0 {
		return faults[0]
	}
	return nil
}
