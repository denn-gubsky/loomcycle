// Package a2a implements the outbound A2A client proxy (RFC G slice
// A2A-6): the synthetic `a2a__<peer>__<skill>` tool an agent invokes to
// call a remote A2A peer, mirroring the MCP `mcp__<server>__<tool>`
// pattern. The peer set is operator-authoritative (resolved from
// registered A2AAgentDefs), never model-supplied — the model only picks
// which already-registered peer+skill to call.
//
// This is the CLIENT side of A2A. The server side (well-known card + the
// three protocol bindings) lives in internal/api/a2a; the SDK bridge
// (executor/task store) lives in internal/a2a.
package a2a

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"github.com/denn-gubsky/loomcycle/internal/a2a/sign"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// verifyCardSignature enforces the strict-verification path taken when an
// A2AAgentDef sets verify_signed_card=true: the fetched peer card MUST
// carry a JWS signature and it MUST verify against the public key
// embedded in that signature's protected header. An unsigned card or a
// tampered/invalid signature is refused (the call does not proceed).
//
// The default (verify_signed_card=false) is tolerant: this function is
// not called at all, so an unsigned peer card is accepted — see
// newSDKPeerClient.
func verifyCardSignature(card *a2asdk.AgentCard) error {
	return sign.VerifyCardSelfContained(card)
}

// peerClient is the narrow slice of the A2A SDK client the synthetic
// tool needs. Declared as an interface so unit tests inject a fake peer
// without standing up a real HTTP A2A server, and so the dispatch logic
// (credential resolution, result extraction) is testable in isolation
// from the SDK transport. The real implementation (sdkPeerClient) wraps
// a2aclient.Client.
type peerClient interface {
	// SendMessage sends one message to the peer and returns the
	// terminal SendMessageResult (a *Task or a *Message per the SDK
	// sealed union).
	SendMessage(ctx context.Context, req *a2asdk.SendMessageRequest) (a2asdk.SendMessageResult, error)
	// Close releases any transport resources. Safe to call once.
	Close() error
}

// peerClientFactory builds a peerClient for a resolved A2AAgentDef +
// bearer. Injected into the Tool so tests substitute a fake; production
// uses newSDKPeerClient. bearer is the already-resolved Authorization
// credential ("" when the def declares no auth) — this factory never
// reads credentials itself, keeping the secret-resolution seam in one
// place (the Tool).
type peerClientFactory func(ctx context.Context, def config.A2AAgent, bearer string) (peerClient, error)

// bindingToTransport maps an A2AAgentDef direct-endpoint `binding`
// value (validated upstream to one of jsonrpc/grpc/rest) onto the SDK's
// TransportProtocol. Returns ("", false) for an unknown binding so the
// caller refuses rather than guessing a transport.
func bindingToTransport(binding string) (a2asdk.TransportProtocol, bool) {
	switch binding {
	case "jsonrpc":
		return a2asdk.TransportProtocolJSONRPC, true
	case "grpc":
		return a2asdk.TransportProtocolGRPC, true
	case "rest":
		return a2asdk.TransportProtocolHTTPJSON, true
	default:
		return "", false
	}
}

// bearerInterceptor attaches a static Bearer token to every outbound
// A2A call by stamping ServiceParams["Authorization"], which the SDK
// transports copy onto the HTTP request headers. We use this instead of
// the SDK's card-driven AuthInterceptor because loomcycle resolves the
// credential per-run from its own RunIdentity seam (RFC F), not from the
// peer card's declared security schemes.
//
// The token is held only for the lifetime of one peerClient (one tool
// call) and is never logged — CLAUDE.md rule 1/4.
type bearerInterceptor struct {
	a2aclient.PassthroughInterceptor
	bearer string
}

