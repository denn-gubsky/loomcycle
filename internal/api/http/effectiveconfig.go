package http

import (
	"context"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Where an effective value came from. The SOURCE is the point of this report:
// "max_iterations: 16" is almost useless on its own, because a reader cannot
// tell a deliberate setting from a default nobody chose, and those call for
// opposite actions.
const (
	sourceRun        = "run"        // a per-run override set it
	sourceDefinition = "definition" // the agent definition set it
	sourceUserTier   = "user_tier"  // operator tier policy
	sourceOperator   = "operator"   // operator env / global configuration
	sourceResolved   = "resolved"   // decided at runtime: the tier cascade, the driver, the model
	sourceDefault    = "default"    // a fixed constant in the runtime
)

// effectiveValue is one field's answer plus where it came from.
type effectiveValue struct {
	Value  any    `json:"value"`
	Source string `json:"source"`
}

// runFieldReaders answers "did the RUN set this" for each overridable field,
// keyed by the same Go field name agentDefOverridability uses.
//
// It cannot be reflection like the definition side: the run's record is not
// shaped like an AgentDef — its overrides live in Routing / Resources / Tuning
// sub-records, and several are pointers whose nil-ness IS the answer.
//
// Every entry that agentDefOverridability marks overridable must appear here.
// TestEffectiveConfig_EveryOverridableFieldIsReported enforces that, so a new
// override cannot ship and quietly go unreported.
var runFieldReaders = map[string]func(runConfigRecord) (any, bool){
	"Model": func(r runConfigRecord) (any, bool) {
		return pick(r.Routing != nil && r.Routing.Model != "", func() any { return r.Routing.Model })
	},
	"Provider": func(r runConfigRecord) (any, bool) {
		return pick(r.Routing != nil && r.Routing.Provider != "", func() any { return r.Routing.Provider })
	},
	"Tier": func(r runConfigRecord) (any, bool) {
		return pick(r.Routing != nil && r.Routing.Tier != "", func() any { return r.Routing.Tier })
	},
	"Effort": func(r runConfigRecord) (any, bool) {
		return pick(r.Routing != nil && r.Routing.Effort != "", func() any { return r.Routing.Effort })
	},

	"MaxTokens": func(r runConfigRecord) (any, bool) {
		return pick(r.Resources != nil && r.Resources.MaxTokens != 0, func() any { return r.Resources.MaxTokens })
	},
	"MaxIterations": func(r runConfigRecord) (any, bool) {
		return pick(r.Resources != nil && r.Resources.MaxIterations != 0, func() any { return r.Resources.MaxIterations })
	},
	"MaxConcurrentChildren": func(r runConfigRecord) (any, bool) {
		return pick(r.Resources != nil && r.Resources.MaxConcurrentChildren != 0, func() any { return r.Resources.MaxConcurrentChildren })
	},
	"UnboundedIterations": func(r runConfigRecord) (any, bool) {
		return pick(r.Resources != nil && r.Resources.UnboundedIterations != nil, func() any { return *r.Resources.UnboundedIterations })
	},

	"RetryAttempts": func(r runConfigRecord) (any, bool) {
		return pick(r.Tuning != nil && r.Tuning.RetryAttempts != nil, func() any { return *r.Tuning.RetryAttempts })
	},
	"MemoryInjectMaxTokens": func(r runConfigRecord) (any, bool) {
		return pick(r.Tuning != nil && r.Tuning.MemoryInjectMaxTokens != nil, func() any { return *r.Tuning.MemoryInjectMaxTokens })
	},
	"MemoryIndexMaxBytes": func(r runConfigRecord) (any, bool) {
		return pick(r.Tuning != nil && r.Tuning.MemoryIndexMaxBytes != nil, func() any { return *r.Tuning.MemoryIndexMaxBytes })
	},
	"InjectToolGuide": func(r runConfigRecord) (any, bool) {
		return pick(r.Tuning != nil && r.Tuning.InjectToolGuide != nil, func() any { return *r.Tuning.InjectToolGuide })
	},

	"Sampling": func(r runConfigRecord) (any, bool) { return pick(r.Sampling != nil, func() any { return r.Sampling }) },
	"Compaction": func(r runConfigRecord) (any, bool) {
		return pick(r.Compaction != nil, func() any { return r.Compaction })
	},
	"Context": func(r runConfigRecord) (any, bool) { return pick(r.Context != nil, func() any { return r.Context }) },
	"MaxContextTokens": func(r runConfigRecord) (any, bool) {
		return pick(r.MaxContextTokens != 0, func() any { return r.MaxContextTokens })
	},
	"RunTimeoutSeconds": func(r runConfigRecord) (any, bool) {
		return pick(r.RunTimeoutSeconds != 0, func() any { return r.RunTimeoutSeconds })
	},
	"Interactive": func(r runConfigRecord) (any, bool) {
		return pick(r.Interactive != nil, func() any { return *r.Interactive })
	},
	"Interruption": func(r runConfigRecord) (any, bool) {
		return pick(r.Interruption != nil, func() any { return r.Interruption })
	},

	// Tools is narrowing-only and lives on the run's identity rather than its
	// config record, so the run never reports it here; the definition does.
	"Tools": func(runConfigRecord) (any, bool) { return nil, false },
}

// pick evaluates the value ONLY when it is set, so a reader can dereference a
// pointer in its closure without each entry repeating the nil guard.
func pick(set bool, val func() any) (any, bool) {
	if !set {
		return nil, false
	}
	return val(), true
}

// fallbacks answers "and if NEITHER set it?" — the layer the report exists for.
// A field missing here reports source "resolved" with a null value, which is the
// honest answer for the ones only the driver or the model can settle.
var fallbacks = map[string]func(effectiveCtx) (any, string){
	"MaxIterations":         func(effectiveCtx) (any, string) { return loop.DefaultMaxIterations, sourceDefault },
	"MaxConcurrentChildren": func(effectiveCtx) (any, string) { return builtin.DefaultMaxConcurrentChildren, sourceDefault },
	"MemoryInjectMaxTokens": func(effectiveCtx) (any, string) { return config.DefaultMemoryInjectMaxTokens, sourceDefault },
	"MemoryIndexMaxBytes":   func(effectiveCtx) (any, string) { return config.DefaultMemoryIndexMaxBytes, sourceDefault },
	"InjectToolGuide":       func(effectiveCtx) (any, string) { return false, sourceDefault },
	"UnboundedIterations":   func(effectiveCtx) (any, string) { return false, sourceDefault },
	"Interactive":           func(c effectiveCtx) (any, string) { return c.startedInteractive, sourceRun },
	"Effort":                func(effectiveCtx) (any, string) { return "", sourceDefault },

	"RetryAttempts":     func(c effectiveCtx) (any, string) { return c.retryAttempts, sourceUserTier },
	"RunTimeoutSeconds": func(effectiveCtx) (any, string) { return 0, sourceDefault },

	// Settled at runtime. Model and Provider are the post-cascade answer, which
	// is the only one worth reporting — a tier tells you where it will look, not
	// what it found.
	"Model":    func(c effectiveCtx) (any, string) { return c.resolvedModel, sourceResolved },
	"Provider": func(c effectiveCtx) (any, string) { return c.resolvedProvider, sourceResolved },
}

// effectiveCtx carries what only the server can answer.
type effectiveCtx struct {
	resolvedModel      string
	resolvedProvider   string
	retryAttempts      int
	startedInteractive bool
}

// handleEffectiveConfig serves GET /v1/runs/{run_id}/effective-config — for
// every overridable field, the value this run will actually use and WHERE it
// came from.
//
// WHY THIS NEEDED NEW CODE. `effectiveDef` is definition-plus-run-overrides and
// nothing else: no tiers, no driver defaults, no operator configuration. A field
// neither layer set comes out of it as a zero and is settled later somewhere
// else — in the loop, in the resolver, in a driver. Four layers, assembled
// nowhere, so nothing could answer "what will this run actually do".
//
// The field list is DERIVED from agentDefOverridability, which already has a
// completeness test. A new overridable field therefore fails the build here
// rather than quietly going unreported — the failure mode this report exists to
// remove would otherwise reappear inside it.
func (s *Server) handleEffectiveConfig(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if !validIdent(runID) {
		http.Error(w, "run_id must match [A-Za-z0-9_-]{1,128}", http.StatusBadRequest)
		return
	}
	run, err := s.runForSteer(r.Context(), runID)
	if err != nil {
		http.Error(w, "no in-flight run for that run_id", http.StatusNotFound)
		return
	}
	agentDef, found := s.lookupAgent(r.Context(), run.TenantID, run.Agent)
	if !found {
		http.Error(w, "the run's agent definition is no longer available", http.StatusNotFound)
		return
	}
	rec, _ := decodeRunConfig(run.RunConfig)

	eff, err := s.effectiveDef(r.Context(), agentDef, runOverrides{
		Routing: rec.Routing, Resources: rec.Resources, Tuning: rec.Tuning,
	})
	if err != nil {
		// A stored record the definition can no longer satisfy is worth saying
		// out loud rather than reporting a configuration nobody can run.
		writeResolveError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"run_id": runID,
		"agent":  run.Agent,
		"fields": s.effectiveFields(r.Context(), run, agentDef, eff, rec),
	})
}

