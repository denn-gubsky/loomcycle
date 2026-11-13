// Package builtin is the help corpus embedded in the binary, and the two pure
// parsers every reader of it shares: the frontmatter split and the `## Examples`
// call-example parser.
//
// It is apart from internal/help because it is data, not a service. Package
// help sits in the runtime layer (it indexes topics into memory for search),
// while the config layer must read the same articles to validate a
// {{help:<topic>}} system-prompt placeholder at load (RFC GL P4). Keeping the
// corpus and its parsers here, importing nothing in this module, lets both read
// one copy of the files through one parser, so what config load accepts is
// exactly what prompt assembly renders and what Context op=help shows.
package builtin

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
)

//go:embed *.md tools
var corpus embed.FS

// FS returns the embedded corpus: top-level `<topic>.md` files and the
// `tools/` tree of tool and operation articles. Read-only by construction.
func FS() fs.FS { return corpus }

// SplitFrontmatter splits a help .md file into its YAML frontmatter text and
// its body (trimmed). The file must open with a `---` line and close the
// frontmatter with another; an empty body is refused as a likely typo.
func SplitFrontmatter(data []byte) (frontmatter, body string, err error) {
	src := string(data)
	if !strings.HasPrefix(src, "---\n") && !strings.HasPrefix(src, "---\r\n") {
		return "", "", fmt.Errorf("missing opening frontmatter `---`")
	}
	// Strip leading delimiter.
	src = strings.TrimPrefix(src, "---\n")
	src = strings.TrimPrefix(src, "---\r\n")
	// Find closing delimiter at start of a line.
	closeIdx := strings.Index(src, "\n---\n")
	if closeIdx < 0 {
		closeIdx = strings.Index(src, "\n---\r\n")
	}
	if closeIdx < 0 {
		return "", "", fmt.Errorf("missing closing frontmatter `---`")
	}
	frontmatter = src[:closeIdx]
	body = src[closeIdx:]
	// Skip past the closing delimiter line.
	body = strings.TrimPrefix(body, "\n---\n")
	body = strings.TrimPrefix(body, "\n---\r\n")
	// Body may be empty — operators can ship description-only
	// placeholder topics, but we reject it as a likely typo.
	body = strings.TrimSpace(body)
	if body == "" {
		return "", "", fmt.Errorf("empty topic body (frontmatter parsed; nothing after closing `---`)")
	}
	return frontmatter, body, nil
}

// Example is one call example in a help topic: the ARGUMENTS object a model
// would pass to Tool. Examples are written as the argument object alone —
// never a {name, arguments} envelope or a tagged block — because an envelope
// is exactly the shape a small model copies into its reply as text instead of
// making the call.
type Example struct {
	// Tool is the tool the example calls: the fence's `tool=` attribute, else
	// the article's own tool.
	Tool string
	// Input is the argument object, verbatim.
	Input json.RawMessage
	// Line is the 1-based line of the opening fence within the topic body, for
	// error messages.
	Line int
}

// ParseExamples returns the call examples in the body of the topic named
// name, whose own tool (for a tool or operation article) is tool. A call
// example is a fenced block whose info string starts with `json`, inside a
// `## Examples` section (the heading may carry a suffix: "## Examples —
// tenant scope"). Within that section a fence tagged `json result` shows what
// came back and is not a call. A fence may name its tool (```json
// tool=Memory), which is how a topic that is not a tool article — the scopes
// primer — carries checked examples.
//
// A malformed example is reported as an error rather than skipped: an example
// that does not parse teaches nothing and should not reach a model.
func ParseExamples(name, body, tool string) ([]Example, error) {
	var (
		out       []Example
		inSection bool
		inFence   bool
		isCall    bool
		fenceTool string
		fenceLine int
		lines     []string
	)
	for i, ln := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "```") {
			if !inFence {
				inFence = true
				fenceLine = i + 1
				lines = lines[:0]
				isCall, fenceTool = false, ""
				info := strings.Fields(strings.TrimPrefix(trimmed, "```"))
				if inSection && len(info) > 0 && info[0] == "json" {
					isCall = true
					for _, attr := range info[1:] {
						switch {
						case attr == "result":
							isCall = false
						case strings.HasPrefix(attr, "tool="):
							fenceTool = strings.TrimPrefix(attr, "tool=")
						}
					}
				}
				continue
			}
			inFence = false
			if !isCall {
				continue
			}
			raw := strings.Join(lines, "\n")
			var obj map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &obj); err != nil {
				return nil, fmt.Errorf("%s: example at line %d is not a JSON object: %w", name, fenceLine, err)
			}
			exTool := fenceTool
			if exTool == "" {
				exTool = tool
			}
			if exTool == "" {
				return nil, fmt.Errorf("%s: example at line %d names no tool (add tool=<Name> to the fence)", name, fenceLine)
			}
			out = append(out, Example{Tool: exTool, Input: json.RawMessage(raw), Line: fenceLine})
			continue
		}
		if inFence {
			lines = append(lines, ln)
			continue
		}
		if strings.HasPrefix(ln, "## ") {
			inSection = strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(ln, "## ")), "Examples")
		}
	}
	if inFence {
		return nil, fmt.Errorf("%s: unclosed code fence opened at line %d", name, fenceLine)
	}
	return out, nil
}
