package otel

import (
	"fmt"
	"strconv"
	"strings"
)

// defaultCardinalityLimit is the series one instrument may hold before new
// label sets fold into its overflow series. The SDK's own default, named here
// so the docs and the env knob have one number to point at.
const defaultCardinalityLimit = 2000

// MetricsConfig is the metrics configuration: the in-process instruments,
// which are always on because /metrics renders them, and the optional OTLP
// export of the same instruments.
//
// As for traces, the exporter's endpoint, headers, timeout, compression and
// TLS are absent: the OTLP metric exporter reads OTEL_EXPORTER_OTLP[_METRICS]_*
// itself, and the periodic reader reads OTEL_METRIC_EXPORT_INTERVAL and
// OTEL_METRIC_EXPORT_TIMEOUT.
type MetricsConfig struct {
	// Export: OTEL_METRICS_EXPORTER=otlp. Off by default, unlike traces: a
	// deployment that sets an endpoint for its traces has not asked for a
	// second stream of data to land in its collector.
	Export   bool
	Protocol Protocol
	// HeadersFile is LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE, shared with
	// traces: one collector, one credential. Headers is its content when the
	// binary has read it (and registered its values with the redactors);
	// nil reads the file at InitMetrics.
	HeadersFile string
	Headers     map[string]string
	// Warnings are values ResolveMetricsConfig ignored, as Config.Warnings.
	Warnings []string
	// CardinalityLimit is LOOMCYCLE_METRICS_CARDINALITY_LIMIT.
	CardinalityLimit int
	// AgentLabel is LOOMCYCLE_METRICS_AGENT_LABEL: the runs and tokens
	// instruments carry the agent name.
	AgentLabel bool

	// The resource, filled by the binary as for Config.
	ServiceVersion string
	InstanceID     string
	Environment    string
}

// ResolveMetricsConfig reads and validates the metrics environment through
// getenv, by ResolveConfig's rules. loomcycle's own LOOMCYCLE_METRICS_* knobs
// are always checked, because the instruments are always on. The OTLP export
// variables are checked only when export is on: off, the OTEL_* environment
// may belong to other OpenTelemetry programs, and nothing here reads it. On,
// a value that would make export silently not work (a missing or malformed
// endpoint, a protocol, a header loomcycle cannot honour) stops the boot,
// while a name it does not support beside one it does (a console exporter) is
// ignored with a warning in Warnings.
func ResolveMetricsConfig(getenv func(string) string) (MetricsConfig, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	cfg := MetricsConfig{Protocol: ProtocolHTTPProtobuf, CardinalityLimit: defaultCardinalityLimit}

	if v := get("LOOMCYCLE_METRICS_CARDINALITY_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return MetricsConfig{}, fmt.Errorf("LOOMCYCLE_METRICS_CARDINALITY_LIMIT=%q: want a positive integer", v)
		}
		cfg.CardinalityLimit = n
	}
	if v := get("LOOMCYCLE_METRICS_AGENT_LABEL"); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			return MetricsConfig{}, fmt.Errorf("LOOMCYCLE_METRICS_AGENT_LABEL=%q: want 1 or 0", v)
		}
		cfg.AgentLabel = on
	}
	cfg.HeadersFile = get("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE")

	on, warns := metricsExporter(get("OTEL_METRICS_EXPORTER"))
	cfg.Warnings = warns
	if !on || strings.EqualFold(get("OTEL_SDK_DISABLED"), "true") {
		return cfg, nil
	}
	if get("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && get("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") == "" {
		// The exporter would fall back to localhost and fail every interval;
		// asking for export and naming no collector is a mistake to stop on.
		return MetricsConfig{}, fmt.Errorf("OTEL_METRICS_EXPORTER=otlp needs OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")
	}
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
		if err := checkEndpoint(k, get(k)); err != nil {
			return MetricsConfig{}, err
		}
	}

	proto, protoVar := get("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"), "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"
	if proto == "" {
		proto, protoVar = get("OTEL_EXPORTER_OTLP_PROTOCOL"), "OTEL_EXPORTER_OTLP_PROTOCOL"
	}
	switch Protocol(proto) {
	case "", ProtocolHTTPProtobuf:
	case ProtocolGRPC:
		cfg.Protocol = ProtocolGRPC
	default:
		return MetricsConfig{}, fmt.Errorf("%s=%q: loomcycle supports http/protobuf or grpc", protoVar, proto)
	}

	for _, k := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS"} {
		v := get(k)
		if v == "" {
			continue
		}
		if cfg.HeadersFile != "" {
			return MetricsConfig{}, fmt.Errorf("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE and %s are both set: keep the collector headers in one place", k)
		}
		// As for traces: the exporter would report a malformed entry through
		// the SDK logger with its value.
		if _, err := parseHeaderList(k, v); err != nil {
			return MetricsConfig{}, err
		}
	}
	cfg.Export = true
	return cfg, nil
}

// metricsExporter reads OTEL_METRICS_EXPORTER, a comma-separated list as the
// specification has it. Unlike traces it is off when unset: a deployment
// exporting its traces has not asked for a second stream in its collector.
func metricsExporter(v string) (on bool, warnings []string) {
	none := false
	for _, e := range strings.Split(strings.ToLower(v), ",") {
		switch e = strings.TrimSpace(e); e {
		case "otlp":
			on = true
		case "none":
			none = true
		case "":
		default:
			warnings = append(warnings, fmt.Sprintf("OTEL_METRICS_EXPORTER names %q, which loomcycle does not support; ignored", e))
		}
	}
	if on && none {
		warnings = append(warnings, "OTEL_METRICS_EXPORTER names both otlp and none; exporting over otlp")
	}
	return on, warnings
}
