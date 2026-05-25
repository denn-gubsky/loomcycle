// Package anthropic_oauth_dev implements the v0.11.9 Anthropic OAuth-dev
// provider. Authenticates against the operator's Claude Pro/Max
// subscription via reverse-engineered OAuth (Pi's `pi-ai` package is the
// reference: github.com/earendil-works/pi).
//
// This package is OPT-IN. It registers as the `anthropic-oauth-dev`
// provider ONLY when LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1 is set in
// the environment AND a token file exists. The production Anthropic
// provider (`anthropic`) under `internal/providers/anthropic/` is
// untouched.
//
// Scope: research workloads, load testing, self-evolution experiments.
// NOT for production deployment. Single-operator only. Tokens stored in
// the operator's local config dir (chmod 0600).
//
// See `~/work/loomcycle-internal/doc-internal/rfcs/anthropic-oauth-dev.md`
// for the full design + risk acknowledgement.
package anthropic_oauth_dev

// ClaudeCodeClientID is the OAuth client_id Claude Code itself uses
// against Anthropic's authorization server. Publicly visible in Pi's
// open-source code at 51K stars (github.com/earendil-works/pi).
// Not a secret per se, but kept here as a single source of truth so a
// future rotation lands in one place.
const ClaudeCodeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// AuthorizeURL is Anthropic's OAuth authorization endpoint. Operators
// open this in their browser during `loomcycle anthropic login` to
// approve the loomcycle client's access to their Claude subscription.
const AuthorizeURL = "https://claude.ai/oauth/authorize"

// TokenURL is the OAuth token endpoint — used for both the initial
// code-for-token exchange and subsequent refresh-token rotations.
const TokenURL = "https://platform.claude.com/v1/oauth/token"

// MessagesURL is the production Messages API endpoint. Same URL the
// API-key driver hits — subscription routing is server-side, keyed off
// the token + beta-marker combination.
const MessagesURL = "https://api.anthropic.com/v1/messages"

// PinnedClaudeCodeVersion is the Claude Code version string sent in the
// per-request User-Agent header. Anthropic's subscription-billing
// detection cross-checks this; mismatch can cause requests to be
// rejected or attributed to API credits instead of the subscription
// quota.
//
// Operators can override via LOOMCYCLE_CLAUDE_CODE_VERSION. When
// Anthropic forces a Claude Code version bump (auth-surface drift), the
// procedure is: update this constant + ship a hotfix, OR operators
// self-patch via the env var until the hotfix lands.
const PinnedClaudeCodeVersion = "2.1.75"

// PinnedAnthropicBetas is the beta-marker string appended to every
// OAuth-dev request's `anthropic-beta` header. The two markers together
// signal "this is a Claude Code OAuth client" to Anthropic's request
// router:
//
//   - `claude-code-20250219`: identifies the client family.
//   - `oauth-2025-04-20`: identifies the OAuth auth mode (vs API key).
//
// The driver appends any additional betas its callers want (e.g.
// `prompt-caching-2024-07-31`) to this base list.
const PinnedAnthropicBetas = "claude-code-20250219,oauth-2025-04-20"

// CallbackHost is the loopback hostname embedded in the redirect_uri
// query param. MUST be the literal string "localhost" — Anthropic's
// OAuth authorization server matches the registered redirect_uri by
// exact string equality, and the Claude Code client_id is registered
// with `http://localhost:53692/callback` (not `http://127.0.0.1:...`).
// Pi confirms: pi-ai's `packages/ai/src/utils/oauth/anthropic.ts`
// builds `REDIRECT_URI = http://localhost:${CALLBACK_PORT}/callback`.
// Using "127.0.0.1" produces a 400 from claude.ai's authorize page:
// "Redirect URI ... is not supported by client."
const CallbackHost = "localhost"

// CallbackBindIP is the address the local listener actually binds to.
// Pi uses 127.0.0.1 explicitly (not "localhost") — `net.Listen("tcp",
// "localhost:port")` would rely on OS resolution which may return
// IPv6 ::1 on some systems, leaving the listener IPv6-only while
// browsers default to IPv4. Binding 127.0.0.1 explicitly is the
// most-compatible loopback bind. The mismatch with CallbackHost (URL
// says "localhost", listener binds "127.0.0.1") is intentional — the
// URL string must match Anthropic's whitelist; the TCP bind uses the
// most-reliable loopback IP.
const CallbackBindIP = "127.0.0.1"

// CallbackPath is the path component of the redirect URI. The PKCE
// callback server only handles this single path; anything else 404s.
const CallbackPath = "/callback"

// DefaultCallbackPort is the local TCP port the callback server binds
// to. Configurable via LOOMCYCLE_ANTHROPIC_OAUTH_CALLBACK_PORT —
// operators with another process bound to 53692 can pick any free
// port. The authorize URL embeds the same port in its redirect_uri
// query param.
const DefaultCallbackPort = 53692

// Scopes is the OAuth scope set Pi's `pi-ai` requests. Mirrored
// verbatim so the request shape matches what Claude Code sends.
//
// `user:inference` is the load-bearing one — it grants the token the
// right to call /v1/messages on behalf of the subscription. The
// others (`org:create_api_key`, `user:profile`, `user:sessions:claude_code`,
// `user:mcp_servers`, `user:file_upload`) are part of Claude Code's
// canonical scope request; including them is part of looking like
// Claude Code to Anthropic's auth surface.
var Scopes = []string{
	"org:create_api_key",
	"user:profile",
	"user:inference",
	"user:sessions:claude_code",
	"user:mcp_servers",
	"user:file_upload",
}

// EnvEnabled gates the entire OAuth-dev surface. When unset (or any
// value other than "1"), provider registration is skipped at boot AND
// the `loomcycle anthropic` CLI subcommands refuse to run.
const EnvEnabled = "LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED"

// EnvClaudeCodeVersion overrides PinnedClaudeCodeVersion at runtime
// without recompiling. Operator self-patch path for auth-surface drift.
const EnvClaudeCodeVersion = "LOOMCYCLE_CLAUDE_CODE_VERSION"

// EnvCallbackPort overrides DefaultCallbackPort at runtime. Useful when
// another process on the operator's machine binds 53692.
const EnvCallbackPort = "LOOMCYCLE_ANTHROPIC_OAUTH_CALLBACK_PORT"
