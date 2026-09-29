package snapshot

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Capture findings (RFC DP §3.2 e, V11): a header value that looks like a
// literal credential travels as authored, and is REPORTED by location in the
// envelope's capture_findings section, the capture's warnings, and the restore
// warnings — never by value.

// Markers no other fixture spells, so a hit is this test's value.
const (
	markMCPBearer  = "dp1amark-mcp-bearer-literal-0123456789"
	markHookKey    = "dp1amark-hook-apikey-literal-0123456789"
	markChanToken  = "dp1amark-channel-token-literal-0123456789"
	markAgentAuth  = "dp1amark-agent-auth-literal-0123456789"
	markTeamToken  = "dp1amark-team-token-literal-0123456789"
	markRunCfgAuth = "dp1amark-runconfig-auth-literal-0123456789"
)

var allFindingMarks = []string{markMCPBearer, markHookKey, markChanToken, markAgentAuth, markTeamToken, markRunCfgAuth}

// plantLiteralHeaders writes one row per header-bearing location, each holding
// a literal credential, plus reference-form headers beside them that must not
// be reported. Returns the findings the capture must report, as
// "section|name|def_id|field".
func plantLiteralHeaders(t *testing.T, s store.Store) []string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("plant %s: %v", what, err)
		}
	}

	_, err := s.MCPServerDefCreate(ctx, store.MCPServerDefRow{
		DefID: "mcpdef_dp1a", TenantID: "acme", Name: "dp1a-mcp", Version: 1, CreatedAt: now,
		Definition: json.RawMessage(`{"transport":"streamable-http","url":"https://mcp.example.test/mcp","headers":{` +
			`"Authorization":"Bearer ` + markMCPBearer + `",` +
			`"X-Api-Key":"${LOOMCYCLE_DP1A_KEY}",` +
			`"X-Session-Token":"$cred:dp1a-session",` +
			`"Proxy-Authorization":"Bearer ${LOOMCYCLE_DP1A_PROXY}",` +
			`"Content-Type":"application/json"}}`),
	})
	must("mcp_server_defs", err)

	_, err = s.HookDefCreate(ctx, store.HookDefRow{
		DefID: "hdf_dp1a", TenantID: "acme", Name: "dp1a-gate", Version: 1, CreatedAt: now,
		Definition: json.RawMessage(`{"event":"pre","body":{"kind":"http","url":"https://hooks.example.test/gate",` +
			`"headers":{"X-Api-Key":"` + markHookKey + `","X-Trace":"on"}}}`),
	})
	must("hook_defs", err)

	must("channels", s.ChannelsCreate(ctx, store.ChannelRow{
		Name: "dp1a-feed", TenantID: "acme", Scope: "tenant", Semantic: "queue", CreatedAt: now,
		Hooks: json.RawMessage(`{"channel_publish":["dp1a-gate",{"name":"inline","url":"https://hooks.example.test/c",` +
			`"headers":{"X-Channel-Token":"` + markChanToken + `"}}]}`),
	}))

	_, err = s.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "adf_dp1a", Name: "dp1a-agent", Version: 1, CreatedAt: now,
		Definition: json.RawMessage(`{"system_prompt":"x","tool_hooks":{"Bash":{"pre":[{"name":"g","url":"https://hooks.example.test/a",` +
			`"headers":{"Authorization":"token ` + markAgentAuth + `"}}]}}}`),
	})
	must("agent_defs", err)

	_, err = s.TeamDefCreate(ctx, store.TeamDefRow{
		DefID: "tdf_dp1a", TenantID: "acme", Name: "dp1a-team", Version: 1, CreatedAt: now,
		Definition: json.RawMessage(`{"states":[{"state":"start","handler":{"kind":"agent","agent":"dp1a-agent",` +
			`"hooks":{"agent_start":[{"name":"t","url":"https://hooks.example.test/t","headers":{"X-Team-Token":"` + markTeamToken + `"}}]}}}]}`),
	})
	must("teamdefs", err)

	sess, err := s.CreateSession(ctx, "acme", "dp1a-agent", "alice")
	must("session", err)
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_dp1a", UserID: "alice", TenantID: "acme",
		RunConfig: json.RawMessage(`{"hooks":{"hooks":{"run_end":[{"name":"r","url":"https://hooks.example.test/r",` +
			`"headers":{"Authorization":"Bearer ` + markRunCfgAuth + `"}}]}}}`),
	})
	must("run", err)
	must("pause", s.SetRunPauseState(ctx, run.ID, store.PauseStatePaused))

	return []string{
		"agent_defs|dp1a-agent|adf_dp1a|tool_hooks.Bash.pre[0].headers.Authorization",
		"channel_defs|dp1a-feed||hooks.channel_publish[1].headers.X-Channel-Token",
		"hook_defs|dp1a-gate|hdf_dp1a|body.headers.X-Api-Key",
		"mcp_server_defs|dp1a-mcp|mcpdef_dp1a|headers.Authorization",
		"paused_runs|" + run.ID + "||run_config.hooks.hooks.run_end[0].headers.Authorization",
		"team_defs|dp1a-team|tdf_dp1a|states[0].handler.hooks.agent_start[0].headers.X-Team-Token",
	}
}

