package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/denn-gubsky/loomcycle/internal/teamspec"
)

// teams.go — the top-level `teams:` map (RFC GS P1).
//
// A team in the yaml is written in the keys TeamDef create takes, so there is
// no second schema: the entry's graph keys are turned into the JSON the API
// would have been sent. The load checks what it can from here — the name, the
// size, definition_file, that there is a graph at all. The graph itself is
// teamgraph's to judge, which sits above config: every caller that loads a
// config to use it (the server's boot and reload, `validate`, `doctor`) runs
// teamgraph.CheckConfigTeams next. The checks that need the running deployment
// — a team's own agents against the AgentDef rules, channel and decision
// authority — are the create path's own, and run when the boot writes the team
// into the store (P2).

// Team is one entry of the top-level `teams:` map.
type Team struct {
	// Description is the team's one-line summary, as TeamDef create's.
	Description string
	// DefinitionFile names a JSON or YAML file holding the graph, in place of
	// the inline keys. A relative path is read from the directory of the last
	// config file, as system_prompt_file is.
	DefinitionFile string
	// Definition is the graph as JSON — the inline keys, or the file once
	// resolveTeams has read it.
	Definition json.RawMessage
}

// teamMetaKeys are the entry's keys that are not part of the graph.
const (
	teamKeyDescription    = "description"
	teamKeyDefinitionFile = "definition_file"
)

// UnmarshalYAML takes description and definition_file and keeps every other
// key as the graph, untouched: which keys a graph may have is the
// definition's to say (teamgraph.KeyIssues), not this decoder's.
func (t *Team) UnmarshalYAML(n *yaml.Node) error {
	var m map[string]any
	if err := n.Decode(&m); err != nil {
		return err
	}
	var err error
	if t.Description, err = teamStringKey(m, teamKeyDescription); err != nil {
		return err
	}
	if t.DefinitionFile, err = teamStringKey(m, teamKeyDefinitionFile); err != nil {
		return err
	}
	t.Definition = nil
	if len(m) > 0 {
		raw, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("team graph: %w", err)
		}
		t.Definition = raw
	}
	return nil
}

// teamStringKey removes key from m and returns its string value.
func teamStringKey(m map[string]any, key string) (string, error) {
	v, ok := m[key]
	delete(m, key)
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string, not %T", key, v)
	}
	return s, nil
}

// MarshalYAML writes the team as resolved — the graph inline, with no
// definition_file — so a reload's section diff sees a changed file, and what
// it writes loads back as the same team.
func (t Team) MarshalYAML() (any, error) {
	m := map[string]any{}
	if len(t.Definition) > 0 {
		if err := json.Unmarshal(t.Definition, &m); err != nil {
			return nil, err
		}
	}
	if t.Description != "" {
		m[teamKeyDescription] = t.Description
	}
	return m, nil
}

// resolveTeams reads each team's definition_file and checks every team's name
// and size, and that it has a graph. Every problem in every team is reported,
// each at its yaml path, and any one stops the load.
func resolveTeams(cfg *Config, configPath string) error {
	if len(cfg.Teams) == 0 {
		return nil
	}
	configDir, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	names := make([]string, 0, len(cfg.Teams))
	for name := range cfg.Teams {
		names = append(names, name)
	}
	sort.Strings(names)
	var problems []string
	for _, name := range names {
		team := cfg.Teams[name]
		if err := readTeamFile(&team, configDir); err != nil {
			problems = append(problems, fmt.Sprintf("teams.%s: %v", name, err))
			continue
		}
		cfg.Teams[name] = team
		for _, p := range teamProblems(name, team) {
			problems = append(problems, "teams."+name+p)
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// readTeamFile replaces a team's definition_file with the graph it holds. A
// file of JSON is kept byte for byte, so its keys are checked as written; any
// other file is read as YAML.
//
// SECURITY: as system_prompt_file — the yaml is trusted operator input, and
// the path may leave the config directory.
func readTeamFile(team *Team, configDir string) error {
	if team.DefinitionFile == "" {
		return nil
	}
	if len(team.Definition) > 0 {
		return fmt.Errorf("definition_file and an inline graph are mutually exclusive; give one")
	}
	p := team.DefinitionFile
	if !filepath.IsAbs(p) {
		p = filepath.Join(configDir, p)
	}
	// #nosec G304 -- definition_file from static config, relative to the config dir; no runtime def can set it
	body, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("read definition_file: %w", err)
	}
	if json.Valid(body) {
		team.Definition = body
		return nil
	}
	var m map[string]any
	if err := yaml.Unmarshal(body, &m); err != nil {
		return fmt.Errorf("definition_file %s: %w", p, err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("definition_file %s: %w", p, err)
	}
	team.Definition = raw
	return nil
}

// teamProblems is what the load refuses in one team, each as ": …" after the
// team's yaml path.
func teamProblems(name string, team Team) []string {
	var out []string
	if err := teamspec.ValidateName(name); err != nil {
		out = append(out, ": "+err.Error())
	}
	if len(team.Definition) == 0 {
		return append(out, ": no graph: give entry, states and transitions, or definition_file")
	}
	if len(team.Definition) > teamspec.MaxDefinitionBytes {
		out = append(out, fmt.Sprintf(": definition (%d bytes) exceeds max %d", len(team.Definition), teamspec.MaxDefinitionBytes))
	}
	return out
}
