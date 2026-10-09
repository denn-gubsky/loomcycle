package builtin

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// teamdef_issues.go — what is wrong with a team definition, as a list.
//
// create and fork refuse with the first refusal; verify reports every one. Both
// read the same list from the same gates (authoringIssues), so verify cannot
// pass a definition a save would refuse, and a gate added to the save is a gate
// verify runs. Before this, verify of an unsaved draft did not exist and a save
// stopped at the first problem, so an author fixing a hand-written team learned
// one error per round trip.

// Issue severities. A refused issue stops a create or fork; an unrunnable one is
// stored but would fail when a walk reaches it; an advisory one stops nothing.
const (
	severityRefused    = "refused"
	severityUnrunnable = "unrunnable"
	severityAdvisory   = "advisory"
)

// Issue kinds. Wire values, read by clients: add new ones, never rename. The TS
// adapter's TeamIssueKind lists them (TestTeamIssue_TSMirrorsEveryKindAndKey).
const (
	// The definition as written.
	teamIssueNameInvalid    = "name_invalid"
	teamIssueParentNotFound = "parent_not_found"
	teamIssueOverlayInvalid = "overlay_invalid"
	teamIssueGraphInvalid   = "graph_invalid"
	teamIssueSizeCap        = "size_cap"
	// Channels.
	teamIssueChannelReserved     = "channel_reserved"
	teamIssueChannelAuthority    = "channel_authority"
	teamIssueACLMissing          = "acl_missing"
	teamIssueChannelUndeclared   = "channel_undeclared"
	teamIssueLocalChannelInvalid = "local_channel_invalid"
	teamIssueLocalChannelMissing = "local_channel_missing"
	// The team's own agents, skills and webhooks.
	teamIssueLocalAgentInvalid      = "local_agent_invalid"
	teamIssueLocalAgentAuthority    = "local_agent_authority"
	teamIssueLocalAgentCollision    = "local_agent_collision"
	teamIssueLocalAgentMissing      = "local_agent_missing"
	teamIssueLocalAgentUnreferenced = "local_agent_unreferenced"
	teamIssueLocalSkillInvalid      = "local_skill_invalid"
	teamIssueLocalWebhookAuthority  = "local_webhook_authority"
	teamIssueLocalWebhookInvalid    = "local_webhook_invalid"
	// Members that resolve elsewhere.
	teamIssueAgentMissing = "agent_missing"
	// A decision state this deployment cannot run (see teamdef_decision.go):
	// no decision models at all, a model the operator does not list, or more
	// questions or options than the named model takes.
	// A decision state its author may not write: an agent that does not hold
	// the Decision tool, or a model its own narrowing excludes.
	teamIssueDecisionAuthority    = "decision_authority"
	teamIssueDecisionUnconfigured = "decision_unconfigured"
	teamIssueDecisionModelUnknown = "decision_model_unknown"
	teamIssueDecisionLimits       = "decision_limits"
	// The server cannot make a check a save needs (a store fault, or no tool
	// wired to judge a kind of local body). A save refuses rather than store
	// what it could not check.
	teamIssueUncheckable = "uncheckable"
)

// teamIssue is one problem with a team definition.
type teamIssue struct {
	Kind     string
	Severity string
	// Path is the JSON path of the offending value in the definition
	// ("states[2].handler.sink.channel", "local.agents.reviewer"), "" when the
	// problem is the definition as a whole.
	Path string
	// State and Field locate a problem inside a state, as the reference sweep
	// always has: the state id and the handler field.
	State, Field string
	// What it names.
	Channel, Agent, Skill, Side string
	// Detail is the human text — for a refusal, exactly what create or fork
	// says after its "create: " / "fork: " prefix.
	Detail string
}

