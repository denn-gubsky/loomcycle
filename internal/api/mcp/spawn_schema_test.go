package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// Both spawn tools unmarshal their arguments into connector.SpawnRunRequest, so
// every json tag on that struct is ACCEPTED by both of them. A field the schema
// does not name is still honoured when sent — and invisible to the agent
// deciding what to send. For a model-facing tool that is indistinguishable from
// not having the feature at all.
//
// The expectation is DERIVED from the struct rather than listed here, because a
// hand-maintained list is exactly what failed. The transport-parity guard in
// internal/api/http grepped tools.go as a whole FILE, so spawn_run carrying the
// twelve per-run overrides satisfied it while spawn_runs carried none of them —
// and neither tool had ever advertised `sampling` or `metadata`.
func TestSpawnSchemas_AdvertiseEveryFieldTheyAccept(t *testing.T) {
	typ := reflect.TypeOf(connector.SpawnRunRequest{})
	accepted := jsonFieldNames(t, typ)
	// EXACT, not a floor. A floor of 25 against a real count of 30 let five
	// fields quietly lose their tags — to a bad merge, or a `json:"-"` added by
	// mistake — and drop out of the expectation without tripping anything. The
	// count is the thing a stale expectation shows up in, so pin it: a field
	// added to the shape fails HERE, next to the sentence explaining why, rather
	// than silently going unchecked.
	if want := typ.NumField(); len(accepted) != want {
		t.Fatalf("derived %d wire names from connector.SpawnRunRequest's %d fields (%v).\n\n"+
			"Every field on this struct is part of the accepted wire shape, so an untagged one "+
			"is still populated by encoding/json from its GO name — accepted by the handler and "+
			"absent from the schema, which is exactly the failure this test exists to prevent. "+
			"Give it a json tag, or `json:\"-\"` if it genuinely is not wire state.",
			len(accepted), want, accepted)
	}

	for _, tc := range []struct {
		tool  string
		props func(*testing.T, map[string]json.RawMessage) map[string]json.RawMessage
		// exempt names a field the tool genuinely does not take, with the reason.
		exempt map[string]string
	}{
		{tool: "spawn_run", props: topLevelProperties},
		{tool: "spawn_runs", props: childSpecProperties, exempt: map[string]string{
			// handleSpawnRuns requires `agent` on every child; a batch never
			// continues a session, and the tool description says so.
			"session_id": "a batch child is always a fresh run",
		}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			advertised := tc.props(t, schemaObject(t, toolInputSchema(t, tc.tool)))
			if len(advertised) == 0 {
				t.Fatal("parsed zero properties — the schema shape changed and this check has stopped checking")
			}
			var missing []string
			for _, name := range accepted {
				if _, ok := tc.exempt[name]; ok {
					continue
				}
				if _, ok := advertised[name]; !ok {
					missing = append(missing, name)
				}
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				t.Errorf("the %s schema accepts but does not advertise: %s\n\nThe handler will "+
					"honour these when sent, so nothing fails — they are simply undiscoverable "+
					"to the agent reading the tool list.", tc.tool, strings.Join(missing, ", "))
			}
		})
	}
}

