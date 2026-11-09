package otel

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Protocol is the OTLP wire protocol traces leave in.
type Protocol string

const (
	ProtocolHTTPProtobuf Protocol = "http/protobuf"
	ProtocolGRPC         Protocol = "grpc"
)

// InboundMode is what a run does with a caller's trace context.
type InboundMode string

const (
	// InboundParent makes the caller's span the parent of the run span.
	InboundParent InboundMode = "parent"
	// InboundLink starts the run as a new trace root that links to the
	// caller's span, so the caller cannot steer loomcycle's sampling.
	InboundLink InboundMode = "link"
	// InboundIgnore never reads the caller's trace context.
	InboundIgnore InboundMode = "ignore"
)

// The batch-processor defaults applied when OTEL_BSP_* is unset. The SDK's own
// export timeout default is 30 s; a shorter one bounds how long a dead
// collector holds the single export goroutine before the queue starts
// dropping, which is the behaviour an operator wants from telemetry.
const (
	defaultMaxQueueSize       = 2048
	defaultMaxExportBatchSize = 512
	defaultExportTimeout      = 10 * time.Second
	defaultScheduleDelay      = 5 * time.Second
)

// Config is the tracing configuration: what ResolveConfig read from the
// environment, plus the facts only the binary knows (ServiceVersion,
// InstanceID, UserHashKey), which the caller fills before Init.
//
// The exporter's endpoint, headers, timeout, compression, TLS and mTLS
// settings are deliberately absent: the OTLP exporter reads
// OTEL_EXPORTER_OTLP[_TRACES]_* itself, and any option loomcycle passed would
// override the operator's environment.
type Config struct {
	// Enabled: an OTLP endpoint is set and nothing turned tracing off.
	Enabled  bool
	Protocol Protocol
	// SamplerName / SamplerArg are OTEL_TRACES_SAMPLER and its argument.
	SamplerName string
	SamplerArg  float64
	// HeadersFile is LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE: collector
	// headers kept out of the process environment. Read by Init.
	HeadersFile string
	// Inbound is LOOMCYCLE_OTEL_INBOUND_CONTEXT.
	Inbound InboundMode
	// Propagators is OTEL_PROPAGATORS, parsed: "tracecontext", "baggage";
	// empty for "none".
	Propagators []string
	// Environment is LOOMCYCLE_ENVIRONMENT, the deployment.environment.name
	// resource attribute; omitted when empty.
	Environment string
	// RawUserID is LOOMCYCLE_OTEL_RAW_USER_ID: spans carry the raw
	// loomcycle.user_id beside loomcycle.user_hash.
	RawUserID bool
	// TenantSamplerRatios is the per-tenant ratio for a run that starts a
	// trace. ResolveConfig fills it from LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS;
	// the binary lays it over the yaml's observability.tracing
	// tenant_sampler_ratios (MergeTenantSamplerRatios) before Init.
	TenantSamplerRatios map[string]float64

	// The batch span processor's bounds (OTEL_BSP_* or the defaults above).
	MaxQueueSize       int
	MaxExportBatchSize int
	ExportTimeout      time.Duration
	ScheduleDelay      time.Duration

	// ServiceVersion is the binary's version (service.version).
	ServiceVersion string
	// InstanceID is the replica id /metrics labels its series with
	// (service.instance.id); omitted when empty.
	InstanceID string
	// UserHashKey keys loomcycle.user_hash, derived from LOOMCYCLE_SECRET_KEY
	// by the binary. Nil (no secret key) emits the raw user id instead.
	UserHashKey []byte
}

