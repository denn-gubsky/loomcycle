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

// RFC GW P15 Q3: op=self names each removed item the run's definition still
// names, in table order, with what replaces it, and nothing else.
func TestContextSelf_ListsDroppedItems(t *testing.T) {
	self := func(p tools.RunPolicy) map[string]any {
		t.Helper()
		res, _ := (&Context{Cfg: &config.Config{}}).Execute(tools.WithRunPolicy(context.Background(), p), json.RawMessage(`{"op":"self"}`))
		if res.IsError {
			t.Fatalf("self: %s", res.Text)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("self JSON: %v", err)
		}
		return out
	}
	out := self(tools.RunPolicy{
		Tools:   []string{"Read", "Workspace"},
		Removed: []string{"shell_credentials", "Bash", "Bashbox"},
	})
	var got []string
	list, _ := out["deprecations"].([]any)
	for _, e := range list {
		m := e.(map[string]any)
		r, _ := m["replacement"].(string)
		if !strings.Contains(r, "removed in "+config.RemovedIn) || !strings.Contains(r, config.MigrationGuide) {
			t.Errorf("%v: the replacement does not say it was removed and where to read", m)
		}
		got = append(got, m["name"].(string))
	}
	if want := []string{"Bash", "Bashbox", "shell_credentials"}; !reflect.DeepEqual(got, want) {
		t.Errorf("deprecations = %v, want %v (table order)", got, want)
	}
	if _, ok := out["shell_credentials"]; ok {
		t.Errorf("op=self still reports shell_credentials: %v", out["shell_credentials"])
	}

	out = self(tools.RunPolicy{Tools: []string{"Read", "Workspace"}})
	if _, ok := out["deprecations"]; ok {
		t.Errorf("a run whose definition names nothing removed reports %v, want no field", out["deprecations"])
	}
}
