// Package mcp owns the content-signature for MCPServerDef rows —
// the third substrate primitive after AgentDef (v0.8.5) and SkillDef
// (v0.8.22). Mirrors the canonical-encoding rules from internal/agents
// and internal/skills so the same operator who ran `loomcycle hash
// agent` against a YAML file gets a familiar workflow when verifying a
// dynamically-registered MCP server's content.
//
// What gets hashed (content-only — NOT metadata, identity, or cache):
//
//	name, description, transport ("http" | "streamable-http"), url,
//	headers (map<string, string>)
//
// Explicitly excluded:
//   - def_id, version, parent_def_id, created_at, created_by_*,
//     retired, bootstrapped_from_static (lifecycle / identity).
//   - discovered_tools (cache; refreshed via `rediscover` op, not part
//     of the operator's authored content).
//
// Canonical encoding rules (mirror of agents.Sign / skills.Sign):
//   - Go's encoding/json renders struct fields in declaration order
//     and map keys in sorted order; both stable since Go 1.12.
//   - Empty strings and nil maps normalise out via `omitempty`.
//   - description has trailing whitespace stripped and CRLF normalised
//     to LF, same as system_prompt on the agent side.
//
// Output: "sha256:" + 64 lowercase hex chars. Matches Docker's image-
// digest convention and leaves room for future algorithms.
package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// MCPServerContent is the closed set of fields that participate in
// the content hash. Tag order matches alphabetical json-tag order so
// the underlying JSON encoding is stable — see internal/agents.Sign
// for the architectural rationale.
//
// Tag order is load-bearing; do NOT reorder without bumping the hash-
// format version on every existing row + re-running the backfill.
type MCPServerContent struct {
	Description string            `json:"description,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Name        string            `json:"name,omitempty"`
	Transport   string            `json:"transport,omitempty"`
	URL         string            `json:"url,omitempty"`
}

// Sign returns "sha256:" + the lowercase-hex SHA-256 of the canonical
// JSON encoding of c. See agents.Sign for the rule set.
func Sign(c MCPServerContent) string {
	normalize(&c)
	buf, err := json.Marshal(c)
	if err != nil {
		buf = []byte("{}")
	}
	sum := sha256.Sum256(buf)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// normalize collapses zero-equivalent values so a registration with
// `headers: {}` hashes identically to one with no headers key at all.
func normalize(c *MCPServerContent) {
	if len(c.Headers) == 0 {
		c.Headers = nil
	}
	c.Description = normalizeText(c.Description)
}

// normalizeText converts CRLF to LF and trims trailing whitespace.
// Idempotent. Identical to agents.normalizeText / skills.normalizeText.
func normalizeText(s string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, " \t\n\r")
	return s
}

// FromOverlay parses a JSON overlay (the structured form `MCPServerDef
// set` / `fork` passes over the wire) into an MCPServerContent. Unknown
// fields are silently dropped — same posture as agents.FromOverlay.
func FromOverlay(definition json.RawMessage) (MCPServerContent, error) {
	var c MCPServerContent
	if len(definition) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(definition, &c); err != nil {
		return MCPServerContent{}, err
	}
	return c, nil
}
