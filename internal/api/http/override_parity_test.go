package http

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// overrideWireNames is every per-run override, by its wire name. One list, and
// every transport is checked against it.
//
// RFC DC P6 exists because each transport ENUMERATES its fields — the proto
// lists them, the MCP schema lists them, both adapters list them twice (type and
// serializer). A field added to one list and not another is invisible on that
// transport and silent everywhere: it compiles, it ships, and the caller's value
// is dropped. RFC DA shipped exactly that.
var overrideWireNames = []string{
	"model", "provider", "tier", "effort",
	"max_tokens", "max_iterations", "unbounded_iterations", "max_concurrent_children",
	"retry_attempts", "memory_inject_max_tokens", "memory_index_max_bytes", "inject_tool_guide",
}

// The Go side first: RunInput and the connector's spawn shape must both be able
// to carry every override, or the transports have nowhere to put them.
func TestOverrideParity_GoShapesCarryEveryOverride(t *testing.T) {
	goNames := map[string]string{
		"model": "Model", "provider": "Provider", "tier": "Tier", "effort": "Effort",
		"max_tokens": "MaxTokens", "max_iterations": "MaxIterations",
		"unbounded_iterations": "UnboundedIterations", "max_concurrent_children": "MaxConcurrentChildren",
		"retry_attempts": "RetryAttempts", "memory_inject_max_tokens": "MemoryInjectMaxTokens",
		"memory_index_max_bytes": "MemoryIndexMaxBytes", "inject_tool_guide": "InjectToolGuide",
	}
	for _, tc := range []struct {
		name string
		typ  reflect.Type
	}{
		{"runner.RunInput", reflect.TypeOf(runner.RunInput{})},
		{"connector.SpawnRunRequest", reflect.TypeOf(connector.SpawnRunRequest{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var missing []string
			for _, wire := range overrideWireNames {
				if _, ok := tc.typ.FieldByName(goNames[wire]); !ok {
					missing = append(missing, goNames[wire])
				}
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				t.Errorf("%s cannot carry: %s", tc.name, strings.Join(missing, ", "))
			}
		})
	}
}

// Each transport ENUMERATES. Asserted over the source of each list, because the
// failure is a field present in one list and absent from another — which no
// amount of Go-side testing sees.
func TestOverrideParity_EveryTransportEnumeratesEveryOverride(t *testing.T) {
	for _, tc := range []struct {
		what string
		path string
		// how the field appears in that file's own vocabulary
		form func(wire string) string
	}{
		{"the gRPC proto", "../../../proto/loomcycle.proto", func(w string) string { return " " + w + " = " }},
		{"the MCP spawn schema", "../mcp/tools.go", func(w string) string { return `"` + w + `": {` }},
		{"the TS serializer", "../../../adapters/ts/src/client.ts", func(w string) string { return "body." + w + " =" }},
		{"the Python client", "../../../adapters/python/loomcycle/client.py", func(w string) string { return w + "=" + w }},
	} {
		t.Run(tc.what, func(t *testing.T) {
			b, err := os.ReadFile(tc.path)
			if err != nil {
				t.Skipf("%s not readable from here: %v", tc.path, err)
			}
			src := string(b)
			var missing []string
			for _, wire := range overrideWireNames {
				if !strings.Contains(src, tc.form(wire)) {
					missing = append(missing, wire)
				}
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				t.Errorf("%s does not carry: %s\n\nA field present on one transport and absent "+
					"from another is DROPPED there silently — it compiles, it ships, and the "+
					"caller's value never arrives.", tc.what, strings.Join(missing, ", "))
			}
		})
	}
}

// Non-vacuity: a list that names nothing passes every check above.
func TestOverrideParity_TheListItselfIsPopulated(t *testing.T) {
	if len(overrideWireNames) < 12 {
		t.Fatalf("overrideWireNames has %d entries; it is meant to name every per-run "+
			"override and has stopped doing so", len(overrideWireNames))
	}
}