// Before implements a2aclient.CallInterceptor. It sets the Authorization
// header param when a bearer is present; absent bearer is a no-op (the
// Tool decides whether a missing-but-required credential is an error
// before ever constructing the client).
func (b *bearerInterceptor) Before(ctx context.Context, req *a2aclient.Request) (context.Context, any, error) {
	if b.bearer != "" {
		req.ServiceParams["Authorization"] = []string{"Bearer " + b.bearer}
	}
	return ctx, nil, nil
}

// sdkPeerClient is the production peerClient backed by a2aclient.Client.
type sdkPeerClient struct {
	cl *a2aclient.Client
}

func (c *sdkPeerClient) SendMessage(ctx context.Context, req *a2asdk.SendMessageRequest) (a2asdk.SendMessageResult, error) {
	return c.cl.SendMessage(ctx, req)
}

func (c *sdkPeerClient) Close() error { return c.cl.Destroy() }

// newSDKPeerClient is the production peerClientFactory. It builds an SDK
// client from the def's discovery shape:
//
//   - agent_card_url set → fetch + parse the peer's AgentCard, optionally
//     verify its JWS signature (verify_signed_card), and build the client
//     from the card's advertised interfaces.
//   - endpoint+binding set → build the client directly from the single
//     declared interface, skipping card discovery.
//
// The bearer is wired via bearerInterceptor so it rides every call.
func newSDKPeerClient(ctx context.Context, def config.A2AAgent, bearer string) (peerClient, error) {
	opts := []a2aclient.FactoryOption{
		a2aclient.WithCallInterceptors(&bearerInterceptor{bearer: bearer}),
	}

	if def.AgentCardURL != "" {
		card, err := fetchPeerCard(ctx, def.AgentCardURL)
		if err != nil {
			return nil, fmt.Errorf("fetch peer card: %w", err)
		}
		if def.VerifySignedCard {
			if err := verifyCardSignature(card); err != nil {
				return nil, fmt.Errorf("verify peer card signature: %w", err)
			}
		}
		cl, err := a2aclient.NewFromCard(ctx, card, opts...)
		if err != nil {
			return nil, fmt.Errorf("build client from card: %w", err)
		}
		return &sdkPeerClient{cl: cl}, nil
	}

	transport, ok := bindingToTransport(def.Binding)
	if !ok {
		return nil, fmt.Errorf("unknown binding %q (want jsonrpc|grpc|rest)", def.Binding)
	}
	iface := a2asdk.NewAgentInterface(def.Endpoint, transport)
	cl, err := a2aclient.NewFromEndpoints(ctx, []*a2asdk.AgentInterface{iface}, opts...)
	if err != nil {
		return nil, fmt.Errorf("build client from endpoint: %w", err)
	}
	return &sdkPeerClient{cl: cl}, nil
}

// peerCardFetchTimeout bounds the well-known card GET so a slow or
// hanging peer cannot stall the agent run indefinitely. The agent's own
// run ctx still applies; this is a belt-and-braces upper bound for the
// discovery hop specifically.
const peerCardFetchTimeout = 15 * time.Second

// fetchPeerCard resolves a peer AgentCard from its well-known URL using
// the SDK's resolver. agentCardURL may point at either the well-known
// path directly or the origin; the resolver appends the default
// well-known path when given a bare origin, so we pass the URL as the
// base and strip a trailing well-known suffix the operator may have
// included to avoid double-appending.
func fetchPeerCard(ctx context.Context, agentCardURL string) (*a2asdk.AgentCard, error) {
	hc := &http.Client{Timeout: peerCardFetchTimeout}
	resolver := agentcard.NewResolver(hc)

	const wellKnown = "/.well-known/agent-card.json"
	base := agentCardURL
	if strings.HasSuffix(base, wellKnown) {
		// Operator gave the full well-known URL; resolve against the
		// origin so the resolver's default path append lands on the same
		// URL rather than doubling the suffix.
		base = strings.TrimSuffix(base, wellKnown)
		return resolver.Resolve(ctx, base)
	}
	return resolver.Resolve(ctx, base)
}
