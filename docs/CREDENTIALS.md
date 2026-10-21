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
- **It also keys operator tokens.** With no `LOOMCYCLE_OPERATOR_TOKEN_PEPPER`,
  the token-hash pepper is derived from this key, so removing it logs out every
  operator token, and `LOOMCYCLE_SECRET_KEY_PREVIOUS` must stay until every
  token has logged in once under the new key (each is rehashed at that login).
  See CONFIGURATION.md, "Token pepper".
- **A stored credential that can't be decrypted is never replaced.** If the key
  is removed, rotated without `LOOMCYCLE_SECRET_KEY_PREVIOUS`, or wrong — or a
  row is corrupt — every credential stored under it is *held but unusable*,
  exactly like an expired one: a provider call that names it fails instead of
  using the operator's provider key, a Bash / Bashbox variable is left unset
  instead of taking the operator's host variable, an MCP / hook header or a
  webhook verification secret refuses the call or delivery. The error names
  the credential, never key material. Only a name with **no stored row** falls
  back as before. (A store *read* error still falls back, logged: it says
  nothing about whether the run holds a key, and failing closed on it would
  turn a database blip into an outage for every tenant.)
- **Queued runs:** the same key seals a queued run's `user_bearer` and
  `user_credentials` on its row while it waits for a slot, so a restart can
  queue it again. Without the key such a run still queues, but a restart fails
  it (its secrets were never stored).

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

### Expiry — `expires_at`

`create` takes an optional RFC3339 `expires_at`. It is **enforced**: from that
instant on (`now >= expires_at`) the credential is refused everywhere it is
referenced — a `$cred:` header (the MCP call is refused; a closed hook denies), a
remote peer's or git source's key (the dial or clone is refused), a `$ghapp:` App
config (no token is minted, a cached one is not served), a webhook's stored
verification secret (the delivery is refused; an expired `previous_secret` just
drops out of a rotation), a provider / tool key override, and a Bash / Bashbox
env injection. The error names the credential,
its scope and when it expired, never the value.

**An expired credential is never replaced by the operator's value.** Where a
*missing* credential falls back to something the operator supplies, an expired
one does not:

- **Provider / tool key override** (below): the LLM call fails with the expiry
  error instead of using the operator's host key, and the provider cascade does
  not carry it to another provider on the operator's key either. `WebSearch`
  fails the search the same way. A run restricted from operator keys
  (`LOOMCYCLE_OPERATOR_KEY_RESTRICTION`) does not count a provider whose key
  expired as one it can key — the same answer a URL-restricted key gets — so it
  is routed to a provider it holds a valid key for, or refused
  `operator_key_restricted`.
- **Bash / Bashbox env injection**: the variable is left **unset**, and an
  operator-allowlisted host variable of the same name is removed too; the
  command's output (Bash) or stderr (the Bashbox host-command fallback) says
  which variable and why.

Also:

- An expired credential does **not** fall through to a less specific scope: a
  user whose own token expired is refused, not sent under the tenant's token of
  the same name.
- The row is **kept**: `get` / `list` show it with `"expired": true` (and
  `"expired": false` while it is still valid).
- To rotate, `create` it again with the same name. A create without `expires_at`
  clears the expiry; send a new one to keep it time-bound.
- `create` refuses an `expires_at` that is not in the future (such a credential
  could never be used) or is past the year 2262; leave it unset for no expiry.

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
- **A restricted credential is given only to a caller that names its destination,** and only when the destination is covered. These name one:
  - an MCP server's `$cred:<name>` or `$ghapp:<name>` header: the server's URL;
  - a hook's webhook header: the hook's callback URL;
  - a remote memory backend's or document source's `api_key_env: "$cred:<name>"`: its `base_url` (saving such a definition checks this too);
  - a git source for an ephemeral volume: the repository's URL.

  A provider key and a `Bash` / `Bashbox` environment name no destination, and a restricted credential does **not** resolve there — and, like an expired one, nothing is used in its place: the provider call fails naming it instead of taking the operator's key, and the `Bash` / `Bashbox` variable is left unset, the operator's same-named host variable included.
