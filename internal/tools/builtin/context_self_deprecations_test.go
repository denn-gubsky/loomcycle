package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC GW P14 (owner ruling Q5): op=self names each deprecated tool or field
// the run was given, with its replacement, and says nothing of the
// deployment's environment or of a credential the field lists.
func TestContextSelf_DeprecationsNameWhatTheRunUsesWithItsReplacement(t *testing.T) {
	cfg := &config.Config{Env: config.Env{BashAllowedCreds: []string{"GITHUB_TOKEN"}}}
	self := func(p tools.RunPolicy) (map[string]any, string) {
		t.Helper()
		res, _ := (&Context{Cfg: cfg}).Execute(tools.WithRunPolicy(context.Background(), p), json.RawMessage(`{"op":"self"}`))
		if res.IsError {
			t.Fatalf("self: %s", res.Text)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("self JSON: %v", err)
		}
		return out, res.Text
	}
	names := func(out map[string]any) []string {
		var got []string
		list, _ := out["deprecations"].([]any)
		for _, e := range list {
			m := e.(map[string]any)
			if r, _ := m["replacement"].(string); r == "" {
				t.Errorf("%v has no replacement", m)
			}
			got = append(got, m["name"].(string))
		}
		return got
	}

	keep := config.CredentialNames{"GITHUB_TOKEN"}
	out, raw := self(tools.RunPolicy{
		Tools:      []string{"Read", "Bashbox", "Bash", "mcp__sandbox__exec", "mcp__sandbox__read", "Workspace"},
		ShellCreds: tools.ShellCredsFor(&keep),
	})
	want := []string{"Bash", "Bashbox", "mcp__sandbox__", "shell_credentials"}
	if got := names(out); !reflect.DeepEqual(got, want) {
		t.Errorf("deprecations = %v, want %v (table order, one per row)", got, want)
	}
	list, _ := json.Marshal(out["deprecations"])
	if strings.Contains(string(list), "GITHUB_TOKEN") || strings.Contains(string(list), "LOOMCYCLE_") {
		t.Errorf("deprecations name a credential or a variable: %s", list)
	}
	_ = raw

	out, _ = self(tools.RunPolicy{Tools: []string{"Read", "Workspace"}})
	if _, ok := out["deprecations"]; ok {
		t.Errorf("a run using nothing deprecated reports %v, want no field", out["deprecations"])
	}
}
