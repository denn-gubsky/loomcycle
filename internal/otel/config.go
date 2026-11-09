package otel

import (
	"bufio"
	"errors"
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
// environment, plus the two facts only the binary knows (ServiceVersion,
// InstanceID), which the caller fills before Init.
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
	// Warnings are values ResolveConfig ignored (an unsupported propagator
	// or exporter name), for the caller to log.
	Warnings []string
	// Environment is LOOMCYCLE_ENVIRONMENT, the deployment.environment.name
	// resource attribute; omitted when empty.
	Environment string

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
}

// ResolveConfig reads and validates the tracing environment through getenv
// (os.Getenv in production).
//
// With tracing on, a value that would make it silently not work (a protocol,
// sampler, ratio, batch bound, endpoint or header loomcycle cannot honour) is
// an error, so a typo stops the boot instead of tracing differently than the
// operator asked. A value naming something loomcycle does not support beside
// what it does (a b3 propagator, a console exporter) is ignored with a
// warning in Warnings. With tracing off, no OTEL_* value fails the boot: the
// environment may be shared with other OpenTelemetry programs, and nothing
// here reads it. loomcycle's own LOOMCYCLE_OTEL_* settings are always checked.
func ResolveConfig(getenv func(string) string) (Config, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	cfg := Config{
		Protocol: ProtocolHTTPProtobuf, SamplerName: "parentbased_always_on", SamplerArg: 1, Inbound: InboundParent,
		MaxQueueSize: defaultMaxQueueSize, MaxExportBatchSize: defaultMaxExportBatchSize,
		ExportTimeout: defaultExportTimeout, ScheduleDelay: defaultScheduleDelay,
	}

	switch m := InboundMode(get("LOOMCYCLE_OTEL_INBOUND_CONTEXT")); m {
	case "":
	case InboundParent, InboundLink, InboundIgnore:
		cfg.Inbound = m
	default:
		return Config{}, fmt.Errorf("LOOMCYCLE_OTEL_INBOUND_CONTEXT=%q: want parent, link or ignore", m)
	}
	cfg.HeadersFile = get("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE")
	cfg.Environment = get("LOOMCYCLE_ENVIRONMENT")

	exporterOn, warns := tracesExporter(get("OTEL_TRACES_EXPORTER"))
	endpointSet := get("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
	cfg.Enabled = endpointSet && exporterOn && !strings.EqualFold(get("OTEL_SDK_DISABLED"), "true")
	props, pwarns := parsePropagators(get("OTEL_PROPAGATORS"))
	cfg.Propagators = props
	if !cfg.Enabled {
		// Nothing exports, so nothing below is read. The propagators still
		// carry a caller's context, so they take effect either way; their
		// warnings are about what loomcycle ignores, not an error.
		cfg.Warnings = append(warns, pwarns...)
		return cfg, nil
	}
	cfg.Warnings = append(warns, pwarns...)

	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
		if err := checkEndpoint(k, get(k)); err != nil {
			return Config{}, err
		}
	}

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

	for _, k := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS"} {
		v := get(k)
		if v == "" {
			continue
		}
		if cfg.HeadersFile != "" {
			return Config{}, fmt.Errorf("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE and %s are both set: keep the collector headers in one place", k)
		}
		// Checked here because the exporter, finding a malformed entry,
		// reports it through the SDK logger WITH the value.
		if _, err := parseHeaderList(k, v); err != nil {
			return Config{}, err
		}
	}

	var err error
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

