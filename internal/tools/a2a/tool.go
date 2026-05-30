package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// DefResolver resolves a peer NAME to its effective config.A2AAgent
// (active substrate version OR static yaml entry). The Tool calls it at
// Execute time — NOT at registration — so a substrate fork is picked up
// without re-registering the tool, mirroring how mcpTool re-resolves its
// pool entry on each call. Returns (zero, false) when the peer is no
// longer registered, which the Tool surfaces as a clean tool error.
type DefResolver func(ctx context.Context, name string) (config.A2AAgent, bool)

// Tool is the synthetic outbound A2A tool the model sees as
// `a2a__<peer>__<skill>`. One Tool instance fronts exactly one
// (peer, skill) pair. The peer + skill come from operator-registered
// A2AAgentDefs at registration time, so the trust boundary holds: the
// model can only invoke peer/skill combinations an operator blessed, and
// cannot reach an arbitrary host by crafting tool input.
//
// Dependencies are injected (resolver + client factory + logf) so the
// dispatch path is unit-testable against a fake peer with no real HTTP.
type Tool struct {
	peer        string
	skill       string
	description string

	resolve DefResolver
	newPeer peerClientFactory
	// logf emits one triage line per notable event (missing credential,
	// signature failure). MUST NOT include the bearer value — CLAUDE.md
	// rule 4. Nil is allowed (silent).
	logf func(format string, args ...any)
}

var _ tools.Tool = (*Tool)(nil)

// NewTool builds one synthetic A2A tool for a (peer, skill) pair. skill
// is the remote skill id the tool targets; description is the
// human/model-facing blurb (typically the expected-skill or resolved
// card skill description). newPeer defaults to the production SDK
// factory when nil so callers in tests can omit it only when they pass
// their own.
func NewTool(peer, skill, description string, resolve DefResolver, newPeer peerClientFactory, logf func(string, ...any)) *Tool {
	if newPeer == nil {
		newPeer = newSDKPeerClient
	}
	return &Tool{
		peer:        peer,
		skill:       skill,
		description: description,
		resolve:     resolve,
		newPeer:     newPeer,
		logf:        logf,
	}
}

// Name is the synthetic tool name the model invokes. Mirrors the MCP
// `mcp__<server>__<tool>` shape so operators and agents reason about A2A
// peers with the same mental model as MCP servers.
func (t *Tool) Name() string {
	return "a2a__" + sanitiseName(t.peer) + "__" + sanitiseName(t.skill)
}

func (t *Tool) Description() string {
	if t.description != "" {
		return t.description
	}
	return fmt.Sprintf("Call the %q skill on remote A2A peer %q.", t.skill, t.peer)
}

// InputSchema is a single free-text `message` field. Richer typed input
// (files, structured data) is a later concern — the bridge accepts only
// text parts on the server side too, so the client mirrors that.
func (t *Tool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","description":"The message text to send to the remote A2A peer skill."}},"required":["message"]}`)
}

type toolInput struct {
	Message string `json:"message"`
}

// Execute resolves the peer def, resolves the bearer credential from the
// run identity, builds a peer client, sends the message, and returns the
// peer's terminal response as the tool result text. Every failure path
// is a recoverable IsError result (so the model can self-correct) EXCEPT
// ctx cancellation, which propagates as a hard error matching mcpTool.
func (t *Tool) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var in toolInput
	if err := json.Unmarshal(input, &in); err != nil {
		return tools.Result{Text: fmt.Sprintf("a2a: bad input: %s", err), IsError: true}, nil
	}
	if strings.TrimSpace(in.Message) == "" {
		return tools.Result{Text: "a2a: input field \"message\" is required and must be non-empty", IsError: true}, nil
	}

	def, ok := t.resolve(ctx, t.peer)
	if !ok {
		return tools.Result{Text: fmt.Sprintf("a2a: peer %q is no longer registered", t.peer), IsError: true}, nil
	}

	bearer, err := t.resolveBearer(ctx, def)
	if err != nil {
		// Absent-but-required credential: a clear tool error PLUS a
		// tracing event, never a silent empty bearer (slice contract).
		t.trace("a2a: peer=%q skill=%q credential resolution failed: %v", t.peer, t.skill, err)
		t.emitToolEvent(ctx, true, err.Error())
		return tools.Result{Text: fmt.Sprintf("a2a: %s", err), IsError: true}, nil
	}

	peer, err := t.newPeer(ctx, def, bearer)
	if err != nil {
		if ctx.Err() != nil {
			return tools.Result{}, ctx.Err()
		}
		t.trace("a2a: peer=%q skill=%q client build failed: %v", t.peer, t.skill, err)
		return tools.Result{Text: fmt.Sprintf("a2a: connect to peer %q failed: %s", t.peer, err), IsError: true}, nil
	}
	defer func() { _ = peer.Close() }()

	msg := a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart(in.Message))
	// The remote skill is selected via Message.Metadata["skillId"], the
	// same carrier loomcycle's own server reads (see internal/a2a
	// Executor.agentFor). This keeps both sides of the loomcycle↔peer
	// link on one skill-selection convention.
	msg.Metadata = map[string]any{"skillId": t.skill}

	result, err := peer.SendMessage(ctx, &a2asdk.SendMessageRequest{Message: msg})
	if err != nil {
		if ctx.Err() != nil {
			return tools.Result{}, ctx.Err()
		}
		t.trace("a2a: peer=%q skill=%q send failed: %v", t.peer, t.skill, err)
		return tools.Result{Text: fmt.Sprintf("a2a: call to peer %q skill %q failed: %s", t.peer, t.skill, err), IsError: true}, nil
	}

	text, isErr := resultText(result)
	return tools.Result{Text: text, IsError: isErr}, nil
}

