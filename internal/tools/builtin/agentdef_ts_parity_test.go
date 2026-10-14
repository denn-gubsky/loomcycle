package builtin

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The TS adapter's AgentDefOverlay (adapters/ts/src/types.ts) is a HAND-WRITTEN
// mirror of the Go mergedDef overlay. It keeps an `[extra: string]: unknown`
// index signature, so an unmirrored key is not refused — but it is untyped: a
// TS caller gets no completion, no doc and no type check for it, and has to
// know the wire name from elsewhere. That is how the mirror fell 29 fields
// behind (sampling, compaction, volumes, the whole *_def_scopes family …)
// before it was closed; nothing objected as it grew.
//
// Both sides are read from source, so neither list is restated here.

const tsTypesPath = "../../../adapters/ts/src/types.ts"

// goJSONTags returns the json tag names of one struct in a Go source file.
func goJSONTags(t *testing.T, path, typeName string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := regexp.MustCompile(`(?s)\ntype ` + typeName + ` struct \{(.*?)\n\}`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("could not find %s in %s — this test asserts nothing until it matches again", typeName, path)
	}
	var tags []string
	for _, f := range regexp.MustCompile("`json:\"([a-z0-9_]+)").FindAllSubmatch(m[1], -1) {
		tags = append(tags, string(f[1]))
	}
	if len(tags) == 0 {
		t.Fatalf("no json tags parsed from %s — the pattern has stopped matching", typeName)
	}
	return tags
}

// tsInterfaceFields returns the top-level field names of one exported TS
// interface. A field is a line of exactly two-space indent, so a JSDoc line
// (`   *`) and a nested object's fields are not read.
func tsInterfaceFields(t *testing.T, src []byte, name string) map[string]bool {
	t.Helper()
	m := regexp.MustCompile(`(?s)\nexport interface ` + name + ` \{(.*?)\n\}`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("could not find interface %s in types.ts", name)
	}
	fields := map[string]bool{}
	for _, f := range regexp.MustCompile(`(?m)^  ([a-z0-9_]+)\??:`).FindAllSubmatch(m[1], -1) {
		fields[string(f[1])] = true
	}
	if len(fields) == 0 {
		t.Fatalf("no fields parsed from %s — the pattern has stopped matching", name)
	}
	return fields
}

func readTSTypes(t *testing.T) []byte {
	t.Helper()
	src, err := os.ReadFile(tsTypesPath)
	if err != nil {
		t.Skipf("TS adapter not present: %v", err)
	}
	return src
}

// TestAgentDefOverlay_TSMirrorsEveryOverlayField: every key the server's
// overlay decoder accepts is a typed field of the TS AgentDefOverlay.
func TestAgentDefOverlay_TSMirrorsEveryOverlayField(t *testing.T) {
	// Keys deliberately NOT offered to TS callers, each with its reason. Empty:
	// every overlay key is caller-settable today. An entry here must say why a
	// TS caller should not be able to set it.
	serverOnly := map[string]string{}

	goFields := goJSONTags(t, "agentdef.go", "mergedDef")
	if len(goFields) < 20 {
		t.Fatalf("only %d overlay fields parsed — the pattern has stopped matching", len(goFields))
	}
	ts := tsInterfaceFields(t, readTSTypes(t), "AgentDefOverlay")

	var missing []string
	for _, f := range goFields {
		if !ts[f] && serverOnly[f] == "" {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("overlay fields the TS AgentDefOverlay does not type: %s\n\nAdd them to "+
			"adapters/ts/src/types.ts AgentDefOverlay (in their wire spelling — the overlay is "+
			"sent verbatim), or to serverOnly here with the reason a TS caller must not set them.",
			strings.Join(missing, ", "))
	}
	var stale []string
	for f := range serverOnly {
		if ts[f] {
			stale = append(stale, f)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("in serverOnly but typed in TS — remove them from serverOnly: %s", strings.Join(stale, ", "))
	}
}

// TestAgentDefOverlay_TSMirrorsNestedBlocks: the nested blocks the overlay
// carries verbatim are mirrored field for field in their wire spelling. The
// camelCase run-option twins (SamplingOptions, …) are converted by the client
// and would be silently ignored inside an overlay, so they cannot stand in.
func TestAgentDefOverlay_TSMirrorsNestedBlocks(t *testing.T) {
	ts := readTSTypes(t)
	for goType, tsType := range map[string]string{
		"Sampling":      "AgentSampling",
		"Compaction":    "AgentCompaction",
		"Context":       "AgentContext",
		"TierCandidate": "TierCandidate",
		"CoreBlock":     "CoreBlock",
	} {
		fields := tsInterfaceFields(t, ts, tsType)
		var missing []string
		for _, f := range goJSONTags(t, "../../config/config.go", goType) {
			if !fields[f] {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			t.Errorf("config.%s fields the TS %s does not type: %s", goType, tsType, strings.Join(missing, ", "))
		}
	}
}