// tracesExporter reads OTEL_TRACES_EXPORTER, a comma-separated list: unset or
// naming otlp exports over OTLP; none, or only exporters loomcycle does not
// ship (console, zipkin), exports nothing. Unsupported names are warned about.
func tracesExporter(v string) (on bool, warnings []string) {
	if v == "" {
		return true, nil
	}
	none := false
	for _, e := range strings.Split(strings.ToLower(v), ",") {
		switch e = strings.TrimSpace(e); e {
		case "otlp":
			on = true
		case "none":
			none = true
		case "":
		default:
			warnings = append(warnings, fmt.Sprintf("OTEL_TRACES_EXPORTER names %q, which loomcycle does not support; ignored", e))
		}
	}
	if on && none {
		warnings = append(warnings, "OTEL_TRACES_EXPORTER names both otlp and none; exporting over otlp")
	}
	return on, warnings
}

// checkEndpoint refuses an endpoint the exporter cannot reach: one without an
// http(s) scheme and a host. The retired form, a bare host:port, turned
// tracing on and exported nowhere. The error never quotes the value: an
// endpoint can carry credentials (userinfo, a query token).
func checkEndpoint(name, v string) error {
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return nil
	}
	return fmt.Errorf("%s must be an absolute http(s) URL with a host, e.g. http://collector:4318", name)
}

// parseHeaderList parses an OTEL_EXPORTER_OTLP_HEADERS-format value the way
// the exporter does: comma-separated key=value, values percent-decoded. An
// error names the variable and the entry's position, never its content.
func parseHeaderList(name, v string) (map[string]string, error) {
	out := map[string]string{}
	for i, pair := range strings.Split(v, ",") {
		k, val, err := parseHeaderPair(pair)
		if err != nil {
			return nil, fmt.Errorf("%s: entry %d %s", name, i+1, err)
		}
		out[k] = val
	}
	return out, nil
}

// parseHeaderPair parses one key=value header entry exactly as the OTLP
// exporters' envconfig.stringToHeader does (v1.47): split at the first '=',
// the key trimmed and NOT percent-decoded, then held to the RFC 7230 token
// characters; the value percent-decoded (PathUnescape) and then trimmed. An
// entry this accepts the exporter accepts silently, and one it refuses the
// exporter would have logged; TestParseHeaderPair_MatchesTheExporter holds
// the two together. Its error says what is wrong without quoting the entry.
func parseHeaderPair(pair string) (string, string, error) {
	k, v, ok := strings.Cut(pair, "=")
	k = strings.TrimSpace(k)
	switch {
	case !ok:
		return "", "", errors.New("is not key=value")
	case !validHeaderKey(k):
		return "", "", errors.New("has an invalid header name")
	}
	dv, err := url.PathUnescape(v)
	if err != nil {
		return "", "", errors.New("has a malformed %-escape in its value")
	}
	return k, strings.TrimSpace(dv), nil
}

// validHeaderKey: an HTTP header name is a non-empty RFC 7230 token, the
// exporter's isValidHeaderKey (ASCII letters, digits and !#$%&'*+-.^_`|~).
func validHeaderKey(k string) bool {
	if k == "" {
		return false
	}
	for _, c := range []byte(k) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
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

// parsePropagators reads OTEL_PROPAGATORS: tracecontext and baggage are
// supported, none alone turns propagation off, anything else is ignored with
// a warning.
func parsePropagators(v string) (props, warnings []string) {
	if v == "" {
		return []string{"tracecontext", "baggage"}, nil
	}
	parts := strings.Split(v, ",")
	for _, p := range parts {
		switch p = strings.TrimSpace(p); p {
		case "tracecontext", "baggage":
			props = append(props, p)
		case "none":
			if len(parts) > 1 {
				warnings = append(warnings, "OTEL_PROPAGATORS names none beside other propagators; none ignored")
			}
		case "":
		default:
			warnings = append(warnings, fmt.Sprintf("OTEL_PROPAGATORS names %q, which loomcycle does not support; ignored", p))
		}
	}
	return props, warnings
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
			if strings.TrimSpace(pair) == "" {
				continue
			}
			k, v, err := parseHeaderPair(pair)
			if err != nil {
				return nil, fmt.Errorf("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE line %d %s", line, err)
			}
			out[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE: %w", err)
	}
	return out, nil
}
