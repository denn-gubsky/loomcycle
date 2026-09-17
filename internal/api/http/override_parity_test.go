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

// Each transport ENUMERATES, and each SURFACE within a transport enumerates
// separately. Asserted over the source of each list, because the failure is a
// field present in one list and absent from another — which no amount of Go-side
// testing sees.
//
// A FILE IS NOT A SURFACE. This table used to grep whole files, and that is how
// it passed through the very breakage it exists to catch:
//
//   - MCP: internal/api/mcp/tools.go declares TWO spawn tools, so spawn_run
//     carrying every override satisfied the check while spawn_runs carried NONE.
//   - Python: every wire name appears TWICE in client.py (run_streaming and
//     continue_session each pass `model=model`), so the row stayed green while
//     `_build_run_request` had no override parameters at all — every fresh-run
//     call raised TypeError before reaching the wire — and while
//     `_run_request_from_dict` carried none of them.
//   - The proto had the same shape: `tier = ` appears in BOTH RunRequest and
//     ContinueRequest, so deleting it from one still left a match.
//
// TS was the only row that was sound, and not by luck: `body.model =` appears
// exactly ONCE because applyOverridesToWire is a single shared function. One
// enumeration, one check.
//
// So: the proto is checked PER MESSAGE below, and the two surfaces whose values
// live in their own language are checked at the strongest level available to
// them, in the package that owns them —
//   - MCP    → TestSpawnSchemas_AdvertiseEveryFieldTheyAccept (internal/api/mcp),
//     walking each tool's PARSED InputSchema
//   - Python → tests/test_override_parity.py, over inspect.signature and the
//     generated pb2 descriptors
//
// Both derive their expectation from the shared shape rather than from a list.
// Do NOT reinstate a whole-file grep for either; it cannot tell the surfaces apart.
func TestOverrideParity_EveryTransportEnumeratesEveryOverride(t *testing.T) {
	for _, tc := range []struct {
		what string
		path string
		// surfaces splits the file into the independent declarations that must
		// EACH carry every override. Returning the whole file is what this test
		// used to do and is exactly the bug.
		surfaces func(src string) map[string]string
		// how the field appears in that file's own vocabulary
		form func(wire string) string
	}{
		{
			what: "the gRPC proto",
			path: "../../../proto/loomcycle.proto",
			surfaces: func(src string) map[string]string {
				return map[string]string{
					"RunRequest":      protoMessageBody(src, "RunRequest"),
					"ContinueRequest": protoMessageBody(src, "ContinueRequest"),
				}
			},
			form: func(w string) string { return " " + w + " = " },
		},
		{
			what: "the TS serializer",
			path: "../../../adapters/ts/src/client.ts",
			// One shared applyOverridesToWire, so the file IS the surface here.
			// The option TYPES are checked separately, below.
			surfaces: func(src string) map[string]string { return map[string]string{"client.ts": src} },
			form:     func(w string) string { return "body." + w + " =" },
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			b, err := os.ReadFile(tc.path)
			if err != nil {
				t.Skipf("%s not readable from here: %v", tc.path, err)
			}
			surfaces := tc.surfaces(string(b))
			if len(surfaces) == 0 {
				t.Fatal("no surfaces extracted — the file's shape changed and this check has stopped checking")
			}
			names := make([]string, 0, len(surfaces))
			for name := range surfaces {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				src := surfaces[name]
				if strings.TrimSpace(src) == "" {
					t.Errorf("%s: surface %q came back EMPTY — a vacuous pass, not a clean one", tc.what, name)
					continue
				}
				var missing []string
				for _, wire := range overrideWireNames {
					if !strings.Contains(src, tc.form(wire)) {
						missing = append(missing, wire)
					}
				}
				sort.Strings(missing)
				if len(missing) > 0 {
					t.Errorf("%s: %s does not carry: %s\n\nA field present on one surface and absent "+
						"from another is DROPPED there silently — it compiles, it ships, and the "+
						"caller's value never arrives.", tc.what, name, strings.Join(missing, ", "))
				}
			}
		})
	}
}

// protoMessageBody returns the body of `message <name> { … }`, so a per-message
// check cannot be satisfied by a sibling message that happens to declare the
// same field. Returns "" when the message is absent, which the caller reports as
// a vacuous surface rather than a pass.
func protoMessageBody(src, name string) string {
	start := strings.Index(src, "message "+name+" {")
	if start < 0 {
		return ""
	}
	start += len("message " + name + " {")
	depth := 1
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start:i]
			}
		}
	}
	return ""
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
