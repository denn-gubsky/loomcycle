# loomcycle.cloud — moving sign-in to the password gate and the `auth:manage` API

**Status:** PLAN, 2026-10-24. Not started. It depends on loomcycle RFC FA P0 (sessions) and P2 (the password gate), which are being built now.
**Design source:** RFC FA "Auth gates" in the loomcycle document store (`/loomcycle/rfcs/external-authorization-gates`), §6.1 and decisions D15–D18.
**Touches:** `cloud-deployment/landing/server.js`, `cloud-web/index.html`, `cloud-deployment/docker-compose.yaml`, `cloud-deployment/config/loomcycle.yaml`, `.env.insecure.example`, `INSTALL.md`, `README.md`.

## Why this, and not the alternatives

Today the landing server does three jobs: it serves the site, it proxies two public reads, and it **mints tenant tokens with the loomcycle admin token**. That last job is the problem:

- The landing holds `LOOMCYCLE_ADMIN_TOKEN`. That token can do anything on the runtime, in every tenant.
- The browser ends up holding a long-lived API bearer, kept in an IndexedDB vault and handed to `app.loomcycle.cloud` over `postMessage`.
- Nobody signs in to the app. Possession of a token is the only identity.
- A tenant owner cannot add a colleague, except by sharing their own token.

There are three alternatives, and each was ruled out:

- **Keep minting, but with a narrower token.** This doesn't help much. Whatever the website holds still mints bearers that the browser keeps, and there is still no login and no team.
- **Put user management in loomcycle's Web UI only.** That works for a tenant owner who is already inside the app. But the website is where a new person arrives and where a tenant is created. That step needs a credential that is not a tenant's own, and it belongs on the website. So the website keeps the arrival flow.
- **Run a separate password service.** The owner decided against this (D15). The password hashes and the pepper stay inside loomcycle.

**The design:**

- **The landing holds one narrow credential.** It is an operator token with the `auth:manage` scope. That scope covers user and gate management across tenants, and nothing else: no runs, no data, no token minting, no ACL authoring.
- **The website handles arrival and team management.** It creates a tenant's owner invite, lists the team, invites members and issues reset links.
- **Passwords are typed only on loomcycle's own pages** (D17): `app.loomcycle.cloud/ui/invite#<code>` and `/ui/reset#<code>` (the code rides the URL fragment and is POSTed in the body, so it never reaches an access log). The website never sees a password.
- **Signing in to the app is a password login** on `app.loomcycle.cloud`, which then holds a server-side session (P0). The browser holds no bearer.

## Target architecture

```
                         Cloudflare edge (TLS)
 visitor ──▶ loomcycle.cloud ──(Access: Google)──▶ landing:8080
                │   /api/signup, /api/team/*        │  holds LOOMCYCLE_AUTH_MANAGE_TOKEN (auth:manage)
                │                                   ▼
                │                         tailscale:8787  /v1/_authgate/cloud/*, /v1/_users*
                │
                └─ redirect ─▶ app.loomcycle.cloud/ui/invite#<code>    (set login + password; session cookie)
                               app.loomcycle.cloud/ui/login           (login + password [+ TOTP])
```

- **Who someone is on the website:** the Cloudflare Access Google identity, unchanged. The landing still verifies `Cf-Access-Jwt-Assertion` against the team JWKS and audience.
- **Who someone is in the app:** a password-gate login, mapped to a `(tenant, subject)` through `user_identities` (D18).
- **The owner rule:** whoever holds a verified Google e-mail owns its derived tenant, `t_<sha256(email)[:12]>`. This is the same derivation as today, so existing tenants keep their ids. The website acts only on that one tenant. It never takes a tenant from the request.

## loomcycle side (configuration only, once P0 + P2 ship)

`cloud-deployment/config/loomcycle.yaml`:

