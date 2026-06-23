---
name: operator-tokens
description: multi-tenant auth — bearer tokens bound to an authoritative principal (tenant + subject + scopes), rotation, audit, and the LOOMCYCLE_AUTH_TOKEN migration.
---
loomcycle's default auth is one shared secret, `LOOMCYCLE_AUTH_TOKEN`:
everyone who holds it can do everything, and the wire `tenant_id` /
`user_id` on a run are caller-asserted LABELS — not trust boundaries.
That is the right shape for one operator and wrong the moment a team or
a small VPS fronts users who don't trust each other's claims.

**OperatorTokenDef** (RFC L) replaces the shared secret with a
substrate of bearer tokens, each bound to an **authoritative principal**
the auth middleware resolves *from the token* and stamps into the
request — so the keys downstream isolation already uses become
authority-derived instead of forgeable.

## The principal model

A token resolves to `{tenant_id, subject, scopes}`:

- **tenant_id** — the data-isolation boundary. Memory tenancy partitions
  on it; distinct subjects under one tenant SHARE the tenant's data
  (they collaborate).
- **subject** — the per-actor id. It becomes the run's `user_id`, so it
  is the **fairness key** and the attribution/audit actor. Fifty
  developers under one tenant get fifty distinct subjects → fifty
  distinct fairness caps, while sharing the tenant's memory.
- **scopes** — a capability set from a closed catalog.

On an authenticated route the principal **overrides** the wire
`tenant_id`/`user_id`; a disagreement is honored server-side and logged
`kind=identity_overridden`. A caller can no longer become a different
user/tenant by editing the request body.

## Scope catalog (closed)

`substrate:admin` (superuser — satisfies every scope), `substrate:tenant`
(tenant operator — RFC AF), `runs:create`, `runs:read`, `channel:publish`,
`channel:read`. Every catalog scope is enforced by at least one route —
operators can't invent scope names, and the catalog intentionally excludes
scopes no route checks (a scope that enforces nothing is a false
limitation). The default at create is `["substrate:admin"]`, so a first
token keeps "single token, full power". Narrow app tokens
(`["runs:create"]`) are the upgrade path; a route that needs a scope the
token lacks returns **403** with `WWW-Authenticate: Bearer scope="…"`.

**`substrate:tenant`** is the middle ground between an admin token and a
narrow `runs:*` app token: it grants FULL power WITHIN the token's own
tenant — runs, channels, authoring all 8 substrate Def families (Agent /
Skill / MCPServer / Schedule / Webhook / MemoryBackend / A2AAgent /
A2AServerCard, where MCPServerDef is the "dynamic MCP tools ingestion"
surface), and registering tool-use hooks — but NOT the operator plane: no
token minting (`/v1/_operatortokendef`), no runtime admin (pause / resume /
snapshot), no loomcycle-as-MCP-server transport (`/v1/_mcp`), and no
cross-tenant access. It satisfies the within-tenant scopes (`runs:*`,
`channel:*`, the def/hook gate) but never `substrate:admin`. Mint a
self-provisioning tenant (e.g. an app that syncs its own AgentDefs + hooks
at boot) with `--scopes substrate:tenant` instead of handing it admin.
Confinement is automatic — a non-admin principal's def writes are stamped
with its authoritative tenant, cross-tenant reads return an opaque 404, and
a tenant-registered hook fires only on that tenant's runs.

## Managing tokens

```sh
# Mint a per-developer token (shown ONCE — store it now).
loomcycle operator-token create --tenant acme --subject alice \
  --scopes runs:create,runs:read

# A narrow production token.
loomcycle operator-token create --tenant acme-prod --subject app --scopes runs:create

loomcycle operator-token list                 # all names (no secrets)
loomcycle operator-token list --name alice    # one name's history
loomcycle operator-token rotate --name alice  # new token; old one graces out
loomcycle operator-token retire --name alice  # immediate revoke
```

`create`/`rotate` show the token plaintext exactly once; it is never
persisted or logged (only `SHA-256(pepper ‖ token)` is stored). A lost
token is rotated, not recovered. The same operations are available over
HTTP (`POST /v1/_operatortokendef`), gRPC, the MCP `operatortokendef`
meta-tool, and the TS client's `operatorTokenDef()`.

### From the Web UI (no shell)

When the binary's CLI isn't reachable — a packaged deployment such as the
TrueNAS app — the same lifecycle is in the **Settings hub**: sign in with an
admin (root) token, click the **gear** (top-right), and open **Tokens**.
Generate (the secret is revealed once with a copy button), list, rotate, and
retire — backed by the same `POST /v1/_operatortokendef`. The gear and the
Tokens panel are **admin-only** (a `substrate:tenant` session never sees them);
the Settings hub also surfaces the embedded **Presets** (RFC AQ), **Runtime**
(pause/resume), and **Health**.

## Rotation grace

`rotate` mints a new token for the same principal and marks the prior
one to retire after a grace window (default 24h,
`LOOMCYCLE_OPERATOR_TOKEN_ROTATION_GRACE_SECONDS`, or per-call
`--grace-seconds`). Both authenticate during the window — a zero-downtime
roll. `retire` revokes immediately.

## Migration from LOOMCYCLE_AUTH_TOKEN (zero-disruption)

The legacy shared secret keeps working until the **first admin-scoped**
OperatorTokenDef exists; then it's disabled (a startup/triage log names
it). To upgrade WITHOUT a flag day, bind the existing env token as a real
admin principal so it keeps authenticating:

```sh
loomcycle operator-token create --tenant default --subject ops --copy-from-env
```

`--copy-from-env` imports `$LOOMCYCLE_AUTH_TOKEN`'s hash (it is never
re-displayed). Now distribute per-principal tokens at your own pace.

## Configuration

- `LOOMCYCLE_OPERATOR_TOKEN_PEPPER` — mixed into the token hash; a stolen
  DB dump without it yields no usable lookup. Set it in multi-tenant
  deployments (env-allowlisted, never logged).
- `LOOMCYCLE_AUTH_CACHE_TTL_SECONDS` — per-replica resolution cache TTL
  (default 30; `0` disables it for immediate revocation). A token
  mutation flushes the cache locally and, in a cluster, broadcasts a
  flush over the backplane (`loomcycle.operator_token_changed`), so
  typical revocation propagates in one round-trip; the TTL is the
  worst-case backstop.
- `LOOMCYCLE_AUDIT_LOG_PATH` — JSONL audit of every create/rotate/retire
  (`{ts, actor_*, action, target_*, scopes_*}` — never a token or hash).
  Per-replica local file; ship it with logrotate/fluentd/Loki.
- `LOOMCYCLE_AUTH_VERBOSE=1` — log a server-side reason on a rejected
  bearer (the wire 401 stays opaque regardless).

## The OSS ceiling

OSS does *basic* multi-tenancy: token-per-principal, scopes, rotation,
file audit. SSO/OIDC/SAML, RBAC roles, SCIM, automated rotation policy,
signed/queryable audit, and compliance evidence are the enterprise
edition — layered on this same substrate without changing it.