// effectiveFields is the assembly itself: run override, else definition, else
// the fallback layer — reporting the source at each step.
func (s *Server) effectiveFields(ctx context.Context, run store.Run, def, eff config.AgentDef, rec runConfigRecord) map[string]effectiveValue {
	ec := s.effectiveCtxFor(ctx, run, eff)
	out := make(map[string]effectiveValue, len(agentDefOverridability))

	names := make([]string, 0, len(agentDefOverridability))
	for name, kind := range agentDefOverridability {
		if kind != notOverridable {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	for _, goName := range names {
		wire := wireNameFor(goName)
		if read, ok := runFieldReaders[goName]; ok {
			if v, set := read(rec); set {
				out[wire] = effectiveValue{Value: v, Source: sourceRun}
				continue
			}
		}
		if v, set := reflectField(def, goName); set {
			out[wire] = effectiveValue{Value: v, Source: sourceDefinition}
			continue
		}
		if fb, ok := fallbacks[goName]; ok {
			v, src := fb(ec)
			out[wire] = effectiveValue{Value: v, Source: src}
			continue
		}
		// No fallback declared: the value is settled by a driver or the model
		// itself and is not knowable here. Saying "resolved, unknown" is more
		// useful than omitting the field, which reads as "does not exist".
		out[wire] = effectiveValue{Value: nil, Source: sourceResolved}
	}
	return out
}

// reflectField reads the definition's value by the Go field name the
// classification table uses, so the two cannot disagree about what a field is
// called. A zero value means the definition did not set it.
func reflectField(def config.AgentDef, goName string) (any, bool) {
	v := reflect.ValueOf(def).FieldByName(goName)
	if !v.IsValid() || v.IsZero() {
		return nil, false
	}
	return v.Interface(), true
}

// wireNameFor maps a Go field name to the snake_case key every other surface
// uses. Derived from the definition's own json tag rather than a second table:
// a name that differs from what the def serialises would make the report
// unjoinable with /v1/_library/agents, which is the whole point of reading it.
func wireNameFor(goName string) string {
	f, ok := reflect.TypeOf(config.AgentDef{}).FieldByName(goName)
	if !ok {
		return strings.ToLower(goName)
	}
	if tag := f.Tag.Get("json"); tag != "" && tag != "-" {
		if name := strings.Split(tag, ",")[0]; name != "" {
			return name
		}
	}
	if tag := f.Tag.Get("yaml"); tag != "" && tag != "-" {
		if name := strings.Split(tag, ",")[0]; name != "" {
			return name
		}
	}
	return strings.ToLower(goName)
}

// effectiveCtxFor gathers what only the server can answer: the post-cascade
// routing, and the two operator-configured values published on no endpoint.
//
// A resolution failure is not fatal here. This is a REPORT — answering "we could
// not resolve the model right now" for one field is far better than failing the
// whole request and telling a reader nothing about the other eighteen.
func (s *Server) effectiveCtxFor(ctx context.Context, run store.Run, eff config.AgentDef) effectiveCtx {
	out := effectiveCtx{
		// retry_attempts falls back to the user TIER's policy, which is operator
		// configuration published on no endpoint — one of the three values a
		// client genuinely could not know.
		retryAttempts:      s.retryAttemptsForAgent(eff, run.UserTier),
		startedInteractive: run.Interactive,
	}
	providerID, model, _, err := s.resolveAgentDef(ctx, eff, run.TenantID, run.UserID, run.Agent, run.UserTier, run.OperatorKeyRestricted)
	if err == nil {
		out.resolvedProvider, out.resolvedModel = providerID, model
	}
	return out
}
