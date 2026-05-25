package anthropic_oauth_dev

import "github.com/denn-gubsky/loomcycle/internal/providers"

// claudeCodeIdentityText is the verbatim system-prompt prefix Pi sends
// as the FIRST system block under OAuth mode. Required by Anthropic's
// subscription-billing detection: the validator rejects requests
// whose system block doesn't start with this identity string with the
// surface error "messages: Input should be a valid array" (the error
// is misleading — the validator surfaces a generic message-array
// complaint when the broader request shape fails OAuth checks).
//
// Pi reference: `packages/ai/src/providers/anthropic.ts` lines
// 904-913 (the `if (isOAuthToken) { params.system = [{type:"text",
// text:"You are Claude Code, ..."}] }` branch).
//
// Do NOT modify this string. The exact wording is what Anthropic's
// validator pattern-matches. If a future Claude Code update bumps the
// version, this constant follows — the matching change in Pi is the
// reference signal.
const claudeCodeIdentityText = "You are Claude Code, Anthropic's official CLI for Claude."

// adaptSystemForOAuth prepends the Claude Code identity block to the
// operator's system prompt. Returns a new slice — does NOT mutate the
// caller's input (the loop may pass the same Request to multiple
// providers in one iteration via tier fallback).
//
// The identity block is type:"text" with the verbatim Pi-equivalent
// string. The operator's existing system blocks follow in their
// declared order with Cacheable hints preserved.
//
// Inputs with an empty / nil system array still get the identity
// block prepended — every OAuth request needs it regardless of
// whether the agent declared a system prompt.
func adaptSystemForOAuth(in []providers.ContentBlock) []providers.ContentBlock {
	out := make([]providers.ContentBlock, 0, len(in)+1)
	out = append(out, providers.ContentBlock{
		Type: "text",
		Text: claudeCodeIdentityText,
		// Cacheable=true so the static identity block lands inside
		// Anthropic's prompt cache — same posture as the operator-
		// supplied system blocks. The identity text is identical on
		// every call so it's a perfect cache hit.
		Cacheable: true,
	})
	out = append(out, in...)
	return out
}
