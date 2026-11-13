// Package tenanttelemetry lets a tenant export its own traces, and its own
// series of the runtime's metrics, to an OTLP collector of its own. It owns the per-tenant policy (where, with which
// header credential, which signals): it checks a write against the operator's
// rules, keeps the tracer's TenantRouter in step with the stored rows on every
// replica, and answers the status and the test.
//
// The router (internal/otel) decides nothing about who may export or where
// to; everything it is told comes from here, after these checks.
package tenanttelemetry

import (
	"fmt"
	"strconv"
	"strings"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
)

// Config is the operator's settings for tenant export.
type Config struct {
	// Enabled is LOOMCYCLE_TENANT_TELEMETRY: off, there are no routes, no
	// router and no change to tracing at all.
	Enabled bool
	// AllowHTTP is LOOMCYCLE_TENANT_TELEMETRY_ALLOW_HTTP: an http:// endpoint
	// is accepted (a lab without TLS). Off, only https://.
	AllowHTTP bool
	// AllowedHosts is observability.tenant_telemetry.allowed_hosts: when set, an
	// endpoint's host must be one of them, exactly or under a "*.suffix"
	// entry. Empty allows any host the dial-time guard lets through.
	AllowedHosts []string
	// MaxRatio is LOOMCYCLE_TENANT_TELEMETRY_MAX_RATIO, the highest sampler
	// ratio a tenant may ask for (0..1, default 1).
	MaxRatio float64
	// QueueSize is each tenant pipeline's queue (512): past it spans are
	// dropped and counted, never waited on.
	QueueSize int
	// MaxPipelines is the tenant pipelines open at once (256): past it the
	// least recently used is flushed and closed.
	MaxPipelines int
	// ContentCapture is LOOMCYCLE_TENANT_CONTENT_CAPTURE: a tenant may turn
	// content capture on (its runs' prompts, answers and tool calls on the
	// spans it receives). Off, a policy asking for it is refused, and one
	// stored with it on captures nothing.
	ContentCapture bool
}

// ConfigFromEnv reads the operator's settings through getenv; allowedHosts is
// the yaml's observability.tenant_telemetry.allowed_hosts, checked and
// lower-cased by the config load. With the gate off nothing else is read, so
// a stray value cannot stop a boot that does not use it.
func ConfigFromEnv(getenv func(string) string, allowedHosts []string) (Config, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	cfg := Config{MaxRatio: 1, QueueSize: lcotel.DefaultTenantQueueSize, MaxPipelines: lcotel.DefaultTenantMaxPipelines}
	on, err := envBool(get, "LOOMCYCLE_TENANT_TELEMETRY")
	if err != nil || !on {
		return cfg, err
	}
	cfg.Enabled = true
	if cfg.AllowHTTP, err = envBool(get, "LOOMCYCLE_TENANT_TELEMETRY_ALLOW_HTTP"); err != nil {
		return Config{}, err
	}
	if cfg.ContentCapture, err = envBool(get, "LOOMCYCLE_TENANT_CONTENT_CAPTURE"); err != nil {
		return Config{}, err
	}
	cfg.AllowedHosts = allowedHosts
	if v := get("LOOMCYCLE_TENANT_TELEMETRY_MAX_RATIO"); v != "" {
		r, err := strconv.ParseFloat(v, 64)
		if err != nil || !(r >= 0 && r <= 1) {
			return Config{}, fmt.Errorf("LOOMCYCLE_TENANT_TELEMETRY_MAX_RATIO=%q: want a number between 0 and 1", v)
		}
		cfg.MaxRatio = r
	}
	return cfg, nil
}

func envBool(get func(string) string, k string) (bool, error) {
	v := get(k)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s=%q: want 1 or 0", k, v)
	}
	return b, nil
}

// hostAllowed reports whether host is admitted by the operator's list: empty
// admits every host; otherwise an exact entry or a "*.suffix" entry under
// which host lies (not the suffix itself).
func hostAllowed(allowed []string, host string) bool {
	if len(allowed) == 0 {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range allowed {
		if suffix, ok := strings.CutPrefix(a, "*"); ok {
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
			continue
		}
		if host == a {
			return true
		}
	}
	return false
}
