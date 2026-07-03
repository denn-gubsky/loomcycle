# Tenant credentials (CredentialDef) — RFC AR

CredentialDef is a **secure, per-tenant, encrypted-at-rest** store for named API
secrets — provider keys, search-provider keys, **per-user Telegram/Slack bot
tokens** — that other config references by name and the runtime binds
server-side. The model never sees a secret value.

> Why not just env vars? loomcycle's `${LOOMCYCLE_*}` refs resolve from **host**
> env, which a tenant can't set. CredentialDef gives per-tenant (and per-user)
> secrets a durable, encrypted, isolated home a tenant manages themselves.

## Enabling it (operator)

The inline backend encrypts secrets with a deployment master key. Generate one
and put it in `.env.local` (it's a secret):

```
LOOMCYCLE_SECRET_KEY=$(openssl rand -base64 32)   # 32 bytes, base64
```

- **Fail-closed:** with no `LOOMCYCLE_SECRET_KEY`, inline credentials can't be
  created (the runtime never stores plaintext). External backends aren't affected.
- **Rotation:** set `LOOMCYCLE_SECRET_KEY_PREVIOUS` to the old key during a grace
  window — rows sealed under it still decrypt while new writes use the new key.

Encryption: AES-256-GCM, a per-tenant key derived via HKDF from the master key,
with the ciphertext bound to its row (a row copied to another tenant/name won't
decrypt). Credentials are **excluded from snapshots** (secrets don't ride out on
backups; a restore re-provisions).

## Managing credentials (tenant)

Via the `credentialdef` MCP meta-tool (thin client) or in-band
(`allowed_tools:[CredentialDef]`). Ops: `create`, `get`, `list`, `delete`.
`get`/`list` return **metadata only** — never a secret.

```jsonc
// tenant-shared secret (all agents in the tenant)
{"op":"create","scope":"tenant","name":"serper_api_key","value":"<secret>"}

// per-user secret — keyed to YOUR subject; another user can't read it
{"op":"create","scope":"user","name":"telegram_bot_token","value":"<secret>"}
```

Scope is `tenant` | `user` | `agent`; `scope_id` is derived from your identity,
never supplied. `create` is create-or-rotate (re-supplying the value re-seals it).

## Using a credential — `$cred:<name>`

Reference a credential by name as `$cred:<name>` in an **http / streamable-http**
MCP server's headers. It's resolved **per request** from the run's identity, with
scope precedence **agent > user > tenant** — so a user's own token shadows a
tenant default:

```yaml
mcp_servers:
  telegram:
    transport: streamable-http
    url: https://your-telegram-mcp.example.com/mcp
    headers:
      Authorization: "Bearer $cred:telegram_bot_token"   # scope: user
```

Now an agent run **on behalf of user A** resolves A's `telegram_bot_token` and
posts to A's channel; user B's run resolves B's — the same pooled server, each
request bound to its own user's token, with zero plaintext in the transcript. An
unresolved `$cred:` ref drops the header (no literal token is ever sent).

> **Per-user tokens require http/streamable-http MCP servers.** A stdio MCP
> server is a pooled, long-lived process whose env is set once at spawn, so it
> can't carry per-user tokens. Use an http MCP server (token as a per-request
> header) for the per-user case.

## Limitations / roadmap

- Tenants can't set host env, so a `$cred:` value must be provisioned as a
  CredentialDef (above), not a `${LOOMCYCLE_*}` ref.
- External backends (Vault / AWS SM / GCP SM / 1Password) — interface locked,
  implementation is RFC AR Phase 4.
- A bundled search/messaging MCP catalog + the per-agent LLM-provider-key seam
  are follow-ons (RFC AR).
