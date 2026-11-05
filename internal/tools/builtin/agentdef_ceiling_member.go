package builtin

import (
	"context"
	"fmt"
	"path"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// agentdef_ceiling_member.go — the capability ceiling for a member authoring
// off-run (RFC FY Part B).
//
// Three authors, three ceilings:
//
//   - the operator plane is the root of authority and is not narrowed;
//   - a run is narrowed by its own policies (checkCapabilityCeiling);
//   - a member (or an isolated user) on a substrate plane is narrowed by what
//     it may do itself. The plane stamps the same broad policies for every
//     caller, so they say nothing about the member: each field's ceiling is
//     read from the member's own rules where a verb covers the grant, and is
//     empty — the field may only be left unset — where the field confers
//     authority over other people's data or the operator's powers and no verb
//     covers it.
//
// The classification, one row per check (memberCeilingClass, which
// TestMemberCeiling_EveryCeilingCheckIsClassified holds to the checks):
//
//	check                    member's ceiling
//	-----------------------  ---------------------------------------------------
//	tools                    rules: invoke on each tool (tool:, mcp:, a2a:, …)
//	ceilVolumes              rules: mount volume:<name> (the `default` volume when unset)
//	ceilMemory               rules: write memory:<scope>, within the plane's grant
//	ceilSQL                  rules: write sqlmem:<scope>, within the plane's grant
//	ceilHistory              rules: read history:<scope>, within the plane's grant
//	ceilCrypto               rules: use crypto:<scope>, for user and agent only;
//	                         tenant and run keys are the operator's
//	ceilChannels             the plane's grant; each publish/subscribe is asked
//	                         of the runner's rules in the run
//	ceilSkills               the plane's grant; the runner's rules narrow skills
//	                         at run start
//	ceilEvaluation           the token's grant (submit_self for a member)
//	ceilShellCreds           explicit for an agent holding Bash or Bashbox: unset
//	                         (every credential the operator lists) is refused,
//	                         and the only list allowed is [] — the credential
//	                         rules (credential:<scope>:<name>) name stored
//	                         credentials, not the operator's host env names, so
//	                         no rule can grant one. Unset without a shell tool.
//	ceilInterruption         unset only: no plane stamps a member's policy
//	ceilGitRemote            operator-grade: unset only
//	ceilStatementMethods     operator-grade: unset only
//	ceilDefScopes            operator-grade: unset only (every *_def_scopes)
//	decisionAuthorityIssues  rules: use gateway:decide
//
// With no compiled rule set because no resolver is wired, the member's class
// baseline stands in for it. When a resolver is wired and the set could not be
// read (auth.Principal.ACLFault), nothing is granted: the fault must not stand
// in for the member's own denies.
//
// A fork by a member is judged with NO base: the forked version's grants are
// its author's, not the member's, so a member's fork may carry only what the
// member could grant itself (an operator-grade field must be emptied), and the
// member may not make it the active version of an operator's agent
// (memberActivationGuard).
var memberCeilingClass = map[string]string{
	"tools":                   "rules: invoke",
	"ceilVolumes":             "rules: mount volume",
	"ceilMemory":              "rules: write memory",
	"ceilSQL":                 "rules: write sqlmem",
	"ceilHistory":             "rules: read history",
	"ceilCrypto":              "rules: use crypto (user, agent)",
	"ceilChannels":            "plane",
	"ceilSkills":              "plane",
	"ceilEvaluation":          "token",
	"ceilShellCreds":          "explicit [] with a shell tool",
	"ceilInterruption":        "unset only",
	"ceilGitRemote":           "operator-grade",
	"ceilStatementMethods":    "operator-grade",
	"ceilDefScopes":           "operator-grade",
	"decisionAuthorityIssues": "rules: use gateway:decide",
}

// memberCeilingSet is the rule set a member's grants are judged against: its
// own compiled set, or its class baseline when none was resolved. nil only
// when the baseline does not compile, and nil grants nothing.
func memberCeilingSet(ctx context.Context) *authz.Effective {
	if eff := tools.CallerACL(ctx); eff != nil {
		return eff
	}
	p, ok := auth.PrincipalFromContext(ctx)
	if p.ACLFault {
		return nil
	}
	name := authz.BaselineMember
	if auth.IsIsolated(p, ok) {
		name = authz.BaselineIsolated
	}
	eff, err := authz.Compile(authz.Input{
		Subject:  authz.Subject{TenantID: p.TenantID, ID: p.Subject},
		Bindings: []authz.Binding{{ACL: name, SubjectKind: authz.SubjectTenant}},
		ACLs:     map[string]authz.ACLVersion{name: {ACL: authz.Baselines()[name]}},
		Now:      time.Now(),
	})
	if err != nil {
		return nil
	}
	return eff
}

// memberCeilingCtx is ctx with the plane's policies replaced, for the ceiling
// checks only, by what the member may grant (the table above). The name gate
// and everything else keep the plane's ctx.
func (a *AgentDef) memberCeilingCtx(ctx context.Context, def mergedDef) context.Context {
	eff := memberCeilingSet(ctx)
	grants := func(action, typ, name string) bool {
		return authz.Grants(ctx, eff, action, authz.Resource{Type: typ, Name: name})
	}
	keep := func(scopes []string, action, typ string) []string {
		var out []string
		for _, s := range scopes {
			if grants(action, typ, s) {
				out = append(out, s)
			}
		}
		return out
	}
	vols := tools.VolumePolicyValue{Active: true}
	for _, v := range a.effectiveVolumes(def) {
		if grants("mount", "volume", v) {
			vols.Bindings = append(vols.Bindings, tools.VolumeBinding{Name: v})
		}
	}
	shell := tools.ShellCredsPolicyValue{}
	if holdsShellTool(def.Tools) {
		shell = tools.ShellCredsPolicyValue{Set: true}
	}
	p, ok := auth.PrincipalFromContext(ctx)
	// Every field is set here, from the member and the fixed candidate lists,
	// never read from the plane's ctx: the plane grants the same to everyone,
	// and register_agent reaches this with no plane ctx at all.
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Volumes = vols
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: keep([]string{"agent", "user", "tenant", "global"}, "write", "memory")}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: keep([]string{"agent", "user", "tenant"}, "write", "sqlmem")}
		rp.History = tools.HistoryPolicyValue{Scopes: keep([]string{"self", "user", "tenant", "global"}, "read", "history")}
		rp.Crypto = tools.CryptoPolicyValue{Scopes: keep([]string{"user", "agent"}, "use", "crypto")}
		rp.Channel = tools.ChannelPolicyValue{AllPublish: true, AllSubscribe: true}
		rp.Skill = tools.SkillPolicyValue{Granted: true}
		rp.Evaluation = tools.OffRunEvaluationPolicy(p, ok)
		rp.Interruption = nil
		rp.ShellCreds = shell
		rp.StatementMethods = nil
		rp.GitRemote = tools.GitRemotePolicyValue{}
		rp.AgentDef = tools.AgentDefPolicyValue{}
		rp.VolumeDef = tools.VolumeDefPolicyValue{}
	})
	ctx = tools.WithScheduleDefPolicy(ctx, tools.ScheduleDefPolicyValue{})
	ctx = tools.WithA2AServerCardDefPolicy(ctx, tools.A2AServerCardDefPolicyValue{})
	return tools.WithA2AAgentDefPolicy(ctx, tools.A2AAgentDefPolicyValue{})
}

// holdsShellTool reports whether tools names Bash or Bashbox, by name or by a
// pattern that covers one.
func holdsShellTool(names []string) bool {
	for _, t := range names {
		for _, shell := range []string{"Bash", "Bashbox"} {
			if ok, _ := path.Match(t, shell); ok || t == shell {
				return true
			}
		}
	}
	return false
}

// memberToolsCeiling refuses a tool the member may not invoke itself: the
// plane's wildcard tools ceiling is the plane's, not the member's.
func memberToolsCeiling(ctx context.Context, def mergedDef) error {
	eff := memberCeilingSet(ctx)
	for _, t := range def.Tools {
		if !authz.Grants(ctx, eff, "invoke", authz.ResourceForTool(t)) {
			return fmt.Errorf("tools: %q is not a tool you may use yourself. A new agent can only be granted authority its author holds: "+
				"remove it, or ask an operator to author this agent", t)
		}
	}
	return nil
}

// memberMayUseDecisions is the decision-state ceiling for a member: the
// question POST /v1/_decide asks.
func memberMayUseDecisions(ctx context.Context) bool {
	return authz.Grants(ctx, memberCeilingSet(ctx), "use", authz.Resource{Type: "gateway", Name: "decide"})
}
