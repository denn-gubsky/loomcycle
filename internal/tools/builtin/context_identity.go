package builtin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The identity surface of Context: op=self's `identity` block, op=can and
// op=reach. Everything here is keyed by the run's own identity — the
// compiled set the run carries (tools.CallerACL) and the tenant on its run
// identity. Nothing the model sends names a subject, a tenant or a group:
// op=can and op=reach take an action and a resource, which are checked
// against the catalog and evaluated in memory, and never reach the store.
// TestContextIdentity_NoModelInputReachesAStoreCall holds that for this file.

// maxCanQueries bounds an op=can batch. A plan that needs more questions at
// once is better served by op=reach.
const maxCanQueries = 32

// maxIdentityGroupRows bounds the group rows op=self reads for descriptions.
// Groups past it are still named (with how the user is in them); only their
// description and external id are left out. A user in more groups than this
// is rare, and op=self should not cost a read per group without limit.
const maxIdentityGroupRows = 64

// canResource is the resource half of an op=can question.
type canResource struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

// canQuery is one op=can question.
type canQuery struct {
	Action   string       `json:"action"`
	Resource *canResource `json:"resource"`
}

// identitySubject returns the subject the run acts for, the tenant that
// subject belongs to, and its compiled set: the set's subject when the run
// carries one (inside a run it is the originating principal's, inherited down
// the spawn tree), else the authenticated principal's. "" when there is
// neither — open mode, or a trigger that runs under a definition rather than
// a person.
func identitySubject(ctx context.Context) (subject, subjectTenant string, eff *authz.Effective) {
	eff = tools.CallerACL(ctx)
	if eff != nil && eff.Subject.ID != "" {
		return eff.Subject.ID, eff.Subject.TenantID, eff
	}
	if p, ok := auth.PrincipalFromContext(ctx); ok && p.Subject != "" {
		return p.Subject, p.TenantID, eff
	}
	return "", "", eff
}

// identityReport is op=self's `identity` block: the user row of the run's own
// subject in the run's own tenant, its groups (with how the user is in each),
// and the access lists that apply. nil when the run acts for no subject.
//
// Store reads are best-effort: a missing row (an operator token, a
// config-declared principal) leaves its fields out, and a store fault is
// logged and leaves them out too — op=self must not fail because a directory
// read did.
func (c *Context) identityReport(ctx context.Context) map[string]any {
	subject, subjectTenant, eff := identitySubject(ctx)
	if subject == "" {
		return nil
	}
	tenant := tools.RunIdentity(ctx).TenantID
	out := map[string]any{"tenant_id": tenant, "subject": subject}
	// The directory rows are read only when the subject belongs to the run's
	// tenant. A credential of another tenant running here (an operator
	// focused on a tenant) has no row in it, and a row there under the same
	// subject id would be someone else's.
	readRows := c.Store != nil && subjectTenant == tenant
	if readRows {
		row, err := c.Store.UserGet(ctx, tenant, subject)
		switch {
		case err == nil:
			out["display_name"] = row.DisplayName
			out["access_mode"] = row.AccessMode
			out["status"] = row.Status
			if !row.CreatedAt.IsZero() {
				out["created_at"] = row.CreatedAt.UTC().Format(time.RFC3339)
			}
			if row.Status == store.UserStatusPending {
				out["status_note"] = "This account is waiting for an operator to approve it. Do not do real work for it: tell the user their account is not approved yet."
			}
		case !isNotFound(err):
			slog.WarnContext(ctx, "context: reading the run's user row failed", "tenant", tenant, "subject", subject, "err", err)
		}
	}
	if a := identityAuth(ctx, subject, subjectTenant); a != nil {
		out["auth"] = a
	}
	if readRows {
		if ids := c.identityIdentities(ctx, tenant, subject); len(ids) > 0 {
			out["identities"] = ids
		}
	}
	if eff == nil {
		// No compiled set: access control is off for this run, so there are
		// no groups or lists to report.
		return out
	}
	out["groups"] = c.identityGroups(ctx, tenant, eff, readRows)
	out["acl"] = identityACL(ctx, eff)
	return out
}

