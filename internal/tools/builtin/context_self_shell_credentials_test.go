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

// RFC EW D5: op=self names the credentials this run's Bash and Bashbox host
// commands receive after its agent's shell_credentials narrowed the
// operator's lists. Names only: no value appears, and a run of a deployment
// that lists no shell credentials reports nothing.
func TestContextSelf_ReportsTheEffectiveShellCredentials(t *testing.T) {
	cfg := &config.Config{Env: config.Env{
		BashAllowedCreds:            []string{"GITHUB_TOKEN", "NPM_TOKEN"},
		BashboxFallbackAllowedCreds: []string{"git:GITHUB_TOKEN", "NPM_TOKEN"},
	}}
	self := func(c *Context, declared *config.CredentialNames) (map[string]any, string) {
		t.Helper()
		ctx := tools.WithRunPolicy(context.Background(), tools.RunPolicy{ShellCreds: tools.ShellCredsFor(declared)})
		res, _ := c.Execute(ctx, json.RawMessage(`{"op":"self"}`))
		if res.IsError {
			t.Fatalf("self: %s", res.Text)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("self JSON: %v", err)
		}
		sc, _ := out["shell_credentials"].(map[string]any)
		return sc, res.Text
	}
	list := func(v any) []string {
		var out []string
		for _, s := range v.([]any) {
			out = append(out, s.(string))
		}
		return out
	}

	sc, _ := self(&Context{Cfg: cfg}, nil)
	if sc == nil || sc["narrowed"] != false || !reflect.DeepEqual(list(sc["bash"]), []string{"GITHUB_TOKEN", "NPM_TOKEN"}) {
		t.Errorf("absent field: %v, want every operator name and narrowed=false", sc)
	}

	keep := config.CredentialNames{"GITHUB_TOKEN"}
	sc, raw := self(&Context{Cfg: cfg}, &keep)
	if sc["narrowed"] != true {
		t.Errorf("narrowed = %v, want true", sc["narrowed"])
	}
	if got := list(sc["bash"]); !reflect.DeepEqual(got, []string{"GITHUB_TOKEN"}) {
		t.Errorf("bash = %v, want [GITHUB_TOKEN]", got)
	}
	if got := list(sc["bashbox_fallback"]); !reflect.DeepEqual(got, []string{"git:GITHUB_TOKEN"}) {
		t.Errorf("bashbox_fallback = %v, want [git:GITHUB_TOKEN] (the binding kept)", got)
	}
	if strings.Contains(raw, "NPM_TOKEN") {
		t.Errorf("a credential the agent gave up is still reported: %s", raw)
	}

	none := config.CredentialNames{}
	if sc, _ := self(&Context{Cfg: cfg}, &none); len(list(sc["bash"])) != 0 || len(list(sc["bashbox_fallback"])) != 0 {
		t.Errorf("[]: %v, want empty lists", sc)
	}

	if sc, _ := self(&Context{Cfg: &config.Config{}}, nil); sc != nil {
		t.Errorf("no operator lists: reported %v, want nothing", sc)
	}
}
