package builtin

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// The body validators a snapshot restore runs over the def sections it has
// carried longest: agent, skill, team, hook and MCP server defs. Each must
// refuse what the tool's create/fork refuses on THIS host and accept what an
// author here could store.
func TestExistingDefBodyValidators_RefuseWhatAuthoringRefuses(t *testing.T) {
	cfg := &config.Config{}
	cfg.Env.HTTPHostAllowlist = []string{"mcp.example.com"}
	cfg.Env.HTTPPrivateHostAllowlist = []string{"localhost"}
	mcp := MCPServerDefBodyValidator(cfg)
	stdioCfg := &config.Config{}
	stdioCfg.Env.MCPAllowDynamicStdio = true
	mcpStdio := MCPServerDefBodyValidator(stdioCfg)
	hookNoCode := HookDefBodyValidator(nil)
	hookCode := HookDefBodyValidator(func(src string) error {
		if strings.Contains(src, "syntax error") {
			return errors.New("SyntaxError: unexpected token")
		}
		return nil
	})

	cases := []struct {
		name     string
		validate func(json.RawMessage) error
		body     string
		wantErr  string // "" = must pass
	}{
		{"mcp allowlisted host", mcp, `{"transport":"streamable-http","url":"https://mcp.example.com/mcp"}`, ""},
		{"mcp private allowlist", mcp, `{"transport":"http","url":"http://localhost:3000/api/mcp"}`, ""},
		{"mcp host off the allowlist", mcp, `{"transport":"http","url":"https://evil.example.net/mcp"}`, "not in LOOMCYCLE_HTTP_HOST_ALLOWLIST"},
		{"mcp metadata IP", mcp, `{"transport":"http","url":"http://169.254.169.254/latest/meta-data/"}`, "not in LOOMCYCLE_HTTP_HOST_ALLOWLIST"},
		{"mcp file scheme", mcp, `{"transport":"http","url":"file:///etc/passwd"}`, "scheme must be http or https"},
		{"mcp no transport", mcp, `{"url":"https://mcp.example.com/mcp"}`, "transport is required"},
		{"mcp stdio without opt-in", mcp, `{"transport":"stdio","command":"/bin/sh"}`, "not allowed for dynamic registration"},
		{"mcp stdio with opt-in", mcpStdio, `{"transport":"stdio","command":"/usr/local/bin/peer"}`, ""},
		{"mcp stdio without command", mcpStdio, `{"transport":"stdio"}`, "requires a command"},
		{"mcp not a def", mcp, `{"headers":"x"}`, "does not decode"},

		{"agent plain", ValidateAgentDefBody, `{"system_prompt":"hi","tools":["Read"]}`, ""},
		{"agent inline hook", ValidateAgentDefBody,
			`{"tools":["Read"],"hooks":{"agent_start":[{"name":"audit","url":"https://hooks.example.com/a","headers":{"Authorization":"Bearer $cred:audit"}}]}}`, ""},
		{"agent hook header sets Host", ValidateAgentDefBody,
			`{"hooks":{"agent_start":[{"name":"audit","url":"https://hooks.example.com/a","headers":{"Host":"internal"}}]}}`, "set by the call itself"},
		{"agent hook header with a line break", ValidateAgentDefBody,
			`{"hooks":{"agent_start":[{"name":"audit","url":"https://hooks.example.com/a","headers":{"X-A":"a\r\nX-B: b"}}]}}`, "line break"},
		{"agent hook non-http url", ValidateAgentDefBody,
			`{"hooks":{"agent_start":[{"name":"audit","url":"file:///etc/passwd"}]}}`, "http:// or https://"},
		{"agent tool hook on a tool it lacks", ValidateAgentDefBody,
			`{"tools":["Read"],"tool_hooks":{"Bash":{"pre":["gate"]}}}`, "not in the agent's tools"},
		{"agent malformed", ValidateAgentDefBody, `{"tools":"Read"}`, "does not decode"},

		{"skill ok", ValidateSkillDefBody, `{"body":"Do the thing.","tools":["Read"]}`, ""},
		{"skill blank body", ValidateSkillDefBody, `{"body":"   "}`, "non-whitespace"},
		{"skill malformed", ValidateSkillDefBody, `[1]`, "does not decode"},

		{"team no hooks", ValidateTeamDefBody, `{"entry":"a","states":[{"state":"a","handler":{"kind":"terminal"}}]}`, ""},
		{"team walk hook bad header", ValidateTeamDefBody,
			`{"entry":"a","states":[],"hooks":{"run_end":[{"name":"n","url":"https://hooks.example.com/","headers":{"Content-Length":"1"}}]}}`, "set by the call itself"},
		{"team malformed", ValidateTeamDefBody, `{"states":"x"}`, "invalid JSON"},

		{"hook http", hookNoCode, `{"event":"pre","body":{"kind":"http","url":"https://hooks.example.com/g","headers":{"X-Token":"$cred:g"}},"fail_mode":"open"}`, ""},
		{"hook header sets Host", hookNoCode, `{"event":"pre","body":{"kind":"http","url":"https://hooks.example.com/g","headers":{"Host":"x"}}}`, "set by the call itself"},
		{"hook unknown field", hookNoCode, `{"event":"pre","body":{"kind":"http","url":"https://h.example.com/"},"extra":1}`, "does not decode"},
		{"hook unknown kind", hookNoCode, `{"event":"pre","body":{"kind":"shell"}}`, "is not code-js or http"},
		{"hook code, code hooks off", hookNoCode, `{"event":"pre","body":{"kind":"code-js","code":"function hook(){}"}}`, "code hooks are not enabled"},
		{"hook code compiles", hookCode, `{"event":"pre","body":{"kind":"code-js","code":"function hook(){}"}}`, ""},
		{"hook code does not compile", hookCode, `{"event":"pre","body":{"kind":"code-js","code":"syntax error"}}`, "body.code"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.validate(json.RawMessage(c.body))
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("refused a body an author could store: %v", err)
			case c.wantErr != "" && err == nil:
				t.Errorf("accepted a body authoring refuses (want %q)", c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Errorf("error %q does not say %q", err, c.wantErr)
			}
		})
	}
}
