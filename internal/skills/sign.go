// sign.go — deterministic SHA-256 signing of a skill's content fields.
// Mirror of internal/agents/sign.go for the SkillDef substrate
// (v0.8.22). Same algorithm + same encoding rules + same prefix on
// the output string, only the field set differs (skills are smaller —
// body + allowed_tools + description, plus the name).
//
// See agents/sign.go for the architectural rationale (canonical
// encoding, why we don't pull RFC 8785, the CLI / runtime / verify
// triangle that uses this same code).
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// SkillContent is the closed set of fields that participate in the
// skill content-hash. Field order matches alphabetical json-tag
// order for the same stability reason as AgentContent.
type SkillContent struct {
	AllowedTools []string `json:"allowed_tools,omitempty"`
	Body         string   `json:"body,omitempty"`
	Description  string   `json:"description,omitempty"`
	Name         string   `json:"name,omitempty"`
}

// Sign returns "sha256:" + the lowercase-hex SHA-256 of the canonical
// JSON encoding of c. See agents.Sign for the rule set.
func Sign(c SkillContent) string {
	normalize(&c)
	buf, err := json.Marshal(c)
	if err != nil {
		buf = []byte("{}")
	}
	sum := sha256.Sum256(buf)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func normalize(c *SkillContent) {
	if len(c.AllowedTools) == 0 {
		c.AllowedTools = nil
	}
	// Skill body is the primary content. Normalise line endings and
	// trim trailing whitespace so editor drift doesn't cause spurious
	// hash mismatches. Internal whitespace (paragraph spacing, code-
	// block indentation) is preserved verbatim.
	c.Body = normalizeText(c.Body)
}

func normalizeText(s string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, " \t\n\r")
	return s
}

// FromSkill builds a SkillContent from a parsed *Skill (boot-time
// load + CLI `hash skill` subcommand path). The Path field is
// excluded.
func FromSkill(s *Skill) SkillContent {
	if s == nil {
		return SkillContent{}
	}
	return SkillContent{
		Name:         s.Name,
		Description:  s.Description,
		AllowedTools: s.AllowedTools,
		Body:         s.Body,
	}
}

// FromOverlay parses a JSON overlay (`SkillDef set` / `SkillDef fork`
// wire form) into a SkillContent. Unknown fields silently dropped.
func FromOverlay(definition json.RawMessage) (SkillContent, error) {
	var c SkillContent
	if len(definition) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(definition, &c); err != nil {
		return SkillContent{}, err
	}
	return c, nil
}
