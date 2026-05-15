// Package cases loads bench case YAML files from disk.
//
// A case is a single capability test: an input prompt, an expected
// shape on three axes (structural, functional, semantic), and a
// per-axis pass criterion. Cases live under bench/cases/<tier>/*.yaml
// and are loaded into a Case struct for the runner + grader pipeline.
package cases

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Case is one bench fixture. Fields mirror the YAML on disk verbatim;
// no derived state lives here.
type Case struct {
	ID           string   `yaml:"id"`
	Tier         string   `yaml:"tier"` // "low" | "middle"
	Description  string   `yaml:"description"`
	AllowedTools []string `yaml:"allowed_tools"`
	MaxTurns     int      `yaml:"max_turns"`
	InputText    string   `yaml:"input_text"`
	Expected     Expected `yaml:"expected"`
}

// Expected groups the three grading axes for the case.
type Expected struct {
	Structural Structural `yaml:"structural"`
	Functional Functional `yaml:"functional"`
	Semantic   Semantic   `yaml:"semantic"`
}

// Structural is the binary output-shape check.
//
// MustMatch / MustNotMatch are go regexp patterns applied to the final
// assistant text. Empty pattern = skip that side.
//
// Schema is a JSON-schema-ish object the final text must parse as.
// We use a lightweight in-tree validator (see grader/structural.go)
// rather than a full JSON Schema implementation: the cases declare a
// modest subset (required, properties, type, enum, min/maxLength,
// minimum/maximum, items, pattern, minItems/maxItems). Adding a third-
// party validator would be 100KB of dep for features the cases don't
// need.
//
// SchemaAfterSeparator is for mid-08-format-switching: the prose-then-
// JSON case. When set, the validator looks for an exact "\n---\n" in
// the text and validates only the JSON after it.
type Structural struct {
	MustMatch            string `yaml:"must_match"`
	MustNotMatch         string `yaml:"must_not_match"`
	Schema               string `yaml:"schema"`
	SchemaAfterSeparator string `yaml:"schema_after_separator"`
}

// Functional grades the tool-use trace. Each entry in ToolCalls is one
// expected tool call (or a constraint on the count). OrderStrict
// requires the calls to appear in the listed order; default false
// means any order is fine.
type Functional struct {
	ToolCalls         []ToolCall `yaml:"tool_calls"`
	OrderStrict       bool       `yaml:"order_strict"`
	ForbidRepeatCalls bool       `yaml:"forbid_repeat_calls"`
}

// ToolCall is one expected call. Name is the tool name (e.g.
// "mcp__jobs__getAgentContext"). ArgsMustInclude is a map of
// constraints on the tool input — see grader/functional.go for the
// supported keys (exact-match, contains, has_field, *_is_object).
//
// MinCalls and MaxCalls let cases declare "between K and N calls to
// this tool", useful for cases like batched ingest where the exact
// count isn't pinned.
type ToolCall struct {
	Name            string         `yaml:"name"`
	ArgsMustInclude map[string]any `yaml:"args_must_include"`
	MinCalls        int            `yaml:"min_calls"`
	MaxCalls        int            `yaml:"max_calls"`
}

// Semantic is the judge-model grading axis.
type Semantic struct {
	Rubric    string `yaml:"rubric"`
	Threshold int    `yaml:"threshold"` // 0-100; case passes if judge score >= threshold
}

// LoadAll loads every *.yaml under root/cases/<tier>/ and returns the
// cases sorted by ID for deterministic iteration. tier may be "low",
// "middle", or "" (both). root is bench/ on disk.
func LoadAll(root string, tier string) ([]Case, error) {
	var cases []Case
	tiers := []string{"low", "middle"}
	if tier != "" {
		tiers = []string{tier}
	}
	for _, t := range tiers {
		dir := filepath.Join(root, "cases", t)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
				continue
			}
			c, err := Load(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			if c.Tier != t {
				return nil, fmt.Errorf("%s: declared tier %q does not match directory %q", e.Name(), c.Tier, t)
			}
			cases = append(cases, c)
		}
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	return cases, nil
}

// Load reads one case file. Exposed for tests.
func Load(path string) (Case, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Case{}, fmt.Errorf("read %s: %w", path, err)
	}
	var c Case
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Case{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.ID == "" {
		return Case{}, fmt.Errorf("%s: id is required", path)
	}
	if c.Tier != "low" && c.Tier != "middle" {
		return Case{}, fmt.Errorf("%s: tier must be 'low' or 'middle', got %q", path, c.Tier)
	}
	if c.InputText == "" {
		return Case{}, fmt.Errorf("%s: input_text is required", path)
	}
	if c.MaxTurns <= 0 {
		c.MaxTurns = 4
	}
	return c, nil
}
