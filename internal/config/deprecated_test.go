package config

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// deprecationLines are the load's warnings that announce a removal.
func deprecationLines(cfg *Config) []string {
	var out []string
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "will be removed in a future release") {
			out = append(out, w)
		}
	}
	return out
}

const plainYAML = "defaults:\n  provider: mock\n  model: mock\n"

// Every deprecated variable set gives one warning naming the variable, what
// replaces it and the migration guide's section, and never its value.
func TestDeprecated_EachVariableSetWarnsOnceNamingItsReplacement(t *testing.T) {
	for _, d := range Deprecations() {
		if d.Kind != DeprecatedKindEnv {
			continue
		}
		t.Run(d.Name, func(t *testing.T) {
			t.Setenv(d.Name, "secret-value-7f3a")
			cfg, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte(plainYAML)})
			if err != nil {
				t.Fatalf("LoadLayers: %v", err)
			}
			got := deprecationLines(cfg)
			if len(got) != 1 {
				t.Fatalf("got %d deprecation warnings, want 1: %q", len(got), got)
			}
			w := got[0]
			for _, want := range []string{d.Name, d.Replacement, MigrationGuide + "#" + d.Section} {
				if !strings.Contains(w, want) {
					t.Errorf("warning %q lacks %q", w, want)
				}
			}
			if strings.Contains(w, "secret-value-7f3a") {
				t.Errorf("a warning carries the variable's value: %q", w)
			}
		})
	}
}

// An agent the operator wrote that names a deprecated tool, or sets
// shell_credentials, is warned about by name; the credentials it lists are
// not named.
func TestDeprecated_AnOperatorsAgentWarnsForEachDeprecatedItem(t *testing.T) {
	yaml := plainYAML + `agents:
  shell:
    tools: [Read, Bash, Bashbox]
  sidecar:
    tools: [mcp__sandbox__sandbox_exec]
  narrowed:
    tools: [Read]
    shell_credentials: [cred-name-91c2]
  clean:
    tools: [Read, Workspace]
`
	cfg, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte(yaml)})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	got := strings.Join(deprecationLines(cfg), "\n")
	for _, want := range []string{
		`agent "shell" names Bash`, `agent "shell" names Bashbox`,
		`agent "sidecar" names mcp__sandbox__sandbox_exec`,
		`agent "narrowed" sets shell_credentials`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no warning %q in:\n%s", want, got)
		}
	}
	for _, not := range []string{`"clean"`, "cred-name-91c2"} {
		if strings.Contains(got, not) {
			t.Errorf("warnings name %s:\n%s", not, got)
		}
	}
	if n := len(deprecationLines(cfg)); n != 4 {
		t.Errorf("got %d deprecation warnings, want 4:\n%s", n, got)
	}
}

// A bundle's own agents are the product's, moved by later phases: they are
// not warned about one by one. What an operator adds to such an agent is.
func TestDeprecated_ABundlesOwnAgentIsNotWarnedButAnOperatorsAdditionIs(t *testing.T) {
	bundle := Layer{Name: "chat", Embedded: true, Data: []byte(`agents:
  chat/medium:
    tools: [Read, Bash]
`)}
	cfg, err := LoadLayers(bundle, Layer{Name: "t.yaml", Data: []byte(plainYAML)})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if got := deprecationLines(cfg); len(got) != 0 {
		t.Fatalf("a bundle's own agent was warned about: %q", got)
	}
	overlay := Layer{Name: "t.yaml", Data: []byte(plainYAML + `agents:
  chat/medium:
    tools: [Read, Bash, Bashbox]
`)}
	cfg, err = LoadLayers(bundle, overlay)
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	got := deprecationLines(cfg)
	if len(got) != 1 || !strings.Contains(got[0], `agent "chat/medium" names Bashbox`) {
		t.Fatalf("want one warning for the operator's Bashbox, got %q", got)
	}
}

// Selecting a deprecated bundle is warned about by the bundle's name.
func TestDeprecated_ADeprecatedBundleSelectedWarns(t *testing.T) {
	for _, name := range []string{"sandbox", "dev-exec"} {
		t.Run(name, func(t *testing.T) {
			cfg, err := LoadLayers(Layer{Name: name, Embedded: true, Data: []byte("{}\n")}, Layer{Name: "t.yaml", Data: []byte(plainYAML)})
			if err != nil {
				t.Fatalf("LoadLayers: %v", err)
			}
			got := deprecationLines(cfg)
			if len(got) != 1 || !strings.Contains(got[0], "the "+name+" bundle") {
				t.Fatalf("want one warning naming the %s bundle, got %q", name, got)
			}
		})
	}
	// An operator's own file of that name is not the bundle.
	cfg, err := LoadLayers(Layer{Name: "sandbox", Data: []byte(plainYAML)})
	if err != nil {
		t.Fatal(err)
	}
	if got := deprecationLines(cfg); len(got) != 0 {
		t.Fatalf("an operator's layer named sandbox was warned about: %q", got)
	}
}

func TestDeprecated_NothingDeprecatedInUseWarnsNothing(t *testing.T) {
	cfg, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte(plainYAML + "agents:\n  a:\n    tools: [Read, Workspace]\n")})
	if err != nil {
		t.Fatal(err)
	}
	if got := deprecationLines(cfg); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

// DeprecatedTool answers for the names the table holds and their prefixes.
func TestDeprecatedTool_MatchesTheTablesToolsOnly(t *testing.T) {
	for name, want := range map[string]bool{
		"Bash": true, "Bashbox": true, "mcp__sandbox__sandbox_exec": true, "mcp__sandbox__*": true,
		"bash": false, "BashboxX": false, "-Bash": false, "Read": false, "Workspace": false, "mcp__sandboxer__x": false,
	} {
		if _, got := DeprecatedTool(name); got != want {
			t.Errorf("DeprecatedTool(%q) = %v, want %v", name, got, want)
		}
	}
}

// Every LOOMCYCLE_BASH* name the sources spell out is either deprecated
// (still read, warned about) or retired (no longer read): a new read cannot
// slip in without a warning. The guard reads the sources, not a list.
func TestDeprecated_TableCoversEveryShellVariableTheSourcesName(t *testing.T) {
	repo := filepath.Join("..", "..")
	found := envNamesInSources(t, filepath.Join(repo, "cmd"), filepath.Join(repo, "internal"))
	n := 0
	for name, file := range found {
		if !strings.HasPrefix(name, "LOOMCYCLE_BASH") {
			continue
		}
		n++
		if _, retired := retiredEnv[name]; retired {
			continue
		}
		if !isDeprecatedEnv(name) {
			t.Errorf("%s (named in %s) is neither deprecated nor retired", name, file)
		}
	}
	if n == 0 {
		t.Fatal("the sources name no LOOMCYCLE_BASH* variable: the guard reads nothing")
	}
}

// Each row's section exists in the migration guide, so every warning's link
// lands somewhere.
func TestDeprecated_EveryWarningLinksASectionOfTheGuide(t *testing.T) {
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
	for _, d := range Deprecations() {
		if !slugs[d.Section] {
			t.Errorf("%s links #%s, which no heading of %s makes", d.Name, d.Section, MigrationGuide)
		}
	}
}
