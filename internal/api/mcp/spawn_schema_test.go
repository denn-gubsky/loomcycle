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
