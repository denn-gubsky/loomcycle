package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/errclassify"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Decision asks one of the operator's decision models typed questions about a
// piece of state and returns the model's answers unchanged.
//
// It has ONE operation and no `op` field: a call is the questions. Which model
// answers is the caller's choice among the models the operator listed, less
// any the agent's own `decision` block narrows away; the tool never reaches a
// model by a name outside that list.
//
// It is registered only when the operator declared a decision: block, so an
// agent that lists it on a deployment without one is simply not offered it —
// a tool whose every call would fail is worse in a model's tool list than no
// tool. A tool built with no Service still answers (decision_not_configured)
// rather than panicking.
//
// ⚠️ A call needs a run, or a context the server prepared for a call without
// one. A run's context carries what a call must be held to: whether it may
// spend the operator's provider key (a fail-open bit that only a run-start
// site stamps) and the run its tokens are charged to. The server's run-less
// path (tools.WithMeteredOffRunCall) stamps the same bit from the caller's
// principal and names who is charged instead. Any other dispatch would hand a
// restricted tenant the operator's key and bill nobody, so it is refused.
type Decision struct {
	// Service is the operator's decision models. Its usage callback books each
	// call's tokens against the run on the call's context.
	Service *decision.Service
}

// The codes this tool adds to the driver's (internal/decisionq). Every
// failure's text starts "Decision: <code>: ", so a caller that parses results
// (a code agent) can branch on the code without reading the sentence.
const (
	decisionNotConfigured = "decision_not_configured"
	// decisionInvalidInput: the arguments are not the documented shape at all
	// (not JSON, or no state object), as distinct from one bad question.
	decisionInvalidInput = "invalid_input"
	// decisionNoRun: the call's context carries no run and was not prepared by
	// the server's run-less path. A model inside a run never sees it; it is what
	// a surface that dispatched the tool directly gets.
	decisionNoRun = "no_run"
	// decisionKeyRestricted: the run may not spend the operator's provider key
	// and has none of its own. The same code the HTTP surface uses for the same
	// refusal, and kept apart from call_failed because nothing about the call
	// can fix it.
	decisionKeyRestricted = "operator_key_restricted"
)

// The tool's own codes, for a transport that maps a failed result onto its
// status. The driver's are decision.Code*.
const (
	DecisionCodeNotConfigured = decisionNotConfigured
	DecisionCodeInvalidInput  = decisionInvalidInput
	DecisionCodeNoRun         = decisionNoRun
	DecisionCodeKeyRestricted = decisionKeyRestricted
)

// DecisionFailureCode is the code a failed Decision result's text starts with
// ("Decision: <code>: …"), or "" when the text is not the tool's own: a refusal
// the dispatcher made before the tool ran (an unknown argument).
func DecisionFailureCode(text string) string {
	rest, ok := strings.CutPrefix(text, "Decision: ")
	if !ok {
		return ""
	}
	code, _, ok := strings.Cut(rest, ": ")
	if !ok {
		return ""
	}
	return code
}

func (d *Decision) Name() string { return "Decision" }

func (d *Decision) Description() string {
	return "Ask a decision model typed questions about text you supply, and get each answer with probabilities instead of prose: a judgement, not generated text. " +
		"Every question in a call is answered about the same `state` (a JSON object). Question types: " +
		"choice — pick one of 2-26 options you describe; " +
		"noul — yes or no, returned as the probability of yes; " +
		"score — a position on a scale whose levels you describe, lowest first. " +
		"Use it to route, gate, rank or grade given text. " +
		"Do NOT use it for anything that needs reasoning, lookup or written output: it reads only what is in state and returns only the answers. " +
		"The probabilities are NOT calibrated: compare options within one answer, and do not treat a fixed threshold as a guarantee. " +
		"The request must fit the model's context and is never shortened for you. " +
		"Formats and worked examples: Context op=help topic=Decision."
}

const decisionInputSchema = `{
	"type": "object",
	"properties": {
		"model":     {"type": "string", "description": "Which decision model answers. Omit it for the default. A name outside the allowed list is refused, and the refusal lists the names."},
		"state":     {"type": "object", "description": "What the questions are about: any JSON object (strings, numbers, nested objects, arrays). The model reads all of it and nothing else."},
		"questions": {"type": "object", "description": "1 to 64 questions, keyed by names you choose; each answer comes back under its question's name.", "additionalProperties": {
			"type": "object",
			"properties": {
				"type":         {"type": "string", "enum": ["choice", "noul", "score"]},
				"instructions": {"type": "string", "description": "The question itself, in plain words."},
				"criteria":     {"description": "choice (required): an object mapping each option to a description, or to null when the option explains itself; the option names come back as the answer. noul (optional): an object describing \"true\" and/or \"false\". score (required): an array of level descriptions, lowest first."}
			},
			"required": ["type", "instructions"]
		}}
	},
	"required": ["state", "questions"]
}`

