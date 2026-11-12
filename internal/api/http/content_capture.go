package http

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/config"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"go.opentelemetry.io/otel/trace"
)

// startRunSpan opens a run's loomcycle.run span (lcotel.RecordRunStart) and
// decides, for the run of def, whether its spans carry its content
// (withContentCapture), on the tenant and no-trace mode the span resolved —
// a sub-run's inherited from its parent's ctx included. Every run start goes
// through it, so none can open a run span and skip the decision.
func (s *Server) startRunSpan(ctx context.Context, def config.AgentDef, a lcotel.RunStartAttrs) (context.Context, trace.Span) {
	ctx, span := lcotel.RecordRunStart(ctx, a)
	return s.withContentCapture(ctx, lcotel.TenantFromContext(ctx), def, lcotel.NoTraceFromContext(ctx)), span
}

// withContentCapture decides, at a run's start, whether its spans carry its
// content for its tenant's own telemetry destination (RFC GG P3), and sets
// ctx to match: on only when the tenant's policy captures content on this
// replica (the operator's gate and the tenant's choice, ContentCaptureFor),
// the agent has not opted out (telemetry.content_capture: never), nor has any
// run it was spawned under, the run is not no-trace, and the server has a
// redactor to pass every value through. Otherwise it CLEARS any capture on
// ctx: a sub-run decides for its own agent and tenant, and must not inherit
// its parent's capture (only its parent's never).
//
// A run decides at its start whether it may capture; while it runs, each
// recording also asks whether the tenant still captures (Active), so a tenant
// that turns capture off stops it for a run already running too.
func (s *Server) withContentCapture(ctx context.Context, tenant string, def config.AgentDef, noTrace bool) context.Context {
	m, r := s.tenantTelemetry, s.redactor
	// An agent that never captures takes its whole spawn tree with it: a
	// sub-run's input is the spawning run's text, so a child that captured
	// would record what its never parent wrote.
	never := def.Telemetry.NeverCapturesContent() || neverCapturesContent(ctx)
	if never {
		ctx = context.WithValue(ctx, ctxKeyNeverCaptureContent{}, true)
		// Its answer is its content too. A sub-run starts under the tool
		// call that spawned it (ctx still holds that call's mark): the call's
		// span, and its parent's later model input, record a marker for it.
		tools.MarkContentNotCaptured(ctx)
	}
	// With redaction switched off (LOOMCYCLE_REDACT_SECRETS=0) there is no
	// way to keep a secret out of what leaves, so nothing is captured.
	if m == nil || r == nil || tenant == "" || noTrace || never {
		return lcotel.WithContentCapture(ctx, nil)
	}
	n, ok := m.ContentCaptureFor(ctx, tenant)
	if !ok {
		return lcotel.WithContentCapture(ctx, nil)
	}
	return lcotel.WithContentCapture(ctx, &lcotel.ContentCapture{
		Tenant:     tenant,
		MaxChars:   n,
		Redact:     r.String,
		RedactArgs: func(tool string, args []byte) []byte { return redactToolArgs(r, tool, args) },
		Active:     func() bool { return m.ContentCaptureActive(tenant) },
	})
}

// ctxKeyNeverCaptureContent marks a run, and with it every run spawned under
// it, whose agent (or an ancestor's) says telemetry.content_capture: never.
type ctxKeyNeverCaptureContent struct{}

// neverCapturesContent reports whether ctx belongs to a run under an agent
// that never captures content.
func neverCapturesContent(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeyNeverCaptureContent{}).(bool)
	return v
}

// notCapturedMarkKey is the key a stored tool_result carries when no span may
// capture its text (providers.Event.ContentNotCaptured, which is never
// serialised): the stored event is otherwise as it was emitted.
const notCapturedMarkKey = "content_not_captured"

// withNotCapturedMark is payload, a stored event's JSON object, with the
// not-captured mark set. A payload that is not an object is returned as it
// is (an event always marshals to one).
func withNotCapturedMark(payload []byte) []byte {
	var obj map[string]json.RawMessage
	if json.Unmarshal(payload, &obj) != nil || obj == nil {
		return payload
	}
	obj[notCapturedMarkKey] = json.RawMessage("true")
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// storedNotCaptured reports whether a stored event carries the not-captured
// mark.
func storedNotCaptured(payload []byte) bool {
	var m struct {
		NotCaptured bool `json:"content_not_captured"`
	}
	return json.Unmarshal(payload, &m) == nil && m.NotCaptured
}

// withResumedNeverAncestry gives a resumed sub-run the never its live run
// inherited from an ancestor (withContentCapture): a resume starts from a
// fresh ctx, so the chain is read back from the run rows instead. Whenever
// it cannot be read to its root — a missing, looping, too-deep or
// other-tenant ancestor, a definition that cannot be resolved, a sub-run row
// that names no parent run — the run is held to never too. Read only when
// content capture could apply at all.
func (s *Server) withResumedNeverAncestry(ctx context.Context, run store.Run) context.Context {
	if s.tenantTelemetry == nil || !isSubRun(run) {
		return ctx
	}
	if run.ParentRunID == "" || s.resumedAncestorNeverCaptures(ctx, run) {
		return context.WithValue(ctx, ctxKeyNeverCaptureContent{}, true)
	}
	return ctx
}

// resumedAncestorNeverCaptures walks run's parents (as resumedRootRunID does)
// and reports whether any one's agent never captures content, or the walk
// failed.
func (s *Server) resumedAncestorNeverCaptures(ctx context.Context, run store.Run) bool {
	cur, seen := run, map[string]bool{run.ID: true}
	for hops := 0; cur.ParentRunID != ""; hops++ {
		id := cur.ParentRunID
		if hops == maxResumeAncestry || seen[id] {
			return true
		}
		seen[id] = true
		parent, err := s.store.GetRun(ctx, id)
		if err != nil || parent.ID != id || parent.TenantID != run.TenantID {
			return true
		}
		rec, _ := decodeRunConfig(parent.RunConfig)
		def, _, err := s.resumedAgentDef(ctx, parent, rec.AgentVersion)
		if err != nil || def.Telemetry.NeverCapturesContent() {
			return true
		}
		cur = parent
	}
	return false
}

// redactToolArgs masks a tool call's arguments as the stored transcript masks
// them: the credential and crypto tools' secret fields by name, then every
// string leaf through the redactor. Arguments that are not JSON are masked as
// text (Bytes would hand them back unchanged).
func redactToolArgs(r *redact.Redactor, tool string, args []byte) []byte {
	masked := maskCredentialCreateValue(tool, args)
	if json.Valid(masked) {
		return r.Bytes(masked)
	}
	return []byte(r.String(string(masked)))
}
