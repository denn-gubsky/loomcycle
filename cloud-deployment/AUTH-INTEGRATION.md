# loomcycle.cloud — moving sign-in to the password gate and the `auth:manage` API

## Shared tenant (2026-10-31)

The owner changed the model on 2026-10-31. **Everyone now lives in one tenant, `loomcycle-dev`**, instead of each person owning a `t_<hash>` tenant. The rest of this document describes the per-person design; it still holds when the landing runs with `LANDING_TENANT` empty, and its rules (Access-verified identity, `auth:manage` only, no logged links, whitelisted error codes) apply unchanged.

**Why.** loomcycle.cloud is a place to work together on the same documents, memory and agents. Per-person tenants kept people apart, and each new tenant needed an owner, a team console and a token vault the website had to run. One shared tenant needs none of that; what it needs is a door that an operator controls.

**The decisions:**
1. **One tenant**, `loomcycle-dev`. It already exists and has users. The owner gives those existing users passwords by hand in the app, not through the website.
2. **Arrival is self-signup.** A visitor who passed Cloudflare Access gets a new-user password-gate invite into `loomcycle-dev`, with the login pinned to their verified e-mail, and is redirected straight to it. After choosing a password they are **pending** until a tenant operator approves them in the app's Users console. The website never approves anyone.
3. **Tenant-wide reach for members.** The gate's provisioning floor is `access_mode: tenant`. The landing never sends `access_mode` or `groups` on an invite, so the floor alone decides.