func (d *Decision) InputSchema() json.RawMessage { return json.RawMessage(decisionInputSchema) }

type decisionInput struct {
	Model     string                       `json:"model"`
	State     json.RawMessage              `json:"state"`
	Questions map[string]decision.Question `json:"questions"`
}

// decisionOutput is the tool result. Answers are the model's own bytes: a field
// a later model version adds reaches the caller without this tool knowing it.
type decisionOutput struct {
	// Model is the name asked, as the operator listed it (the default's name
	// when the call named none); ServedModel is what the provider says answered.
	Model       string                     `json:"model"`
	Provider    string                     `json:"provider"`
	ServedModel string                     `json:"served_model"`
	Answers     map[string]json.RawMessage `json:"answers"`
	Usage       decisionUsage              `json:"usage"`
}

type decisionUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// parseDecisionInput reads a call's arguments. It is the tool's own input
// check, apart from Execute so the help articles' examples are held to it.
func parseDecisionInput(raw json.RawMessage) (decisionInput, map[string]any, error) {
	var in decisionInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, nil, &decision.Error{Code: decisionInvalidInput, Message: "the arguments are not the documented shape: " + err.Error()}
	}
	state := bytes.TrimSpace(in.State)
	if len(state) == 0 || state[0] != '{' {
		return in, nil, &decision.Error{Code: decisionInvalidInput, Message: "state is required and must be a JSON object"}
	}
	var st map[string]any
	if err := json.Unmarshal(state, &st); err != nil {
		return in, nil, &decision.Error{Code: decisionInvalidInput, Message: "state is not a JSON object: " + err.Error()}
	}
	return in, st, nil
}

// allowedModels resolves the model a call uses and the names it could have
// used, from the operator's list and the calling agent's narrowing of it.
func (d *Decision) allowedModels(ctx context.Context, asked string) (name string, allowed []string, ok bool) {
	infos := d.Service.Models()
	operator := make([]string, 0, len(infos))
	for _, m := range infos {
		operator = append(operator, m.Name)
	}
	return tools.DecisionPolicy(ctx).Pick(asked, d.Service.Default(), operator)
}

func (d *Decision) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if d.Service == nil {
		return decisionFailure(ctx, &decision.Error{Code: decisionNotConfigured,
			Message: "this deployment declares no decision models"}, nil), nil
	}
	// Fail closed on a call with neither a run nor the server's run-less
	// preparation (see the type's comment): refused here, before a key is
	// resolved, whatever surface dispatched it.
	if _, metered := tools.MeteredOffRunCall(ctx); !metered && tools.RunID(ctx) == "" {
		return decisionFailure(ctx, &decision.Error{Code: decisionNoRun,
			Message: "a decision model is asked from inside a run, or through the decision API"}, nil), nil
	}
	in, state, err := parseDecisionInput(raw)
	if err != nil {
		return decisionFailure(ctx, err, nil), nil
	}
	name, allowed, ok := d.allowedModels(ctx, in.Model)
	if !ok {
		// Worded for either caller: an agent in a run, or a caller with no run
		// and no agent (the HTTP, gRPC and MCP surfaces).
		msg := fmt.Sprintf("model %q is not one of the decision models you may ask", in.Model)
		if in.Model == "" {
			msg = "none of the decision models you may ask is offered by this deployment"
		}
		return decisionFailure(ctx, &decision.Error{Code: decision.CodeModelNotAllowed, Message: msg}, allowed), nil
	}
	resp, err := d.Service.Decide(ctx, name, state, in.Questions)
	if err != nil {
		return decisionFailure(ctx, err, allowed), nil
	}
	out := decisionOutput{
		Model: name, Provider: resp.Provider, ServedModel: resp.Model,
		Answers: make(map[string]json.RawMessage, len(resp.Answers)),
		Usage:   decisionUsage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens},
	}
	for q, a := range resp.Answers {
		if len(a.Raw) > 0 {
			out.Answers[q] = a.Raw
			continue
		}
		// A driver that kept no raw bytes: the typed fields are all there is.
		b, merr := json.Marshal(a)
		if merr != nil {
			return decisionFailure(ctx, &decision.Error{Code: decision.CodeCallFailed, Question: q, Message: "the answer is not readable"}, allowed), nil
		}
		out.Answers[q] = b
	}
	body, err := json.Marshal(out)
	if err != nil {
		return decisionFailure(ctx, &decision.Error{Code: decision.CodeCallFailed, Message: "the answers are not readable"}, allowed), nil
	}
	return tools.Result{Text: string(body)}, nil
}

