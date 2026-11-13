package config

import (
	"fmt"
	"sort"
	"strings"
)

// retiredEnv lists LOOMCYCLE_* variables the runtime used to read, or that
// its own examples used to advertise, and no longer honours. A variable is
// removed outright (there is no compatibility read), so without this table an
// operator who still sets one gets no sign that the line does nothing.
//
// It is a table of names loomcycle itself retired, not "every name the
// runtime does not read": operators use their own LOOMCYCLE_* names from YAML
// (${LOOMCYCLE_MY_WEBHOOK_SECRET}, token_env, an env allowlist), and those
// must not be reported.
//
// Each PR that deletes a variable's read adds its row here. Instead says what
// to do, in a form that completes "… is no longer read: ".
var retiredEnv = map[string]string{
	"LOOMCYCLE_INTERRUPTION_DEFAULT_TIMEOUT_MS":    "set interruption.default_timeout_ms in the yaml",
	"LOOMCYCLE_INTERRUPTION_MAX_TIMEOUT_MS":        "set interruption.max_timeout_ms in the yaml",
	"LOOMCYCLE_INTERRUPTION_MAX_PENDING_PER_RUN":   "set interruption.max_pending_per_run in the yaml",
	"LOOMCYCLE_INTERRUPTION_HEARTBEAT_INTERVAL_MS": "the interval is fixed",
	"LOOMCYCLE_SNAPSHOT_MAX_BYTES":                 "pass max_bytes on the snapshot request",
	"LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT":        "retired: use OTEL_EXPORTER_OTLP_ENDPOINT, with its scheme (http://host:4318)",
	"LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS":         "retired: use OTEL_EXPORTER_OTLP_HEADERS or LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE",
	"LOOMCYCLE_OTEL_SERVICE_NAME":                  "retired: use OTEL_SERVICE_NAME",
	"LOOMCYCLE_OTEL_TRACES_SAMPLER_RATIO":          "retired: use OTEL_TRACES_SAMPLER=parentbased_traceidratio with OTEL_TRACES_SAMPLER_ARG",
}

// retiredEnvWarnings returns one advisory per retired variable present in
// environ (the os.Environ form), sorted by name. A variable set to the empty
// string counts: the line is still dead weight in the operator's file. Values
// are never included; several retired names held secrets.
func retiredEnvWarnings(environ []string) []string {
	var out []string
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		instead, retired := retiredEnv[name]
		if !retired {
			continue
		}
		out = append(out, fmt.Sprintf("%s is set but no longer read: %s", name, strings.TrimPrefix(instead, "retired: ")))
	}
	sort.Strings(out)
	return out
}
