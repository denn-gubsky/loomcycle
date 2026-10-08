package decision

import (
	"context"
	"fmt"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// ModelSpec is one model a Service may ask: the name the operator listed it
// under, and the provider, served model and driver the name resolved to.
type ModelSpec struct {
	Name     string
	Provider string
	Model    string
	Driver   Driver
}

// ModelInfo describes one allowed model to a caller choosing among them.
type ModelInfo struct {
	// Name is what the operator wrote, and what Decide is called with.
	Name     string `json:"name"`
	Provider string `json:"provider"`
	// Model is the model the provider serves under that name.
	Model  string `json:"model"`
	Limits Limits `json:"limits"`
}

// Service is the operator's decision models: the ones a caller may name, and the
// one used when it names none. Safe for concurrent use: everything is set at
// boot, before the server serves, and only read afterwards.
type Service struct {
	def     string
	models  map[string]ModelSpec
	names   []string
	onUsage func(ctx context.Context, u *providers.Usage)
}

// NewService builds a Service over models, with def the default. def must be
// one of them.
func NewService(def string, models []ModelSpec) (*Service, error) {
	s := &Service{def: def, models: make(map[string]ModelSpec, len(models))}
	for _, m := range models {
		if m.Name == "" || m.Model == "" || m.Driver == nil {
			return nil, fmt.Errorf("decision: model %q is incomplete", m.Name)
		}
		if _, dup := s.models[m.Name]; dup {
			return nil, fmt.Errorf("decision: model %q is listed twice", m.Name)
		}
		s.models[m.Name] = m
		s.names = append(s.names, m.Name)
	}
	if _, ok := s.models[def]; !ok {
		return nil, fmt.Errorf("decision: the default model %q is not among the models", def)
	}
	sort.Strings(s.names)
	return s, nil
}

// Default is the name of the model used when a caller names none.
func (s *Service) Default() string { return s.def }

// Models lists the allowed models, sorted by name.
func (s *Service) Models() []ModelInfo {
	out := make([]ModelInfo, 0, len(s.names))
	for _, name := range s.names {
		m := s.models[name]
		out = append(out, ModelInfo{Name: m.Name, Provider: m.Provider, Model: m.Model, Limits: m.Driver.Limits(m.Model)})
	}
	return out
}

// SetOnUsage records each call's tokens against the run whose context it is.
// Set once, before the server serves.
func (s *Service) SetOnUsage(f func(ctx context.Context, u *providers.Usage)) { s.onUsage = f }

// Decide asks questions about state of the model listed as name ("" = the
// default). A name outside the list is CodeModelNotAllowed: the list is the
// operator's, and a caller cannot reach a model by knowing what it is called.
// Every question asked has an answer in the response, or the call fails.
func (s *Service) Decide(ctx context.Context, name string, state map[string]any, questions map[string]Question) (*Response, error) {
	if name == "" {
		name = s.def
	}
	m, ok := s.models[name]
	if !ok {
		return nil, &Error{Code: CodeModelNotAllowed,
			Message: fmt.Sprintf("model %q is not one of the decision models (%v)", name, s.names)}
	}
	resp, err := m.Driver.Decide(ctx, Request{Model: m.Model, State: state, Questions: questions})
	if err != nil {
		return nil, err
	}
	// Booked before the answers are checked: tokens a provider reports were spent.
	if s.onUsage != nil && (resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0) {
		u := resp.Usage
		s.onUsage(ctx, &u)
	}
	for q := range questions {
		if _, answered := resp.Answers[q]; !answered {
			return nil, &Error{Code: CodeCallFailed, Question: q, Message: "the reply carries no answer"}
		}
	}
	return resp, nil
}
