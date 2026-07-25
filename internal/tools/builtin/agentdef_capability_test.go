package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestAgentDefTool_ForkInheritsScopeGates is the fail-before regression for the
// v1.34.0 capability-gate drop. `memory_scopes` used to be the ONLY scope on
// the overlay, so an agent declaring `history_scope` / `sql_scopes` /
// `volumes` lost them at persist and every FORK of it was born default-deny —
// observed live as:
//
//	history: no history_scope policy (default-deny); grant history_scope: [self]
//
// The fork here supplies an overlay that says nothing about the gates, which is
// exactly the reported case: inheritance, not restatement. Revert any one of
// the four mergedDef fields and this fails on that gate.
func TestAgentDefTool_ForkInheritsScopeGates(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.WithAgentTools(ctx, []string{"*"})

	create := `{"op":"create","name":"gated-agent","promote":true,"overlay":{
		"system_prompt":"read the chats",
		"tools":["Memory","History","Document"],
		"memory_scopes":["user"],
		"history_scope":["user"],
		"sql_scopes":["user"],
		"sql_quota_bytes":4096,
		"volumes":["workspace"]
	}}`
	if res, _ := tool.Execute(ctx, json.RawMessage(create)); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}

	// Fork WITHOUT restating the gates — they must be inherited from the parent.
	fork := `{"op":"fork","name":"gated-agent","promote":true,"overlay":{"system_prompt":"read the chats, briefly"}}`
	if res, _ := tool.Execute(ctx, json.RawMessage(fork)); res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}

	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "gated-agent")
	if !ok {
		t.Fatal("resolve: gated-agent not found after create+fork+promote")
	}
	if got := def.SystemPrompt; got != "read the chats, briefly" {
		t.Errorf("fork overlay did not apply: system_prompt = %q", got)
	}
	for _, tc := range []struct {
		field string
		got   []string
	}{
		{"history_scope", def.HistoryScope},
		{"sql_scopes", def.SqlScopes},
		{"volumes", def.Volumes},
		{"memory_scopes", def.MemoryScopes},
	} {
		if len(tc.got) != 1 {
			t.Errorf("%s = %v after fork, want it inherited from the parent — an empty gate is DEFAULT-DENY, so the forked agent silently loses the capability", tc.field, tc.got)
		}
	}
	if def.SqlQuotaBytes != 4096 {
		t.Errorf("sql_quota_bytes = %d after fork, want 4096 inherited", def.SqlQuotaBytes)
	}
}

// TestAgentDefTool_CreateDoesNotDedupAcrossChangedVolumes is the fail-before
// regression for the collision that excluding `volumes` from the content hash
// opens up.
//
// `create` returns the ACTIVE row with `deduplicated: true` when
// content_sha256 matches. Since volumes is not hashed, a create that changes
// ONLY the volume bindings hashed identically — so the caller got back the OLD
// definition, with the OLD filesystem bindings, and a success response. A
// filesystem trust boundary that silently does not move is the worst shape of
// this bug, which is why it is pinned on `volumes` specifically.
func TestAgentDefTool_CreateDoesNotDedupAcrossChangedVolumes(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.WithAgentTools(ctx, []string{"*"})

	first := `{"op":"create","name":"vol-agent","promote":true,"overlay":{
		"system_prompt":"work","tools":["Read"],"volumes":["a"]}}`
	res, _ := tool.Execute(ctx, json.RawMessage(first))
	if res.IsError {
		t.Fatalf("first create: %s", res.Text)
	}

	// Byte-identical EXCEPT the volume binding.
	second := `{"op":"create","name":"vol-agent","promote":true,"overlay":{
		"system_prompt":"work","tools":["Read"],"volumes":["b"]}}`
	res2, _ := tool.Execute(ctx, json.RawMessage(second))
	if res2.IsError {
		t.Fatalf("second create: %s", res2.Text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res2.Text), &out); err != nil {
		t.Fatalf("parse second create response: %v\n%s", err, res2.Text)
	}
	if out["deduplicated"] == true {
		t.Error("create deduplicated against the active row despite a CHANGED volume binding — the caller was handed back the old definition, and the filesystem trust boundary silently did not move")
	}

	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "vol-agent")
	if !ok {
		t.Fatal("resolve: vol-agent not found")
	}
	if len(def.Volumes) != 1 || def.Volumes[0] != "b" {
		t.Errorf("active def volumes = %v, want [b] — the second create must have minted and promoted a new version", def.Volumes)
	}

	// ...and a genuinely identical create still dedups, or the idempotent
	// re-register flow this probe protects would start spamming the lineage.
	res3, _ := tool.Execute(ctx, json.RawMessage(second))
	var out3 map[string]any
	if err := json.Unmarshal([]byte(res3.Text), &out3); err != nil {
		t.Fatalf("parse third create response: %v\n%s", err, res3.Text)
	}
	if out3["deduplicated"] != true {
		t.Errorf("a byte-identical re-create did NOT dedup — idempotent create is broken: %s", res3.Text)
	}
}

