---
name: operator-tokens
description: multi-tenant auth — bearer tokens bound to an authoritative principal (tenant + subject + scopes), rotation, audit, and the LOOMCYCLE_AUTH_TOKEN migration.
---
loomcycle's default auth is one shared secret, `LOOMCYCLE_AUTH_TOKEN`:
everyone who holds it can do everything, and the wire `tenant_id` /
`user_id` on a run are caller-asserted LABELS — not trust boundaries.
That is the right shape for one operator and wrong the moment a team or
a small VPS fronts users who don't trust each other's claims.

**OperatorTokenDef** replaces the shared secret with a
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
(tenant operator), `runs:create`, `runs:read`, `channel:publish`,
`channel:read`, `runs:delete` (may delete a run tree; tenant-implied, and
not implied for an isolated user or a member token, so grant it by name),
`providers:operator-key` (may spend the operator's
host provider key; tenant-implied), `llm:gateway` (may call the raw model
endpoints; tenant-implied, and needed by name only on an isolated user's
token). Every catalog scope is enforced by at least one route —
operators can't invent scope names, and the catalog intentionally excludes
scopes no route checks (a scope that enforces nothing is a false
limitation). The default at create is `["substrate:admin"]`, so a first
token keeps "single token, full power". Narrow app tokens
(`["runs:create"]`) are the upgrade path; a route that needs a scope the
token lacks returns **403** with the JSON body
`{"code":"insufficient_scope","error":"insufficient scope","required_scope":"…"}`
and the same scope in `WWW-Authenticate: Bearer scope="…"`.

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

**`providers:operator-key`** gates whether a run may fall back to the
operator's HOST provider API key. It is **tenant-implied** — `substrate:admin`
and `substrate:tenant` (and the legacy `LOOMCYCLE_AUTH_TOKEN`) already have it —
and **inert unless** the deployment sets `LOOMCYCLE_OPERATOR_KEY_RESTRICTION=1`.
To make a tenant pay its own way on a restricting deployment: (1) set the gate;
(2) mint that tenant's principals with granular scopes that OMIT this one (and
`substrate:*`), e.g. `--scopes runs:create,runs:read`; (3) give the tenant its
own provider key (a CredentialDef named after the key env-var). A
restricted run then routes only to providers the tenant can key itself and is
refused **403** `operator_key_restricted` (gRPC `PermissionDenied`) if none —
never touching the operator's key. Because it's tenant-implied, a
`substrate:tenant` token can't itself be restricted; restriction is expressed
with granular tokens (the confined-API-consumer shape anyway).

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
the Settings hub also surfaces the embedded **Presets**, **Runtime**
(pause/resume), and **Health**.

## Rotation grace

`rotate` mints a new token for the same principal and marks the prior
one to retire after a grace window (default 24h,
`LOOMCYCLE_OPERATOR_TOKEN_ROTATION_GRACE_SECONDS`, or per-call
`--grace-seconds`). Both authenticate during the window — a zero-downtime
roll. `retire` revokes immediately.

## Token lifetime

A token can be minted with a fixed lifetime: `--ttl-seconds N` on
`operator-token create` (`ttl_seconds` on the `create` op), or
`{"ttl_seconds": N}` in the body of `POST /v1/_users/{subject}/tokens`.
It stops authenticating when the lifetime runs out, exactly as a retired
token does. Use it for a token that lives on a machine you do not
control.

- `rotate` does not extend a lifetime: the replacement expires when the
  original would have. An expired token cannot be rotated.
- A token holding `substrate:admin` cannot have a lifetime — it would lock
  the deployment out when it ran out.
- The per-user mint also takes `name` (a label, stored as `u-<name>`) and
  `max_active` (keep at most N of the user's tokens alive; the oldest
  beyond it are retired). An unknown field in that body is refused.
- A token cannot mint one that outlives it: when the minting token has a
  lifetime, a longer `ttl_seconds` is refused and a mint that asks for
  none ends when the minting token does.
- Nor one less restricted than itself: a minted token (a per-user token,
  or an operator token from `create`) carries the stricter of each
  restriction of the minting token and of the request — `retention: none`
  if either has it, the lower `rate`, only the `overrides` both allow, only
  the `agents` both list. A `rotate` holds the new token to the rotating
  token's restrictions too, since the caller gets its secret.