// maxIdentityIdentities bounds the linked sign-in identities op=self lists.
// A person links one per gate, and a deployment configures a handful of
// gates; the bound only keeps a pathological row set from growing the answer.
const maxIdentityIdentities = 16

// identityAuth is the identity block's `auth`: how the principal the run
// acts for signed in. A Web UI session carries its gate, methods and
// authentication time; a token a gate issued names that gate alone. nil for
// any other credential — an operator token went through no gate — and when
// the principal on ctx is not the subject reported, so one person's sign-in
// is never reported as another's.
func identityAuth(ctx context.Context, subject, subjectTenant string) map[string]any {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.Subject != subject || p.TenantID != subjectTenant {
		return nil
	}
	switch {
	case p.Login != nil && p.Login.Gate != "":
		out := map[string]any{"gate": p.Login.Gate, "amr": nonNilStrings(p.Login.AMR)}
		if !p.Login.AuthTime.IsZero() {
			out["auth_time"] = p.Login.AuthTime.UTC().Format(time.RFC3339)
		}
		return out
	case p.IssuedByGate != "":
		return map[string]any{"gate": p.IssuedByGate}
	}
	return nil
}

// identityIdentities lists the sign-in identities linked to the run's own
// subject: the gate each is at, whether its e-mail is verified, and the last
// login through it. The e-mail itself only when the gate verified it — this
// is the person's own agent, and an address nobody verified is a claim, not
// a contact. The external subject and issuer are left out: provenance the
// agent cannot act on. A store fault is logged and leaves the list out.
func (c *Context) identityIdentities(ctx context.Context, tenant, subject string) []map[string]any {
	rows, err := c.Store.UserIdentityListSubject(ctx, tenant, subject)
	if err != nil {
		slog.WarnContext(ctx, "context: reading the run's linked identities failed", "tenant", tenant, "subject", subject, "err", err)
		return nil
	}
	if len(rows) > maxIdentityIdentities {
		rows = rows[:maxIdentityIdentities]
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		id := map[string]any{"gate": r.Gate, "email_verified": r.EmailVerified}
		if r.EmailVerified && r.Email != "" {
			id["email"] = r.Email
		}
		if !r.LastLoginAt.IsZero() {
			id["last_login_at"] = r.LastLoginAt.UTC().Format(time.RFC3339)
		}
		out = append(out, id)
	}
	return out
}

// identityGroups lists the set's (closed) groups with how the subject is in
// each and, from the group rows, their description and external id.
func (c *Context) identityGroups(ctx context.Context, tenant string, eff *authz.Effective, readRows bool) []map[string]any {
	names := append([]string(nil), eff.Subject.Groups...)
	sort.Strings(names)
	via := eff.GroupVia()
	groups := make([]map[string]any, 0, len(names))
	for i, name := range names {
		g := map[string]any{"name": name}
		if v, ok := via[name]; ok {
			g["via"] = v
		}
		if readRows && i < maxIdentityGroupRows {
			row, err := c.Store.GroupGet(ctx, tenant, name)
			switch {
			case err == nil:
				if row.Description != "" {
					g["description"] = row.Description
				}
				if row.ExternalID != "" {
					g["external_id"] = row.ExternalID
				}
			case !isNotFound(err):
				// A config-declared group has no row; that is a miss, not
				// a fault. Anything else is worth a line.
				slog.WarnContext(ctx, "context: reading a group row failed", "tenant", tenant, "group", name, "err", err)
			}
		}
		groups = append(groups, g)
	}
	return groups
}