// TestAgentDefTool_CreateDoesNotDedupAcrossChangedDefScopes covers the same
// collision for the authority grants (an agent that may suddenly author
// schedules is as much a boundary change as one that may read a new volume).
func TestAgentDefTool_CreateDoesNotDedupAcrossChangedDefScopes(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.WithAgentTools(ctx, []string{"*"})

	base := `{"op":"create","name":"meta","promote":true,"overlay":{
		"system_prompt":"author","tools":["Read"]%s}}`
	if res, _ := tool.Execute(ctx, json.RawMessage(fmt.Sprintf(base, ""))); res.IsError {
		t.Fatalf("first create: %s", res.Text)
	}
	res2, _ := tool.Execute(ctx, json.RawMessage(fmt.Sprintf(base, `,"schedule_def_scopes":["any"]`)))
	var out map[string]any
	if err := json.Unmarshal([]byte(res2.Text), &out); err != nil {
		t.Fatalf("parse: %v\n%s", err, res2.Text)
	}
	if out["deduplicated"] == true {
		t.Error("create deduplicated despite a NEW schedule_def_scopes grant — the caller believes the agent may author schedules when the active def still denies it")
	}
	def, _ := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "meta")
	if len(def.ScheduleDefScopes) != 1 {
		t.Errorf("active def schedule_def_scopes = %v, want the granted [any]", def.ScheduleDefScopes)
	}
}

// TestUnhashedCapabilityTags_CoversEveryUnhashedOverlayField pins the dedup
// probe's field list to exactly (mergedDef ∖ agents.AgentContent). Without this
// the next field added to the overlay but left out of the hash silently
// reopens the collision the two tests above regress.
func TestUnhashedCapabilityTags_CoversEveryUnhashedOverlayField(t *testing.T) {
	mergedTags := jsonTagsOfFields(reflect.TypeOf(mergedDef{}))
	hashedTags := jsonTagsOfFields(reflect.TypeOf(agents.AgentContent{}))

	want := map[string]bool{}
	for tag := range mergedTags {
		if !hashedTags[tag] {
			want[tag] = true
		}
	}
	got := map[string]bool{}
	for _, tag := range unhashedCapabilityTags {
		got[tag] = true
	}
	for tag := range want {
		if !got[tag] {
			t.Errorf("mergedDef field %q is NOT in the content hash and NOT compared by the dedup probe — a create changing only it would dedup and silently hand back the old definition. Add it to unhashedCapabilityTags (or hash it).", tag)
		}
	}
	for tag := range got {
		if !want[tag] {
			t.Errorf("unhashedCapabilityTags lists %q, but that field IS in the content hash (or is not on mergedDef) — comparing it is redundant; remove it", tag)
		}
	}
}