- **A webhook's verification secret is compared, never sent.** A webhook's `auth.secret: "$cred:<name>"` (and its `previous_secret`) is a tenant-level credential of the webhook's own tenant, which the receiver checks a request's signature or token against. It resolves whatever its list says, so store it with `allowed_urls: []`: then it can verify deliveries and can never be sent anywhere (`Context.help input-webhooks`).
- **A request carrying a restricted credential follows no redirect.** The request's URL was checked; a redirect is the server choosing another one, and an HTTP client forwards most headers to it (a custom header such as `X-API-Key` to any host). So the redirect is refused and the call fails. An unrestricted credential follows redirects as before.
- **Rotation keeps the list.** Creating the credential again without `allowed_urls` keeps the list it has; sending a list replaces it. To remove a restriction, delete the credential and create it again.
- **The list cannot be changed apart from the value.** It is stored in the clear, so a listing can show it, and it is part of what the value is sealed with. A row whose list was edited, added or removed outside the tool no longer opens.

A `$ghapp:<name>` App config can be restricted the same way: the minted token then goes only to a covered URL. For a git source the token is also narrowed to the one repository, read-only (`docs/CONFIGURATION.md` §9d.2.1).

### A user name — `username`

`create` takes an optional `username`: the user name sent with the value where a destination takes one. Today that is HTTP basic auth for a git source, which sends `<username>:<value>`. Without one a git source uses a default by kind of host (`docs/CONFIGURATION.md` §9d.2.1); set it when a forge wants a particular name.

- It may not contain `:` (it would split the basic-auth pair) or a control character; at most 256 bytes.
- `get` and `list` show it. It is not a secret.
- **Rotation keeps it.** Creating the credential again without `username` keeps the one it has; `"username": ""` removes it.
- It is stored in the clear and is **not** sealed with the value, unlike `allowed_urls`: changing it cannot send the value anywhere it was not already allowed to go.

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
tenant/user's requests, falling back to the operator's key when absent (not when expired — see Expiry above). Scope
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

## Into a shell — `Bash` and Bashbox host commands

`LOOMCYCLE_BASH_ALLOWED_CREDS` and `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS`
name credentials injected, by name, into the environment of a `Bash` command or
of a Bashbox host command (the `git` / `gh` an operator allowlisted with
`LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS`), resolved for each run like the key
override above.

**Unlike every other use on this page, the model can read these.** A `Bash`
command prints them with `env`; an allowlisted host command can be told to
(`git -c alias.x='!env' x` runs a host shell that does). The value is masked
in the stored transcript (unless `LOOMCYCLE_REDACT_SECRETS=0`), but the live
tool result carries it, and so does the conversation sent to the LLM provider
on the next call. Give a shell
only credentials you would hand the model, and prefer the ones that keep the
token out of reach: a git-source volume (`VolumeDef create source:{git:…}`)
for a clone, an MCP server with a `$cred:` header for an API.

For the Bashbox fallback, bind each credential to the one command that needs
it: `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS=git:GITHUB_TOKEN,gh:GH_TOKEN`
hands `GITHUB_TOKEN` to `git` only. A bare name still reaches every
allowlisted command. A binding does not stop the command from passing the
value on: whatever it starts inherits it.

## Crypto keys and the `Crypto` tool

A credential can also be a **crypto key** (`usage: crypto`): a key the `Crypto` tool uses to encrypt, decrypt, sign, verify, MAC or make one-time codes. Its material is never shown — not when it is generated, not to the agent that uses it, not in `get` or `list`. A crypto key is never sent anywhere either: `$cred:<name>` refuses it on every path (MCP headers, provider keys, Bash variables), and does not fall back to a same-named credential in a wider scope.

```yaml
agents:
  invoice-bot:
    tools: [Crypto]
    crypto_scopes: [tenant, run]   # which keys the agent may use: agent, user, tenant, run
```

**Making and managing keys** (CredentialDef ops):

