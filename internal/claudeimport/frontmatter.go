package claudeimport

import (
	"fmt"
	"strings"
)

// splitFrontmatter pulls the YAML frontmatter (if any) off the front
// of a Claude Code MD file and returns (yamlBytes, body, err).
//
// The shape is: leading "---\n", a YAML mapping body, a closing "---"
// line (either "\n---\n..." with body following, or trailing "\n---"
// with no body). An MD without a leading "---\n" is treated as body-
// only — yamlBytes is empty and body is the whole input.
//
// This is the same delimiter logic used by internal/agents/loader.go
// and internal/skills/loader.go (private parseAgent / parseSkill), but
// lifted into a generic splitter so the importer can feed yamlBytes
// into its own typed struct instead of the loaders' (which include
// loomcycle-specific defaults the importer shouldn't apply).
func splitFrontmatter(data []byte) (yamlBytes, body []byte, err error) {
	text := string(data)
	// Normalise CRLF to LF for the line-based delimiter scan.
	text = strings.ReplaceAll(text, "\r\n", "\n")

	if !strings.HasPrefix(text, "---\n") {
		return nil, []byte(text), nil
	}
	rest := text[len("---\n"):]

	// Closing delimiter is a line that is exactly "---". We accept
	// either "\n---\n..." or a trailing "\n---" with no body.
	endIdx := strings.Index(rest, "\n---\n")
	bodyOffset := -1
	if endIdx >= 0 {
		bodyOffset = endIdx + len("\n---\n")
	} else if strings.HasSuffix(rest, "\n---") {
		endIdx = len(rest) - len("\n---")
		bodyOffset = len(rest)
	} else {
		return nil, nil, fmt.Errorf("frontmatter has no closing ---")
	}

	yamlBytes = []byte(rest[:endIdx])
	if bodyOffset < len(rest) {
		body = []byte(rest[bodyOffset:])
	}
	return yamlBytes, body, nil
}
