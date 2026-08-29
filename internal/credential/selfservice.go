package credential

import "encoding/json"

// ConstrainToUserScope enforces the RFC CN isolated-user rule on a CredentialDef
// tool input: the effective scope must be user. It is transport-neutral — the HTTP
// handler and the MCP dispatch both call it and map its refusal onto their own wire
// error (a 403 / an MCP tool-error), so the rule cannot drift between the two.
//
// Behaviour:
//   - An omitted scope defaults to user and the body is rewritten to pin it. The
//     tool's own default is tenant, which is wrong for a self-service caller and
//     would leak the secret into the tenant-shared bucket.
//   - An explicit scope=user passes through (with scope pinned to user).
//   - An explicit scope=tenant/agent is refused (refused=true, out nil) — those
//     require substrate:tenant.
//   - A non-object body (array / scalar) is passed through unchanged so the tool
//     rejects it with its own message rather than this masking it as a scope error.
//
// The caller must apply this ONLY to an isolated substrate:user principal; members,
// tenant operators, and admin keep their existing (unconstrained) authority.
func ConstrainToUserScope(body []byte) (out []byte, refused bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		// Non-object body — leave it for the tool to reject.
		return body, false
	}
	scope := ""
	if raw, present := fields["scope"]; present {
		_ = json.Unmarshal(raw, &scope) // a non-string value stays "" → defaulted below
	}
	if scope == "" {
		scope = "user"
	}
	if scope != "user" {
		return nil, true
	}
	// Pin scope=user so an omitted scope cannot fall through to the tool's tenant
	// default.
	fields["scope"] = json.RawMessage(`"user"`)
	rewritten, err := json.Marshal(fields)
	if err != nil {
		return body, false // unreachable (re-marshalling validated fields); keep the original
	}
	return rewritten, false
}