- `LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS` caps a per-user token's
  lifetime; with it set, a mint that asks for none gets the cap.
- An expired token's row is deleted
  `LOOMCYCLE_EXPIRED_TOKEN_RETENTION_SECONDS` after it expired (default 7
  days; `0` keeps them). A token minted without a lifetime is never
  deleted this way.

## Limiting a token to certain agents

`--agents a,b` on `operator-token create` (`agents` on the `create` op,
or `{"agents": [...]}` in the body of `POST /v1/_users/{subject}/tokens`,
or `agents:` on a declared principal) limits the token to starting those
agents as top-level runs.

- Any other agent is refused (`agent_not_allowed`), whether or not it
  exists.
- The token is limited to the run endpoints: starting, continuing,
  reading and controlling runs. It is refused everywhere else
  (`restricted_token`) whatever its scopes — definitions, team walks,
  schedules, the model gateways, a direct decision, channels, memory,
  documents, the credential store, A2A. It may still ask which per-run
  settings a run of a listed agent may set
  (`GET /v1/_agentdef/{name}/overridable`).
- A sub-agent that a listed agent spawns is not limited; it follows the
  parent agent's definition.
- The list is fixed at mint and carried over by `rotate`. An empty list
  is refused. A token holding `substrate:admin` cannot have one.

## Locking a token's per-run settings

`--overrides none` on `operator-token create` (`overrides` on the
`create` op, `{"overrides": "none"}` in the body of
`POST /v1/_users/{subject}/tokens`, or `overrides: none` on a declared
principal) locks the per-run overrides for runs the token starts: the
model, provider, tier and effort, the `routing` block, the limits, sampling, compaction,
output format, hooks and the rest. A list (`--overrides effort,tier`)
names the ones the token may still set.

- A request that sets a locked setting is refused
  (`override_not_allowed`, naming the field); it is never ignored.
- It applies at a run start, a continuation, a configured run and a
  retune.
- Fields that only narrow, bound or name a run (`tools`,
  `allowed_hosts`, `max_wall_seconds`, `idempotency_key`, `metadata`)
  are not locked.
- The lock is fixed at mint and carried over by `rotate`.
- To lock an agent for every caller instead, an operator included, set
  `allow_overrides: false` on the agent's definition.
- To learn which settings a run of an agent may set before sending it,
  `GET /v1/_agentdef/{name}/overridable` answers for the calling token:
  one entry per setting, `overridable` true or false, and the lock that
  refuses it (`token_locked`, or `agent_locked` for the agent's own
  `allow_overrides: false`).

## Limiting how often a token starts runs

`--rate-per-minute N [--rate-burst M]` on `operator-token create` (`rate`
on the `create` op, `{"rate": {"per_minute": N, "burst": M}}` in the body
of `POST /v1/_users/{subject}/tokens`, or `rate:` on a declared principal)
bounds how often the token may start runs. `burst` is how many it may
start at once from idle (default: `per_minute`).

- A start past the rate is refused (`rate_limited`) with the time to
  wait. Nothing is read or written for it.
- A run start, each child of a batch, a continuation, a replay, a
  configured run and a direct decision each count as one. Sub-agents and
  an agent's own calls do not.
- The rate is per user: two tokens of one user share it.
- `LOOMCYCLE_RUN_RATE_PER_MINUTE` (with `LOOMCYCLE_RUN_RATE_BURST`) sets
  a default for tokens that carry no rate. It does not bind an admin, a
  tenant operator or the legacy token.
- It is per replica.

## Making a token's runs no-trace

`--retention none` on `operator-token create` (`retention` on the `create`
op, `{"retention": "none"}` in the body of
`POST /v1/_users/{subject}/tokens`, or `retention: none` on a declared
principal) makes every run the token starts a no-trace run: what the run
says is delivered to the caller and stored nowhere.

- A request or an agent cannot loosen it: asking for `default` still runs
  as `none`.
- Such a token cannot store a prompt as a draft, start a team walk, or
  continue a chat (`retention_none_unsupported`).
- The setting is fixed at mint and carried over by `rotate`.
- See `loomcycle help no-trace-runs` for what a no-trace run keeps and
  what it cannot do.

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
