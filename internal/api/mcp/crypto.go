package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/errclassify"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// The `crypto` meta-tool is the Crypto tool outside a run (RFC EP P1b): the
// same input as the in-run tool, plus the operator API's admin-only top-level
// "tenant". connector.Crypto owns every rule (which keys, which tenant, the
// rate); this file only advertises the shape and renders its refusals.

const cryptoMCPTenantProperty = `{"type": "string", "description": "Admin only: use this tenant's keys instead of your own (and then no user keys). Any other token naming a tenant not its own is refused; leave it out to use your own tenant's."}`

// cryptoMCPSchema is the Crypto builtin's input schema with the tenant
// property added. It is derived rather than restated so every other field
// keeps tracking the tool's real validation; the properties are carried as raw
// JSON, so each one stays byte-for-byte the builtin's. A shape it cannot edit
// falls back to the builtin's schema, which
// TestCryptoMCP_SchemaIsTheToolsPlusTenant catches.
func cryptoMCPSchema() json.RawMessage {
	base := (&builtin.Crypto{}).InputSchema()
	var s map[string]json.RawMessage
	if err := json.Unmarshal(base, &s); err != nil {
		return base
	}
	var props map[string]json.RawMessage
	if err := json.Unmarshal(s["properties"], &props); err != nil || props == nil {
		return base
	}
	props["tenant"] = json.RawMessage(cryptoMCPTenantProperty)
	encoded, err := marshalNoHTMLEscape(props)
	if err != nil {
		return base
	}
	s["properties"] = encoded
	out, err := marshalNoHTMLEscape(s)
	if err != nil {
		return base
	}
	return out
}

func marshalNoHTMLEscape(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimSpace(buf.Bytes())), nil
}

// handleCrypto forwards to connector.Crypto under the session's principal.
// Tenant and subject come from that principal (mcpPrincipalCtx), never the
// wire; the one wire field that names a tenant is the admin-only "tenant",
// which the connector refuses for anyone else.
func handleCrypto(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	if env.connector == nil {
		return nil, fmt.Errorf("crypto: no connector wired")
	}
	res, err := env.connector.Crypto(mcpPrincipalCtx(ctx), args)
	if err != nil {
		if errors.Is(err, runner.ErrRateLimited) {
			return cryptoRateSpent(err), nil
		}
		return toolErrFrom("crypto", err), nil
	}
	return toolResultFromConnector(res), nil
}

// cryptoRateSpent renders a refused rate. The refusal's own text speaks of
// starting runs, which is what the rate is named for; a crypto caller started
// none, so the text says which rate it spent. The classification is the
// shared one (retryable), with the wait the refusal carries as the hint.
func cryptoRateSpent(err error) *loommcp.CallToolResult {
	msg := "crypto: this token's rate is spent (each call takes one from the token's run rate)"
	var rd interface{ RetryDelay() time.Duration }
	var wait time.Duration
	if errors.As(err, &rd) {
		wait = rd.RetryDelay()
	}
	if wait > 0 {
		msg += fmt.Sprintf("; retry in %ds", int((wait+time.Second-1)/time.Second))
	} else {
		msg += "; " + err.Error()
	}
	res := toolErr(msg)
	if info, ok := errclassify.CategoryOf(err); ok {
		if wait > 0 {
			info.RetryAfter = &wait
		}
		res.StructuredContent = loommcp.StructuredErrorJSON(info)
	}
	return res
}
