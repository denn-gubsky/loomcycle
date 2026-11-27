package builtin

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC GW P15 Q1: a NEW definition naming what v1.172.0 removed is refused
// with the line to change; the author can fix the request.
func TestAgentDefCreate_NamingARemovedItemIsRefused(t *testing.T) {
	tool, ctx := ceilingFixture(t)
	for overlay, item := range map[string]string{
		`{"tier":"middle","system_prompt":"s","tools":["Read","Bash"]}`:                            "tools names Bash",
		`{"tier":"middle","system_prompt":"s","tools":["Bashbox"]}`:                                "tools names Bashbox",
		`{"tier":"middle","system_prompt":"s","tools":["Read"],"shell_credentials":["cred-91c2"]}`: "sets shell_credentials",
		`{"tier":"middle","system_prompt":"s","tools":["Read"],"shell_credentials":[]}`:            "sets shell_credentials",
	} {
		res, err := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"sdlc/new","overlay":`+overlay+`}`))
		if err != nil {
			t.Fatal(err)
		}
		wantRefused(t, res, overlay, item, "removed in "+config.RemovedIn, config.MigrationGuide)
		if strings.Contains(res.Text, "cred-91c2") {
			t.Errorf("the refusal names a credential: %s", res.Text)
		}
	}
	// A fork's overlay is new authoring too.
	putActive(t, tool, ctx, "sdlc/forkme", `{"tier":"middle","system_prompt":"s","tools":["Read"]}`)
	wantRefused(t, forkPromote(t, tool, ctx, "sdlc/forkme", `{"tools":["Read","Bash"]}`), "fork overlay naming Bash", "tools names Bash")
}

// RFC GW P15 Q1: a fork that only INHERITS a removed item from the version it
// forks is not refused: the item is stripped, the new version does not carry
// it, and the result says what went.
func TestAgentDefFork_InheritingARemovedItemStripsIt(t *testing.T) {
	tool, ctx := ceilingFixture(t)
	putActive(t, tool, ctx, "sdlc/old", `{"tier":"middle","system_prompt":"s","tools":["Read","Bash","Bashbox"],"shell_credentials":["cred-91c2"]}`)

	res := forkPromote(t, tool, ctx, "sdlc/old", `{"system_prompt":"rewritten"}`)
	if res.IsError {
		t.Fatalf("a fork inheriting a removed item was refused: %s", res.Text)
	}
	var out struct {
		Removed []string `json:"removed"`
		Note    string   `json:"note"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Removed, []string{"Bash", "Bashbox", "shell_credentials"}) || !strings.Contains(out.Note, config.RemovedIn) {
		t.Errorf("result removed=%v note=%q, want the three items named", out.Removed, out.Note)
	}
	if strings.Contains(res.Text, "cred-91c2") {
		t.Errorf("the result names a credential: %s", res.Text)
	}
	d, ok := activeDef(t, tool, ctx, "sdlc/old")
	if !ok {
		t.Fatal("the fork is not active")
	}
	if !slices.Equal(d.Tools, []string{"Read"}) || d.SystemPrompt != "rewritten" {
		t.Errorf("new version tools=%v prompt=%q, want [Read] and the rewritten prompt", d.Tools, d.SystemPrompt)
	}
	row, err := tool.Store.AgentDefGetActive(ctx, "t1", "sdlc/old")
	if err != nil {
		t.Fatal(err)
	}
	if got := config.RemovedFieldsInJSON(row.Definition); len(got) != 0 {
		t.Errorf("the new version still holds %v", got)
	}

	// A fork of a clean version says nothing.
	res = forkPromote(t, tool, ctx, "sdlc/old", `{"system_prompt":"again"}`)
	if res.IsError || strings.Contains(res.Text, `"removed"`) {
		t.Errorf("a clean fork: %s", res.Text)
	}
}

// putActive stores one active version of name in the author's tenant, as an
// earlier release would have written it.
func putActive(t *testing.T, tool *AgentDef, ctx context.Context, name, definition string) {
	t.Helper()
	if _, err := tool.Store.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "def_" + strings.ReplaceAll(name, "/", "_"), Name: name, Version: 1, TenantID: "t1",
		Definition: []byte(definition), CreatedAt: time.Now(), OperatorAuthored: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tool.Store.AgentDefSetActive(ctx, "t1", name, "def_"+strings.ReplaceAll(name, "/", "_"), ""); err != nil {
		t.Fatal(err)
	}
}