// toMap is the wire shape. Empty fields are left out, and an advisory issue
// keeps the `advisory: true` it carried before severities existed, so a client
// that reads that key is not broken by the new one.
func (i teamIssue) toMap() map[string]any {
	m := map[string]any{"kind": i.Kind, "severity": i.Severity, "detail": i.Detail}
	for k, v := range map[string]string{
		"path": i.Path, "state": i.State, "field": i.Field,
		"channel": i.Channel, "agent": i.Agent, "skill": i.Skill, "side": i.Side,
	} {
		if v != "" {
			m[k] = v
		}
	}
	if i.Severity == severityAdvisory {
		m["advisory"] = true
	}
	return m
}

// refused builds a refusal; most issues are.
func refused(kind, path, detail string) teamIssue {
	return teamIssue{Kind: kind, Severity: severityRefused, Path: path, Detail: detail}
}

// graphIssues converts teamgraph's violations. An undeclared "./<name>" keeps
// the kind teamgraph gives it — the sweep's kind — so the two can be matched.
func graphIssues(in []*teamgraph.Issue) []teamIssue {
	out := make([]teamIssue, 0, len(in))
	for _, gi := range in {
		kind := gi.Kind
		if kind == "" {
			kind = teamIssueGraphInvalid
		}
		out = append(out, teamIssue{
			Kind: kind, Severity: severityRefused,
			Path: gi.Path, State: gi.State, Field: gi.Field, Detail: gi.Msg,
		})
	}
	return out
}

// firstRefusal is the issue a create or fork refuses with, or nil.
func firstRefusal(issues []teamIssue) *teamIssue {
	for i := range issues {
		if issues[i].Severity == severityRefused {
			return &issues[i]
		}
	}
	return nil
}

// verdict is the pair a report leads with: valid = a save would be accepted;
// runnable = valid, and a walk could run what it stores.
func verdict(issues []teamIssue) (valid, runnable bool) {
	valid, runnable = true, true
	for _, i := range issues {
		switch i.Severity {
		case severityRefused:
			valid, runnable = false, false
		case severityUnrunnable:
			runnable = false
		}
	}
	return valid, runnable
}

func issueMaps(issues []teamIssue) []map[string]any {
	out := make([]map[string]any, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.toMap())
	}
	return out
}

// authoringIssues runs every gate create and fork apply to a merged
// definition, in the order they have always run, and returns everything they
// find. A save refuses with the first refusal, so its error is unchanged by
// the gates after it running too; verify returns the lot.
//
// Ordering notes carried over from the gates themselves: the author's own
// channel authority is judged before the preflight, because "you may not
// grant this" is the harder refusal and the one to fix first.
func (t *TeamDef) authoringIssues(ctx context.Context, op, team string, defJSON []byte, def teamgraph.Definition, parent *teamgraph.Definition, description string) []teamIssue {
	var issues []teamIssue
	issues = append(issues, graphIssues(teamgraph.ValidateAll(def))...)
	issues = append(issues, localChannelIssues(team, def)...)
	issues = append(issues, teamChannelAuthorityIssues(ctx, def)...)
	issues = append(issues, t.decisionAuthorityIssues(ctx, def)...)
	issues = append(issues, localWebhookIssues(ctx, team, def)...)
	issues = append(issues, t.preflightChannelIssues(ctx, def)...)
	issues = append(issues, t.localSkillIssues(ctx, op, team, def)...)
	issues = append(issues, t.localAgentIssues(ctx, op, team, def, parent)...)
	issues = append(issues, t.sizeCapIssues(defJSON, description)...)
	return issues
}

// withoutRepeats drops a sweep finding a refusal already reports — the same
// kind at the same place — so each problem is listed once. The preflight and
// the sweep both check a team's ACL and the declared channels, and Validate and
// the sweep both check its "./<name>" references.
func withoutRepeats(refusals, sweep []teamIssue) []teamIssue {
	type key struct{ kind, path string }
	seen := map[key]bool{}
	for _, i := range refusals {
		seen[key{i.Kind, i.Path}] = true
	}
	out := refusals
	for _, i := range sweep {
		if !seen[key{i.Kind, i.Path}] {
			out = append(out, i)
		}
	}
	return out
}