// Every tool's InputSchema is handed to an MCP client verbatim, so a schema that
// does not parse is a broken tool surface rather than a failed test somewhere.
// Splicing a shared fragment into two schemas is easy to get one comma wrong.
func TestSpawnSchemas_EveryToolSchemaIsValidJSON(t *testing.T) {
	for _, tool := range toolDescriptors() {
		if len(tool.InputSchema) == 0 {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal(tool.InputSchema, &v); err != nil {
			t.Errorf("%s: input schema is not valid JSON: %v", tool.Name, err)
		}
	}
}

// jsonFieldNames returns the wire names of a struct's json-tagged fields,
// skipping the ones marked `json:"-"` (genuinely not wire state).
//
// A field with NO tag at all is also skipped here, but that is not a safe
// omission: encoding/json still populates it from its Go name, so the handler
// would accept a field this list never requires the schema to advertise. The
// caller's exact-count assertion is what closes that — do not relax it.
func jsonFieldNames(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		if name := strings.Split(tag, ",")[0]; name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func toolInputSchema(t *testing.T, name string) json.RawMessage {
	t.Helper()
	for _, tool := range toolDescriptors() {
		if tool.Name == name {
			return tool.InputSchema
		}
	}
	t.Fatalf("no %q tool in the MCP surface", name)
	return nil
}

func schemaObject(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	return obj
}

// topLevelProperties: spawn_run takes one run spec directly.
func topLevelProperties(t *testing.T, schema map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	props, ok := schema["properties"]
	if !ok {
		t.Fatal("schema has no properties")
	}
	return schemaObject(t, props)
}

// childSpecProperties: spawn_runs nests the same run spec under spawns[].items.
func childSpecProperties(t *testing.T, schema map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	spawns, ok := topLevelProperties(t, schema)["spawns"]
	if !ok {
		t.Fatal("spawn_runs schema has no spawns property")
	}
	items, ok := schemaObject(t, spawns)["items"]
	if !ok {
		t.Fatal("spawn_runs.spawns has no items schema")
	}
	return topLevelProperties(t, schemaObject(t, items))
}

// Advertising a field is only half of it: the batch handler must also carry it
// to the connector. Testing the schema and testing SpawnRun's field copy both
// pass while the leg BETWEEN them — unmarshal, per-child validation, the batch
// loop — drops everything, so this asserts the crossing itself.
//
// Every value here is one the schema newly advertises, and the meaningful zeros
// are deliberate: retry_attempts 0, inject_tool_guide false and
// unbounded_iterations false are pointers precisely so "off" survives, and a
// non-pointer would silently read as unset right here.
func TestSpawnRuns_CarriesEveryAdvertisedFieldToTheConnector(t *testing.T) {
	child := map[string]any{
		"agent":                    "rev",
		"segments":                 []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "trusted-text", "text": "hi"}}}},
		"metadata":                 map[string]any{"repo": "loomcycle"},
		"sampling":                 map[string]any{"temperature": 0.25, "top_p": 0.9, "seed": float64(7), "stop": []any{"END"}},
		"tool_choice":              map[string]any{"mode": "tool", "name": "WebSearch", "until": "until_called"},
		"compaction":               map[string]any{"enabled": true, "keep_last_n": float64(2)},
		"context":                  map[string]any{"mode": "recap", "keep_last_n": float64(3)},
		"max_context_tokens":       float64(131072),
		"model":                    "some-model",
		"provider":                 "some-provider",
		"tier":                     "middle",
		"effort":                   "high",
		"max_tokens":               float64(4096),
		"max_iterations":           float64(12),
		"unbounded_iterations":     false,
		"max_concurrent_children":  float64(2),
		"retry_attempts":           float64(0),
		"memory_inject_max_tokens": float64(0),
		"memory_index_max_bytes":   float64(0),
		"inject_tool_guide":        false,
	}
	args, err := json.Marshal(map[string]any{"spawns": []any{child}})
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}

	mc := &mockConnector{batchResult: connector.BatchSpawnResult{Spawned: 1}}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"spawn_runs","arguments":` + string(args) + `}}`,
	}, "\n") + "\n"
	driveServer(t, srv, in)

	stored, _ := mc.batchReq.Load().(connector.BatchSpawnRequest)
	if len(stored.Spawns) != 1 {
		t.Fatalf("connector saw %d spawns, want 1 — the call was refused before dispatch", len(stored.Spawns))
	}
	// Round-trip the child the connector actually received, so the comparison is
	// against what the Go shape held rather than against the bytes we sent.
	got := map[string]any{}
	b, err := json.Marshal(stored.Spawns[0])
	if err != nil {
		t.Fatalf("marshal recorded spawn: %v", err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal recorded spawn: %v", err)
	}
	var dropped []string
	for name, want := range child {
		if name == "segments" {
			continue // shape is normalised by loop.PromptSegment; covered elsewhere
		}
		if !reflect.DeepEqual(got[name], want) {
			dropped = append(dropped, name+": got "+describe(got[name])+", want "+describe(want))
		}
	}
	sort.Strings(dropped)
	if len(dropped) > 0 {
		t.Errorf("spawn_runs did not carry to the connector:\n  %s", strings.Join(dropped, "\n  "))
	}
}

func describe(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<unmarshalable>"
	}
	return string(b)
}

// spawnRunStreaming builds runner.RunInput by hand from connector.SpawnRunRequest
// — thirty fields, copied one by one — and NOTHING covered it: both fake runners
// declared `_ runner.RunInput` and threw the value away.
//
// That is the branch a session takes when it opts into run events
// (initialize.capabilities.loomcycle.runEvents=true), which Claude Code does. So
// a field added to the accepted shape and to the schema, but missed in that
// literal, is dropped for every streaming caller while the schema test, the
// crossing test and the connector's own field copy all stay green.
//
// Derived from the struct for the same reason the schema check is: a hand list
// here would be one more enumeration to forget.
func TestSpawnRunStreaming_CarriesTheRequestIntoTheRunInput(t *testing.T) {
	fr := &fakeRunner{agentID: "a_1", runID: "r_1", sessionID: "s_1"}
	sess := NewSession()
	sess.MarkInitialized()
	sess.SetRunEventsEnabled(true) // the branch Claude Code takes
	env := &handlerEnv{connector: &mockConnector{}, runner: fr, session: sess}

	args := `{"agent":"rev","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}],
	          "user_id":"u1","tenant_id":"t1","user_tier":"pro","user_bearer":"tok",
	          "user_credentials":{"github":"g"},"tools":["Read"],
	          "max_context_tokens":4096,"model":"m","provider":"p","tier":"middle","effort":"high",
	          "max_tokens":100,"max_iterations":9,"max_concurrent_children":2,
	          "retry_attempts":0,"memory_inject_max_tokens":0,"memory_index_max_bytes":0,
	          "inject_tool_guide":false,"unbounded_iterations":false,
	          "tool_choice":{"mode":"tool","name":"WebSearch","until":"until_called"}}`
	if _, err := handleSpawnRun(context.Background(), env, json.RawMessage(args)); err != nil {
		t.Fatalf("handleSpawnRun: %v", err)
	}

	in := fr.lastInput
	for _, tc := range []struct {
		field string
		got   any
		want  any
	}{
		{"Agent", in.Agent, "rev"},
		{"UserID", in.UserID, "u1"},
		{"TenantID", in.TenantID, "t1"},
		{"UserTier", in.UserTier, "pro"},
		{"UserBearer", in.UserBearer, "tok"},
		{"MaxContextTokens", in.MaxContextTokens, 4096},
		{"Model", in.Model, "m"},
		{"Provider", in.Provider, "p"},
		{"Tier", in.Tier, "middle"},
		{"Effort", in.Effort, "high"},
		{"MaxTokens", in.MaxTokens, 100},
		{"MaxIterations", in.MaxIterations, 9},
		{"MaxConcurrentChildren", in.MaxConcurrentChildren, 2},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v — dropped by the streaming path's hand-copy", tc.field, tc.got, tc.want)
		}
	}
	if len(in.Segments) == 0 {
		t.Error("Segments were dropped — the run would reach the model with an empty prompt")
	}
	if in.ToolChoice == nil || in.ToolChoice.Name != "WebSearch" || in.ToolChoice.Until != "until_called" {
		t.Errorf("ToolChoice = %+v, want tool/WebSearch/until_called — dropped by the streaming path's hand-copy", in.ToolChoice)
	}
	if in.UserCredentials["github"] != "g" {
		t.Errorf("UserCredentials = %v, want github=g", in.UserCredentials)
	}
	// The meaningful zeros: pointers precisely so "off" survives, and exactly
	// what a hand-copy that tests only non-zero values would miss.
	for _, tc := range []struct {
		field string
		set   bool
	}{
		{"RetryAttempts", in.RetryAttempts != nil},
		{"MemoryInjectMaxTokens", in.MemoryInjectMaxTokens != nil},
		{"MemoryIndexMaxBytes", in.MemoryIndexMaxBytes != nil},
		{"InjectToolGuide", in.InjectToolGuide != nil},
		{"UnboundedIterations", in.UnboundedIterations != nil},
	} {
		if !tc.set {
			t.Errorf("%s is nil — its meaningful zero was dropped on the streaming path", tc.field)
		}
	}
}

// retune_run accepts a strictly SMALLER set than a spawn: the twelve per-run
// overrides plus interactive and interruption, and no sampling / compaction /
// context / max_context_tokens / metadata.
//
// Both directions are failures, and the second is the one that nearly shipped:
// a field the endpoint takes and the tool hides is unreachable, and a field the
// tool advertises and the endpoint ignores is a promise it does not keep. The
// first draft of this tool spliced the SPAWN fragment and would have advertised
// four fields that go nowhere.
//
// Derived from connector.RunOverrides, which is what the endpoint unmarshals.
func TestRetuneSchema_MatchesWhatTheEndpointAccepts(t *testing.T) {
	accepted := jsonFieldNames(t, reflect.TypeOf(connector.RunOverrides{}))
	if len(accepted) < 12 {
		t.Fatalf("derived only %d fields from connector.RunOverrides (%v) — the shape changed "+
			"and a vacuous pass here is how the tool drifts", len(accepted), accepted)
	}

	props := topLevelProperties(t, schemaObject(t, toolInputSchema(t, "retune_run")))
	advertised := make(map[string]bool, len(props))
	for name := range props {
		advertised[name] = true
	}

	var missing []string
	for _, name := range accepted {
		if !advertised[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("retune_run accepts but does not advertise: %s", strings.Join(missing, ", "))
	}

	// And nothing extra, beyond the handle itself.
	acc := make(map[string]bool, len(accepted))
	for _, n := range accepted {
		acc[n] = true
	}
	var extra []string
	for name := range advertised {
		if name != "agent_id" && !acc[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("retune_run advertises fields the endpoint does not accept: %s\n\nThey are "+
			"silently ignored, which is a promise the tool does not keep.", strings.Join(extra, ", "))
	}
}
