package snapshot

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// An agent def's $cred: references resolve for that agent, so a credential
// stored at the agent's scope satisfies them: the scan asks with the agent's
// name, and says nothing when the agent-scoped key is there.
func TestCredScan_AgentDefChecksAgentScope(t *testing.T) {
	ctx := context.Background()
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	_, err := src.AgentDefCreate(ctx, store.AgentDefRow{DefID: "ad_scope", TenantID: "acme", Name: "helper", Version: 1, CreatedAt: triggerBase(),
		Definition: json.RawMessage(`{"hooks":{"agent_start":[{"name":"a","url":"https://h.example.test/a","headers":{"Authorization":"$cred:agent-hook"}}]}}`)})
	if err != nil {
		t.Fatal(err)
	}

	checks := &fakeCredChecks{creds: map[string]bool{"acme/helper//agent-hook": true}}
	res := mustRestore(t, dst, mustCapture(t, src), checks.opts())
	if !slices.Contains(checks.asked, "acme/helper//agent-hook") {
		t.Errorf("the agent def's reference was not checked in the agent's scope (asked %v)", checks.asked)
	}
	if hasWarning(res, "missing credential:", "agent-hook") {
		t.Errorf("warned about an agent-scoped credential that exists: %v", res.Warnings)
	}
}

// A definition that runs for whichever user starts the run cannot be checked
// for a per-user credential — there is no user to ask about. A miss says what
// was checked instead of calling the reference missing outright; a trigger
// that names its user keeps the plain wording.
func TestCredScan_UserScopedRefWarningSaysUnchecked(t *testing.T) {
	ctx := context.Background()
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	_, err := src.AgentDefCreate(ctx, store.AgentDefRow{DefID: "ad_user", TenantID: "acme", Name: "helper", Version: 1, CreatedAt: triggerBase(),
		Definition: json.RawMessage(`{"hooks":{"agent_start":[{"name":"a","url":"https://h.example.test/a","headers":{"Authorization":"$cred:per-user-key"}}]}}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.MCPServerDefCreate(ctx, store.MCPServerDefRow{DefID: "md_user", TenantID: "acme", Name: "peer", Version: 1, CreatedAt: triggerBase(),
		Definition: json.RawMessage(`{"transport":"http","url":"https://peer.example.test/mcp","headers":{"Authorization":"Bearer $cred:peer-user-key"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	plantScanFixture(t, src) // its schedule names user alice

	res := mustRestore(t, dst, mustCapture(t, src), (&fakeCredChecks{}).opts())
	for _, want := range [][]string{
		{"agent_def acme/helper", "$cred:per-user-key", "no tenant-level or agent-level credential", `tenant "acme"`, "per-user credentials were not checked"},
		{"mcp_server_def acme/peer", "$cred:peer-user-key", "no tenant-level credential", `tenant "acme"`, "per-user credentials were not checked"},
		{"webhook_def acme/gh", "$cred:jobs-token", "per-user credentials were not checked"},
	} {
		if !hasWarning(res, append([]string{"missing credential:"}, want...)...) {
			t.Errorf("no warning with %q; warnings:\n%s", want, strings.Join(res.Warnings, "\n"))
		}
	}
	// The schedule runs as alice, whose credentials were checked.
	for _, w := range res.Warnings {
		if strings.Contains(w, "schedule_def acme/digest") && strings.Contains(w, "$ghapp:gh-app") &&
			strings.Contains(w, "not checked") {
			t.Errorf("a trigger with a user says per-user credentials were not checked: %s", w)
		}
	}
	if !hasWarning(res, "missing credential:", "schedule_def acme/digest", "$ghapp:gh-app", "which no credential on this host provides") {
		t.Errorf("the user-bound schedule lost its plain missing warning: %v", res.Warnings)
	}
}