// ResolveConfig reads and validates the tracing environment through getenv
// (os.Getenv in production). A value loomcycle cannot honour is an error, so a
// typo stops the boot instead of silently tracing differently than the
// operator asked.
func ResolveConfig(getenv func(string) string) (Config, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	cfg := Config{Protocol: ProtocolHTTPProtobuf, SamplerName: "parentbased_always_on", SamplerArg: 1, Inbound: InboundParent}

	exporter := strings.ToLower(get("OTEL_TRACES_EXPORTER"))
	switch exporter {
	case "", "otlp", "none":
	default:
		return Config{}, fmt.Errorf("OTEL_TRACES_EXPORTER=%q: loomcycle supports otlp or none", exporter)
	}
	endpointSet := get("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
	cfg.Enabled = endpointSet && exporter != "none" && !strings.EqualFold(get("OTEL_SDK_DISABLED"), "true")

	proto := get("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
	protoVar := "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
	if proto == "" {
		proto, protoVar = get("OTEL_EXPORTER_OTLP_PROTOCOL"), "OTEL_EXPORTER_OTLP_PROTOCOL"
	}
	switch Protocol(proto) {
	case "", ProtocolHTTPProtobuf:
	case ProtocolGRPC:
		cfg.Protocol = ProtocolGRPC
	default:
		return Config{}, fmt.Errorf("%s=%q: loomcycle supports http/protobuf or grpc", protoVar, proto)
	}

	if v := get("OTEL_TRACES_SAMPLER"); v != "" {
		cfg.SamplerName = v
	}
	if arg := get("OTEL_TRACES_SAMPLER_ARG"); arg != "" {
		f, err := strconv.ParseFloat(arg, 64)
		if err != nil || f < 0 || f > 1 {
			return Config{}, fmt.Errorf("OTEL_TRACES_SAMPLER_ARG=%q: want a number from 0 to 1", arg)
		}
		cfg.SamplerArg = f
	}
	if _, err := Sampler(cfg.SamplerName, cfg.SamplerArg); err != nil {
		return Config{}, err
	}

	cfg.HeadersFile = get("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE")
	if cfg.HeadersFile != "" {
		for _, k := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS"} {
			if get(k) != "" {
				return Config{}, fmt.Errorf("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE and %s are both set: keep the collector headers in one place", k)
			}
		}
	}

	switch m := InboundMode(get("LOOMCYCLE_OTEL_INBOUND_CONTEXT")); m {
	case "":
	case InboundParent, InboundLink, InboundIgnore:
		cfg.Inbound = m
	default:
		return Config{}, fmt.Errorf("LOOMCYCLE_OTEL_INBOUND_CONTEXT=%q: want parent, link or ignore", m)
	}

	props, err := parsePropagators(get("OTEL_PROPAGATORS"))
	if err != nil {
		return Config{}, err
	}
	cfg.Propagators = props
	cfg.Environment = get("LOOMCYCLE_ENVIRONMENT")
	if v := get("LOOMCYCLE_OTEL_RAW_USER_ID"); v != "" {
		raw, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("LOOMCYCLE_OTEL_RAW_USER_ID=%q: want 1 or 0", v)
		}
		cfg.RawUserID = raw
	}
	if v := get("LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS"); v != "" {
		if cfg.TenantSamplerRatios, err = ParseTenantSamplerRatios(v); err != nil {
			return Config{}, err
		}
	}

	if cfg.MaxQueueSize, err = envInt(get, "OTEL_BSP_MAX_QUEUE_SIZE", defaultMaxQueueSize); err != nil {
		return Config{}, err
	}
	if cfg.MaxExportBatchSize, err = envInt(get, "OTEL_BSP_MAX_EXPORT_BATCH_SIZE", defaultMaxExportBatchSize); err != nil {
		return Config{}, err
	}
	// The SDK caps a batch at the queue; so does loomcycle, rather than fail.
	cfg.MaxExportBatchSize = min(cfg.MaxExportBatchSize, cfg.MaxQueueSize)
	if cfg.ExportTimeout, err = envMillis(get, "OTEL_BSP_EXPORT_TIMEOUT", defaultExportTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ScheduleDelay, err = envMillis(get, "OTEL_BSP_SCHEDULE_DELAY", defaultScheduleDelay); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Sampler builds the head sampler OTEL_TRACES_SAMPLER names. arg is the ratio
// for the two ratio samplers and ignored by the others.
func Sampler(name string, arg float64) (sdktrace.Sampler, error) {
	switch name {
	case "always_on":
		return sdktrace.AlwaysSample(), nil
	case "always_off":
		return sdktrace.NeverSample(), nil
	case "traceidratio":
		return sdktrace.TraceIDRatioBased(arg), nil
	case "parentbased_always_on":
		return sdktrace.ParentBased(sdktrace.AlwaysSample()), nil
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample()), nil
	case "parentbased_traceidratio":
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(arg)), nil
	}
	return nil, fmt.Errorf("OTEL_TRACES_SAMPLER=%q: want always_on, always_off, traceidratio, parentbased_always_on, parentbased_always_off or parentbased_traceidratio", name)
}

func parsePropagators(v string) ([]string, error) {
	if v == "" {
		return []string{"tracecontext", "baggage"}, nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		switch p = strings.TrimSpace(p); p {
		case "tracecontext", "baggage":
			out = append(out, p)
		case "none":
			// "none" alone turns propagation off; next to another name it
			// is meaningless, so it is refused rather than guessed at.
			if len(strings.Split(v, ",")) > 1 {
				return nil, fmt.Errorf("OTEL_PROPAGATORS=%q: none cannot be combined with another propagator", v)
			}
			return nil, nil
		default:
			return nil, fmt.Errorf("OTEL_PROPAGATORS=%q: loomcycle supports tracecontext, baggage or none", v)
		}
	}
	return out, nil
}

func envInt(get func(string) string, key string, def int) (int, error) {
	v := get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s=%q: want a positive integer", key, v)
	}
	return n, nil
}

func envMillis(get func(string) string, key string, def time.Duration) (time.Duration, error) {
	n, err := envInt(get, key, int(def/time.Millisecond))
	if err != nil {
		return 0, fmt.Errorf("%w (milliseconds)", err)
	}
	return time.Duration(n) * time.Millisecond, nil
}

// ReadHeadersFile reads collector headers from path: `k=v` pairs separated by
// commas or newlines, blank and `#` lines ignored, values URL-decoded as the
// OTEL_EXPORTER_OTLP_HEADERS format specifies. Errors never quote a value.
func ReadHeadersFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE: %w", err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		for _, pair := range strings.Split(text, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			k, v, ok := strings.Cut(pair, "=")
			k = strings.TrimSpace(k)
			if !ok || k == "" {
				return nil, fmt.Errorf("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE line %d: want key=value", line)
			}
			dv, err := url.PathUnescape(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE line %d: header %q has a malformed escape", line, k)
			}
			out[k] = dv
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE: %w", err)
	}
	return out, nil
}
