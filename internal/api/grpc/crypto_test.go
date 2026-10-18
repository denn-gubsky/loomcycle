package grpc

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The Crypto RPC hands input_json to the connector as is (the connector, not
// this transport, takes the operator-only "tenant" field out) and returns the
// tool's own bytes; a tool refusal stays a response with is_error and its
// structure, not a status error.
func TestGrpcCrypto_DispatchesInputAndReturnsResult(t *testing.T) {
	mc := &substrateMock{cryptoResult: connector.ToolResult{Text: `{"ciphertext":"lc1:abc"}`}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	in := `{"op":"encrypt","key":"tenant:fields","plaintext":"hi","tenant":"acme"}`
	resp, err := client.Crypto(context.Background(), &loomcyclepb.SubstrateRequest{InputJson: []byte(in)})
	if err != nil {
		t.Fatalf("Crypto: %v", err)
	}
	if string(mc.gotCryptoInput) != in {
		t.Errorf("connector input = %s, want %s", mc.gotCryptoInput, in)
	}
	if resp.GetIsError() || string(resp.GetOutputJson()) != `{"ciphertext":"lc1:abc"}` {
		t.Errorf("resp = is_error %v, output %s", resp.GetIsError(), resp.GetOutputJson())
	}

	mc.cryptoResult = connector.ToolResult{
		Text: "decryption failed", IsError: true,
		ErrorInfo: &tools.ErrorInfo{Category: tools.CategoryValidation, Description: "check the key"},
	}
	resp, err = client.Crypto(context.Background(), &loomcyclepb.SubstrateRequest{InputJson: []byte(`{"op":"decrypt"}`)})
	if err != nil {
		t.Fatalf("a tool refusal became a status error: %v", err)
	}
	if !resp.GetIsError() || resp.GetErrorInfo() == nil {
		t.Errorf("refusal = is_error %v, error_info %v; want both set", resp.GetIsError(), resp.GetErrorInfo())
	}

	if _, err := client.Crypto(context.Background(), &loomcyclepb.SubstrateRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty input_json: code = %v, want InvalidArgument", status.Code(err))
	}
}

// cryptoRateErr stands in for the HTTP package's unexported rate refusal: it
// is runner.ErrRateLimited to errors.Is and names the limiter's wait.
type cryptoRateErr struct{ wait time.Duration }

func (e cryptoRateErr) Error() string             { return runner.ErrRateLimited.Error() + ": retry in 3s" }
func (e cryptoRateErr) Is(target error) bool      { return target == runner.ErrRateLimited }
func (e cryptoRateErr) RetryDelay() time.Duration { return e.wait }

// A spent token rate is a refusal the caller should wait out, not a server
// fault: ResourceExhausted with the rate_limited reason and the limiter's own
// wait as RetryInfo — the same answer the Run RPC gives. Before the mapping it
// reached the caller as Internal.
func TestGrpcCrypto_RateLimitedIsResourceExhausted(t *testing.T) {
	mc := &substrateMock{cryptoErr: cryptoRateErr{wait: 3 * time.Second}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	_, err := client.Crypto(context.Background(), &loomcyclepb.SubstrateRequest{InputJson: []byte(`{"op":"random","bytes":16}`)})
	v := viewOf(t, err)
	if v.code != codes.ResourceExhausted || v.reason != "rate_limited" {
		t.Fatalf("rate-limited Crypto = %v / %q (%v), want ResourceExhausted / rate_limited", v.code, v.reason, err)
	}
	if !v.hasRetry || v.retryIn != 3*time.Second {
		t.Errorf("RetryInfo present=%v delay=%v, want the limiter's 3s", v.hasRetry, v.retryIn)
	}

	// Any other connector failure is still Internal.
	mc.cryptoErr = context.DeadlineExceeded
	if _, err := client.Crypto(context.Background(), &loomcyclepb.SubstrateRequest{InputJson: []byte(`{"op":"random"}`)}); status.Code(err) != codes.Internal {
		t.Errorf("other connector error: code = %v, want Internal", status.Code(err))
	}
}

// The crossing: the real connector (the HTTP server's Crypto, which takes one
// from the token's rate per call) behind the gRPC handler. The second call in
// a one-a-minute bucket is refused with the limiter's whole-second wait. The
// rate is the token's own: a tenant operator is exempt from the server default.
func TestGrpcCrypto_RealConnectorRateRefusalCarriesTheWait(t *testing.T) {
	_, adapter, _, _ := configuredServers(t, nil)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant},
		Restrictions: auth.Restrictions{Rate: &auth.RateLimit{PerMinute: 1, Burst: 1}},
	})
	req := &loomcyclepb.SubstrateRequest{InputJson: []byte(`{"op":"random","bytes":16}`)}
	// The first call takes the bucket's only token, whatever the tool makes of it.
	_, _ = adapter.Crypto(ctx, req)

	_, err := adapter.Crypto(ctx, req)
	v := viewOf(t, err)
	if v.code != codes.ResourceExhausted || v.reason != "rate_limited" {
		t.Fatalf("second Crypto = %v / %q (%v), want ResourceExhausted / rate_limited", v.code, v.reason, err)
	}
	if !v.hasRetry || v.retryIn <= 0 || v.retryIn > time.Minute || v.retryIn%time.Second != 0 {
		t.Errorf("RetryInfo present=%v delay=%v, want a whole-second wait in (0, 1m]", v.hasRetry, v.retryIn)
	}
}

// substrate:tenant (and admin) reach Crypto; a runs:create-only member and an
// isolated substrate:user do not. The credential self-service carve-out must
// not extend to it: decrypting with the tenant's keys reads what the tenant
// sealed.
func TestGrpcCrypto_ScopeGate(t *testing.T) {
	method := grpcMethodPrefix + "Crypto"
	if got := requiredScopeForRPC(method); got != auth.ScopeTenant {
		t.Errorf("Crypto scope = %q, want substrate:tenant", got)
	}
	allowed := []auth.Principal{
		{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}},
		{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}},
	}
	for _, p := range allowed {
		if err := enforceScope(auth.WithPrincipal(context.Background(), p), method); err != nil {
			t.Errorf("%v on Crypto: %v, want nil", p.Scopes, err)
		}
	}
	denied := []auth.Principal{
		{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate}},
		{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeUser}},
	}
	for _, p := range denied {
		if err := enforceScope(auth.WithPrincipal(context.Background(), p), method); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%v on Crypto: code = %v, want PermissionDenied", p.Scopes, status.Code(err))
		}
	}
}