```yaml
auth:
  mode: token                  # stays token during the migration; strict in phase 6
  gates:
    token:  { kind: token }    # the paste box, still needed during the migration
    cloud:
      kind: password
      label: "loomcycle.cloud account"
      require_mfa: false       # true at the strict switch (phase 6)
      invites: { default_ttl: 168h, max_ttl: 720h }
      resets:  { ttl: 1h }
      allowed_return_to: ["https://loomcycle.cloud/"]
      # Lets the landing reset an owner's password and attach a password to a
      # legacy owner's subject (phase 4). Without it, the website cannot recover
      # an owner, only members. It makes the website's credential able to take
      # over any owner's account, which is acceptable here only because the
      # landing proves the owner's Google identity first (RFC FA §6.1.1).
      manager_may_recover_owners: true
  # Without these every login would look like it came from cloudflared and one
  # attacker's failures would throttle everyone. Trust ONLY cloudflared's fixed
  # address on the compose `edge` network (the tunnel route targets
  # http://loomcycle-edge:8787): on loomnet any container, the agent-driven
  # browser sidecar included, could forge CF-Connecting-IP.
  trusted_proxies: ["10.253.253.2/32"]
  client_ip_header: CF-Connecting-IP
```

What `auth:manage` may not do, on any deployment (RFC FA §6.1.1):
- **Bootstrap a tenant twice.** Once a tenant has had a user, deleting users never makes it bootstrap-able again.
- **Touch a user above the floor** (an owner, or anyone in a group beyond the new-user groups), apart from the recovery switch above: no delete, disable, re-enable, demote, rename or credential removal.
- **Disable, re-enable or approve** anyone but a floor member.

`docker-compose.yaml`, on the `loomcycle` service:
- `LOOMCYCLE_PUBLIC_URL=https://app.loomcycle.cloud`. It does two jobs. P2 builds invite and reset links from it, and a password gate refuses to load without it. P0 also reads it to recognise the TLS terminator: the tunnel delivers plain HTTP, but a request whose Host is `app.loomcycle.cloud` counts as HTTPS, so the session cookie is `__Host-lc_session; Secure`. Leave the tunnel's Host header as it is (don't set `httpHostHeader`).

**The website's credential:** an admin mints one operator token with `scopes: ["auth:manage"]`, for example named `cloud-landing`. It goes into `.env.secure` as **`LOOMCYCLE_AUTH_MANAGE_TOKEN`**. It replaces `LOOMCYCLE_ADMIN_TOKEN` in the landing service's environment (phase 5). Rotate it like any operator token.

## Landing server (`landing/server.js`) — new routes

All the new routes follow the same rules:
- The Access JWT is verified, and the tenant is derived only from it.
- The existing `APP_ORIGIN` check applies to every POST, PATCH and DELETE.
- Responses carry `Cache-Control: no-store`.
- Upstream calls use `LOOMCYCLE_AUTH_MANAGE_TOKEN`, with fixed upstream paths, as the `proxyGet` allowlist does today.
- Nothing logs an invite or reset URL. Until it is redeemed, a link is a credential.

| Route | Does | Upstream |
|---|---|---|
| `POST /api/signup` | Creates the visitor's workspace, or re-enters it. **Empty tenant:** create a bootstrap owner invite (`access_mode: tenant`, `login` = e-mail, `return_to: https://loomcycle.cloud/?welcome=1`), then **redirect the browser straight to the invite link**. Redeeming it also binds the owner to `builtin:tenant-operator` in their tenant, so their app session can run the tenant's operator plane (RFC FA §6.1.2). The visitor sets a password at once, so no e-mail is needed. **Tenant has users, and the e-mail is a legacy subject with no password:** a migration invite that attaches to that subject (phase 4). **Otherwise:** `409 {signed_up: true, app: "https://app.loomcycle.cloud/ui/login"}`. | `GET /v1/_users?tenant=` (does the tenant have users?), `POST /v1/_authgate/cloud/invites` |
| `GET /api/team` | Lists the members, `{subject, display_name, login, access_mode, status}` | `GET /v1/_users?tenant=`, `GET /v1/_authgate/cloud/identities` |
| `POST /api/team/invites` `{email, display_name?}` | Invites a member. The member gets the floor (`isolated`, `[everyone, new-users]`), the API's own rule (D16). The response is the link plus an expiry, for the owner to copy. | `POST /v1/_authgate/cloud/invites` |
| `GET /api/team/invites`, `DELETE /api/team/invites/:id` | Lists pending invites (never the code), and revokes one | the matching routes |
| `POST /api/team/:subject/reset` | Issues a reset link for a member, and returns the link for the owner to send. An owner who forgot their own password signs in with Google and uses "Reset my password", which resets their own subject; that relies on `manager_may_recover_owners`. | `POST /v1/_authgate/cloud/resets` |
| `POST /api/team/:subject/disable`, `/enable` | Disables a member, which ends their sessions and retires their tokens, or re-enables them | `PATCH /v1/_users/{subject}` |
| `DELETE /api/team/:subject` | Removes a member through the lifecycle stages (disable at once, then unbind; erasure stays an operator action) | `DELETE /v1/_users/{subject}` |

