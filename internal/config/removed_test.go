package config

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const plainYAML = "defaults:\n  provider: mock\n  model: mock\n"

// Each removed shell variable set to a value refuses the start, naming the
// variable and what replaces it, never its value; set empty it is a dead
// line, advised.
func TestRetiredEnv_EachRemovedShellVariableRefusesTheStart(t *testing.T) {
	n := 0
	for _, r := range Removals() {
		if r.Kind != RemovedKindEnv {
			continue
		}
		n++
		t.Run(r.Name, func(t *testing.T) {
			t.Setenv(r.Name, "secret-value-7f3a")
			_, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte(plainYAML)})
			if err == nil {
				t.Fatal("the load passed with a removed variable set")
			}
			for _, want := range []string{r.Name, "removed in " + RemovedIn, r.Replacement, MigrationGuide + "#" + r.Section} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q lacks %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "secret-value-7f3a") {
				t.Errorf("the refusal carries the variable's value: %q", err)
			}

			t.Setenv(r.Name, "")
			cfg, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte(plainYAML)})
			if err != nil {
				t.Fatalf("set empty, the load refused: %v", err)
			}
			if !strings.Contains(strings.Join(cfg.Warnings, "\n"), r.Name+" is set but no longer read") {
				t.Errorf("set empty, no advisory names %s: %q", r.Name, cfg.Warnings)
			}
		})
	}
	if n != 6 {
		t.Fatalf("the table has %d removed variables, want the six RFC GW P15 removed", n)
	}
}

// Every variable row of the table is a refusing row of the retired table.
func TestRemoved_EveryEnvRowRefuses(t *testing.T) {
	for _, r := range Removals() {
		if r.Kind != RemovedKindEnv {
			continue
		}
		if _, ok := retiredEnv[r.Name]; !ok {
			t.Errorf("%s is not in retiredEnv", r.Name)
		}
		rule, ok := retiredEnvRefuse[r.Name]
		if !ok {
			t.Errorf("%s is not in retiredEnvRefuse", r.Name)
			continue
		}
		if !rule.loosens("x", retiredEnvCtx{}) {
			t.Errorf("%s set to a value is not refused", r.Name)
		}
	}
}

// Nothing in the sources reads a removed variable. The guard reads the table
// and the sources, and holds no list of its own.
func TestRemoved_NothingReadsARemovedName(t *testing.T) {
	repo := filepath.Join("..", "..")
	found := envNamesInSources(t, filepath.Join(repo, "cmd"), filepath.Join(repo, "internal"))
	for _, r := range Removals() {
		if r.Kind != RemovedKindEnv {
			continue
		}
		if file, read := found[r.Name]; read {
			t.Errorf("%s was removed in %s but %s still names it", r.Name, RemovedIn, file)
		}
	}
	for name, file := range found {
		if strings.HasPrefix(name, "LOOMCYCLE_BASH") {
			if _, retired := retiredEnv[name]; !retired {
				t.Errorf("%s (named in %s) is a shell variable the retired table does not hold", name, file)
			}
		}
	}
}

// An operator's yaml agent that names Bash or Bashbox refuses the start,
// with the line to change.
func TestLoad_AYamlAgentNamingARemovedToolRefusesTheStart(t *testing.T) {
	for _, tool := range []string{"Bash", "Bashbox"} {
		t.Run(tool, func(t *testing.T) {
			_, err := LoadLayers(Layer{Name: "op.yaml", Data: []byte(plainYAML + "agents:\n  coder:\n    tools: [Read, " + tool + "]\n")})
			if err == nil {
				t.Fatal("the load passed")
			}
			r, _ := RemovedTool(tool)
			for _, want := range []string{`agent "coder" in op.yaml: tools names ` + tool, "removed in " + RemovedIn, r.Replacement, MigrationGuide + "#" + r.Section} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q lacks %q", err, want)
				}
			}
		})
	}
}

// shell_credentials in an operator's yaml refuses the start, whatever it
// holds, the empty list included, and never names the credentials.
func TestLoad_SettingShellCredentialsRefusesTheStart(t *testing.T) {
	for _, value := range []string{"[]", "[cred-name-91c2]"} {
		t.Run(value, func(t *testing.T) {
			_, err := LoadLayers(Layer{Name: "op.yaml", Data: []byte(plainYAML + "agents:\n  coder:\n    tools: [Read]\n    shell_credentials: " + value + "\n")})
			if err == nil {
				t.Fatal("the load passed")
			}
			if !strings.Contains(err.Error(), `agent "coder" in op.yaml sets shell_credentials, which was removed in `+RemovedIn) {
				t.Errorf("refusal = %q", err)
			}
			if strings.Contains(err.Error(), "cred-name-91c2") {
				t.Errorf("the refusal names a credential: %q", err)
			}
		})
	}
}

// A markdown agent is the operator's too: its front matter is checked.
func TestLoad_AMarkdownAgentNamingARemovedItemRefusesTheStart(t *testing.T) {
	dir := t.TempDir()
	md := "---\nname: shell\ndescription: x\ntools: [Read, Bash]\nshell_credentials: [cred-name-91c2]\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "shell.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOMCYCLE_AGENTS_ROOT", dir)
	_, err := LoadLayers(Layer{Name: "op.yaml", Data: []byte(plainYAML)})
	if err == nil {
		t.Fatal("the load passed")
	}
	for _, want := range []string{`markdown agent "shell": tools names Bash`, `markdown agent "shell" sets shell_credentials`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "cred-name-91c2") {
		t.Errorf("the refusal names a credential: %q", err)
	}
}

