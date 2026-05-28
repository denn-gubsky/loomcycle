package claudeimport

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/denn-gubsky/loomcycle/internal/recipes"
)

// WriteEmittedRecipes flushes the report's EmitRecipePath entries
// (planned by walkMCP under opts.EmitRecipes) to disk. Caller invokes
// this only under --write; the dry-run reports describe what WOULD
// be written via MCPEntry.EmitRecipePath + EmitRecipeJSON.
//
// Refuses to clobber an existing overlay file without `force`. Each
// successful write contributes a one-line status string to the
// returned slice so the CLI can render a per-file summary; failures
// return at the first error (the operator should fix and re-run).
func WriteEmittedRecipes(report *ImportReport, force bool) ([]string, error) {
	if report == nil {
		return nil, nil
	}
	var written []string
	for _, m := range report.MCPServers {
		if m.EmitRecipePath == "" || m.EmitRecipeJSON == "" {
			continue
		}
		if _, err := os.Stat(m.EmitRecipePath); err == nil && !force {
			return written, fmt.Errorf("emit-recipes: %s already exists (use --force to overwrite)",
				m.EmitRecipePath)
		} else if err != nil && !os.IsNotExist(err) {
			return written, fmt.Errorf("emit-recipes: stat %s: %w", m.EmitRecipePath, err)
		}
		if err := os.WriteFile(m.EmitRecipePath, []byte(m.EmitRecipeJSON), 0o644); err != nil {
			return written, fmt.Errorf("emit-recipes: write %s: %w", m.EmitRecipePath, err)
		}
		written = append(written, fmt.Sprintf("wrote %s", m.EmitRecipePath))
	}
	return written, nil
}

// WriteSkillCopies copies SKILL.md files from the report's SkillEntry
// SourcePath → DestinationPath. Mirrors WriteEmittedRecipes's
// refuse-on-collision semantics. Called by the CLI under --write.
//
// Supplementary files in multi-file skills are NOT copied per the
// RFC sharp edge — operators decide what to bring over manually.
func WriteSkillCopies(report *ImportReport, force bool) ([]string, error) {
	if report == nil {
		return nil, nil
	}
	var written []string
	for _, s := range report.Skills {
		if s.SourcePath == "" || s.DestinationPath == "" {
			continue
		}
		if _, err := os.Stat(s.DestinationPath); err == nil && !force {
			return written, fmt.Errorf("skill copy: %s already exists (use --force to overwrite)",
				s.DestinationPath)
		} else if err != nil && !os.IsNotExist(err) {
			return written, fmt.Errorf("skill copy: stat %s: %w", s.DestinationPath, err)
		}
		if err := os.MkdirAll(parentDir(s.DestinationPath), 0o755); err != nil {
			return written, fmt.Errorf("skill copy: mkdir for %s: %w", s.DestinationPath, err)
		}
		data, err := os.ReadFile(s.SourcePath)
		if err != nil {
			return written, fmt.Errorf("skill copy: read %s: %w", s.SourcePath, err)
		}
		if err := os.WriteFile(s.DestinationPath, data, 0o644); err != nil {
			return written, fmt.Errorf("skill copy: write %s: %w", s.DestinationPath, err)
		}
		written = append(written, fmt.Sprintf("copied %s → %s", s.SourcePath, s.DestinationPath))
	}
	return written, nil
}

// WriteMCPToConfig appends each report.MCPServers entry's underlying
// recipe to the target yaml's `mcp_servers:` block via
// recipes.AppendToConfig (the canonical writer). Skips entries that
// were suppressed by --no-yaml (recipe still set, but the CLI's
// caller wants only the overlay write).
//
// Returns a per-server status string for the CLI render, or the
// first error encountered (so the operator fixes + re-runs).
func WriteMCPToConfig(report *ImportReport, target string, force bool) ([]string, error) {
	if report == nil {
		return nil, nil
	}
	var written []string
	for _, m := range report.MCPServers {
		if m.YAMLFragment == "" {
			// --no-yaml suppressed yaml emission for this entry.
			continue
		}
		rec, ok := m.recipe.(*recipes.Recipe)
		if !ok || rec == nil {
			return written, fmt.Errorf("mcp_servers.%s: walker did not produce a recipe", m.Name)
		}
		newContents, err := recipes.AppendToConfig(rec, target, recipes.AppendOptions{Force: force})
		if err != nil {
			return written, fmt.Errorf("AppendToConfig %s: %w", m.Name, err)
		}
		if err := os.WriteFile(target, newContents, 0o644); err != nil {
			return written, fmt.Errorf("write %s: %w", target, err)
		}
		written = append(written, fmt.Sprintf("wrote mcp_servers.%s into %s", m.Name, target))
	}
	return written, nil
}

