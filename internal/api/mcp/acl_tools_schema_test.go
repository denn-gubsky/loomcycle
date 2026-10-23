package mcp

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// Each access-control tool's input schema is valid JSON whose op enum is the
// family's op table, and keeps the family's own fields beside op and tenant —
// so a fragment that stopped parsing (aclToolSchema drops it rather than
// splice it into the schema) fails here instead of shipping a bare schema.
func TestACLToolSchemas_AreValidWithTheOpTable(t *testing.T) {
	for _, d := range aclToolDescriptors() {
		var schema struct {
			Type       string                     `json:"type"`
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(d.InputSchema, &schema); err != nil {
			t.Errorf("%s: schema is not valid JSON: %v", d.Name, err)
			continue
		}
		if schema.Type != "object" || !reflect.DeepEqual(schema.Required, []string{"op"}) {
			t.Errorf("%s: type %q required %v, want object and [op]", d.Name, schema.Type, schema.Required)
		}
		var op struct {
			Enum []string `json:"enum"`
		}
		if err := json.Unmarshal(schema.Properties["op"], &op); err != nil || !reflect.DeepEqual(op.Enum, connector.ACLManagementOps(d.Name)) {
			t.Errorf("%s: op enum %v, want the op table %v (err %v)", d.Name, op.Enum, connector.ACLManagementOps(d.Name), err)
		}
		if _, ok := schema.Properties["tenant"]; !ok {
			t.Errorf("%s: no tenant property", d.Name)
		}
		if len(schema.Properties) <= 2 {
			t.Errorf("%s: only op and tenant — the family's own fields were lost", d.Name)
		}
	}
}