**More rules for the team routes:**
- **Per-subject routes:** every `:subject` must exist in the owner's tenant. Check it with `GET /v1/_users/{subject}?tenant=` before acting. A cross-tenant id gets a `404`, the same as an unknown one.
- **Limits:** each tenant gets at most 20 invites per day and 50 pending invites. Each source address gets 10 signups per hour. These limits live in the landing, in memory, because there is one landing process. Move them to Redis if the landing ever scales out.
- **Removed:** `POST /api/mint` and its `LOOMCYCLE_ADMIN_TOKEN` (phase 5). `/api/whoami` stays, and gains `{signed_up}`.

## Website (`cloud-web/index.html`)

The current three-step console is: sign in, then the token vault, then the token shown once. It becomes:

1. **Sign in with Google.** This is the Cloudflare Access flow, unchanged.
2. **Create your workspace.** The button calls `POST /api/signup` and follows its redirect to the app's invite page, where the visitor sets a login and a password. A returning owner gets "Open app" (`app.loomcycle.cloud/ui/login`) and "Manage team".
3. **Team.** A member table with status, a reset action and a disable/enable action. An invite form takes an e-mail and returns a link with a copy button and its expiry. The page shows a list of pending invites with a revoke button, and a note that loomcycle sends no e-mail, so the owner sends the link themselves.

The vault and the `postMessage` handoff stay **read-only for one release**. Existing tokens still work in `token` mode, but the page offers no new minting. After that release the vault is removed.

**API tokens for SDK, CLI and MCP** are minted inside the app, from the Web UI's token page. That page is self-service, uses the person's own session, and calls `POST /v1/_users/{subject}/tokens`. Later, RFC FA P5's device login (`loomcycle login`) replaces pasting a token. The website mints nothing.

## Phases

| # | What | Done when |
|---|---|---|
| 0 | loomcycle P0 + P2 released, and the cloud runtime on that version | `/v1/_authgate/cloud/invites` answers on the cloud host |
| 1 | The runtime config above. Mint the `auth:manage` token into `.env.secure` (as `LOOMCYCLE_AUTH_MANAGE_TOKEN`), alongside the admin token for now. Set `LOOMCYCLE_PUBLIC_URL`. | `app.loomcycle.cloud/ui/login` shows both the token box and the password gate; the session cookie is `__Host-lc_session` |
| 2 | Landing `POST /api/signup` + `/api/team/*`, with a fake-runtime unit suite and an extended `smoke.mjs` | Smoke passes against a local runtime (`MINT_DEV_ALLOW_UNVERIFIED=1`) |
| 3 | `cloud-web` signup + Team UI; the vault becomes read-only | Manual: a fresh Google account → workspace → password → app; an owner invites a member, the member redeems, the owner resets and disables them |
| 4 | Migrate the existing tenants (below) | Every existing owner can log in with a password |
| 5 | Remove `/api/mint`, `LOOMCYCLE_ADMIN_TOKEN` from the landing and its compose env, and the vault UI. Retire the old minted tenant tokens on a published date. | `grep LOOMCYCLE_ADMIN_TOKEN cloud-deployment/` is empty, apart from the runtime's own `LOOMCYCLE_AUTH_TOKEN` |
| 6 | `auth.mode: strict`; drop the `token` gate from the config; `require_mfa: true` | The paste box is gone, and a hand-minted user token is refused on the UI |
| 7 | Later: an OIDC `google` gate on the app (RFC FA P1); owners link their Google identity explicitly (D14); the website can drop Cloudflare Access for its own OIDC login | Google sign-in on the app works for a linked user |

