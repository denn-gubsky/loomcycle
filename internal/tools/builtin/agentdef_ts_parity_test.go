package builtin

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestAgentDefOverlay_TSMirrorDoesNotDriftFurther.
//
// `AgentDefOverlay` in adapters/ts/src/types.ts is a HAND-WRITTEN MIRROR of the Go
// mergedDef overlay, and it has no index signature — so TypeScript's excess-property
// checking REFUSES any key the interface does not declare. A field present in Go and
// absent there is not a documentation gap; it is a field a typed TS caller cannot set
// at all.
//
// ⚠️ THE MIRROR IS ALREADY 30 FIELDS BEHIND. That is pre-existing and predates this
// test — `memory_consolidation`, `sampling`, `compaction`, `history_scope`, `volumes`
// and the whole *_def_scopes family are all unreachable from a typed TS caller. This
// test does NOT fix that, and deliberately does not pretend to: fixing it is a
// decision about the adapter's surface, not about any one feature.
//
// What it does is FREEZE the debt. The known-missing set below is a register, not a
// vocabulary — every entry is a field someone chose not to mirror, and the list may
// only ever SHRINK. Add a 31st unmirrored field and this fails, which is the point:
// the drift got this large precisely because nothing objected as it grew.
func TestAgentDefOverlay_TSMirrorDoesNotDriftFurther(t *testing.T) {
	// Pre-existing gaps, frozen. Removing an entry (by mirroring the field in TS) is
	// always safe; adding one requires a deliberate edit here and a reason.
	knownMissing := map[string]bool{
		"a2a_agent_def_scopes": true, "a2a_server_card_def_scopes": true,
		"agent_def_scopes": true, "compaction": true, "context": true,
		"core_blocks": true, "description": true, "history_scope": true,
		"inherit_core_blocks": true, "inject_tool_guide": true, "internal": true,
		"max_context_tokens": true, "memory_consolidation": true,
		"memory_index_max_bytes": true, "memory_inject_max_tokens": true,
		"memory_protocol": true, "memory_roots": true, "models": true,
		"providers": true, "run_timeout_seconds": true, "sampling": true,
		"schedule_def_scopes": true, "search_providers": true,
		"sql_quota_bytes": true, "sql_scopes": true, "system_prompt_base": true,
		"unbounded_iterations": true, "volume_def_scopes": true, "volumes": true,
	}

	goSrc, err := os.ReadFile("agentdef.go")
	if err != nil {
		t.Fatalf("read agentdef.go: %v", err)
	}
	m := regexp.MustCompile(`(?s)type mergedDef struct \{(.*?)\n\}`).FindSubmatch(goSrc)
	if m == nil {
		t.Fatal("could not find mergedDef — this test asserts nothing until it matches again")
	}
	var goFields []string
	for _, f := range regexp.MustCompile("`json:\"([a-z0-9_]+)").FindAllSubmatch(m[1], -1) {
		goFields = append(goFields, string(f[1]))
	}
	if len(goFields) < 20 {
		t.Fatalf("only %d overlay fields parsed — the pattern has stopped matching", len(goFields))
	}

	tsSrc, err := os.ReadFile("../../../adapters/ts/src/types.ts")
	if err != nil {
		t.Skipf("TS adapter not present: %v", err)
	}
	tm := regexp.MustCompile(`(?s)export interface AgentDefOverlay \{(.*?)\n\}`).FindSubmatch(tsSrc)
	if tm == nil {
		t.Fatal("could not find AgentDefOverlay in types.ts")
	}
	ts := map[string]bool{}
	for _, f := range regexp.MustCompile(`(?m)^\s*([a-z0-9_]+)\??:`).FindAllSubmatch(tm[1], -1) {
		ts[string(f[1])] = true
	}
	// An index signature would make the mirror open and this test moot — say so
	// rather than passing silently for the wrong reason.
	if regexp.MustCompile(`\[\s*key\s*:\s*string\s*\]`).Match(tm[1]) {
		t.Skip("AgentDefOverlay gained an index signature — extra keys are accepted, " +
			"so the mirror can no longer block a caller")
	}

	var newlyMissing []string
	for _, f := range goFields {
		if !ts[f] && !knownMissing[f] {
			newlyMissing = append(newlyMissing, f)
		}
	}
	sort.Strings(newlyMissing)
	if len(newlyMissing) > 0 {
		t.Errorf("Go overlay fields a typed TS caller CANNOT set, and which are not in the "+
			"frozen known-missing register: %s\n\nAdd them to adapters/ts/src/types.ts "+
			"AgentDefOverlay, or add them to knownMissing with the reason they must not be "+
			"reachable from TS. The register may only shrink.", strings.Join(newlyMissing, ", "))
	}
	// And the register must not rot: an entry that IS mirrored now should be removed.
	var stale []string
	for f := range knownMissing {
		if ts[f] {
			stale = append(stale, f)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("these are in the known-missing register but ARE mirrored in TS now — "+
			"remove them from the register so it keeps meaning something: %s",
			strings.Join(stale, ", "))
	}
}
