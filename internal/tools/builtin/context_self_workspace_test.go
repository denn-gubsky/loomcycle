package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC GW: op=self reports the run's workspace grant, present (ops [], open
// and share false) when the run has none.
func TestContextSelf_ReportsTheEffectiveWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy tools.WorkspacePolicyValue
		want   map[string]any
	}{
		{"none", tools.WorkspacePolicyValue{}, map[string]any{"ops": []any{}, "open": false, "share": false, "templates": false, "egress": []any{}, "engine": ""}},
		{"read and exec in containers", tools.WorkspacePolicyValue{Read: true, Exec: true, Engine: "container"},
			map[string]any{"ops": []any{"read", "exec"}, "open": false, "share": false, "templates": false, "egress": []any{}, "engine": "container"}},
		{"open", tools.WorkspacePolicyValue{Write: true, Open: true},
			map[string]any{"ops": []any{"write"}, "open": true, "share": false, "templates": false, "egress": []any{}, "engine": ""}},
		{"share", tools.WorkspacePolicyValue{Read: true, Share: true},
			map[string]any{"ops": []any{"read"}, "open": false, "share": true, "templates": false, "egress": []any{}, "engine": ""}},
		// RFC GW P4: the routes a workspace the run opens may reach.
		{"egress", tools.WorkspacePolicyValue{Read: true, Open: true, Egress: []string{"pypi", "npm"}},
			map[string]any{"ops": []any{"read"}, "open": true, "share": false, "templates": false, "egress": []any{"pypi", "npm"}, "engine": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tools.WithRunPolicy(context.Background(), tools.RunPolicy{Workspace: tc.policy})
			res, _ := (&Context{Cfg: &config.Config{}}).Execute(ctx, json.RawMessage(`{"op":"self"}`))
			if res.IsError {
				t.Fatalf("self: %s", res.Text)
			}
			var out map[string]any
			if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
				t.Fatalf("self JSON: %v", err)
			}
			if got := out["workspace"]; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("workspace = %#v, want %#v", got, tc.want)
			}
		})
	}
}