The release automation (`loomcycle-cloud-deploy` on Gitea) overwrites the host's `docker-compose.yaml` and `config/loomcycle.yaml` from this repo on each release tag. So phases 1, 5 and 6 ship as commits here and take effect at the next tag. `.env.secure` changes stay manual, on the host.

## Migrating existing tenants (phase 4)

Existing cloud tenants were created by `/api/mint`: a tenant `t_<hash>`, a subject equal to the owner's e-mail, and operator tokens with `substrate:tenant,runs:create`. Most of them probably have no `users` row, because the mint path never created one. Check this once on the host with an admin call before phase 4.

**Steps:**
1. When a legacy owner signs in on the website, `POST /api/signup` sees that the tenant has a subject (or tokens) but no password identity.
2. If no `users` row exists, the landing creates one: `POST /v1/_users` with `subject` = the e-mail and `access_mode: tenant`. It is the tenant's first user, so the bootstrap rule allows it.
3. The landing then creates an invite with `subject` = the e-mail. This is an invite that attaches to an existing user and keeps its subject, groups and data (RFC FA §6.1). The owner is above the floor, so this needs `manager_may_recover_owners: true`. The browser is redirected to that invite.
4. Their old tokens keep working until phase 5.

**Team members who were sharing a group token** have no subject of their own. The owner invites them as new members. Their old shared token stops at phase 5.

**Owners who never return before phase 5** keep their tenant data. They lose token access at phase 5, and get back in by signing up again, which takes the same migration path.

## Security notes

- **`auth:manage` is a cross-tenant credential.** It cannot read data, start runs or mint tokens. A bug in the landing that took a tenant from the request would still let one owner manage another tenant's users. That is why the tenant comes only from the verified Access identity, and a test asserts it: a request carrying another tenant's id or subject gets a `404`.
- **With `manager_may_recover_owners: true`, a leaked `LOOMCYCLE_AUTH_MANAGE_TOKEN` can take over any owner's account**, by issuing a reset link for them. It still cannot delete, disable or demote an owner, or re-bootstrap a tenant (RFC FA §6.1.1). The recovery events are audited with `recovery: true`, so alert on any that the landing did not initiate. Keep the token only in `.env.secure` and rotate it on any suspicion.
- **Invite and reset links are bearer-equivalent until redeemed.** They are single-use and short-lived, stored hashed by loomcycle, never logged by the landing, and shown only to the owner who asked for them.
- **The landing still has no host port.** It is reachable only through the tunnel, with `/api/*` behind Cloudflare Access. Compared with today, its blast radius goes from "anything, anywhere" to "user records".
- **Signup is a redirect, not an e-mail.** The person who proved the Google identity is the one who sets the password, in the same browser, within seconds. So no mailbox has to be trusted for the owner.

## Open questions (with recommendations)

1. **Promoting a member to co-owner.** Recommended: the website can't. `auth:manage` is held to the floor on a tenant that has users, so an owner promotes a member in the app's Users console, using their own session's reach. The website shows each member's role read-only.
2. **E-mail delivery of member invites.** Recommended: v1 is copy-a-link. A mailer (SMTP or a provider) is a separate later step in the landing; loomcycle stays mail-free.
3. **loomboard (`board.loomcycle.cloud`) sign-in after the vault goes.** Recommended: until device login (P5) or OIDC (P1) exists, the person mints a token on the app's token page and pastes it into loomboard. Revisit when the app can issue tokens itself.
4. **When to require TOTP.** Recommended: optional during phases 1–5, required at the strict switch (D3's strict default).
5. **Website access for members.** Recommended: none. The website is the owner's console. Members only ever use `app.loomcycle.cloud`.
