package snapshot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The older def sections are scanned where they name a credential: every
// header map (an MCP server's, an http HookDef's, the inline hook webhooks of
// an agent, a team and a channel), and an MCP server's url and stdio env.
// ${run.*} placeholders are not env vars, and a literal names nothing.
func TestCredentialScan_CoversTheOlderDefSections(t *testing.T) {
	ctx := context.Background()
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	now := triggerBase()
	mustOK := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := src.MCPServerDefCreate(ctx, store.MCPServerDefRow{DefID: "md_1", TenantID: "acme", Name: "peer", Version: 1, CreatedAt: now,
		Definition: json.RawMessage(`{"transport":"http","url":"https://${LOOMCYCLE_PEER_HOST}/mcp",` +
			`"headers":{"Authorization":"Bearer $cred:peer-token","X-Trace":"${run.id}","X-Plain":"on"}}`)})
	mustOK(err)
	_, err = src.MCPServerDefCreate(ctx, store.MCPServerDefRow{DefID: "md_2", TenantID: "acme", Name: "local", Version: 1, CreatedAt: now,
		Definition: json.RawMessage(`{"transport":"stdio","command":"/bin/peer","env":{"PEER_KEY":"${LOOMCYCLE_PEER_KEY}"}}`)})
	mustOK(err)
	_, err = src.HookDefCreate(ctx, store.HookDefRow{DefID: "hd_1", TenantID: "acme", Name: "gate", Version: 1, CreatedAt: now,
		Definition: json.RawMessage(`{"event":"pre","body":{"kind":"http","url":"https://h.example.test/g","headers":{"X-Key":"${LOOMCYCLE_GATE_KEY}"}}}`)})
	mustOK(err)
	_, err = src.AgentDefCreate(ctx, store.AgentDefRow{DefID: "ad_1", TenantID: "acme", Name: "helper", Version: 1, CreatedAt: now,
		Definition: json.RawMessage(`{"hooks":{"agent_start":[{"name":"a","url":"https://h.example.test/a","headers":{"Authorization":"$cred:agent-hook"}}]}}`)})
	mustOK(err)
	_, err = src.TeamDefCreate(ctx, store.TeamDefRow{DefID: "td_1", TenantID: "acme", Name: "crew", Version: 1, CreatedAt: now,
		Definition: json.RawMessage(`{"states":[{"state":"s","handler":{"kind":"agent","hooks":{"agent_start":[{"name":"t","url":"https://h.example.test/t","headers":{"X-T":"${LOOMCYCLE_TEAM_KEY}"}}]}}}]}`)})
	mustOK(err)
	mustOK(src.ChannelsCreate(ctx, store.ChannelRow{Name: "feed", TenantID: "acme", Scope: "tenant", Semantic: "queue", CreatedAt: now,
		Hooks: json.RawMessage(`{"channel_publish":[{"name":"c","url":"https://h.example.test/c","headers":{"X-C":"$cred:chan-hook"}}]}`)}))

	checks := &fakeCredChecks{env: map[string]bool{"LOOMCYCLE_TEAM_KEY": true}, creds: map[string]bool{"acme///chan-hook": true}}
	res := mustRestore(t, dst, mustCapture(t, src), checks.opts())
	for _, want := range [][]string{
		{"mcp_server_def acme/peer", "url", "LOOMCYCLE_PEER_HOST", "not set on this host"},
		{"mcp_server_def acme/peer", "headers.Authorization", "$cred:peer-token", `tenant "acme"`},
		{"mcp_server_def acme/local", "env.PEER_KEY", "LOOMCYCLE_PEER_KEY"},
		{"hook_def acme/gate", "body.headers.X-Key", "LOOMCYCLE_GATE_KEY"},
		{"agent_def acme/helper", "hooks.agent_start[0].headers.Authorization", "$cred:agent-hook"},
	} {
		if !hasWarning(res, append([]string{"missing credential:"}, want...)...) {
			t.Errorf("no missing-credential warning with %q; warnings:\n%s", want, strings.Join(res.Warnings, "\n"))
		}
	}
	for _, w := range res.Warnings {
		for _, quiet := range []string{"run.id", "X-Plain", "LOOMCYCLE_TEAM_KEY", "chan-hook"} {
			if strings.Contains(w, "missing credential") && strings.Contains(w, quiet) {
				t.Errorf("warned about %s, which is not a missing reference: %s", quiet, w)
			}
		}
	}
	// The team's and the channel's references were checked, so their silence
	// above is an answer, not a skip.
	if !strings.Contains(strings.Join(checks.asked, " "), "acme///chan-hook") {
		t.Errorf("the channel hook's credential was not checked (asked %v)", checks.asked)
	}
	if hasWarning(res, "missing-credential scan:", "not checked") {
		t.Errorf("a wired check reported references it did not check: %v", res.Warnings)
	}
}

// A refused definition names nothing: the scan covers only what was restored.
func TestCredentialScan_SkipsARefusedOlderDef(t *testing.T) {
	ctx := context.Background()
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	if _, err := src.MCPServerDefCreate(ctx, store.MCPServerDefRow{DefID: "md_bad", TenantID: "acme", Name: "crafted", Version: 1, CreatedAt: triggerBase(),
		Definition: json.RawMessage(`{"transport":"http","url":"https://refuse-me.example/mcp","headers":{"X-K":"${LOOMCYCLE_CRAFTED}"}}`)}); err != nil {
		t.Fatal(err)
	}
	opts := (&fakeCredChecks{}).opts()
	opts.Validators[migrations.SectionMCPServerDefs] = refuseMarked
	res := mustRestore(t, dst, mustCapture(t, src), opts)
	if hasWarning(res, "LOOMCYCLE_CRAFTED") {
		t.Errorf("a refused def's references were scanned: %v", res.Warnings)
	}
	if res.MCPServerDefsRefused != 1 {
		t.Fatalf("mcp_server_defs_refused = %d, want 1 (the fixture must be refused)", res.MCPServerDefsRefused)
	}
}
