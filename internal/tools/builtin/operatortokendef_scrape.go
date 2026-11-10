package builtin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A tenant operator's reach into the token tool (tools.OperatorTokenDefPolicy
// TenantScrape): the metrics scrape tokens of its own tenant, and nothing
// else. A scrape token it mints reads only its tenant's series on /metrics
// (tenant_metrics), so handing one to a tenant's monitoring stack shows the
// tenant nothing of the runtime or of other tenants. Every other token —
// an admin's, another tenant's, an unconfined scrape token in its own tenant
// — is answered as a token that does not exist.

// scrapeTokenNamePrefix starts the name of every scrape token a tenant
// operator mints. Token names are one namespace for the whole deployment, so
// a create refused for a name another tenant holds would tell this tenant
// that name exists. Inside the per-user namespace names may repeat across
// tenants, and nothing acts on one by name across tenants (auth.
// UserTokenNamePrefix), so the duplicate check only ever reads the caller's
// own tenant.
const scrapeTokenNamePrefix = auth.UserTokenNamePrefix + "metrics-"

// scrapeTokenLabelRe is the name a tenant operator gives; the stored name is
// scrapeTokenNamePrefix + the label, within the 64 characters a name allows.
var scrapeTokenLabelRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,54}$`)

func (s *OperatorTokenDef) execTenantScrape(ctx context.Context, tenant string, in operatorTokenDefInput) (tools.Result, error) {
	switch in.Op {
	case "create", "rotate", "retire", "get", "list":
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, rotate, retire, get, list)", in.Op)), nil
	}
	if r, refused := tools.AuthorizeTokenOp(ctx, in.Op, tools.TenantScrapeTokens); refused {
		return r, nil
	}
	switch in.Op {
	case "create":
		return s.createTenantScrape(ctx, tenant, in)
	case "get":
		if in.DefID == "" {
			return errResult("get: missing required field: def_id"), nil
		}
		row, err := s.tenantScrapeByID(ctx, "get", tenant, in.DefID)
		if err != nil {
			return errResult(err.Error()), nil
		}
		return okJSON(operatorTokenRowResponse(row))
	case "list":
		return s.listTenantScrape(ctx, tenant, in.Name)
	case "rotate":
		prior, err := s.resolveTenantScrape(ctx, "rotate", tenant, in)
		if err != nil {
			return errResult(err.Error()), nil
		}
		return s.rotateRow(ctx, prior, in.GraceSeconds)
	default: // retire
		target, err := s.resolveTenantScrape(ctx, "retire", tenant, in)
		if err != nil {
			return errResult(err.Error()), nil
		}
		return s.retireRow(ctx, target)
	}
}

func (s *OperatorTokenDef) createTenantScrape(ctx context.Context, tenant string, in operatorTokenDefInput) (tools.Result, error) {
	// The rest of the input narrows runs, which a scrape token never starts,
	// or binds an existing secret, which is the operator's migration.
	if in.ImportToken != "" || in.Agents != nil || len(in.Surfaces) > 0 || in.Overrides != nil ||
		in.Rate != nil || in.Retention != "" || in.DefID != "" || in.GraceSeconds != nil {
		return errResult("create: a tenant operator's scrape token takes only name, subject, scopes, tenant_id, tenant_metrics and ttl_seconds"), nil
	}
	if len(in.Scopes) != 1 || in.Scopes[0] != auth.ScopeMetricsRead {
		return errResult(`create: a tenant operator mints only metrics scrape tokens: scopes must be exactly ["` + auth.ScopeMetricsRead + `"]`), nil
	}
	if in.TenantID != "" && in.TenantID != tenant {
		return errResult(fmt.Sprintf("create: tenant_id %q is not your tenant; a tenant operator mints only in its own tenant (omit tenant_id)", in.TenantID)), nil
	}
	if in.TenantMetrics != nil && !*in.TenantMetrics {
		return errResult("create: a tenant operator's scrape token always reads only its own tenant's series; omit tenant_metrics or set it true"), nil
	}
	if in.Name == "" {
		return errResult("create: missing required field: name"), nil
	}
	label := strings.TrimPrefix(in.Name, scrapeTokenNamePrefix)
	if !scrapeTokenLabelRe.MatchString(label) {
		return errResult(fmt.Sprintf("create: name %q invalid (must match [a-zA-Z0-9_-]{1,54})", in.Name)), nil
	}
	name := scrapeTokenNamePrefix + label
	subject := in.Subject
	if subject == "" {
		subject = "metrics-" + label
	} else if !operatorTokenNameRe.MatchString(subject) {
		return errResult(fmt.Sprintf("create: subject %q invalid (must match [a-zA-Z0-9_-]{1,64})", subject)), nil
	}
	if live, err := s.liveScrapeName(ctx, tenant, name); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	} else if live {
		return errResult(fmt.Sprintf("create: name %q already has a live token — use `rotate` to replace it", name)), nil
	}
	restrictions, err := heldToMinter(ctx, auth.Restrictions{TenantMetrics: true})
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	scopes := []string{auth.ScopeMetricsRead}
	if err := restrictions.CheckScopes(scopes); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	restrictionsJSON, err := auth.MarshalRestrictions(restrictions)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	var expiresAt time.Time
	if in.TTLSeconds != nil {
		const maxTTLSeconds = int64(100 * 365 * 24 * 3600)
		if *in.TTLSeconds <= 0 || *in.TTLSeconds > maxTTLSeconds {
			return errResult(fmt.Sprintf("create: ttl_seconds must be between 1 and %d", maxTTLSeconds)), nil
		}
		expiresAt = time.Now().Add(time.Duration(*in.TTLSeconds) * time.Second).UTC()
	}
	ends, err := s.minterEnds(ctx)
	if err != nil {
		return errResult(fmt.Sprintf("create: reading the minting token: %s", err)), nil
	}
	if expiresAt, err = boundByMinterEnd(expiresAt, ends, in.TTLSeconds != nil); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	plaintext, suffix, err := auth.MintToken()
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	created, failed := s.insertCreated(ctx, store.OperatorTokenDefRow{
		DefID:            mintDefID(),
		Name:             name,
		TenantID:         tenant,
		Subject:          subject,
		TokenHash:        s.Hasher.Hash(plaintext),
		AllowedScopes:    scopes,
		CreatedByAgentID: tools.RunIdentity(ctx).AgentID,
		ExpiresAt:        expiresAt,
		RestrictionsJSON: restrictionsJSON,
	})
	if failed != nil {
		return *failed, nil
	}
	return okJSON(operatorTokenCreateResponse(created, plaintext, suffix))
}

// liveScrapeName reports whether tenant holds a live token named name. Other
// tenants' rows are not read: their names are not this caller's to learn.
func (s *OperatorTokenDef) liveScrapeName(ctx context.Context, tenant, name string) (bool, error) {
	rows, err := s.Store.OperatorTokenDefListByName(ctx, name)
	if err != nil {
		return false, err
	}
	now := time.Now()
	for _, r := range rows {
		if r.TenantID == tenant && r.ValidAt(now) && r.RetiredAt.IsZero() {
			return true, nil
		}
	}
	return false, nil
}

func (s *OperatorTokenDef) listTenantScrape(ctx context.Context, tenant, name string) (tools.Result, error) {
	names := []string{name}
	if name == "" {
		all, err := s.Store.OperatorTokenDefListNames(ctx)
		if err != nil {
			return errResult(fmt.Sprintf("list: %s", err)), nil
		}
		names = names[:0]
		for _, n := range all {
			if n.TenantID == tenant {
				names = append(names, n.Name)
			}
		}
	}
	out := make([]map[string]any, 0)
	for _, n := range names {
		rows, err := s.Store.OperatorTokenDefListByName(ctx, n)
		if err != nil {
			return errResult(fmt.Sprintf("list: %s", err)), nil
		}
		for _, r := range rows {
			if isTenantScrapeRow(r, tenant) {
				out = append(out, operatorTokenRowResponse(r))
			}
		}
	}
	resp := map[string]any{"tenant_id": tenant, "tokens": out}
	if name != "" {
		resp["name"] = name
	}
	return okJSONCount(resp, len(out))
}

// resolveTenantScrape finds the row a rotate or retire targets: by def_id, or
// the newest live token of the name in tenant. Anything that is not one of
// tenant's scrape tokens is refused as missing, in the words a missing row
// gets, so the answer says nothing about rows the caller may not see.
func (s *OperatorTokenDef) resolveTenantScrape(ctx context.Context, op, tenant string, in operatorTokenDefInput) (store.OperatorTokenDefRow, error) {
	if in.DefID != "" {
		return s.tenantScrapeByID(ctx, op, tenant, in.DefID)
	}
	if in.Name == "" {
		return store.OperatorTokenDefRow{}, fmt.Errorf("%s: need name or def_id", op)
	}
	rows, err := s.Store.OperatorTokenDefListByName(ctx, in.Name)
	if err != nil {
		return store.OperatorTokenDefRow{}, fmt.Errorf("%s: %s", op, err)
	}
	now := time.Now()
	var best store.OperatorTokenDefRow
	for _, r := range rows {
		if isTenantScrapeRow(r, tenant) && r.RetiredAt.IsZero() && r.ValidAt(now) && r.CreatedAt.After(best.CreatedAt) {
			best = r
		}
	}
	if best.DefID == "" {
		return store.OperatorTokenDefRow{}, fmt.Errorf("%s: no live token for name %q", op, in.Name)
	}
	return best, nil
}

// tenantScrapeByID reads def_id when it is one of tenant's scrape tokens.
func (s *OperatorTokenDef) tenantScrapeByID(ctx context.Context, op, tenant, defID string) (store.OperatorTokenDefRow, error) {
	row, err := s.Store.OperatorTokenDefGet(ctx, defID)
	if err != nil {
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			return store.OperatorTokenDefRow{}, fmt.Errorf("%s: %s", op, err)
		}
	}
	if err != nil || !isTenantScrapeRow(row, tenant) {
		return store.OperatorTokenDefRow{}, fmt.Errorf("%s: def_id %q not found", op, defID)
	}
	return row, nil
}

// isTenantScrapeRow reports whether row is a scrape token of tenant confined
// to it: scopes exactly [metrics:read], tenant_metrics held. A record that
// cannot be read is not one.
func isTenantScrapeRow(row store.OperatorTokenDefRow, tenant string) bool {
	if row.TenantID != tenant || len(row.AllowedScopes) != 1 || row.AllowedScopes[0] != auth.ScopeMetricsRead {
		return false
	}
	r, err := auth.ParseRestrictions(row.RestrictionsJSON)
	return err == nil && r.TenantMetrics
}

// minterEnds is when the token that authenticated this call stops working;
// zero when it never does, or when the caller is not a minted token (a
// config-declared principal, the legacy token, open mode). The per-user mint
// holds its tokens to the same end (api/http minterExpiry): a token must not
// mint a credential that outlives it. A minter row that cannot be read
// refuses the mint rather than leaving it unbounded.
func (s *OperatorTokenDef) minterEnds(ctx context.Context) (time.Time, error) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.Legacy || p.TokenDefID == "" || strings.HasPrefix(p.TokenDefID, "cfg:") {
		return time.Time{}, nil
	}
	row, err := s.Store.OperatorTokenDefGet(ctx, p.TokenDefID)
	if err != nil {
		return time.Time{}, err
	}
	return row.ExpiresAt, nil
}

// boundByMinterEnd holds a new token's end under its minter's (zero for
// "never" in both): a lifetime asked for past it is refused, an unasked one
// gets what the minter has left.
func boundByMinterEnd(expires, minterEnds time.Time, asked bool) (time.Time, error) {
	if minterEnds.IsZero() || (!expires.IsZero() && !expires.After(minterEnds)) {
		return expires, nil
	}
	if asked {
		return time.Time{}, errors.New("ttl_seconds would outlive the token minting it")
	}
	return minterEnds, nil
}
