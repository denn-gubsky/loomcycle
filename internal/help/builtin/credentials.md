---
name: credentials
description: Credentials — the CredentialDef encrypted secret store (per tenant/user/agent) and $cred:<name> consumption, so a tenant's own API keys/tokens reach tools without hard-coding secrets in configs or prompts.
aliases: [credential, credentialdef, cred]
---
`CredentialDef` is a durable, **encrypted** store of named secrets (API keys,
tokens, passwords) that tools resolve at call time — so a tenant's own keys reach
the runtime without ever appearing in a config file, a prompt, or a transcript.

## Storing a credential

Use the `CredentialDef` tool: `op=create` a named secret at a **scope**, `op=get`
(metadata only — never the plaintext), `op=list`, `op=delete`. Each credential is
keyed by `(scope, name)`:
- `agent` — visible only to that agent;
- `user` — the end-user behind the run;
- `tenant` — shared across the tenant.

Secrets are encrypted at rest with a per-tenant key derived from the server's
master key; resolution is tenant-isolated (one tenant can never read another's).
A server without the master key configured can store nothing and resolves
nothing (fails soft).

### Restricting where a credential may be sent — `allowed_urls`

`op=create` takes an optional `allowed_urls`: the https URLs the value may be
sent to.

```json
{"op": "create", "name": "repo-acme-app", "value": "<token>",
 "allowed_urls": ["https://github.com/acme/app"]}
```

- An entry covers that URL and everything under it, compared by host and by
  whole path segments. `https://github.com/acme/app` covers
  `https://github.com/acme/app.git` and does not cover
  `https://github.com/acme/app-fork` or `https://github.com/acme`.
- Entries must be plain https URLs: no user or password, no query, no
  fragment. `get` and `list` show them in a canonical form.
- **Left out**: no restriction, as before. **An empty list `[]`**: the value
  is sent nowhere. It can still be compared against, which is what a webhook
  signing secret needs.
- **A restricted credential is given only where the destination is known
  and covered:** an MCP server's header (the server's URL), a hook's webhook
  header (the callback URL), a remote memory backend's or document source's
  key (its `base_url`), and a git source (the repository URL). It does
  **not** resolve as a provider key or in a `Bash` / `Bashbox` environment:
  those name no destination.
- A request carrying a restricted credential **follows no redirect**: the
  redirect is refused and the call fails.
- Creating the credential again without `allowed_urls` (a rotation) **keeps
  the list it has**. Sending a list replaces it. To remove a restriction,
  delete the credential and create it again.
- The list is sealed together with the value. A row whose list was changed
  outside the tool no longer opens.

### Expiry — `expires_at`

`op=create` takes an optional RFC3339 `expires_at` (it must be in the future),
and it is **enforced**: from that instant on the credential is refused wherever
it is referenced (a `$cred:` header, a peer or git key, a `$ghapp:` App config, a
provider key override, a Bash / Bashbox env var). An expired credential is never
replaced by anything else: not by a same-named credential in a wider scope, not
by the operator's provider key (the call fails, naming the credential), and not
by the operator's same-named host variable in Bash / Bashbox (the variable is
left unset and the output says why). `get` and `list` keep showing it, with
`"expired": true`. Rotate it by creating it again with the same name; a create
without `expires_at` clears the expiry.

### A user name — `username`

`op=create` takes an optional `username`: the user name sent with the value
where a destination takes one, today HTTP basic auth for a git source.
Without it a git source uses a default for the kind of host. It may not
contain `:` or a control character. A rotation without `username` keeps the
stored one; `"username": ""` removes it. `get` and `list` show it.

## Consuming a credential — `$cred:<name>`

Where a tool field accepts a `$cred:<name>` token, the runtime substitutes the
resolved secret for **this run's identity** (checking `agent`, then `user`, then
`tenant` scope) just before the call — the model only ever sees the placeholder.
Today `$cred:` is honored in the HTTP-MCP client (e.g. an `Authorization` header
for a per-user Slack/Telegram/HTTP MCP server). The Bashbox host-command fallback
resolves named credentials into a child's env for `git`/`gh` (a per-tenant repo
token). A tool that can't resolve a `$cred:` token drops it rather than sending a
literal placeholder downstream.

A remote memory backend or document source can name its peer key as
`api_key_env: "$cred:<name>"`. That one works differently: it resolves only a
**tenant**-scope credential, in the tenant that owns the definition, never a
user's or agent's credential and never the calling run's tenant. If it does not
resolve, the call to the peer fails and nothing is sent.

## Provider keys

A stored credential named for a provider env var (e.g. `ANTHROPIC_API_KEY`,
`BRAVE_API_KEY`) overrides the operator's host key for that run, so a tenant can
bring its own key and be billed for its own spend.

## vs. per-run credentials

`CredentialDef` is the **durable** store. A trigger (schedule / webhook / A2A)
can also carry a **per-run** named-credentials map supplied at fire time — see
`help(topic="per-run-credentials")`. Prefer the durable store for anything
long-lived; use per-run for one-off or externally-injected secrets.

## Cross-references

- `help(topic="per-run-credentials")` — the per-run named-credentials map on triggers.
- `help(topic="operator-tokens")` — bearer tokens that authenticate a principal (distinct from stored secrets).
- `Context op=self` — the identity (tenant/user/agent) credentials resolve against.
