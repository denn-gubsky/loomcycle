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
(`tools:[CredentialDef]`). Ops: `create`, `get`, `list`, `delete`.
`get`/`list` return **metadata only** — never a secret.

```jsonc
// tenant-shared secret (all agents in the tenant)
{"op":"create","scope":"tenant","name":"serper_api_key","value":"<secret>"}

// per-user secret — keyed to YOUR subject; another user can't read it
{"op":"create","scope":"user","name":"telegram_bot_token","value":"<secret>"}
```

Scope is `tenant` | `user` | `agent`; `scope_id` is derived from your identity,
never supplied. `create` is create-or-rotate (re-supplying the value re-seals it).

### Restricting where a credential may be sent — `allowed_urls`

`create` takes an optional `allowed_urls`: the https URLs the value may be sent to.

```jsonc
{"op":"create","name":"repo-acme-app","value":"<token>",
 "allowed_urls":["https://github.com/acme/app"]}
```

It answers a question a secret store otherwise cannot. When the destination of a request comes from somewhere untrusted, such as a repository URL in a webhook body, is this the place the secret was stored for?

| `allowed_urls` | Meaning |
|---|---|
| left out | no restriction, as before |
| `[]` | the value is sent nowhere. It can still be compared against, which is what a webhook signing secret needs |
| a list | the value is sent only to a URL under one entry |

- **Matching.** By host, exactly and without regard to case, and by whole path segments. `https://github.com/acme/app` covers `https://github.com/acme/app.git` and what is under it, and does not cover `https://github.com/acme/app-fork` or `https://github.com/acme`.
- **Entries** must be plain https URLs: no user or password, no query, no fragment, no encoded or dotted path segment. At most 32. `get` and `list` show them in canonical form.
- **A restricted credential is given only to a caller that names its destination,** and only when the destination is covered. So it does **not** resolve in a `$cred:<name>` header, as a provider key, or in a `Bash` / `Bashbox` environment: none of those name a destination. Such a use fails as an unresolved credential would, and a provider falls back to the operator's key as it does for any credential it cannot read.
- **Rotation keeps the list.** Creating the credential again without `allowed_urls` keeps the list it has; sending a list replaces it. To remove a restriction, delete the credential and create it again.
- **The list cannot be changed apart from the value.** It is stored in the clear, so a listing can show it, and it is part of what the value is sealed with. A row whose list was edited, added or removed outside the tool no longer opens.

Nothing in this release sends a restricted credential anywhere yet: the first consumer that names a destination is the git source for ephemeral volumes. Until then the field is useful for `[]`, and for preparing credentials.

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
unresolved `$cred:` ref **refuses the call** — no literal token is ever sent, and
neither is an unauthenticated request. The tool result is classified `business`
and not retryable, so an agent stops instead of retrying a call that cannot
succeed.

> **Per-user tokens require http/streamable-http MCP servers.** A stdio MCP
> server is a pooled, long-lived process whose env is set once at spawn, so it
> can't carry per-user tokens. Use an http MCP server (token as a per-request
> header) for the per-user case.

## Minting a GitHub App token — `$ghapp:<name>`

For **short-lived, repo-scoped** git auth (preferred over a long-lived PAT), store
a **GitHub App** config instead of a token and reference it with `$ghapp:<name>`.
loomcycle mints a fresh installation access token (an RS256 JWT exchanged at
GitHub's installation-token API), caches it until ~5 min before it expires (~1 h
TTL), and substitutes it exactly like `$cred:`. **The App private key never leaves
loomcycle — only the minted, scoped token is ever sent.**

Store the App config as a credential whose value is a JSON object:

```jsonc
// value is a JSON string; app_id/installation_id/private_key are required.
// repositories + permissions are optional and DOWN-SCOPE the minted token.
{
  "app_id": "123456",
  "installation_id": "7654321",
  "private_key": "-----BEGIN RSA PRIVATE KEY-----\n…\n-----END RSA PRIVATE KEY-----\n",
  "repositories": ["my-repo"],
  "permissions": {"contents": "write"},
  "base_url": "https://api.github.com"   // optional; GitHub Enterprise Server, https only
}
```

```jsonc
// create it (the value is the JSON above, as a single JSON-escaped string)
credentialdef  op=create  scope=user  name=sandbox_github_app  value='{"app_id":"123456",…}'
```

Then reference it in an http-MCP header, the same place a `$cred:` would go:

```yaml
headers:
  Authorization: "Bearer $ghapp:my_github_app"   # or X-Loom-Sandbox-Env-Gh-Token for the dev sandbox
```

An unresolved ref — or a mint failure (bad key, network) — **refuses the call**.
No literal token is ever sent, and neither is a request without it: a peer that
does not authenticate would otherwise serve it anonymously, which is the same
request under a different identity with no error anywhere.

## Overriding a provider / tool API key by its env-var name

Some credentials aren't referenced with `$cred:` — they **override the operator's
host key automatically**, keyed by the credential's *name*. Store a credential
**named after the well-known env var** and loomcycle uses it for that
tenant/user's requests, falling back to the operator's key when absent. Scope
precedence is the same **agent > user > tenant**, and the key stays runtime-side —
the model never sees it.

| Store a credential named | …and it overrides the host key for |
|--------------------------|------------------------------------|
| `ANTHROPIC_API_KEY`      | Anthropic LLM calls                |
| `OPENAI_API_KEY`         | OpenAI LLM calls                   |
| `DEEPSEEK_API_KEY`       | DeepSeek LLM calls                 |
| `GEMINI_API_KEY`         | Gemini LLM calls                   |
| `OLLAMA_API_KEY`         | hosted ollama.com LLM calls        |
| `BRAVE_API_KEY`          | the `WebSearch` tool               |

So a tenant that stores its own `ANTHROPIC_API_KEY` (scope `tenant`) has every
run in that tenant bill Anthropic to **its** key; a single user that stores one
(scope `user`) shadows the tenant default for **that user's** runs only. A tenant
with no override transparently uses the operator's host key. A `BRAVE_API_KEY`
credential even lets a tenant search on its own Brave quota when the operator set
no host key at all.

```jsonc
// as a substrate:tenant operator, over MCP or HTTP
credentialdef  op=create  scope=tenant  name=ANTHROPIC_API_KEY  value=sk-ant-…
```

> This override happens only on the **inference / tool request** path.
> Model-*availability* probes stay on the operator key (which models are reachable
> is an operator concern), and `ollama-local` is unauthenticated so it never takes
> an override.

## Limitations / roadmap

- Tenants can't set host env, so a `$cred:` value must be provisioned as a
  CredentialDef (above), not a `${LOOMCYCLE_*}` ref.
- The provider-key override attributes spend to the run's tenant/user identity;
  per-key usage **accounting / limits** (audit + report + cap per scope) is the
  next step, tracked separately.
- External backends (Vault / AWS SM / GCP SM / 1Password) — interface locked,
  implementation is RFC AR Phase 4.
- A bundled search/messaging MCP catalog is a follow-on (RFC AR).