// decisionFailure turns a fault into the tool's failed result: the text
// "Decision: <code>: <what happened>", a category, and the next step. allowed
// is the list of names the caller may use, for the faults a different model
// could fix.
//
// The texts reach two kinds of caller: an agent inside a run, and a caller with
// no run and no agent (the server's run-less path). Each is worded to be true
// for both, or picked by which one ctx says this is.
//
// Only the fault's own message reaches the text, never the cause it wraps: a
// transport error names the operator's host, which is for the log, not for the
// model or a tenant reading the transcript.
func decisionFailure(ctx context.Context, err error, allowed []string) tools.Result {
	var e *decision.Error
	if !errors.As(err, &e) {
		log.Printf("Decision: %v", err)
		return errFrom("Decision: "+decision.CodeCallFailed+": the call to the decision model failed", err)
	}
	what := e.Message
	if e.Question != "" {
		what = fmt.Sprintf("question %q: %s", e.Question, what)
	}
	text := func(code, what string) string { return "Decision: " + code + ": " + what }
	names := strings.Join(allowed, ", ")

	switch e.Code {
	case decisionNotConfigured:
		return errBusiness(text(e.Code, what),
			"No call to this tool can succeed here. Decide another way, or ask an operator to declare decision models.")
	case decisionNoRun:
		// The caller here is neither of the two that are served, so the hint
		// names both ways in.
		return errBusiness(text(e.Code, what),
			"Ask from a run of an agent that holds the Decision tool, or through the decision API: POST /v1/_decide, the gRPC Decide RPC or the MCP `decision` tool.")
	case decisionInvalidInput:
		return errValidation(text(e.Code, what),
			"Pass `state` (a JSON object holding what the questions are about) and `questions` (an object of named questions).")
	case decision.CodeTooManyQuestions:
		fix := "Ask fewer questions in one call: split them across several calls."
		if e.Limit > 0 {
			fix = fmt.Sprintf("One call takes at most %d questions: split them across several calls.", e.Limit)
		}
		return errValidation(text(e.Code, what), fix)
	case decision.CodeBadOptions:
		fix := "Give the question more or fewer options."
		if e.Max > 0 {
			fix = fmt.Sprintf("A choice takes %d to %d options and a score %d to %d levels: add or remove some, or split the question in two.", e.Min, e.Max, e.Min, e.Max)
		}
		return errValidation(text(e.Code, what), fix)
	case decision.CodeBadQuestion:
		return errValidation(text(e.Code, what),
			`Each question needs a type (choice, noul or score), instructions, and criteria in that type's shape: choice, an object mapping each option to a description or null; noul, nothing or an object describing "true" and "false"; score, an array of level descriptions, lowest first.`)
	case decision.CodePromptTooLarge:
		if e.Tokens > 0 && e.Limit > 0 {
			what = fmt.Sprintf("the request is %d tokens and this model reads at most %d", e.Tokens, e.Limit)
		} else if what == "" {
			what = "the request is larger than this model reads"
		}
		return errBusiness(text(e.Code, what),
			"Nothing is shortened for you. Shorten the state or the criteria, or ask fewer questions per call, and send it again.")
	case decision.CodeModelNotAllowed:
		fix := "Omit `model` to use the default."
		if names != "" {
			fix = "Omit `model` to use the default, or name one of: " + names + "."
		}
		return errValidation(text(e.Code, what), fix)
	case decision.CodeModelNotFound:
		next := "The provider does not serve this model, so the same call will fail again. Ask an operator to fix the entry."
		if len(allowed) > 1 {
			next = "The provider does not serve this model, so the same call will fail again. Name another of: " + names + "."
		}
		return errBusiness(text(e.Code, what), next)
	case decision.CodeTimeout:
		log.Printf("Decision: %v", err)
		return errTransient(text(e.Code, "the decision model did not answer in time"),
			"Send the same call again. If it times out again, ask fewer questions or shorten the state.")
	}
	// call_failed, and any code a driver adds later.
	if errors.Is(err, providers.ErrOperatorKeyForbidden) {
		// Who is barred differs: a run, or with no run the caller itself.
		who := "this run may not use the operator's provider key, and has none of its own for this provider"
		if _, offRun := tools.MeteredOffRunCall(ctx); offRun {
			who = "you may not use the operator's provider key, and have none of your own for this provider"
		}
		res := errPermission(text(decisionKeyRestricted, who), "")
		if info, ok := errclassify.CategoryOf(providers.ErrOperatorKeyForbidden); ok {
			res.Error = &info // the wording every surface gives this refusal
		}
		return res
	}
	log.Printf("Decision: %v", err)
	failed := "the call to the decision model failed"
	if what != "" {
		failed += ": " + what
	}
	return errTransient(text(decision.CodeCallFailed, failed),
		"Send the same call once more. If it fails again, continue without the decision and say that it was unavailable.")
}
