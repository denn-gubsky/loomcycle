package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// op=self names the statement methods this run may stamp, so a code agent can
// tell before it writes whether a rule: stamp will be accepted. A run holding
// none (every model agent) is not told about a grant it can never have.
func TestContextSelf_ReportsTheStatementMethodsGrant(t *testing.T) {
	for _, tc := range []struct {
		name    string
		methods []string
		want    any // nil = the key is absent
	}{
		{"none", nil, nil},
		{"rule", []string{"rule"}, []any{"rule"}},
		{"rule and extractor", []string{"rule", "extractor"}, []any{"rule", "extractor"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tools.WithRunPolicy(context.Background(), tools.RunPolicy{StatementMethods: tc.methods})
			res, _ := (&Context{Cfg: &config.Config{}}).Execute(ctx, json.RawMessage(`{"op":"self"}`))
			if res.IsError {
				t.Fatalf("self: %s", res.Text)
			}
			var out map[string]any
			if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
				t.Fatalf("self JSON: %v", err)
			}
			got, present := out["statement_methods"]
			if tc.want == nil {
				if present {
					t.Errorf("statement_methods = %#v on a run holding none, want it absent", got)
				}
				return
			}
			if !present {
				t.Fatalf("statement_methods missing from op=self: %s", res.Text)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("statement_methods = %#v, want %#v", got, tc.want)
			}
		})
	}
}