// WriteAgentsToConfig appends each report.Agents entry's yaml
// fragment to the target yaml's `agents:` block. Manipulates the
// yaml.v3 Node tree directly so operator-authored comments survive.
// Refuses to clobber an existing agents.<name>: entry without
// `force` (mirrors recipes.AppendToConfig's semantics).
//
// Unlike MCP entries (which have a typed Recipe), the agents path
// builds a yaml.Node by parsing AgentEntry.YAMLFragment. The
// fragment is verified well-formed by the agent walker; the parse
// here is a re-trip.
func WriteAgentsToConfig(report *ImportReport, target string, force bool) ([]string, error) {
	if report == nil || len(report.Agents) == 0 {
		return nil, nil
	}

	// Read target. Missing = create-fresh.
	var source []byte
	if data, err := os.ReadFile(target); err == nil {
		source = data
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", target, err)
	}

	var doc yaml.Node
	if len(bytes.TrimSpace(source)) == 0 {
		doc = yaml.Node{
			Kind:    yaml.DocumentNode,
			Content: []*yaml.Node{{Kind: yaml.MappingNode}},
		}
	} else {
		if err := yaml.Unmarshal(source, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", target, err)
		}
		if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s: top-level must be a YAML mapping", target)
		}
	}
	root := doc.Content[0]

	// Find or create agents: mapping.
	var agentsValue *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "agents" {
			agentsValue = root.Content[i+1]
			break
		}
	}
	if agentsValue == nil {
		agentsKey := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "agents"}
		agentsValue = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, agentsKey, agentsValue)
	}
	if agentsValue.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: agents must be a mapping", target)
	}

	var written []string
	for _, a := range report.Agents {
		// Parse the fragment as a YAML document; the fragment's top
		// level is "<name>: ...".
		var fragDoc yaml.Node
		if err := yaml.Unmarshal([]byte(a.YAMLFragment), &fragDoc); err != nil {
			return written, fmt.Errorf("parse agent %s yaml: %w", a.Name, err)
		}
		if fragDoc.Kind != yaml.DocumentNode || len(fragDoc.Content) == 0 ||
			fragDoc.Content[0].Kind != yaml.MappingNode || len(fragDoc.Content[0].Content) < 2 {
			return written, fmt.Errorf("agent %s: fragment top-level is not a mapping with one entry", a.Name)
		}
		// First (and only) key/value pair in the fragment.
		fragKey := fragDoc.Content[0].Content[0]
		fragVal := fragDoc.Content[0].Content[1]

		// Collision check.
		collision := -1
		for i := 0; i+1 < len(agentsValue.Content); i += 2 {
			if agentsValue.Content[i].Value == a.Name {
				collision = i
				break
			}
		}
		if collision >= 0 {
			if !force {
				return written, fmt.Errorf("agents.%s already exists in %s (use --force to overwrite)",
					a.Name, target)
			}
			agentsValue.Content[collision+1] = fragVal
		} else {
			agentsValue.Content = append(agentsValue.Content, fragKey, fragVal)
		}
		written = append(written, fmt.Sprintf("wrote agents.%s into %s", a.Name, target))
	}

	// Marshal the modified document.
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return written, fmt.Errorf("marshal %s: %w", target, err)
	}
	if err := enc.Close(); err != nil {
		return written, fmt.Errorf("close marshal %s: %w", target, err)
	}
	if err := os.WriteFile(target, buf.Bytes(), 0o644); err != nil {
		return written, fmt.Errorf("write %s: %w", target, err)
	}
	return written, nil
}

// parentDir returns the directory portion of a filesystem path. Stdlib
// has filepath.Dir but we keep a tiny wrapper so the call sites read
// linearly without an import shuffle.
func parentDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			if i == 0 {
				return p[:1]
			}
			return p[:i]
		}
	}
	return "."
}