func findingKeys(fs []CaptureFindingEntry) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Section+"|"+f.Name+"|"+f.DefID+"|"+f.Field)
	}
	sort.Strings(out)
	return out
}

// assertNoMarkPrefix fails when text holds any marker or any prefix of one
// long enough to identify it — the "never a prefix" half of the rule.
func assertNoMarkPrefix(t *testing.T, where, text string) {
	t.Helper()
	for _, m := range allFindingMarks {
		// The shared "dp1amark-" stem is fixture scaffolding; what identifies a
		// value starts after it.
		for n := len("dp1amark-") + 4; n <= len(m); n++ {
			if strings.Contains(text, m[:n]) {
				t.Errorf("%s carries %q, a prefix of a literal header value", where, m[:n])
				break
			}
		}
	}
}

func TestCapture_LiteralHeaderValuesAreReportedByLocationNotValue(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	want := plantLiteralHeaders(t, src)
	sort.Strings(want)

	c, err := CaptureReport(context.Background(), src, CaptureOptions{})
	if err != nil {
		t.Fatalf("CaptureReport: %v", err)
	}
	env := string(c.JSON)

	// The acknowledged exception: the literal travels as authored.
	for _, m := range allFindingMarks {
		if !strings.Contains(env, m) {
			t.Errorf("the envelope lacks %q; a literal header value must travel as authored", m)
		}
	}

	var doc struct {
		Sections Sections `json:"sections"`
	}
	if err := json.Unmarshal(c.JSON, &doc); err != nil {
		t.Fatal(err)
	}
	sec := doc.Sections.CaptureFindings
	if sec == nil {
		t.Fatal("no capture_findings section in the envelope")
	}
	if sec.Version != SectionVersion {
		t.Errorf("capture_findings version = %q, want %q", sec.Version, SectionVersion)
	}
	if got := findingKeys(sec.Entries); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, f := range sec.Entries {
		if f.Section != "agent_defs" && f.TenantID != "acme" {
			t.Errorf("finding %s/%s lost its tenant: %q", f.Section, f.Name, f.TenantID)
		}
		if f.Detector == "" {
			t.Errorf("finding %s/%s names no detector", f.Section, f.Name)
		}
	}

	secBytes, err := json.Marshal(sec)
	if err != nil {
		t.Fatal(err)
	}
	assertNoMarkPrefix(t, "the capture_findings section", string(secBytes))

	if len(c.Warnings) != len(want) {
		t.Fatalf("capture warnings = %d (%q), want one per finding (%d)", len(c.Warnings), c.Warnings, len(want))
	}
	assertNoMarkPrefix(t, "the capture warnings", strings.Join(c.Warnings, "\n"))
	for _, w := range c.Warnings {
		if !strings.Contains(w, "$cred:") {
			t.Errorf("warning %q does not say how to fix it", w)
		}
	}
	if !strings.Contains(strings.Join(c.Warnings, "\n"), "mcp_server_defs dp1a-mcp (tenant acme) def mcpdef_dp1a: headers.Authorization") {
		t.Errorf("no warning names the MCP server def's Authorization header: %q", c.Warnings)
	}

	// Restore re-emits each finding, by location only.
	dst, dstClose := newTestStore(t)
	defer dstClose()
	res, err := Restore(context.Background(), dst, c.JSON, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	var reEmitted []string
	for _, w := range res.Warnings {
		if strings.HasPrefix(w, "capture finding: ") {
			reEmitted = append(reEmitted, strings.TrimPrefix(w, "capture finding: "))
		}
		if strings.Contains(w, "not understood") {
			t.Errorf("this reader does not know its own section: %q", w)
		}
	}
	if strings.Join(reEmitted, "\n") != strings.Join(c.Warnings, "\n") {
		t.Errorf("restore re-emitted:\n%s\nwant the capture's warnings:\n%s", strings.Join(reEmitted, "\n"), strings.Join(c.Warnings, "\n"))
	}
	assertNoMarkPrefix(t, "the restore warnings", strings.Join(res.Warnings, "\n"))
}

// A capture whose headers are all references reports nothing, and its envelope
// has no capture_findings key — byte-compatible with one taken before the
// section existed.
func TestCapture_ReferenceHeadersLeaveNoFindings(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	ctx := context.Background()
	_, err := src.MCPServerDefCreate(ctx, store.MCPServerDefRow{
		DefID: "mcpdef_refs", Name: "refs", Version: 1, CreatedAt: time.Now().UTC(),
		Definition: json.RawMessage(`{"transport":"streamable-http","url":"https://mcp.example.test/mcp","headers":{` +
			`"Authorization":"Bearer ${LOOMCYCLE_REFS_TOKEN}","X-Api-Key":"$cred:refs-key",` +
			`"X-Run-Token":"${run.user_bearer}","X-GitHub-Token":"$ghapp:refs-app"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := CaptureReport(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 0 {
		t.Errorf("reference-only headers produced warnings: %q", c.Warnings)
	}
	if strings.Contains(string(c.JSON), `"capture_findings"`) {
		t.Error("an envelope with no findings carries a capture_findings key")
	}
}

func TestHeaderDetector_FlagsLiteralsNotReferences(t *testing.T) {
	for _, tc := range []struct {
		name, value, want string
	}{
		{"Authorization", "Bearer abc123def456", detectorSecretPattern},
		{"Authorization", "Basic dXNlcjpwYXNz", detectorCredentialHeader},
		{"X-Api-Key", "k-12345", detectorSecretPattern},
		{"X-Custom", "sk-proj-ABCDEFGHIJKLMNOPQRST", detectorSecretPattern},
		{"X-Custom", "ghp_" + strings.Repeat("a", 36), detectorSecretPattern},
		{"X-Upstream-Token", "opaque", detectorSecretPattern},
		{"X-Service-Key", "opaque", detectorCredentialHeader},
		{"Cookie", "session=abc", detectorCredentialHeader},

		{"Authorization", "Bearer ${LOOMCYCLE_X}", ""},
		{"Authorization", "Bearer $cred:x", ""},
		{"Authorization", "$ghapp:app", ""},
		{"Authorization", "Bearer", ""},
		{"X-Api-Key", "${run.user_bearer}", ""},
		{"Content-Type", "application/json", ""},
		{"X-Tenant", "acme", ""},
		{"Accept", "application/json, text/event-stream", ""},
	} {
		if got := headerDetector(tc.name, tc.value); got != tc.want {
			t.Errorf("headerDetector(%q, %q) = %q, want %q", tc.name, tc.value, got, tc.want)
		}
	}
}
