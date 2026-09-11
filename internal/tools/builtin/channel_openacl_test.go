package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestChannelAllowed_BareStarIsNotAWildcard pins the property that makes the
// AllPublish/AllSubscribe discriminator necessary at all.
//
// The matcher supports an exact name and a trailing "/*" prefix, and nothing
// else — so a bare "*" is a channel literally CALLED "*", matching no real
// name. Three operator planes declared open access as []string{"*"} and
// therefore silently held no channel authority. Anyone tempted to "just make
// `*` work here" should read the comment on ChannelPolicyValue first: doing it
// at the matcher would widen every operator-authored allowlist in the system
// that today grants nothing.
func TestChannelAllowed_BareStarIsNotAWildcard(t *testing.T) {
	for _, name := range []string{"verdicts", "pr-events", "findings/alpha", "a"} {
		if channelAllowed(name, []string{"*"}) {
			t.Errorf("channelAllowed(%q, [\"*\"]) = true — the bare star became a wildcard; "+
				"that silently widens every allowlist that contains one", name)
		}
	}
	// The wildcard the matcher DOES support is unchanged.
	if !channelAllowed("findings/alpha", []string{"findings/*"}) {
		t.Error(`channelAllowed("findings/alpha", ["findings/*"]) = false — the prefix wildcard regressed`)
	}
	if channelAllowed("findings", []string{"findings/*"}) {
		t.Error(`"findings/*" must not match "findings" itself`)
	}
}

func TestChannelPolicy_GrantsFor(t *testing.T) {
	p := tools.ChannelPolicyValue{Publish: []string{"a"}, Subscribe: []string{"b"}}
	if all, list := p.GrantsFor("publish"); all || len(list) != 1 || list[0] != "a" {
		t.Errorf("GrantsFor(publish) = %v %v", all, list)
	}
	if all, list := p.GrantsFor("subscribe"); all || len(list) != 1 || list[0] != "b" {
		t.Errorf("GrantsFor(subscribe) = %v %v", all, list)
	}
	// An unknown side grants nothing — the closed default. A typo'd side must
	// not fall through to "unrestricted".
	if all, list := p.GrantsFor("peek"); all || list != nil {
		t.Errorf("GrantsFor(unknown) = %v %v, want false nil", all, list)
	}
	open := tools.ChannelPolicyValue{AllPublish: true, AllSubscribe: true}
	for _, side := range []string{"publish", "subscribe"} {
		if all, _ := open.GrantsFor(side); !all {
			t.Errorf("GrantsFor(%s) on an unrestricted policy = false", side)
		}
	}
}

// TestChannel_UnrestrictedPolicyReachesADeclaredChannel: the discriminator is
// honoured at the agent's own publish/subscribe gate, and the closed-set check
// (is the channel declared at all?) still runs ahead of it — unrestricted means
// "every declared channel", never "any string".
func TestChannel_UnrestrictedPolicyReachesADeclaredChannel(t *testing.T) {
	c := &Channel{}
	declared := map[string]tools.ChannelDef{"verdicts": {Name: "verdicts", Scope: "user"}}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "u1", AgentID: "a1"})

	open := tools.ChannelPolicyValue{AllPublish: true, AllSubscribe: true, Channels: declared}
	for _, side := range []string{"publish", "subscribe"} {
		if _, _, _, err := c.resolveChannel(ctx, open, side, "verdicts"); err != nil {
			t.Errorf("unrestricted policy refused %s on a declared channel: %v", side, err)
		}
	}
	// Still closed over the operator's declared set, and refused BY THAT CHECK —
	// asserting the reason, not merely that something errored. An undeclared
	// channel falling through to a later guard (an empty scope, say) would also
	// produce an error while the closed set had in fact been lifted.
	_, _, _, err := c.resolveChannel(ctx, open, "publish", "never-declared")
	if err == nil || !strings.Contains(err.Error(), "not declared in operator config") {
		t.Errorf("unrestricted policy reached an UNDECLARED channel — it lifts the allowlist, "+
			"not the closed set; err = %v", err)
	}
	// And the old spelling grants nothing, which is the bug this replaces.
	star := tools.ChannelPolicyValue{Publish: []string{"*"}, Subscribe: []string{"*"}, Channels: declared}
	if _, _, _, err := c.resolveChannel(ctx, star, "publish", "verdicts"); err == nil {
		t.Error(`Publish: ["*"] granted publish — a bare star must stay a literal name, not a wildcard`)
	}
}

// TestTeamChannelAuthority_UnrestrictedAuthorMayDeclareAnACL is the gap that
// blocked the Starter: a team ACL may only NARROW what its author holds, and an
// author holding everything was computed as holding nothing — so no plane but
// an in-band agent run could author a runnable Starter team.
func TestTeamChannelAuthority_UnrestrictedAuthorMayDeclareAnACL(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	ctx := tools.WithChannelPolicy(
		tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_operator"}),
		tools.ChannelPolicyValue{AllPublish: true, AllSubscribe: true})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"triage","overlay":`+
		strings.TrimSuffix(starterGraph, "}")+`,"channels":{"publish":["verdicts"],"subscribe":["pr-events"]}}}`))
	if res.IsError {
		t.Fatalf("an unrestricted author could not declare a team ACL: %s", res.Text)
	}

	// The narrowing rule itself is untouched for a normal, listed author.
	narrow := tools.WithChannelPolicy(
		tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_agent"}),
		tools.ChannelPolicyValue{Publish: []string{"verdicts"}, Subscribe: []string{"pr-events"}})
	res, _ = tool.Execute(narrow, json.RawMessage(`{"op":"create","name":"ok-narrow","overlay":`+
		strings.TrimSuffix(starterGraph, "}")+`,"channels":{"publish":["verdicts"],"subscribe":["pr-events"]}}}`))
	if res.IsError {
		t.Fatalf("a listed author was refused its own grants: %s", res.Text)
	}
	res, _ = tool.Execute(narrow, json.RawMessage(`{"op":"create","name":"widen","overlay":`+
		strings.TrimSuffix(starterGraph, "}")+`,"channels":{"publish":["secrets"],"subscribe":["pr-events"]}}}`))
	if !res.IsError || !strings.Contains(res.Text, "never widen") {
		t.Fatalf("a team ACL widened past its author's grants: IsError=%v %s", res.IsError, res.Text)
	}
}
