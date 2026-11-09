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
	// traces: one collector, one credential.
	HeadersFile string
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
// getenv. The in-process knobs are always validated, because the instruments
// are always on. The export variables are validated only when export is on:
// an operator who never asked for OTLP metrics must not have a boot stopped
// by a value only the export would read.
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

	switch exporter := strings.ToLower(get("OTEL_METRICS_EXPORTER")); exporter {
	case "", "none":
		return cfg, nil
	case "otlp":
	default:
		return MetricsConfig{}, fmt.Errorf("OTEL_METRICS_EXPORTER=%q: loomcycle supports otlp or none", exporter)
	}
	if strings.EqualFold(get("OTEL_SDK_DISABLED"), "true") {
		return cfg, nil
	}
	if get("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && get("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") == "" {
		// The exporter would fall back to localhost and fail every interval;
		// asking for export and naming no collector is a mistake to stop on.
		return MetricsConfig{}, fmt.Errorf("OTEL_METRICS_EXPORTER=otlp needs OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")
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

	cfg.HeadersFile = get("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE")
	if cfg.HeadersFile != "" {
		for _, k := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS"} {
			if get(k) != "" {
				return MetricsConfig{}, fmt.Errorf("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE and %s are both set: keep the collector headers in one place", k)
			}
		}
	}
	cfg.Export = true
	return cfg, nil
}