| op | What it does |
|---|---|
| `generate` | makes a new key of `key_type` from the runtime's random source; nobody ever sees it |
| `create` with `usage: crypto` | imports existing key material (`value`, with `key_encoding` raw / base64 / hex / base32 / pem) |
| `rotate` | adds a new current version (generated, or from `value`) |
| `set_min_version` | retires every version below it; it only goes up, at most to current+1 (every version retired) |
| `delete_version` | deletes a retired version |
| `delete` | refused while any version is unretired: retire them all first |
| `export_key` / `import_key` | moves a key to another deployment, encrypted under a passphrase of at least 16 characters; an operator only, outside any run |

`key_type` is one of `aes-256-gcm`, `xchacha20-poly1305` (encrypt / decrypt), `hmac-sha256`, `hmac-sha512` (MAC, JWS), `ed25519`, `ecdsa-p256`, `rsa-pss-2048` / `-3072` / `-4096` (sign / verify, public key), `x25519`, `rsa-oaep-2048` / `-3072` / `-4096` (encrypt to the key's own public half as a JWE, decrypt, public key), `totp` (one-time codes). An RSA key either signs or decrypts, never both. It is fixed for the key's life and sealed into every version, so it cannot be changed behind the engine's back.

Inside a run, an agent creates and changes crypto keys **only in its own agent scope**; tenant and user keys are made by an operator or the user, outside a run. Snapshots never carry credentials, crypto keys included: move a key with `export_key` / `import_key`.

**Using keys.** The `Crypto` tool takes a crypto key by name (`key`), an ordinary credential as key material (`cred`, with its `key_type`; one restricted by `allowed_urls` is refused), or a credential the run's caller passed (`run`). An agent may use only the scopes its `crypto_scopes` grants; with none it can still `hash` and make `random` values. Unlike a `$cred:` secret, where a narrower scope wins, a key name has **no precedence**: a `key` is used only when exactly one of the run's scopes holds a crypto key of that name, and a `cred` only when exactly one holds an ordinary credential of that name (the other kind is ignored either way). When more than one does, the call is refused until it names the scope with `key_scope` (`agent`, `user` or `tenant`), so a key or secret someone stores under another's name can never stand in for it (a user's own value under the tenant's webhook-secret name would otherwise verify MACs they forged). CredentialDef also refuses to create a crypto key under a name another scope the caller sees already holds as one. An envelope records its key's kind and scope (`lcx1:kt:` crypto key in tenant scope, `ku` user, `ka` agent; `ct` / `cu` / `ca` for an ordinary credential; `r` for a run credential), and that is part of the associated data, so a ciphertext decrypts only with the key of the scope that sealed it; a same-named key that appears later can neither open it nor block it. Decrypt and verify failures never say why. `decrypt` with `sensitive: true` keeps the plaintext out of the stored transcript, and the plaintext an agent encrypts is never stored. Every keyed op and every key change is written to the audit log (`LOOMCYCLE_AUDIT_LOG_PATH`), never with data or key material.

**Public keys, passwords, and data the agent never sees.** `encrypt` with `public_key` seals a value to someone else's X25519 or RSA key as a standard JWE (RFC 7516) that only they can open; like `verify` with `public_key`, it holds no key and needs no crypto scope. An `aes-256-gcm` key can produce a JWE too (`format: jwe`). `hash_password` / `verify_password` store and check passwords with Argon2id; the password is masked from the stored transcript. Inside a run, `from` and `to` read an op's input from, and write its output to, a volume file (`volume:<path>`), a Memory key (`memory:<scope>/<key>`) or a document chunk (`document:<scope>/<chunk_id>`), so the data never passes through the model. The run needs the matching tool (`Read`, `Write`, `Memory`, `Document`), gets exactly that tool's access, and the tool's pre-hooks judge the call. References are refused on the operator API.

**Keys held in an external KMS.** A crypto key can reference a key held in **HashiCorp Vault Transit, AWS KMS or Google Cloud KMS** instead of material stored here: `create` with `usage: crypto`, `key_type`, `backend` (`vault`, `aws_kms` or `gcp_kms`) and an `external` object. loomcycle stores only the reference and never holds the material; every Crypto operation on the key is a call to the KMS. The KMS is asked at `create` that the key exists, is enabled and is of `key_type`.

| backend | `external` fields |
|---|---|
| `vault` | `address` (https; its host must be on `LOOMCYCLE_HTTP_HOST_ALLOWLIST` or `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`, checked at create and at every use, and dialled through the SSRF guard), `key` (the Transit key), `mount` (default `transit`), `namespace`, `token` |
| `aws_kms` | `region`, `key_id` (key id, key ARN, `alias/<name>` or alias ARN), `access_key_id`, `secret_access_key`, `session_token` |
| `gcp_kms` | `key` (`projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>`, optionally `/cryptoKeyVersions/<n>` for a signing or RSA key), `service_account` (a service-account key JSON) |

The credential fields are `$cred:<name>` references to ordinary credentials in **the key's own scope**: a user's key runs on that user's KMS credentials and a tenant's on the tenant's, never the other's (a reference to one with `allowed_urls` is refused). The reference is sealed with the key's usage, type and backend, so editing any of them in the database leaves the key unusable rather than pointed elsewhere. Such keys are added by an operator or the user, outside a run. They behave like any crypto key: the envelope carries the KMS's ciphertext and its version (0 for AWS, which has none), `context` reaches the KMS as associated data (Vault `associated_data`, the AWS encryption context, Google's additional authenticated data), and an `rsa-oaep` key encrypts as a JWE to the public key the KMS publishes. An `aes-256-gcm` KMS key has no JWE form, and `ed25519` is Vault only. Rotate and retire such a key in the KMS: `rotate`, `set_min_version`, `delete_version` and `export_key` refuse it, and `delete` removes only the reference. A KMS that is down is a `transient` error and one that refuses the credentials a `permission` error, naming the KMS and at most an HTTP status, never its response.

**Outside a run.** `POST /v1/_crypto` runs the same operations for an operator or a service: a backend that encrypts a field before writing it to its own database, or checks a partner's webhook signature. The body is the `Crypto` tool's input and the answer is its result, with the same envelope and associated data, so a value one side encrypts the other decrypts. It needs `substrate:tenant`; a tenant's members are refused, because decrypting with the tenant's keys would let them read whatever the tenant sealed. A call uses its token's tenant keys and its own subject's user keys, never an agent's, and there are no run credentials outside a run. An admin may name another tenant with a top-level `tenant`, and then uses that tenant's keys only. Each call takes one from the token's run rate when the token has one (`429` with `Retry-After` past it); the per-run op budget does not apply. The audit record names the caller as the actor and the key's tenant as the target. The same operation is the gRPC `Crypto` RPC, the MCP `crypto` tool and `crypto()` in the TS and Python clients, and the route is in the OpenAPI contract (`/v1/openapi.yaml`).

```bash
curl -s -X POST "$LOOMCYCLE/v1/_crypto" -H "Authorization: Bearer $TOKEN" \
  -d '{"op":"encrypt","key":"invoice-fields","plaintext":"4111 1111 1111 1111","context":"invoice:1234"}'
# → {"ciphertext":"lcx1:kt:invoice-fields:3:…","key_version":3}
```

| Env var | Default | Meaning |
|---|---|---|
| `LOOMCYCLE_CRYPTO_MAX_BYTES` | 1048576 | cap on each data argument of a Crypto call |
| `LOOMCYCLE_CRYPTO_MAX_OPS_PER_RUN` | 100 | Crypto calls one run may make (not the operator API) |
| `LOOMCYCLE_KMS_VAULT_ALLOW_HTTP` | off | `1` admits a plain-http Vault address (a dev server) |

`/metrics` reports `loomcycle_crypto_ops_total{op, outcome}`. The tool's own help (`Context op=help topic=Crypto`) documents each operation.

## Limitations / roadmap

- Tenants can't set host env, so a `$cred:` value must be provisioned as a
  CredentialDef (above), not a `${LOOMCYCLE_*}` ref.
- The provider-key override attributes spend to the run's tenant/user identity;
  per-key usage **accounting / limits** (audit + report + cap per scope) is the
  next step, tracked separately.
- External backends (Vault / AWS SM / GCP SM / 1Password) — interface locked,
  implementation is RFC AR Phase 4.
- A bundled search/messaging MCP catalog is a follow-on (RFC AR).