// One error lists every use, so an operator fixes them in one pass.
func TestLoad_ListsEveryUseInOneError(t *testing.T) {
	_, err := LoadLayers(
		Layer{Name: "a.yaml", Data: []byte(plainYAML + "agents:\n  one:\n    tools: [Bash, Bashbox]\n")},
		Layer{Name: "b.yaml", Data: []byte("agents:\n  two:\n    tools: [Read]\n    shell_credentials: []\n")},
	)
	if err == nil {
		t.Fatal("the load passed")
	}
	for _, want := range []string{
		`agent "one" in a.yaml: tools names Bash,`, `agent "one" in a.yaml: tools names Bashbox,`,
		`agent "two" in b.yaml sets shell_credentials`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
}

// A bundle compiled into the binary is the product's, not the operator's: its
// own agents are not refused here (the bundle test holds that none names a
// removed item), while an operator's layer over it is.
func TestLoad_AnEmbeddedLayerIsNotRefused(t *testing.T) {
	bundle := Layer{Name: "chat", Embedded: true, Data: []byte("agents:\n  chat/medium:\n    tools: [Read, Bash]\n")}
	if _, err := LoadLayers(bundle, Layer{Name: "op.yaml", Data: []byte(plainYAML)}); err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	_, err := LoadLayers(bundle, Layer{Name: "op.yaml", Data: []byte(plainYAML + "agents:\n  chat/medium:\n    tools: [Read, Bashbox]\n")})
	if err == nil || !strings.Contains(err.Error(), `agent "chat/medium" in op.yaml: tools names Bashbox`) {
		t.Fatalf("an operator's Bashbox over a bundle agent: err = %v", err)
	}
}

// An agent with nothing removed loads; an mcp__sandbox__ name is an ordinary
// MCP tool name (RFC GW P15 Q2).
func TestLoad_NothingRemovedLoads(t *testing.T) {
	if _, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte(plainYAML + "agents:\n  a:\n    tools: [Read, Workspace, mcp__sandbox__sandbox_exec]\n")}); err != nil {
		t.Fatal(err)
	}
}

// RemovedTool answers for the table's tool names only, exactly.
func TestRemovedTool_MatchesTheTablesToolsOnly(t *testing.T) {
	for name, want := range map[string]bool{
		"Bash": true, "Bashbox": true,
		"mcp__sandbox__sandbox_exec": false, "bash": false, "BashboxX": false, "-Bash": false, "Read": false, "Workspace": false,
	} {
		if _, got := RemovedTool(name); got != want {
			t.Errorf("RemovedTool(%q) = %v, want %v", name, got, want)
		}
	}
}

// Each row's section exists in the migration guide, so every line's link
// lands somewhere.
func TestRemoved_EveryLineLinksASectionOfTheGuide(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", MigrationGuide))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	slugs := map[string]bool{}
	// GitHub's anchors: lower case, word characters, spaces as hyphens.
	nonWord := regexp.MustCompile(`[^a-z0-9_ -]`)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "#") {
			continue
		}
		h := strings.TrimSpace(strings.TrimLeft(line, "#"))
		slugs[strings.ReplaceAll(nonWord.ReplaceAllString(strings.ToLower(h), ""), " ", "-")] = true
	}
	for _, r := range Removals() {
		if !slugs[r.Section] {
			t.Errorf("%s links #%s, which no heading of %s makes", r.Name, r.Section, MigrationGuide)
		}
	}
}

// A stored row names shell_credentials in either spelling it was written
// with; a null (a registered agent's unset pointer) is not holding it.
func TestRemovedFieldsInJSON_ReadsBothSpellingsAndSkipsNull(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"shell_credentials":["A"]}`: true,
		`{"shell_credentials":[]}`:    true,
		`{"ShellCredentials":["A"]}`:  true,
		`{"ShellCredentials":null}`:   false,
		`{"tools":["Read"]}`:          false,
		`not json`:                    false,
	} {
		got := len(RemovedFieldsInJSON([]byte(raw))) == 1
		if got != want {
			t.Errorf("RemovedFieldsInJSON(%s) holds the field = %v, want %v", raw, got, want)
		}
	}
}

// The run-start notice: one capability_inert grant per removed item, gate
// "removed", naming the item and the migration guide.
func TestInertToolGrants_NamesEachRemovedItem(t *testing.T) {
	got := InertToolGrants(AgentDef{Tools: []string{"Read", "Bash"}, RemovedFields: []string{"shell_credentials"}})
	var names []string
	for _, g := range got {
		if g.Gate != RemovedGate {
			continue
		}
		names = append(names, g.Tool)
		if !strings.Contains(g.Message, "removed in "+RemovedIn) || !strings.Contains(g.Message, MigrationGuide) {
			t.Errorf("notice %q does not say it was removed and where to read", g.Message)
		}
	}
	if strings.Join(names, ",") != "Bash,shell_credentials" {
		t.Errorf("removed notices = %v, want [Bash shell_credentials]", names)
	}
}
