// Package tenanttelemetry lets a tenant export its own traces to an OTLP
// collector of its own. It owns the per-tenant policy (where, with which
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
	// AllowedHosts is LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS: when set, an
	// endpoint's host must be one of them, exactly or under a "*.suffix"
	// entry. Empty allows any host the dial-time guard lets through.
	AllowedHosts []string
	// MaxRatio is LOOMCYCLE_TENANT_TELEMETRY_MAX_RATIO, the highest sampler
	// ratio a tenant may ask for (0..1, default 1).
	MaxRatio float64
	// QueueSize is LOOMCYCLE_TENANT_TELEMETRY_QUEUE, each tenant pipeline's
	// queue (default 512).
	QueueSize int
	// MaxPipelines is LOOMCYCLE_TENANT_TELEMETRY_MAX_PIPELINES, the tenant
	// pipelines open at once (default 256).
	MaxPipelines int
	// ContentCapture is LOOMCYCLE_TENANT_CONTENT_CAPTURE: a tenant may turn
	// content capture on (its runs' prompts, answers and tool calls on the
	// spans it receives). Off, a policy asking for it is refused, and one
	// stored with it on captures nothing.
	ContentCapture bool
}

// maxQueueSize and maxPipelinesCap bound the operator's own settings: each
// pipeline's queue is memory per tenant, and each pipeline a goroutine.
const (
	maxQueueSize    = 65536
	maxPipelinesCap = 4096
)

// ConfigFromEnv reads the operator's settings through getenv. With the gate
// off nothing else is read, so a stray value cannot stop a boot that does not
// use it.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
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
	for _, h := range strings.Split(get("LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS"), ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if strings.Contains(h, "*") && (!strings.HasPrefix(h, "*.") || strings.Count(h, "*") > 1 || len(h) < 3) {
			return Config{}, fmt.Errorf("LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS: %q: an entry is a host name or *.suffix", h)
		}
		cfg.AllowedHosts = append(cfg.AllowedHosts, h)
	}
	if v := get("LOOMCYCLE_TENANT_TELEMETRY_MAX_RATIO"); v != "" {
		r, err := strconv.ParseFloat(v, 64)
		if err != nil || !(r >= 0 && r <= 1) {
			return Config{}, fmt.Errorf("LOOMCYCLE_TENANT_TELEMETRY_MAX_RATIO=%q: want a number between 0 and 1", v)
		}
		cfg.MaxRatio = r
	}
	if cfg.QueueSize, err = envInt(get, "LOOMCYCLE_TENANT_TELEMETRY_QUEUE", cfg.QueueSize, maxQueueSize); err != nil {
		return Config{}, err
	}
	if cfg.MaxPipelines, err = envInt(get, "LOOMCYCLE_TENANT_TELEMETRY_MAX_PIPELINES", cfg.MaxPipelines, maxPipelinesCap); err != nil {
		return Config{}, err
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

func envInt(get func(string) string, k string, def, maxV int) (int, error) {
	v := get(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxV {
		return 0, fmt.Errorf("%s=%q: want a whole number from 1 to %d", k, v, maxV)
	}
	return n, nil
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