// identityACL is the identity block's `acl`: the rollout mode and set version,
// the class baseline, every other list that applies with its version and the
// binding it came through, the bound lists that could not be found, and the
// tools this hop's definition lost to the set.
func identityACL(ctx context.Context, eff *authz.Effective) map[string]any {
	lists := []map[string]any{}
	for _, l := range eff.Lists() {
		if l.Name == eff.Baseline() {
			continue // reported as `baseline`
		}
		bound := map[string]any{"kind": string(l.Binding.SubjectKind)}
		if l.Binding.SubjectID != "" {
			bound["id"] = l.Binding.SubjectID
		}
		entry := map[string]any{"name": l.Name, "bound_to": bound}
		if l.Version > 0 {
			entry["version"] = l.Version
		}
		lists = append(lists, entry)
	}
	return map[string]any{
		"mode":          string(authz.ModeFrom(ctx)),
		"version":       eff.Version,
		"baseline":      eff.Baseline(),
		"lists":         lists,
		"missing_acls":  nonNilStrings(eff.Missing()),
		"removed_tools": nonNilStrings(tools.PolicyOf(ctx).ACLRemovedTools),
	}
}

// noACLSet is the refusal op=can and op=reach give a run that carries no
// compiled set.
func noACLSet() tools.Result {
	return errBusiness("access control is not active for this run, so no rule decides anything",
		"Your definition's grants decide what you may do: see op=permissions and the scopes in op=self.")
}

// ---- can ----

// execCan answers "may I do this" for the run's own subject, with the same
// evaluation the gates make (authz.Evaluate against the run's set, under the
// mode, transport and clock on ctx), so the answer cannot drift from what a
// gate will do. The decision fields are the ones the HTTP twin
// (POST /v1/_authz/evaluation) returns for the same subject.
func (c *Context) execCan(ctx context.Context, in contextInput) (tools.Result, error) {
	single := in.Action != "" || in.Resource != nil
	if single && len(in.Queries) > 0 {
		return errValidation("pass either action+resource or queries, not both", "Ask one question with action and resource, or several with queries."), nil
	}
	if !single && len(in.Queries) == 0 {
		return errValidation("missing action and resource", `Pass {"action":"run","resource":{"type":"agent","id":"chat"}}, or a queries list.`), nil
	}
	if len(in.Queries) > maxCanQueries {
		return errValidation(fmt.Sprintf("queries lists %d questions; the limit is %d", len(in.Queries), maxCanQueries), "Split the questions over several calls, or use op=reach for a whole type."), nil
	}
	queries := in.Queries
	if single {
		queries = []canQuery{{Action: in.Action, Resource: in.Resource}}
	}
	// Validate every question before evaluating any: a bad entry is the
	// caller's to fix, and a half-answered batch is harder to read than a
	// refusal.
	resources := make([]authz.Resource, len(queries))
	for i, q := range queries {
		res, err := canQueryResource(q)
		if err != nil {
			if single {
				return errValidation(err.Error(), canFix), nil
			}
			return errValidation(fmt.Sprintf("queries[%d]: %s", i, err), canFix), nil
		}
		resources[i] = res
	}
	eff := tools.CallerACL(ctx)
	if eff == nil {
		return noACLSet(), nil
	}
	answers := make([]map[string]any, len(queries))
	for i, q := range queries {
		answers[i] = canAnswer(ctx, eff, q.Action, resources[i])
	}
	if single {
		return okJSON(answers[0])
	}
	return okJSONCount(map[string]any{"evaluations": answers}, len(answers))
}

const canFix = `Name a catalog type and one of its verbs, e.g. {"action":"run","resource":{"type":"agent","id":"chat/support"}}; op=help topic=Context/can lists them.`

