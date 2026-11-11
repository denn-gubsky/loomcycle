package http

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/config"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/redact"
)

// withContentCapture decides, at a run's start, whether its spans carry its
// content for its tenant's own telemetry destination (RFC GG P3), and sets
// ctx to match: on only when the tenant's policy captures content on this
// replica (the operator's gate and the tenant's choice, ContentCaptureFor),
// the agent has not opted out (telemetry.content_capture: never), the run is
// not no-trace, and the server has a redactor to pass every value through.
// Otherwise it CLEARS any capture on ctx: a sub-run decides for its own agent
// and tenant, and must not inherit its parent's.
//
// A run decides once, at its start: a tenant that turns capture off stops it
// for runs that start afterwards, not for one already running.
func (s *Server) withContentCapture(ctx context.Context, tenant string, def config.AgentDef, noTrace bool) context.Context {
	m, r := s.tenantTelemetry, s.redactor
	// With redaction switched off (LOOMCYCLE_REDACT_SECRETS=0) there is no
	// way to keep a secret out of what leaves, so nothing is captured.
	if m == nil || r == nil || tenant == "" || noTrace || def.Telemetry.NeverCapturesContent() {
		return lcotel.WithContentCapture(ctx, nil)
	}
	n, ok := m.ContentCaptureFor(ctx, tenant)
	if !ok {
		return lcotel.WithContentCapture(ctx, nil)
	}
	return lcotel.WithContentCapture(ctx, &lcotel.ContentCapture{
		MaxChars:   n,
		Redact:     r.String,
		RedactArgs: func(tool string, args []byte) []byte { return redactToolArgs(r, tool, args) },
	})
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
