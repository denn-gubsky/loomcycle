package mcp

import (
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
	accepted := jsonFieldNames(t, reflect.TypeOf(connector.SpawnRunRequest{}))
	// A floor, so a renamed struct or a broken tag parse fails loudly instead of
	// passing vacuously against an empty expectation.
	if len(accepted) < 25 {
		t.Fatalf("only derived %d json fields from connector.SpawnRunRequest (%v) — the shape "+
			"probably changed, and a vacuous pass here is how the schema drifts", len(accepted), accepted)
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
// skipping the ones marked `json:"-"` (not part of the wire shape).
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