// canQueryResource checks one question against the catalog and returns the
// resource it asks about. An omitted id is the type-level question.
func canQueryResource(q canQuery) (authz.Resource, error) {
	if q.Action == "" || q.Resource == nil || q.Resource.Type == "" {
		return authz.Resource{}, errors.New("action and resource.type are required")
	}
	if _, ok := authz.TypeSpecOf(q.Resource.Type); !ok {
		return authz.Resource{}, fmt.Errorf("%q is not a resource type", q.Resource.Type)
	}
	if !authz.ValidVerb(q.Resource.Type, q.Action) {
		spec, _ := authz.TypeSpecOf(q.Resource.Type)
		return authz.Resource{}, fmt.Errorf("%q is not an action on %s (one of: %v)", q.Action, q.Resource.Type, spec.Verbs)
	}
	name := q.Resource.ID
	if name == "" {
		name = authz.AnyInstance
	}
	if err := authz.ValidateResourceName(q.Resource.Type, name); err != nil {
		return authz.Resource{}, err
	}
	return authz.Resource{Type: q.Resource.Type, Name: name}, nil
}

// canAnswer evaluates one question. `decision`, `outcome`, `reasons`,
// `tenant_floor` and `mode` are the HTTP twin's fields: decision is true only
// when a rule allowed. `allowed` is what the access-control gate does with
// that answer under the mode — in compat a question no rule decides is left
// to the scope checks (`fallback: "scopes"`), which the gate lets through and
// the definition's own grants then decide; shadow mode always leaves it to
// them.
func canAnswer(ctx context.Context, eff *authz.Effective, action string, res authz.Resource) map[string]any {
	r := authz.Evaluate(ctx, eff, action, res, true)
	d := r.Decision
	mode := authz.ModeFrom(ctx)
	out := map[string]any{
		"decision": d.Outcome == authz.Allowed,
		"outcome":  d.Outcome.String(),
		"mode":     string(mode),
		"allowed":  r.Allowed && !r.Unavailable,
	}
	if len(d.Reasons) > 0 {
		out["reasons"] = d.Reasons
	}
	if d.TenantFloor {
		out["tenant_floor"] = true
	}
	if (mode == authz.ModeCompat && d.Outcome == authz.NoMatch) || mode == authz.ModeShadow {
		out["fallback"] = "scopes"
	}
	return out
}

// ---- reach ----

// execReach answers "what of this type may I act on" for planning: the
// selector patterns of the rules that apply to the run, unconditional allows
// and denies, with deny-overrides semantics. A conditional rule cannot be a
// pattern; its presence is flagged so the agent asks op=can per resource.
func (c *Context) execReach(ctx context.Context, in contextInput) (tools.Result, error) {
	if in.Type == "" || in.Action == "" {
		return errValidation("type and action are required", `Pass {"op":"reach","type":"agent","action":"run"}.`), nil
	}
	spec, ok := authz.TypeSpecOf(in.Type)
	if !ok {
		return errValidation(fmt.Sprintf("%q is not a resource type", in.Type), "op=help topic=Context/reach lists the types."), nil
	}
	if !authz.ValidVerb(in.Type, in.Action) {
		return errValidation(fmt.Sprintf("%q is not an action on %s (one of: %v)", in.Action, in.Type, spec.Verbs), ""), nil
	}
	eff := tools.CallerACL(ctx)
	if eff == nil {
		return noACLSet(), nil
	}
	mode := authz.ModeFrom(ctx)
	unmatched := "scopes"
	if mode == authz.ModeEnforce {
		unmatched = "denied"
	}
	out := map[string]any{
		"type":      in.Type,
		"action":    in.Action,
		"mode":      string(mode),
		"allow":     nonNilStrings(eff.AllowPatterns(in.Type, in.Action)),
		"deny":      nonNilStrings(eff.DenyPatterns(in.Type, in.Action)),
		"semantics": "deny_overrides",
		"unmatched": unmatched,
	}
	if eff.HasConditionalRules(in.Type, in.Action) {
		out["conditional"] = true
	}
	switch in.Type {
	case "tool":
		out["removed_tools"] = nonNilStrings(tools.PolicyOf(ctx).ACLRemovedTools)
	case "agent":
		out["see_also"] = "op=agents lists the agents this run may start"
	}
	return okJSON(out)
}
