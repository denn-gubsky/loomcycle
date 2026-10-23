package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC FB: op=self lists the Git network operations this run may perform, as
// a list that is empty (not absent) when the run has none.
func TestContextSelf_ReportsTheEffectiveGitRemote(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy tools.GitRemotePolicyValue
		want   []any
	}{
		{"no network", tools.GitRemotePolicyValue{}, []any{}},
		{"fetch", tools.GitRemotePolicyValue{Fetch: true}, []any{"fetch"}},
		{"fetch and push", tools.GitRemotePolicyValue{Fetch: true, Push: true}, []any{"fetch", "push"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tools.WithRunPolicy(context.Background(), tools.RunPolicy{GitRemote: tc.policy})
			res, _ := (&Context{Cfg: &config.Config{}}).Execute(ctx, json.RawMessage(`{"op":"self"}`))
			if res.IsError {
				t.Fatalf("self: %s", res.Text)
			}
			var out map[string]any
			if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
				t.Fatalf("self JSON: %v", err)
			}
			got, present := out["git_remote"]
			if !present {
				t.Fatalf("git_remote missing from op=self: %s", res.Text)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("git_remote = %#v, want %#v", got, tc.want)
			}
		})
	}
}