**What changed:**
- **Landing (`LANDING_TENANT`, default `loomcycle-dev` in compose).** The tenant comes only from that setting. `POST /api/signup` revokes the visitor's own stale website invites (matched by the pinned login and the website's `return_to`, so an operator's invites are left alone) and issues a new-user invite with `{tenant, login, email, return_to}`. loomcycle's `login_taken` (logins are unique per gate, across every tenant) becomes `409 already_signed_up`. An e-mail that is already a user's subject in the tenant gets `409 account_exists` rather than a second account. `/api/team/*` and `/api/mint` answer `404`: in a tenant everyone shares, a website team console would let any visitor manage every member. The landing boots without `LOOMCYCLE_ADMIN_TOKEN`. `/api/whoami` reports `mode: "shared"` and the shared tenant, and never a derived `t_…` tenant.
- **Limits.** Every signup lands in the same tenant, so the tenant-wide caps apply to signups: 20 new invites a day for the tenant (a re-signup that only replaces the visitor's own invite gives its slot back, and refusals cost nothing), 50 live website invites at a time, and 3 signups a day per e-mail on top of the hourly per-source and per-e-mail limits, so one visitor cannot use up the day's budget.
- **Website.** The console offers **Request access**, **Open app** (for people who already signed up) and a plain **Open loomboard** link, and explains that an operator approves new accounts. The team console and the token vault (with its loomboard handoff) are hidden.
- **Runtime config (`config/loomcycle.yaml`, gate `cloud`).** `trusted: true` (loomcycle refuses a tenant-reach floor on an untrusted gate); `new_user: { access_mode: tenant, status: pending }`; `manager_may_recover_owners: false` (nobody owns a tenant through the website any more, and leaving it on would let a leaked `auth:manage` token take over any owner). Also a `loomboard` public OAuth client, so loomboard signs in against loomcycle instead of being handed a bearer from the vault.

**What keeps signup from being open.** Cloudflare Access decides who may ask; `status: pending` decides who may use the workspace. A user created by the website's invite reaches only `GET /v1/_me` until approved. That key needs a runtime with the pending-approval change, v1.130.0 or later. **An older runtime does not refuse it: unknown gate keys are dropped silently** (checked against main on 2026-10-31), and every sign-up would come up active with tenant-wide reach. So there are two locks: deploy the config only on that release, and the landing itself refuses shared-mode signup (`503 approval_unavailable`, logged once per version) while the runtime's `/healthz` version is below `LANDING_MIN_RUNTIME_VERSION` (default `1.130.0`; an unreadable version such as `devel` is refused too). It reads the version at startup and before each signup.

**Not built: password reset from the website.** loomcycle's `auth:manage` API finds a login only by subject (`GET …/identities?subject=`); nothing looks a user up by login. A reset for "the visitor's own account" would mean reading every user's identities on each request. People reset their password in the app instead (an operator issues a reset link from the Users console).

**Status:** IN PROGRESS, 2026-10-24. Phase 1 (runtime config, #2125), phases 2 and 3 (the landing's routes and the website, #2126) and phase 4 (owners on their e-mail subject, so legacy tenants keep their data) are **built**; the account flow stays behind `LOOMCYCLE_AUTH_MANAGE_TOKEN`, and without it the landing behaves as before. Phase 0 (a release with P0 + P2, deployed) and the host steps are pending; phases 5–7 are not started. Where the built code differs from this plan, the code follows loomcycle's API: see [What the build changed](#what-the-build-changed-phases-23).
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
      allowed_return_to: ["https://loomcycle.cloud/?welcome=1"]   # exact match: = the landing's SIGNUP_RETURN_TO
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
| `POST /api/signup` | Creates the visitor's workspace, or re-enters it. **Never-bootstrapped tenant (new, or legacy with tokens only):** create the owner with `POST /v1/_users?tenant=` (`subject` = the verified e-mail, `access_mode: tenant`), which also binds them to `builtin:tenant-operator` (RFC FA §6.1.2), then create an attach invite for that subject (`login` = e-mail, `return_to: https://loomcycle.cloud/?welcome=1`) and **redirect the browser straight to the invite link**. The visitor sets a password at once, so no e-mail is needed. **Tenant has users, and the e-mail is a legacy subject with no password:** a migration invite that attaches to that subject (phase 4). **Otherwise:** `409 {signed_up: true, app: "https://app.loomcycle.cloud/ui/login"}`. | `GET …/tenant-status`, `POST /v1/_users?tenant=`, `POST /v1/_authgate/cloud/invites` |
| `GET /api/team` | Lists the members, `{subject, display_name, login, access_mode, status}` | `GET /v1/_users?tenant=`, `GET /v1/_authgate/cloud/identities` |
| `POST /api/team/invites` `{email, display_name?}` | Invites a member. The member gets the floor (`isolated`, `[everyone, new-users]`), the API's own rule (D16). The response is the link plus an expiry, for the owner to copy. | `POST /v1/_authgate/cloud/invites` |
| `GET /api/team/invites`, `DELETE /api/team/invites/:id` | Lists pending invites (never the code), and revokes one | the matching routes |
| `POST /api/team/:subject/reset` | Issues a reset link for a member, and returns the link for the owner to send. An owner who forgot their own password signs in with Google and uses "Reset my password", which resets their own subject; that relies on `manager_may_recover_owners`. | `POST /v1/_authgate/cloud/resets` |
| `POST /api/team/:subject/disable`, `/enable` | Disables a member, which ends their sessions and retires their tokens, or re-enables them | `PATCH /v1/_users/{subject}` |
| `DELETE /api/team/:subject` | Removes a member through the lifecycle stages (disable at once, then unbind; erasure stays an operator action) | `DELETE /v1/_users/{subject}` |

**More rules for the team routes:**
- **Per-subject routes:** every `:subject` must exist in the owner's tenant. The landing checks it in the tenant's user list (`GET /v1/_users?tenant=`) before acting. A cross-tenant id gets a `404`, the same as an unknown one.
- **Limits:** per tenant, per verified identity and per source address, in the landing's memory (one landing process; move them to Redis if it ever scales out). The figures are under "What the build changed" → Limits.
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
| 1 | **Built** (#2125); host steps pending. The runtime config above. Mint the `auth:manage` token into `.env.secure` (as `LOOMCYCLE_AUTH_MANAGE_TOKEN`), alongside the admin token for now. Set `LOOMCYCLE_PUBLIC_URL`. | `app.loomcycle.cloud/ui/login` shows both the token box and the password gate; the session cookie is `__Host-lc_session` |
| 2 | **Built** (#2126). Landing `POST /api/signup` + `/api/team/*`, with a fake-runtime unit suite and an extended `smoke.mjs` | Smoke passes against a local runtime (`MINT_DEV_ALLOW_UNVERIFIED=1`) |
| 3 | **Built** (#2126). `cloud-web` signup + Team UI; the vault becomes read-only | Manual: a fresh Google account → workspace → password → app; an owner invites a member, the member redeems, the owner resets and disables them |
| 4 | **Built** (#2127). Migrate the existing tenants (below) | Every existing owner can log in with a password |
| 5 | Remove `/api/mint`, `LOOMCYCLE_ADMIN_TOKEN` from the landing and its compose env, and the vault UI. Retire the old minted tenant tokens on a published date. | `grep LOOMCYCLE_ADMIN_TOKEN cloud-deployment/` is empty, apart from the runtime's own `LOOMCYCLE_AUTH_TOKEN` |
| 6 | `auth.mode: strict`; drop the `token` gate from the config; `require_mfa: true` | The paste box is gone, and a hand-minted user token is refused on the UI |
| 7 | Later: an OIDC `google` gate on the app (RFC FA P1); owners link their Google identity explicitly (D14); the website can drop Cloudflare Access for its own OIDC login | Google sign-in on the app works for a linked user |

The release automation (`loomcycle-cloud-deploy` on Gitea) overwrites the host's `docker-compose.yaml` and `config/loomcycle.yaml` from this repo on each release tag. So phases 1, 5 and 6 ship as commits here and take effect at the next tag. `.env.secure` changes stay manual, on the host.

## Migrating existing tenants (phase 4) — built

Existing cloud tenants were created by `/api/mint`: a tenant `t_<hash>`, a subject equal to the owner's lowercased e-mail, operator tokens with `substrate:tenant,runs:create`, and user-scoped data under that subject, but no `users` row. loomcycle counts only `users` rows as "bootstrapped", and an `auth:manage` token can't see operator tokens, so to the landing such a tenant looks new.

That's why **every** owner, new or legacy, is created the same way. Every owner's subject is their verified e-mail:
1. `POST /api/signup` finds the tenant never bootstrapped and revokes any stale owner invite.
2. The landing creates the owner: `POST /v1/_users?tenant=…` with `{subject: <e-mail>, access_mode: tenant}`. loomcycle allows that for a tenant's first user, writes the bootstrap marker, and binds the owner to `builtin:tenant-operator` in the same transaction.
3. The landing creates an attach invite for that subject, with the login pinned to the e-mail, and redirects the browser to it. The owner is above the floor, so this relies on `manager_may_recover_owners: true`.
4. If step 3 fails, a retry finds the owner row without a login and takes the legacy path: it revokes stale attach invites and issues a new one. Signup is safe to repeat.

A legacy owner therefore lands on the subject their old tokens used, and their memory, documents and history stay theirs. Their old tokens keep working until phase 5.

**What it costs.** An owner's subject is their e-mail address, not a random id, so it can't be renamed without moving data. The cloud's tenant id is already derived from that e-mail, so the identity was fixed either way. Members invited later get loomcycle-made `cloud:<random>` subjects.

**Team members who were sharing a group token** have no subject of their own. The owner invites them as new members. Their old shared token stops at phase 5.

**Owners who never return before phase 5** keep their tenant data. They lose token access at phase 5, and get back in by signing up, which takes the same path.

## Security notes

- **`auth:manage` is a cross-tenant credential.** It cannot read data, start runs or mint tokens. A bug in the landing that took a tenant from the request would still let one owner manage another tenant's users. That is why the tenant comes only from the verified Access identity, and a test asserts it: a request carrying another tenant's id or subject gets a `404`.
- **With `manager_may_recover_owners: true`, a leaked `LOOMCYCLE_AUTH_MANAGE_TOKEN` can take over any owner's account**, by issuing a reset link for them. It still cannot delete, disable or demote an owner, or re-bootstrap a tenant (RFC FA §6.1.1). The recovery events are audited with `recovery: true`, so alert on any that the landing did not initiate. Keep the token only in `.env.secure` and rotate it on any suspicion.
- **Invite and reset links are bearer-equivalent until redeemed.** They are single-use and short-lived, stored hashed by loomcycle, never logged by the landing, and shown only to the owner who asked for them.
- **The landing still has no host port.** It is reachable only through the tunnel, with `/api/*` behind Cloudflare Access. Compared with today, its blast radius goes from "anything, anywhere" to "user records".
- **Signup is a redirect, not an e-mail.** The person who proved the Google identity is the one who sets the password, in the same browser, within seconds. So no mailbox has to be trusted for the owner.
- **Member invites are an existence oracle, bounded.** Logins are unique per gate across every tenant (D18), so inviting an address that already has an account anywhere fails where a fresh address gets a link: an owner learns that the address has a loomcycle.cloud account. The landing answers one generic `409 cannot_invite_address` for both causes (an account in another tenant, or one already here), which hides *which*, not *that*. It cannot be closed while the link is shown inline; e-mail delivery with an always-"sent" answer closes it (open question 2). Until then it is bounded: each refusal spends one of the tenant's 20 daily invites, and each verified Access identity may meet it 10 times a day.

## What the build changed (phases 2–3)

Building against loomcycle's actual `auth:manage` API (RFC FA P2) changed these details of the plan. The code follows the API.

**Phase 1 must also do these:**
- **`allowed_return_to` is an exact match**, query string included. The landing's `SIGNUP_RETURN_TO` (set on the host as `LANDING_SIGNUP_RETURN_TO`) defaults to `https://loomcycle.cloud/?welcome=1`, so the gate must list exactly that, not `https://loomcycle.cloud/`. Otherwise every signup fails, and the landing answers `502 invalid_return_to`.
- **The gate's label key is `label:`**, not `display:`.
- **Minting the `auth:manage` token needs `tenant_id` and `subject`.** `POST /v1/_operatortokendef {op: create, name: cloud-landing, tenant_id: <any>, subject: <any>, scopes: ["auth:manage"]}`. An `auth:manage` token ignores its own tenant.

**Routes and checks:**
- **The membership check reads the tenant's user list.** It calls `GET /v1/_users?tenant=` and looks for the subject there. `GET /v1/_users/{subject}` is not on the `auth:manage` route list; only `PATCH` and `DELETE` are.
- **Logins are fetched one user at a time.** `GET /v1/_authgate/{gate}/identities` takes `?subject=`, so `GET /api/team` makes one identities call per user.
- **Bootstrap is decided by `GET /v1/_authgate/{gate}/tenant-status`.** Its `bootstrapped` field means "ever had a user"; the users list doesn't answer that.
- **Owners have their e-mail as subject (phase 4); members have a loomcycle-made subject** (`cloud:<random>`). The landing identifies "you" by a gate login equal to the e-mail, or else by the subject equal to the e-mail.
- **`POST /api/team/invites` takes `{email}` only.** loomcycle's invite has no `display_name`. The login is pinned to the e-mail, so whoever opens the link can't choose another login (loomcycle answers `login_fixed`). Owner and attach invites pin it the same way.
- **`DELETE /api/team/:subject` is one step in loomcycle.** It retires the user's tokens, drops its logins and password, and deletes the user row at once. There is no staged "disable, then unbind". The data stays.
- **Answers:**
  - A visitor who is already signed up gets `409 {error: "already_signed_up", signed_up: true, app}`.
  - A signup whose e-mail is already a login elsewhere gets `409 cannot_sign_up`.
  - Upstream "not found" codes become one `404 not_found`.
  - `login_taken` and `credential_exists` never reach a client.

**Limits.** All are in memory, with one landing process:
- **Signups:** 10 an hour per source address and 10 an hour per verified e-mail. The source is `CF-Connecting-IP`, read only after the Access JWT has verified, since a container on the compose network can forge it. The e-mail key cannot be forged.
- **Invites:** 20 a day per tenant, plus 10 "address can't be invited" refusals a day per identity. A slot is reserved before the first upstream call. It comes back only on a runtime fault: unreachable, a 5xx, or an answer outside the contract. A refusal the caller could repeat keeps it spent, and so does a signup that has already revoked a stale invite.
- **Pending invites:** 50 live per tenant. The landing counts them from loomcycle's own list, under a per-tenant lock.

**Stale invites.** A live owner invite left from an earlier signup attempt is revoked and a new one issued. The plan's alternative was telling the visitor to find their earlier link, but that link was only ever in their address bar. A legacy owner's stale attach invite is replaced the same way.

**Phase 4: legacy tenants that hold only tokens.** loomcycle counts only `users` rows toward "bootstrapped", and an `auth:manage` token can't see operator tokens, so a legacy tenant looks new. Resolved by creating every owner under their e-mail subject (see "Migrating existing tenants"), rather than with an admin pre-step that would have left owners without the tenant-operator binding.

## Open questions (with recommendations)

1. **Promoting a member to co-owner.** Recommended: the website can't. `auth:manage` is held to the floor on a tenant that has users, so an owner promotes a member in the app's Users console, using their own session's reach. The website shows each member's role read-only.
2. **E-mail delivery of member invites.** Recommended: v1 is copy-a-link. A mailer (SMTP or a provider) is a separate later step in the landing; loomcycle stays mail-free.
3. **loomboard (`board.loomcycle.cloud`) sign-in after the vault goes.** Recommended: until device login (P5) or OIDC (P1) exists, the person mints a token on the app's token page and pastes it into loomboard. Revisit when the app can issue tokens itself.
4. **When to require TOTP.** Recommended: optional during phases 1–5, required at the strict switch (D3's strict default).
5. **Website access for members.** Recommended: none. The website is the owner's console. Members only ever use `app.loomcycle.cloud`.