// resolveBearer resolves the def's auth into an Authorization bearer.
//
//   - No auth scheme declared → ("", nil): the peer is called without a
//     bearer (valid for open peers).
//   - Scheme declared with a bearer_credential_ref → look the ref up in
//     the run's UserCredentials (RFC F seam). Missing/empty is an ERROR
//     (never a silent empty bearer), so an operator who declared auth
//     but whose run lacks the credential gets a clear failure.
//   - Scheme declared without a ref → error: a misconfigured def that
//     would otherwise send no Authorization despite claiming auth.
func (t *Tool) resolveBearer(ctx context.Context, def config.A2AAgent) (string, error) {
	if def.Auth.Scheme == "" {
		return "", nil
	}
	ref := def.Auth.BearerCredentialRef
	if ref == "" {
		return "", fmt.Errorf("peer %q declares auth.scheme=%q but no bearer_credential_ref", t.peer, def.Auth.Scheme)
	}
	ident := tools.RunIdentity(ctx)
	v := ident.UserCredentials[ref]
	if v == "" {
		return "", fmt.Errorf("peer %q requires credential %q but it is absent from this run's identity", t.peer, ref)
	}
	return v, nil
}

// emitToolEvent surfaces an A2A call outcome onto the run's event stream
// for observability. It carries NO secret material — only the peer,
// skill, and a non-sensitive detail string.
func (t *Tool) emitToolEvent(ctx context.Context, isErr bool, detail string) {
	// EventEmitter never returns nil — it yields a no-op when no stream
	// is attached, so this is safe to call unconditionally.
	tools.EventEmitter(ctx)(providers.Event{
		Type:    providers.EventToolResult,
		IsError: isErr,
		Text:    fmt.Sprintf("a2a peer=%s skill=%s: %s", t.peer, t.skill, detail),
	})
}

func (t *Tool) trace(format string, args ...any) {
	if t.logf != nil {
		t.logf(format, args...)
	}
}

// resultText extracts the peer's terminal response into a single text
// blob for the model, and whether it should be flagged as an error.
//
// A *Message result is a direct reply (no task created); its text parts
// are the answer. A *Task result carries the terminal status + any
// artifacts; we prefer artifact text (the produced content), falling
// back to the status message. A FAILED/REJECTED task surfaces IsError so
// the model can self-correct.
func resultText(result a2asdk.SendMessageResult) (string, bool) {
	switch r := result.(type) {
	case *a2asdk.Message:
		return partsText(r.Parts), false
	case *a2asdk.Task:
		var b strings.Builder
		for _, art := range r.Artifacts {
			if art == nil {
				continue
			}
			if s := partsText(art.Parts); s != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(s)
			}
		}
		isErr := r.Status.State == a2asdk.TaskStateFailed || r.Status.State == a2asdk.TaskStateRejected
		if b.Len() == 0 && r.Status.Message != nil {
			b.WriteString(partsText(r.Status.Message.Parts))
		}
		if b.Len() == 0 {
			b.WriteString(fmt.Sprintf("(peer task terminated in state %s with no text content)", r.Status.State))
		}
		return b.String(), isErr
	default:
		return "(a2a: peer returned an unrecognised result type)", true
	}
}

// partsText concatenates the text content of a message/artifact's parts,
// skipping non-text parts (the client surface accepts text only).
func partsText(parts a2asdk.ContentParts) string {
	var b strings.Builder
	for _, p := range parts {
		if p == nil {
			continue
		}
		if s := p.Text(); s != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(s)
		}
	}
	return b.String()
}

// sanitiseName replaces characters not valid in a tool name segment with
// underscores. Tool names are matched against allowlists and emitted to
// providers, so they share the MCP charset constraint (alnum + _ + -).
func sanitiseName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, s)
}
