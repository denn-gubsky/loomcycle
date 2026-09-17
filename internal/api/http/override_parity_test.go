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
//
// MCP IS DELIBERATELY NOT IN THIS TABLE. It was, as a grep of internal/api/mcp/
// tools.go — and that file declares TWO spawn tools, so spawn_run carrying every
// override satisfied the check while spawn_runs carried none of them. A file is
// not a surface. The MCP schemas are Go values in their own package, so they are
// checked there, per tool and derived from connector.SpawnRunRequest rather than
// from a list: TestSpawnSchemas_AdvertiseEveryFieldTheyAccept. Do not reinstate a
// whole-file grep here; it cannot tell the two tools apart.
func TestOverrideParity_EveryTransportEnumeratesEveryOverride(t *testing.T) {
	for _, tc := range []struct {
		what string
		path string
		// how the field appears in that file's own vocabulary
		form func(wire string) string
	}{
		{"the gRPC proto", "../../../proto/loomcycle.proto", func(w string) string { return " " + w + " = " }},
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

// The TS adapter has TWO halves and the guard above only checked one.
//
// The serializer lists every field, so that check passed — while ContinueOptions,
// the type handed to it on the continuation path, carried none of them. The
// build failed with a type error; had the signature been looser it would have
// compiled and silently dropped every override on that path.
//
// So: the OPTION TYPES must carry them too, and both request paths must share
// one declaration rather than keeping two lists in step by hand.
func TestOverrideParity_TypeScriptOptionTypesShareOneDeclaration(t *testing.T) {
	b, err := os.ReadFile("../../../adapters/ts/src/types.ts")
	if err != nil {
		t.Skipf("TS types not readable from here: %v", err)
	}
	src := string(b)

	if !strings.Contains(src, "export interface RunOverrideOptions {") {
		t.Fatal("no shared RunOverrideOptions interface; the override fields are declared " +
			"per-request-type, which is how one path silently loses them")
	}
	for _, iface := range []string{"RunOptions", "ContinueOptions"} {
		if !strings.Contains(src, "export interface "+iface+" extends RunOverrideOptions {") {
			t.Errorf("%s does not extend RunOverrideOptions, so a caller on that path cannot "+
				"supply an override the serializer would happily write", iface)
		}
	}

	// And the shared interface really does name them all.
	decl := src[strings.Index(src, "export interface RunOverrideOptions {"):]
	decl = decl[:strings.Index(decl, "\n}\n")]
	camel := map[string]string{
		"model": "model", "provider": "provider", "tier": "tier", "effort": "effort",
		"max_tokens": "maxTokens", "max_iterations": "maxIterations",
		"unbounded_iterations": "unboundedIterations", "max_concurrent_children": "maxConcurrentChildren",
		"retry_attempts": "retryAttempts", "memory_inject_max_tokens": "memoryInjectMaxTokens",
		"memory_index_max_bytes": "memoryIndexMaxBytes", "inject_tool_guide": "injectToolGuide",
	}
	var missing []string
	for _, wire := range overrideWireNames {
		if !strings.Contains(decl, camel[wire]+"?:") {
			missing = append(missing, camel[wire])
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("RunOverrideOptions does not declare: %s", strings.Join(missing, ", "))
	}
}

// Non-vacuity: a list that names nothing passes every check above.
func TestOverrideParity_TheListItselfIsPopulated(t *testing.T) {
	if len(overrideWireNames) < 12 {
		t.Fatalf("overrideWireNames has %d entries; it is meant to name every per-run "+
			"override and has stopped doing so", len(overrideWireNames))
	}
}
