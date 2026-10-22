package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/dynvol"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// OperatorTokenDef is the RFC L built-in that mints, rotates, retires,
// and inspects the bearer tokens that replace the single
// LOOMCYCLE_AUTH_TOKEN shared secret. Each token binds an AUTHORITATIVE
// PRINCIPAL (tenant_id + subject + allowed_scopes) that the auth
// middleware (PR2) stamps into ctx.
//
// This is an OPERATOR-ADMIN capability — it is reached only via the
// admin transports (CLI / POST /v1/_operatortokendef / gRPC / MCP), all
// of which set OperatorTokenDefPolicy{Admin:true} after clearing the
// substrate:admin scope check. Agents do NOT get this tool in their
// tools, and the default policy is deny.
//
// NOT a versioned/forkable substrate Def: a secret has no "content hash
// to fork", so there is no fork op, no version, no active pointer —
// rotation (not promotion) is the lifecycle.
type OperatorTokenDef struct {
	// Store is the persistence backend. Required.
	Store store.Store

	// Hasher turns a minted token into its stored token_hash — the same
	// hasher the auth lookup uses (config.Env.TokenHasher: the explicit
	// LOOMCYCLE_OPERATOR_TOKEN_PEPPER, else one derived from
	// LOOMCYCLE_SECRET_KEY). The zero value hashes with no pepper
	// (single-binary dev).
	Hasher auth.TokenHasher

	// Audit records create/rotate/retire. Never nil at runtime (main.go
	// wires audit.NopSink when no path is configured).
	Audit audit.Sink

	// RotationGraceSeconds is the default window during which a rotated
	// token still authenticates. 0 → 24h.
	RotationGraceSeconds int

	// MaxScopes caps the allowed_scopes list length (defense-in-depth).
	// 0 → 64.
	MaxScopes int

	// LegacyTokenSet reports whether LOOMCYCLE_AUTH_TOKEN is configured.
	// When false, retiring the LAST admin-scoped token would flip
	// authConfigured to false → unauthenticated open mode (or, with config
	// principals declared, leave no admin credential), so the no-fail-open
	// guard in execRetire refuses it. When a legacy fallback exists, retiring
	// the last admin merely re-enables it (recoverable), so the guard does not
	// fire. A func read at retire time, like AdminPrincipalDeclared: a config
	// reload can set or clear the secret, and the bearer check reads it live,
	// so the guard must judge the same config. Nil means not set.
	LegacyTokenSet func() bool

	// AdminPrincipalDeclared reports whether a config principal that can log in
	// holds substrate:admin. Then retiring the last admin token leaves an admin
	// credential and, since declared principals keep auth on, no open mode, so
	// execRetire allows it. A declared principal WITHOUT admin keeps auth on
	// too, but the retire would leave nobody able to manage tokens, so it stays
	// refused. A func read at retire time, not a boot value: a config reload
	// can drop the admin principal, and a stale "yes" would then let the last
	// admin token go — leaving no admin, or with no principal left, open mode.
	// Nil means no admin principal.
	AdminPrincipalDeclared func() bool

	// ConfigTenants are the tenant ids the config `principals:` block declares
	// (config.PrincipalTenants, captured at boot like the principals themselves).
	// create refuses a tenant that differs from one of these only by case.
	ConfigTenants []string
}

// operatorTokenNameRe constrains name / tenant_id / subject. RFC L:
// the subject becomes the authoritative user_id (it overrides the wire
// user_id at run creation), and user_id must satisfy validIdent
// ([A-Za-z0-9_-]) so it is safe as a URL path segment + fairness key —
// hence no '.' or ':' here. Tenants/names share the charset for symmetry.
var operatorTokenNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

const operatorTokenDefDescription = `Mint, rotate, retire, and inspect operator auth tokens (see Context op=help topic=operator-tokens). ` +
	`Each token binds a principal {tenant_id, subject, allowed_scopes}. ` +
	`OPERATOR-ADMIN only. Operations: create, rotate, retire, get, list. ` +
	`The token plaintext is shown ONCE on create/rotate and never again.`

const operatorTokenDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":         {"type": "string", "enum": ["create","rotate","retire","get","list"], "description": "Operation to perform."},
    "name":       {"type": "string", "description": "Token name (required for create/list; create/rotate/retire accept name or def_id)."},
    "def_id":     {"type": "string", "description": "Existing def_id (for get; alternative target for rotate/retire)."},
    "tenant_id":  {"type": "string", "description": "Authoritative tenant (required for create)."},
    "subject":    {"type": "string", "description": "Authoritative subject (optional for create; defaults to tok-<name>)."},
    "scopes":     {"type": "array", "items": {"type": "string"}, "description": "Allowed scopes from the closed catalog. REQUIRED on create (except with import_token) — an omitted list is refused rather than defaulting to [substrate:admin], because that made a mistyped key mint a full-power token. Note the asymmetry: the request field is scopes, the response echoes it as allowed_scopes."},
    "ttl_seconds": {"type": "integer", "description": "Lifetime in seconds (create). The token stops authenticating when it runs out, and a later rotate does not extend it. Omit for a token that never expires. Refused for a token holding substrate:admin."},
    "agents": {"type": "array", "items": {"type": "string"}, "description": "Optional (create). Limits the token to starting these agents as top-level runs, and to the run endpoints: it is refused everywhere else, whatever its scopes. An agent a listed one spawns is not limited. Omit for no limit; an empty list is refused."},
    "overrides": {"description": "Optional (create). Locks the per-run settings for runs this token starts, and for retunes it makes: the string \"none\" (every such setting is locked — the model, provider, tier, effort, sampling, limits, output format and the rest), or an array of the settings it may still set. A request that sets a locked one is refused, not ignored. Omit for no lock."},
    "rate": {"type": "object", "description": "Optional (create). Bounds how often this token may start runs: {\"per_minute\": N, \"burst\": M}. burst is how many it may start at once from idle (default: per_minute). A start past the rate is refused with a time to wait. Omit to use the server's default, if it has one.", "properties": {"per_minute": {"type": "integer"}, "burst": {"type": "integer"}}, "required": ["per_minute"]},
    "retention": {"type": "string", "enum": ["none", "default"], "description": "Optional (create). \"none\" makes every run this token starts a no-trace run: its content is delivered to the caller and stored nowhere, and neither a request nor an agent can change that. Omit (or \"default\") to leave the mode to the request and the agent."},
    "grace_seconds": {"type": "integer", "description": "Rotation grace window override (rotate)."},
    "import_token": {"type": "string", "description": "Migration only (create): bind an existing secret (the legacy LOOMCYCLE_AUTH_TOKEN) instead of minting. Hashed, never echoed."}
  },
  "required": ["op"]
}`

type operatorTokenDefInput struct {
	Op         string   `json:"op"`
	Name       string   `json:"name,omitempty"`
	DefID      string   `json:"def_id,omitempty"`
	TenantID   string   `json:"tenant_id,omitempty"`
	Subject    string   `json:"subject,omitempty"`
	Scopes     []string `json:"scopes,omitempty"`
	TTLSeconds *int64   `json:"ttl_seconds,omitempty"`
	// Agents is a pointer so an absent list (no limit) and an empty one
	// (refused) are told apart.
	Agents       *[]string            `json:"agents,omitempty"`
	Overrides    *auth.OverridePolicy `json:"overrides,omitempty"`
	Rate         *auth.RateLimit      `json:"rate,omitempty"`
	Retention    string               `json:"retention,omitempty"`
	GraceSeconds *int                 `json:"grace_seconds,omitempty"`
	// ImportToken (Decision 10 migration) binds an existing secret —
	// the legacy LOOMCYCLE_AUTH_TOKEN via `--copy-from-env` — instead of
	// minting. Admin-only; the plaintext is hashed, never echoed.
	ImportToken string `json:"import_token,omitempty"`
}

// Name implements tools.Tool.
func (s *OperatorTokenDef) Name() string { return "OperatorTokenDef" }

// Description implements tools.Tool.
func (s *OperatorTokenDef) Description() string { return operatorTokenDefDescription }

// InputSchema implements tools.Tool.
func (s *OperatorTokenDef) InputSchema() json.RawMessage {
	return json.RawMessage(operatorTokenDefInputSchema)
}

// Execute implements tools.Tool.
func (s *OperatorTokenDef) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if s.Store == nil {
		return errResult("OperatorTokenDef tool: not configured (no Store backend)"), nil
	}
	if !tools.OperatorTokenDefPolicy(ctx).Admin {
		// Default-deny: only the admin transports grant this.
		return errResult("OperatorTokenDef tool: requires operator-admin (substrate:admin)"), nil
	}
	// ⚠️ UNKNOWN KEYS ARE REFUSED ON THIS TOOL, which is not how the rest of
	// the substrate reads its input — and the asymmetry is the point.
	//
	// Everywhere else a stray field is harmless: the value is dropped and the
	// caller gets a def that differs from what they typed. Here the dropped
	// field decided PRIVILEGE, and the surrounding defaults turned "I did not
	// understand your key" into "you now hold admin". `allowed_scopes` is the
	// key that did it, and it is not an unreasonable guess: the response echoes
	// the list under that name and so does the stored column.
	//
	// So this input is parsed strictly. An operator who mistypes a field on a
	// token mint gets a 400 naming the field, not a token with more power than
	// they asked for.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in operatorTokenDefInput
	if err := dec.Decode(&in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s%s", err, mintKeyHint(err))), nil
	}
	switch in.Op {
	case "create":
		return s.execCreate(ctx, in)
	case "rotate":
		return s.execRotate(ctx, in)
	case "retire":
		return s.execRetire(ctx, in)
	case "get":
		return s.execGet(ctx, in)
	case "list":
		return s.execList(ctx, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, rotate, retire, get, list)", in.Op)), nil
	}
}

func (s *OperatorTokenDef) grace() time.Duration {
	if s.RotationGraceSeconds > 0 {
		return time.Duration(s.RotationGraceSeconds) * time.Second
	}
	return 24 * time.Hour
}

func (s *OperatorTokenDef) maxScopes() int {
	if s.MaxScopes > 0 {
		return s.MaxScopes
	}
	return 64
}

// ---- create ----

func (s *OperatorTokenDef) execCreate(ctx context.Context, in operatorTokenDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("create: missing required field: name"), nil
	}
	if !operatorTokenNameRe.MatchString(in.Name) {
		return errResult(fmt.Sprintf("create: name %q invalid (must match [a-zA-Z0-9_-]{1,64})", in.Name)), nil
	}
	if auth.IsUserTokenName(in.Name) {
		return errResult(fmt.Sprintf("create: name %q is in the per-user token namespace (%q…) — choose another name",
			in.Name, auth.UserTokenNamePrefix)), nil
	}
	if in.TenantID == "" {
		return errResult("create: missing required field: tenant_id"), nil
	}
	if !operatorTokenNameRe.MatchString(in.TenantID) {
		return errResult(fmt.Sprintf("create: tenant_id %q invalid (must match [a-zA-Z0-9_-]{1,64})", in.TenantID)), nil
	}
	if err := s.checkTenantCaseFold(ctx, in.TenantID); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	// Hoisted above the scope check: the migration path is the one place an
	// omitted scope list still means admin.
	imported := in.ImportToken != ""
	subject := in.Subject
	if subject == "" {
		// Decision 4: default to a stable per-token id so attribution /
		// fairness / audit are still distinct even without an explicit
		// subject. Hyphen (not colon) so the derived user_id stays
		// validIdent-safe — it overrides the wire user_id (RFC L).
		subject = "tok-" + in.Name
	} else if !operatorTokenNameRe.MatchString(subject) {
		return errResult(fmt.Sprintf("create: subject %q invalid (must match [a-zA-Z0-9_-]{1,64})", subject)), nil
	}
	scopes := in.Scopes
	if len(scopes) == 0 {
		// ⚠️ AN OMITTED SCOPE LIST USED TO MEAN [substrate:admin], which made
		// the failure mode of every mistake MAXIMUM PRIVILEGE — and that
		// mistake is self-locking: one admin-scoped def turns off the legacy
		// LOOMCYCLE_AUTH_TOKEN fallback (legacyFallbackDisabled →
		// OperatorTokenDefCountActiveAdminMatching), so an accidental admin token
		// revokes the very bearer that minted it. Hit live on 2026-09-21: a
		// caller sent `allowed_scopes` — the name the RESPONSE and the stored
		// column both use — json.Unmarshal dropped the unknown key, and three
		// tenant tokens came back as admin, two of them locking their
		// deployment out of the API entirely.
		//
		// The legitimate admin mint is not a mistake: it is an operator typing
		// a command. So admin is now stated rather than inherited, and the
		// failure mode of a typo is an inert 400.
		//
		// import_token is the one exception, below: binding the legacy
		// LOOMCYCLE_AUTH_TOKEN means "this IS the admin token", and the
		// migration cannot ask the operator to say so twice.
		if !imported {
			return errResult("create: missing required field: scopes — name them explicitly " +
				"(e.g. [\"substrate:tenant\"], or [\"" + auth.ScopeAdmin + "\"] for a full-power token). " +
				"An omitted list no longer defaults to " + auth.ScopeAdmin + ": that made every " +
				"mis-spelled key mint an admin token, which also disables the legacy " +
				"LOOMCYCLE_AUTH_TOKEN login and locks the deployment out"), nil
		}
		scopes = []string{auth.ScopeAdmin}
	}
	if len(scopes) > s.maxScopes() {
		return errResult(fmt.Sprintf("create: too many scopes (%d > %d)", len(scopes), s.maxScopes())), nil
	}
	if bad := auth.UnknownScopes(scopes); bad != nil {
		return errResult(fmt.Sprintf("create: unknown scope(s) %v — not in the closed catalog", bad)), nil
	}

	var restrictions auth.Restrictions
	if in.Agents != nil {
		restrictions.Agents = *in.Agents
	}
	restrictions.Overrides, restrictions.Rate = in.Overrides, in.Rate
	retention, rerr := auth.ParseRetention(in.Retention)
	if rerr != nil {
		return errResult(fmt.Sprintf("create: %s", rerr)), nil
	}
	restrictions.Retention = retention
	if err := restrictions.Validate(in.Agents != nil); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	restrictions, rerr = heldToMinter(ctx, restrictions)
	if rerr != nil {
		return errResult(fmt.Sprintf("create: %s", rerr)), nil
	}
	// A token confined to the run plane cannot mint, rotate or retire, so an
	// admin token carrying an allowlist would be an admin token that cannot
	// administer — and one that still switches the legacy login off.
	if restrictions.RunPlaneOnly() && auth.HasScope(scopes, auth.ScopeAdmin) {
		return errResult("create: agents is not allowed on a token holding " + auth.ScopeAdmin), nil
	}
	restrictionsJSON, err := auth.MarshalRestrictions(restrictions)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	var expiresAt time.Time
	if in.TTLSeconds != nil {
		if imported {
			return errResult("create: ttl_seconds cannot be combined with import_token"), nil
		}
		// An admin token that expires is a lockout waiting for a date: once
		// one exists the legacy LOOMCYCLE_AUTH_TOKEN login is off, and when
		// the last one runs out nothing can mint its replacement.
		if auth.HasScope(scopes, auth.ScopeAdmin) {
			return errResult("create: ttl_seconds is not allowed on a token holding " + auth.ScopeAdmin +
				" — an expiring admin token can lock the deployment out"), nil
		}
		const maxTTLSeconds = int64(100 * 365 * 24 * 3600)
		if *in.TTLSeconds <= 0 || *in.TTLSeconds > maxTTLSeconds {
			return errResult(fmt.Sprintf("create: ttl_seconds must be between 1 and %d", maxTTLSeconds)), nil
		}
		expiresAt = time.Now().Add(time.Duration(*in.TTLSeconds) * time.Second).UTC()
	}

	// A name has at most one current (non-retired) token; force `rotate`
	// for an existing live name so a create can't silently shadow it.
	if _, err := s.Store.OperatorTokenDefGetCurrentByName(ctx, in.Name); err == nil {
		return errResult(fmt.Sprintf("create: name %q already has a live token — use `rotate` to replace it", in.Name)), nil
	} else {
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			return errResult(fmt.Sprintf("create: %s", err)), nil
		}
	}

	// Token source. Default: mint a fresh CSPRNG token. Migration
	// exception (Decision 10): `import_token` binds an EXISTING secret —
	// the legacy LOOMCYCLE_AUTH_TOKEN, via `--copy-from-env` — to a real
	// principal so the env token keeps working as an admin token after
	// the legacy fallback disables (zero-disruption upgrade). This is the
	// only sanctioned operator-supplied-token path; it imports a hash,
	// never echoes the plaintext back.
	var plaintext, suffix string
	if imported {
		if len(in.ImportToken) < 8 {
			return errResult("create: import_token too short (min 8 chars)"), nil
		}
	} else {
		var err error
		if plaintext, suffix, err = auth.MintToken(); err != nil {
			return errResult(fmt.Sprintf("create: %s", err)), nil
		}
	}
	hashInput := plaintext
	if imported {
		hashInput = in.ImportToken
	}
	ident := tools.RunIdentity(ctx)
	row := store.OperatorTokenDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		TenantID:         in.TenantID,
		Subject:          subject,
		TokenHash:        s.Hasher.Hash(hashInput),
		AllowedScopes:    scopes,
		CreatedByAgentID: ident.AgentID,
		ExpiresAt:        expiresAt,
		RestrictionsJSON: restrictionsJSON,
	}
	created, err := s.Store.OperatorTokenDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	s.audit(ctx, audit.Event{
		Action: "create", TargetDefID: created.DefID, TargetName: created.Name,
		TargetTenant: created.TenantID, TargetSubject: created.Subject, ScopesAfter: scopes,
	})
	if imported {
		// No plaintext to show — the operator already holds the env token.
		m := operatorTokenRowResponse(created)
		m["imported"] = true
		return okJSON(m)
	}
	return okJSON(operatorTokenCreateResponse(created, plaintext, suffix))
}

// checkTenantCaseFold refuses a tenant id that would share an on-disk volume
// tree with another tenant on a case-insensitive filesystem (the macOS
// default, a ZFS dataset with casesensitivity=insensitive): a reserved
// segment in any case ("_Shared"), or an id that differs only by case from a
// tenant that already exists ("Acme" beside "acme").
//
// "Already exists" means: named by any token row, retired ones included (a
// retired tenant's volumes stay on disk), or declared by a config principal.
// Those are the two registries of tenants; there is no tenants table, and a
// principal-bearing run / user / def carries a tenant one of them issued. (A
// legacy-token or open-mode caller may name any wire tenant; that is the
// operator's own choice, not a registry, so it is not consulted — and the
// volume tool still refuses a reserved or non-charset one at use.)
// A tenant spelled exactly as an existing one is that tenant and passes.
// A store fault refuses the mint rather than skipping the check.
func (s *OperatorTokenDef) checkTenantCaseFold(ctx context.Context, tenantID string) error {
	if dynvol.ReservedTenant(tenantID) {
		return fmt.Errorf("tenant_id %q is reserved", tenantID)
	}
	known, err := tokenTenants(ctx, s.Store)
	if err != nil {
		return fmt.Errorf("listing existing tenants: %w", err)
	}
	known = append(known, s.ConfigTenants...)
	for _, k := range known {
		if k == tenantID {
			return nil
		}
	}
	if other, clash := dynvol.CaseFoldCollision(tenantID, known); clash {
		return fmt.Errorf("tenant_id %q differs only by case from existing tenant %q — they would share one volume directory on a case-insensitive filesystem; use %q or a distinct name", tenantID, other, other)
	}
	return nil
}

// tokenTenants returns the tenant id of every token name, retired history
// included.
func tokenTenants(ctx context.Context, st store.Store) ([]string, error) {
	names, err := st.OperatorTokenDefListNames(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, n.TenantID)
	}
	return out, nil
}

// CheckPrincipalTenantsAgainstTokens is the boot-time half of the case-fold
// rule for config principals. Config load compares principals only with each
// other (it has no store); a principal added to the config AFTER a token was
// minted for a tenant differing only by case is caught here instead. A
// principal tenant spelled exactly as a token's tenant is the same tenant.
func CheckPrincipalTenantsAgainstTokens(ctx context.Context, st store.Store, principalTenants []string) error {
	if len(principalTenants) == 0 {
		return nil
	}
	known, err := tokenTenants(ctx, st)
	if err != nil {
		return fmt.Errorf("listing token tenants: %w", err)
	}
	for _, pt := range principalTenants {
		if other, clash := dynvol.CaseFoldCollision(pt, known); clash {
			return fmt.Errorf("config principal tenant %q differs only by case from minted token tenant %q — they would share one volume directory on a case-insensitive filesystem; spell the principal's tenant %q", pt, other, other)
		}
	}
	return nil
}

// ---- rotate ----

func (s *OperatorTokenDef) execRotate(ctx context.Context, in operatorTokenDefInput) (tools.Result, error) {
	prior, err := s.resolveTarget(ctx, "rotate", in)
	if err != nil {
		return errResult(err.Error()), nil
	}
	// The replacement inherits the lifetime, so rotating a token that has run
	// out would mint one that is dead on arrival.
	if !prior.ExpiresAt.IsZero() && !time.Now().Before(prior.ExpiresAt) {
		return errResult(fmt.Sprintf("rotate: token %q has expired — create a new one", prior.DefID)), nil
	}
	// The caller gets the new secret, so a token less restricted than the
	// caller would be a way out of the caller's restrictions. Refused, not
	// narrowed: narrowing would quietly take reach from the token's owner,
	// and let a restricted admin downgrade other principals' tokens.
	prev, pErr := auth.ParseRestrictions(prior.RestrictionsJSON)
	if pErr != nil {
		return errResult(fmt.Sprintf("rotate: token %q: %s", prior.DefID, pErr)), nil
	}
	if held, hErr := heldToMinter(ctx, prev); hErr != nil || !sameRestrictions(held, prev) {
		return errResult("rotate: this token is less restricted than your own; only a caller at least as permissive may rotate it"), nil
	}
	plaintext, suffix, mErr := auth.MintToken()
	if mErr != nil {
		return errResult(fmt.Sprintf("rotate: %s", mErr)), nil
	}
	ident := tools.RunIdentity(ctx)
	row := store.OperatorTokenDefRow{
		DefID:            mintDefID(),
		Name:             prior.Name,
		TenantID:         prior.TenantID,
		Subject:          prior.Subject,
		TokenHash:        s.Hasher.Hash(plaintext),
		AllowedScopes:    prior.AllowedScopes,
		CreatedByAgentID: ident.AgentID,
		RotatedFrom:      prior.DefID,
		// Copied, never recomputed: a rotation replaces the secret, it does
		// not grant the token more time — or more reach.
		ExpiresAt:        prior.ExpiresAt,
		RestrictionsJSON: prior.RestrictionsJSON,
	}
	created, err := s.Store.OperatorTokenDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("rotate: %s", err)), nil
	}
	grace := s.grace()
	if in.GraceSeconds != nil && *in.GraceSeconds >= 0 {
		grace = time.Duration(*in.GraceSeconds) * time.Second
	}
	retireOldAt := time.Now().Add(grace).UTC()
	if err := s.Store.OperatorTokenDefSetRetiredAt(ctx, prior.DefID, retireOldAt); err != nil {
		return errResult(fmt.Sprintf("rotate: retire prior: %s", err)), nil
	}
	s.audit(ctx, audit.Event{
		Action: "rotate", TargetDefID: created.DefID, TargetName: created.Name,
		TargetTenant: created.TenantID, TargetSubject: created.Subject,
		ScopesBefore: prior.AllowedScopes, ScopesAfter: created.AllowedScopes,
	})
	resp := operatorTokenCreateResponse(created, plaintext, suffix)
	resp["rotated_from"] = prior.DefID
	resp["prior_retires_at"] = retireOldAt.Format(time.RFC3339Nano)
	return okJSON(resp)
}

// heldToMinter is r held to the restrictions of the credential minting it,
// the stricter of each (auth.Restrictions.Within): an admin token restricted
// to no-trace runs, a rate or locked overrides must not be able to mint
// itself a token without them. A caller with no principal — the stdio MCP,
// an open-mode deployment — is the unrestricted operator.
func heldToMinter(ctx context.Context, r auth.Restrictions) (auth.Restrictions, error) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return r, nil
	}
	return r.Within(p.Restrictions)
}

// sameRestrictions reports whether a and b restrict the same things: the
// same mode, the same rate (an unset burst reads as the per-minute rate), the
// same overrides allowed and the same agents, in any order.
func sameRestrictions(a, b auth.Restrictions) bool {
	sameSet := func(x, y []string) bool {
		if len(x) != len(y) {
			return false
		}
		in := make(map[string]bool, len(x))
		for _, v := range x {
			in[v] = true
		}
		for _, v := range y {
			if !in[v] {
				return false
			}
		}
		return true
	}
	if a.Retention != b.Retention || !sameSet(a.Agents, b.Agents) {
		return false
	}
	if (a.Rate == nil) != (b.Rate == nil) || (a.Rate != nil &&
		(a.Rate.PerMinute != b.Rate.PerMinute || a.Rate.EffectiveBurst() != b.Rate.EffectiveBurst())) {
		return false
	}
	if (a.Overrides == nil) != (b.Overrides == nil) {
		return false
	}
	return a.Overrides == nil || sameSet(a.Overrides.Allow, b.Overrides.Allow)
}

// ---- retire ----

func (s *OperatorTokenDef) execRetire(ctx context.Context, in operatorTokenDefInput) (tools.Result, error) {
	target, err := s.resolveTarget(ctx, "retire", in)
	if err != nil {
		return errResult(err.Error()), nil
	}
	// No-fail-open guard (RFC L Decision 12). Retiring the last active
	// admin-scoped token drops the active-admin count to zero; with no legacy
	// LOOMCYCLE_AUTH_TOKEN fallback, authConfigured then flips to false and the
	// server falls into UNAUTHENTICATED open mode — a silent fail-open worse
	// than a lockout. Refuse it; the operator must create/rotate another admin
	// token first (or keep the legacy fallback). Rotate is exempt by
	// construction: it mints a replacement admin token for the same principal
	// before retiring the prior, so the count never reaches zero. An admin
	// config principal is a standing admin credential, so it lifts the guard.
	adminPrincipal := s.AdminPrincipalDeclared != nil && s.AdminPrincipalDeclared()
	legacy := s.LegacyTokenSet != nil && s.LegacyTokenSet()
	if !legacy && !adminPrincipal && auth.HasScope(target.AllowedScopes, auth.ScopeAdmin) {
		// Only admin tokens that can still authenticate (RFC ES F3): a row
		// under a pepper no longer configured is no fallback.
		include, exclude := s.Hasher.UsablePrefixes()
		n, cErr := s.Store.OperatorTokenDefCountActiveAdminMatching(ctx, store.TokenHashMatch{Prefixes: include, ExcludePrefixes: exclude})
		if cErr != nil {
			return errResult(fmt.Sprintf("retire: admin-count check: %s", cErr)), nil
		}
		if n <= 1 {
			return errResult("retire: refusing to retire the last admin-scoped token — with no LOOMCYCLE_AUTH_TOKEN fallback and no admin config principal this would leave no admin credential (and, with no config principals, drop the server into unauthenticated open mode); create or rotate another substrate:admin token first"), nil
		}
	}
	now := time.Now().UTC()
	if err := s.Store.OperatorTokenDefSetRetiredAt(ctx, target.DefID, now); err != nil {
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	s.audit(ctx, audit.Event{
		Action: "retire", TargetDefID: target.DefID, TargetName: target.Name,
		TargetTenant: target.TenantID, TargetSubject: target.Subject, ScopesBefore: target.AllowedScopes,
	})
	return okJSON(map[string]any{"def_id": target.DefID, "name": target.Name, "retired_at": now.Format(time.RFC3339Nano)})
}

// ---- get / list ----

func (s *OperatorTokenDef) execGet(ctx context.Context, in operatorTokenDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("get: missing required field: def_id"), nil
	}
	row, err := s.Store.OperatorTokenDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("get: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("get: %s", err)), nil
	}
	return okJSON(operatorTokenRowResponse(row))
}

func (s *OperatorTokenDef) execList(ctx context.Context, in operatorTokenDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("list: missing required field: name"), nil
	}
	rows, err := s.Store.OperatorTokenDefListByName(ctx, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("list: %s", err)), nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, operatorTokenRowResponse(r))
	}
	return okJSONCount(map[string]any{"name": in.Name, "tokens": out}, len(out))
}

// ---- helpers ----

// resolveTarget finds the row a rotate/retire targets, by def_id or by
// name (the name's current non-retired token).
func (s *OperatorTokenDef) resolveTarget(ctx context.Context, op string, in operatorTokenDefInput) (store.OperatorTokenDefRow, error) {
	if in.DefID != "" {
		row, err := s.Store.OperatorTokenDefGet(ctx, in.DefID)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return store.OperatorTokenDefRow{}, fmt.Errorf("%s: def_id %q not found", op, in.DefID)
			}
			return store.OperatorTokenDefRow{}, fmt.Errorf("%s: %s", op, err)
		}
		return row, nil
	}
	if in.Name != "" {
		// Names in the per-user namespace are chosen by tenant operators and
		// repeat across tenants, so "the current token named X" there is
		// whichever tenant minted last. Acting on it by name could rotate or
		// retire another tenant's token than the one meant.
		if auth.IsUserTokenName(in.Name) {
			return store.OperatorTokenDefRow{}, fmt.Errorf("%s: name %q is a per-user token name, which is not unique across tenants — target it by def_id", op, in.Name)
		}
		row, err := s.Store.OperatorTokenDefGetCurrentByName(ctx, in.Name)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return store.OperatorTokenDefRow{}, fmt.Errorf("%s: no live token for name %q", op, in.Name)
			}
			return store.OperatorTokenDefRow{}, fmt.Errorf("%s: %s", op, err)
		}
		return row, nil
	}
	return store.OperatorTokenDefRow{}, fmt.Errorf("%s: need name or def_id", op)
}

func (s *OperatorTokenDef) audit(ctx context.Context, ev audit.Event) {
	if s.Audit == nil {
		return
	}
	ident := tools.RunIdentity(ctx)
	ev.ActorSubject = ident.UserID
	// ActorTenant is wired in PR2 once RunIdentity carries an
	// authoritative TenantID (the identity-threading deliverable).
	_ = s.Audit.Record(ev) // best-effort; never blocks the op
}

// operatorTokenRowResponse renders a row WITHOUT any secret material
// (token plaintext is never stored; token_hash is json:"-").
func operatorTokenRowResponse(row store.OperatorTokenDefRow) map[string]any {
	m := map[string]any{
		"def_id":              row.DefID,
		"name":                row.Name,
		"tenant_id":           row.TenantID,
		"subject":             row.Subject,
		"allowed_scopes":      row.AllowedScopes,
		"created_at":          row.CreatedAt.UTC().Format(time.RFC3339Nano),
		"created_by_agent_id": row.CreatedByAgentID,
		"retired":             !row.RetiredAt.IsZero(),
	}
	if row.RotatedFrom != "" {
		m["rotated_from"] = row.RotatedFrom
	}
	if !row.RetiredAt.IsZero() {
		m["retired_at"] = row.RetiredAt.UTC().Format(time.RFC3339Nano)
	}
	if !row.ExpiresAt.IsZero() {
		m["expires_at"] = row.ExpiresAt.UTC().Format(time.RFC3339Nano)
		m["expired"] = !time.Now().Before(row.ExpiresAt)
	}
	if row.RestrictionsJSON != "" {
		// Shown as stored. A record this build cannot read still shows, so an
		// operator can see why the token is refused at login.
		m["restrictions"] = json.RawMessage(row.RestrictionsJSON)
	}
	return m
}

// operatorTokenCreateResponse adds the show-once plaintext + suffix to
// the row response. The ONLY place the plaintext ever crosses the wire.
func operatorTokenCreateResponse(row store.OperatorTokenDefRow, plaintext, suffix string) map[string]any {
	m := operatorTokenRowResponse(row)
	m["token"] = plaintext
	m["token_suffix"] = suffix
	m["warning"] = "store this token now — it is shown once and cannot be retrieved later"
	return m
}

// mintKeyHint names the one wrong key that has actually cost a deployment, so
// the error says what to type instead of only what was wrong.
//
// `allowed_scopes` is what the response and the stored column call this list —
// a caller reading either one back and sending it forward lands exactly here.
func mintKeyHint(err error) string {
	if err == nil || !strings.Contains(err.Error(), "allowed_scopes") {
		return ""
	}
	return " — the request field is \"scopes\"; \"allowed_scopes\" is what the RESPONSE " +
		"calls it. They were silently different until this check existed, and the " +
		"difference minted admin tokens"
}
