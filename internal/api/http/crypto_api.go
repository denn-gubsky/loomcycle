package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The operator API for the Crypto tool (RFC EP P1b): the same operations a
// run's agent has, for an operator or a service outside any run, such as a
// backend that encrypts a field before writing it to its own database.

// handleSubstrateCrypto serves POST /v1/_crypto. The route is substrate:tenant
// and stays closed to a tenant's members (memberCarveOut): a member who could
// decrypt with the tenant's keys could read whatever the tenant sealed.
func (s *Server) handleSubstrateCrypto(w http.ResponseWriter, r *http.Request) {
	s.dispatchSubstrateCtx(w, r, "Crypto", s.Crypto, substrateAdminUserCtx)
}

// cryptoTenantField is the operator API's one field the in-run tool does not
// have: an admin naming the tenant whose keys to use.
const cryptoTenantField = "tenant"

// Crypto runs one Crypto-tool operation for the caller's principal, outside
// any run. Every transport calls it, so the rules hold the same on each:
//
//   - Keys: the principal's tenant's, and its own subject's user keys. Agent
//     keys are not reachable (they belong to the agent's runs), and there are
//     no run credentials outside a run. The scopes are stamped here from the
//     principal, whatever the transport put on ctx, and never from the wire.
//   - Tenant: the principal's own. An admin (or the legacy token, or open
//     mode) may name another with a top-level "tenant"; it then gets that
//     tenant's keys and no user keys, since its subject is not that tenant's.
//     Anyone else naming a tenant other than its own is refused.
//   - Limits: each call takes one from the token's run rate, when the token
//     has one. The per-run op budget does not apply: there is no run.
//
// The associated data a ciphertext is sealed with names the tenant, so a value
// this API encrypts decrypts in that tenant's runs, and the reverse.
func (s *Server) Crypto(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	p, hasPrincipal := auth.PrincipalFromContext(ctx)
	admin := !hasPrincipal || p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin)

	reqTenant, body, err := splitCryptoTenant(input)
	if err != nil {
		return cryptoAPIRefusal(tools.CategoryValidation, err.Error(), "Pass tenant as a string, or leave it out for your own tenant."), nil
	}
	tenant := p.TenantID
	if reqTenant != "" && reqTenant != tenant {
		if !admin {
			return cryptoAPIRefusal(tools.CategoryPermission, "only an admin may use another tenant's keys",
				"Leave tenant out: a tenant token uses its own tenant's keys."), nil
		}
		tenant = reqTenant
	}

	scopes := []string{"tenant"}
	userID := ""
	if hasPrincipal && p.Subject != "" && tenant == p.TenantID {
		userID = p.Subject
		scopes = append(scopes, "user")
	}

	if hasPrincipal {
		if err := s.AdmitRunRate(ctx, p); err != nil {
			return connector.ToolResult{}, err
		}
	}

	ctx = tools.WithSubstrateOperator(ctx)
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: tenant, UserID: userID})
	ctx = tools.WithAgentName(ctx, "")
	ctx = tools.WithCryptoPolicy(ctx, tools.CryptoPolicyValue{Scopes: scopes})
	return s.dispatchBuiltin(ctx, "Crypto", body)
}

// splitCryptoTenant takes the operator-only "tenant" field out of a Crypto
// call, so what reaches the tool is exactly the tool's own input.
func splitCryptoTenant(input json.RawMessage) (string, json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil {
		return "", input, nil // not an object: the tool reports it in its own words
	}
	raw, ok := fields[cryptoTenantField]
	if !ok {
		return "", input, nil
	}
	var tenant string
	if err := json.Unmarshal(raw, &tenant); err != nil {
		return "", nil, fmt.Errorf("tenant must be a string")
	}
	delete(fields, cryptoTenantField)
	body, err := json.Marshal(fields)
	if err != nil {
		return "", nil, err
	}
	return strings.TrimSpace(tenant), body, nil
}

func cryptoAPIRefusal(cat tools.ErrorCategory, msg, hint string) connector.ToolResult {
	return connector.ToolResult{
		Text:      msg,
		IsError:   true,
		ErrorInfo: &tools.ErrorInfo{Category: cat, Description: hint},
	}
}
