---
name: credentials
description: Credentials — the CredentialDef encrypted secret store (per tenant/user/agent) and $cred:<name> consumption, so a tenant's own API keys/tokens reach tools without hard-coding secrets in configs or prompts.
aliases: [credential, credentialdef, cred]
---
`CredentialDef` is a durable, **encrypted** store of named secrets (API keys,
tokens, passwords) that tools resolve at call time — so a tenant's own keys reach
the runtime without ever appearing in a config file, a prompt, or a transcript.

## Storing a credential

Use the `CredentialDef` tool: `op=put` a named secret at a **scope**, `op=get`
(metadata only — never the plaintext), `op=list`, `op=delete`. Each credential is
keyed by `(scope, name)`:
- `agent` — visible only to that agent;
- `user` — the end-user behind the run;
- `tenant` — shared across the tenant.

Secrets are encrypted at rest with a per-tenant key derived from the server's
master key; resolution is tenant-isolated (one tenant can never read another's).
A server without the master key configured can store nothing and resolves
nothing (fails soft).

## Consuming a credential — `$cred:<name>`

Where a tool field accepts a `$cred:<name>` token, the runtime substitutes the
resolved secret for **this run's identity** (checking `agent`, then `user`, then
`tenant` scope) just before the call — the model only ever sees the placeholder.
Today `$cred:` is honored in the HTTP-MCP client (e.g. an `Authorization` header
for a per-user Slack/Telegram/HTTP MCP server). The Bashbox host-command fallback
resolves named credentials into a child's env for `git`/`gh` (a per-tenant repo
token). A tool that can't resolve a `$cred:` token drops it rather than sending a
literal placeholder downstream.

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
