package http

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// lockableOverrideFields maps each per-run override a token can be locked out
// of (auth.LockableOverrides, by wire name) to the Go field that carries it.
// The field has the same name on every request shape — the HTTP run and
// message bodies, runner.RunInput, a configured run's draft — so one map reads
// them all. TestOverrideLock_EveryRequestFieldIsClassified holds this equal to
// auth's list and makes each new request field be classified.
var lockableOverrideFields = map[string]string{
	"model": "Model", "provider": "Provider", "tier": "Tier", "effort": "Effort", "user_tier": "UserTier",
	"max_tokens": "MaxTokens", "max_iterations": "MaxIterations",
	"unbounded_iterations": "UnboundedIterations", "max_concurrent_children": "MaxConcurrentChildren",
	"run_timeout_seconds": "RunTimeoutSeconds",
	"sampling":            "Sampling", "compaction": "Compaction", "context": "Context",
	"max_context_tokens": "MaxContextTokens", "tool_choice": "ToolChoice", "output_format": "OutputFormat",
	"web_search_filter": "WebSearchFilter", "retry_attempts": "RetryAttempts",
	"memory_inject_max_tokens": "MemoryInjectMaxTokens", "memory_index_max_bytes": "MemoryIndexMaxBytes",
	"inject_tool_guide": "InjectToolGuide",
	"interactive":       "Interactive", "review": "Review", "review_ttl_seconds": "ReviewTTLSeconds",
	"interruption": "Interruption", "hooks": "Hooks", "tool_hooks": "ToolHooks",
}

// overridesSetIn returns the lockable overrides a request sets, by wire name,
// sorted. req is a request struct (or a pointer to one); a lockable field it
// does not have is simply not set on it. Embedded structs are read through, so
// a draft's promoted fields count.
func overridesSetIn(req any) []string {
	v := reflect.ValueOf(req)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	var set []string
	for wire, name := range lockableOverrideFields {
		if f := v.FieldByName(name); f.IsValid() && !f.IsZero() {
			set = append(set, wire)
		}
	}
	sort.Strings(set)
	return set
}

// admitPrincipalOverrides refuses a request that sets a per-run override the
// caller's token is locked out of. set is the overrides the request carries
// (overridesSetIn, or a retune's own list).
//
// It is called wherever a caller can set one: each run intake, a draft's
// create and edit, and a retune. Like the agent allowlist it reads the live
// principal, so a run started under a definition's own identity is not gated.
//
// Refused, never dropped. A dropped override would let a client that asked for
// another model believe it got one; a refusal shows up in its logs.
func (s *Server) admitPrincipalOverrides(ctx context.Context, set []string) error {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.Restrictions.Overrides == nil {
		return nil
	}
	for _, field := range set {
		if !p.Restrictions.OverrideAllowed(field) {
			log.Printf("auth: override_not_allowed field=%q subject=%q token_def=%q", field, p.Subject, p.TokenDefID)
			return fmt.Errorf("%w: %q", runner.ErrOverrideNotAllowed, field)
		}
	}
	return nil
}

// writeOverrideNotAllowed is the HTTP form of that refusal.
func writeOverrideNotAllowed(w http.ResponseWriter, err error) {
	writeJSONError(w, http.StatusForbidden, "override_not_allowed", err.Error())
}