// TestMergedDef_CoversEveryConfigAgentDefField is the guard against the
// capability-gate class of bug, which has now recurred twice.
//
// The overlay (mergedDef) is what an `AgentDef create` / `fork` persists and
// what `bootstrapped_from_static` snapshots. A gate declared on
// config.AgentDef but ABSENT here does not fail loudly — it is silently
// dropped at persist, and the agent comes back DEFAULT-DENY on reload. F40
// was that bug for the *_def_scopes gates; v1.34.0 was that bug again for
// sql_scopes / history_scope / volumes / sql_quota_bytes, where a forked
// consolidator died on:
//
//	history: no history_scope policy (default-deny); grant history_scope: [self]
//
// So: every yaml field on config.AgentDef must have a matching json tag on
// mergedDef, or appear in `exempt` with a reason. Adding a field to `exempt`
// is the conscious decision this test exists to force — do it only when the
// field genuinely must not round-trip through the substrate.
func TestMergedDef_CoversEveryConfigAgentDefField(t *testing.T) {
	exempt := map[string]string{
		// Resolved into SystemPrompt at config-load; it is a path on the
		// operator's host, meaningless (and unreadable) on another deployment,
		// so it must never be persisted into a substrate definition.
		"system_prompt_file": "config-load-time only — resolved into system_prompt",
		// A REMOVED-field tombstone (see config.AgentDef.SkillDefScopes): skill
		// authoring is governed by the unified `skills:` pattern allowlist now,
		// not a def-scope gate. Persisting it would resurrect a dead gate.
		"skill_def_scopes": "removed-field tombstone; superseded by the skills: allowlist",
		// A config-load-time NORMALIZER, not a runtime gate: addContextToolDefaults
		// reads it once to decide whether to append "Context" to Tools, and nothing
		// reads it afterwards. A substrate def carries an explicit Tools list, so
		// the decision is already baked into the field that does round-trip.
		"disable_context": "load-time normalizer — its effect is already baked into tools",
	}

	// Fields that DO round-trip but under a different tag name on the substrate
	// side. Deliberate renames, not drift — listed so the test can't be quieted
	// by accident.
	alias := map[string]string{
		"code": "code_body", // config yaml `code:` persists as `code_body`
	}

	mergedTags := jsonTagsOfFields(reflect.TypeOf(mergedDef{}))
	for _, tag := range yamlTagsOfFields(reflect.TypeOf(config.AgentDef{})) {
		if _, ok := exempt[tag]; ok {
			continue
		}
		if a, ok := alias[tag]; ok {
			tag = a
		}
		if !mergedTags[tag] {
			t.Errorf("config.AgentDef declares %q but builtin.mergedDef has no matching json tag — a fork/bootstrap of an agent using it is born default-deny.\n"+
				"Either thread it through mergedDef (struct + applyOverlay + staticToMergedDef, and lookup.SubstrateDef + ToConfigDef on the read side) "+
				"OR add %q to this test's exempt map with the reason it must not round-trip.", tag, tag)
		}
	}
}

// yamlTagsOfFields walks a struct's exported fields and returns the yaml tag
// names (the part before any ","). Fields tagged `yaml:"-"` are skipped: they
// are derived, not operator-declared, so they are not part of the surface this
// test guards.
func yamlTagsOfFields(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		if j := indexByte(tag, ','); j >= 0 {
			tag = tag[:j]
		}
		if tag == "" || tag == "-" {
			continue
		}
		out = append(out, tag)
	}
	return out
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// F14: an agent authored via `agentdef create` with the full capability set
// (channels / evaluation_scopes / max_iterations / interruption) must
// round-trip the whole config through the substrate persist → resolve path —
// not just tools. Before the mergedDef + SubstrateAgentDef additions,
// these fields were silently dropped at persist/read, so an MCP-authored agent
// could never be a complete interactive/multi-agent agent.
func TestAgentDefTool_CreateWithCapabilityFields_RoundTrips(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	// Operator ceiling (the F11 fix provides this on the MCP/HTTP/gRPC paths).
	ctx = tools.WithAgentTools(ctx, []string{"*"})

	overlay := `{"op":"create","name":"complete-agent","promote":true,"overlay":{
		"system_prompt":"coordinate the loop",
		"tools":["Memory","Channel","Evaluation","Interruption"],
		"memory_scopes":["user"],
		"memory_consolidation":true,
		"evaluation_scopes":["submit_self","read_any"],
		"max_iterations":42,
		"channels":{"publish":["findings"],"subscribe":["tasks"]},
		"interruption":{"enabled":true,"kinds":["question"],"max_pending":3}
	}}`
	if res, _ := tool.Execute(ctx, json.RawMessage(overlay)); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}

	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "complete-agent")
	if !ok {
		t.Fatal("resolve: complete-agent not found after create+promote")
	}
	if def.MaxIterations != 42 {
		t.Errorf("MaxIterations = %d, want 42", def.MaxIterations)
	}
	if got := def.EvaluationScopes; len(got) != 2 || got[0] != "submit_self" || got[1] != "read_any" {
		t.Errorf("EvaluationScopes = %v, want [submit_self read_any]", got)
	}
	if pub, sub := def.Channels.Publish, def.Channels.Subscribe; len(pub) != 1 || pub[0] != "findings" || len(sub) != 1 || sub[0] != "tasks" {
		t.Errorf("Channels = %+v, want publish=[findings] subscribe=[tasks]", def.Channels)
	}
	if i := def.Interruption; !i.Enabled || i.MaxPending != 3 || len(i.Kinds) != 1 || i.Kinds[0] != "question" {
		t.Errorf("Interruption = %+v, want {enabled:true kinds:[question] max_pending:3}", def.Interruption)
	}
	// RFC BL P2: the consolidation grant must survive overlay → persist → resolve.
	if !def.MemoryConsolidation {
		t.Error("MemoryConsolidation grant did not round-trip through create+promote+resolve")
	}
}
