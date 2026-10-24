# loomcycle.cloud — landing page

Static landing page for `loomcycle.cloud`, deployed on Cloudflare Pages.
The runtime itself runs on a VPS behind a reverse proxy at `app.loomcycle.cloud`.

## Layout

```
index.html            the whole page (markup + styles + logic, no build step)
ds-base.js            loads the Modernist design-system stylesheet + bundle
_ds/modernist-*/      the design system (tokens, components) — styles.css is the only sheet
assets/               logo, favicon, loom + mascot clips
functions/api/health.js   Cloudflare Pages Function: same-origin proxy for /healthz
image-slot.js         drag-and-drop image placeholder helper (unused on the current page)
```

No bundler, no npm install. `index.html` opens directly in a browser.

## Deploy

Cloudflare Pages, framework preset **None**:

- Build command: *(none)*
- Output directory: `/`

`functions/` is picked up automatically by Pages Functions.

### Environment

| Variable | Default | Purpose |
| --- | --- | --- |
| `RUNTIME_URL` | `https://app.loomcycle.cloud` | Origin the health proxy fetches |

## The health strip

The top bar and the strip above the soak numbers show live runtime status:
`ok`, `version`, short `commit`, `uptime_seconds`, replica count.

They fetch **`/api/health`** — same-origin, served by
`functions/api/health.js`, which proxies `RUNTIME_URL + /healthz` server-side
and caches for 30s at the edge.

The proxy is required, not a convenience: loomcycle sends no
`Access-Control-Allow-Origin` and registers no `OPTIONS` handler, so a browser
on `loomcycle.cloud` cannot read a cross-origin response from
`app.loomcycle.cloud`. `/healthz` is the runtime's only unauthenticated route
(explicitly exempt from the auth middleware), so no bearer is involved.

If the probe fails the strip degrades to a static `loomcycle vX.Y.Z` with a grey
dot — it never advertises an outage. Change the endpoint via
`data-health-url` on `#statusbar`.

Bump the hard-coded fallback version in two places when releasing: the
`fail()` helper in `index.html` and the `data-live-version` spans.

## Accounts on the password gate (when the landing enables them)

The console has two modes, chosen by `GET /api/whoami`'s `auth_gate`. With
`auth_gate: false` (the landing has no `LOOMCYCLE_AUTH_MANAGE_TOKEN`) it is the
token-vault flow described below, unchanged. With `auth_gate: true`
(`cloud-deployment/AUTH-INTEGRATION.md`, phase 3):

1. **Sign in with Google** — Cloudflare Access, unchanged.
2. **Workspace.** A new visitor gets **Create your workspace**: `POST
   /api/signup` answers `{redirect}` and the page `location.replace()`s it — the
   app's `/ui/invite#<code>` page, where they choose a password (this site never
   sees one; `replace` keeps the code out of this site's history). The app sends
   them back to `/?welcome=1`. A returning owner (`signed_up: true`) gets **Open
   app** (`app.loomcycle.cloud/ui/login`) and **Manage team**.
3. **Team** (`data-pane="4"`). Members (login, role read-only, status) with
   Reset / Disable / Enable / Remove (destructive ones confirm first); an invite
   form that returns a single-use link with a copy button and its expiry —
   loomcycle sends no e-mail, the owner sends the link; pending invites with
   Revoke; **Reset my password** for the owner. Links appear in one box that is
   cleared when the view changes. Results are announced in `role="status"`
   regions.

The vault stays visible **read-only** in this mode: existing tokens still sign
in to the app and loomboard, but there is no Mint and no paste — API tokens are
made in the app. Everything the page shows comes from the landing's
`/api/team/*` JSON, rendered with `textContent` (no HTML from the server).

**Checks:** the landing's `npm test` includes `test/site.test.mjs`, which
compiles every inline script, checks the markup balances and that the panes and
controls the script wires up exist. The flow itself is verified by hand against
a local runtime (see `cloud-deployment/INSTALL.md` → "Enabling accounts").

## Sign-in flow — what is real and what is a prototype

`index.html` contains a working three-step console (nav → **Sign in**). This
section describes the token-vault mode (`auth_gate: false`).

**Real, implemented in the page:**

- **The token vault.** Tokens are encrypted with AES-256-GCM under a key
  generated `extractable: false` and persisted as a live `CryptoKey` in
  IndexedDB. Script can *use* the key on this origin but can never read its
  bytes, so an IndexedDB dump (or an XSS that reads storage) yields ciphertext
  only. Plaintext exists in memory just long enough for the handoff.
  Falls back to in-memory storage if IndexedDB is unavailable.
- **Naming, listing, removing, and the 5-token quota.**
- **Two token sources:** mint for your own tenant, or add a group token someone
  else minted for their tenant (shared documents / memory / Path tree).

**Stubbed — needs a backend:**

- Google OAuth. `[data-signin]` just advances the pane.
- Minting. `randTok()` generates a fake bearer. Replace with a Worker route
  that calls `POST /v1/_operatortokendef` (`op: "create"`) using the admin
  token from the environment, scoped `substrate:tenant runs:create` and bound
  to the caller's `(tenant, subject)`. Enforce the quota server-side too.
- The handoff. `handoff(token)` opens `app.loomcycle.cloud` and posts the
  bearer to it origin-pinned, retrying until the app acks
  `loomcycle.login.ok`. The Web UI needs ~10 lines on boot to listen:

  ```js
  addEventListener('message', (e) => {
    if (e.origin !== 'https://loomcycle.cloud') return;
    if (e.data?.type !== 'loomcycle.login') return;
    storeBearer(e.data.token);
    e.source.postMessage({ type: 'loomcycle.login.ok' }, e.origin);
  });
  ```

### Do not use `?token=` or `?login=`

A query param lands in browser history, the `Referer` header, and every
Cloudflare and nginx access log. loomcycle's own README rejects it. Ranked
alternatives:

1. **`postMessage`** — implemented here. Token never touches a URL.
2. **Fragment** (`/#login=<token>`) — fragments are not sent to the server, so
   proxy logs stay clean; still lands in history, so `history.replaceState` it
   away immediately. Good fallback when the popup is blocked.
3. **One-time exchange code** — Worker stores the token in KV under a random
   30-second single-use code; the redirect carries only the code; the app POSTs
   code → token. Zero exposure, works without a window handle.

### Worth adding

Gate the vault unlock behind a passkey using the WebAuthn **PRF** extension
(Chrome / Edge / Safari 18+): derive the wrapping key from the passkey rather
than generating it, and opening the vault genuinely requires Touch ID.

## Tweaks panel

Bottom-right tab, or press **T**. Page ground swatches, hero animation size,
hero frame on/off, caption vs bare, all-three loom clips vs first only, mascot
band show/hide. Persisted in `localStorage` under `loomcycle-cloud-tweaks`.

The default ground `#f6f7ed` is sampled from the loom clip's own background so
the hero sits on the page with no visible seam.

## Design system

Modernist: flat, Archivo throughout, zero corner radius, 2px rules, a single
accent `#ec3013` spent as a mark — the one place it runs as a field is the
closing poster. Take every color, font and spacing value from
`_ds/modernist-*/styles.css` variables; never hard-code a hex. Photographs go
through `.grayscale`; the brand mascot clips are identity, not photography, and
keep their color.

## Content provenance

Every technical claim on the page is lifted from the loomcycle repo —
`README.md` (soak numbers, providers, tools, substrate, memory facets, MCP,
budgets), `docs/CLAUDE-CODE.md` (the plugin's 6 commands / 4 skills / 2 hooks,
keychain storage, the `--upstream` single-runtime invariant), and
`internal/api/http/server.go` (the `/healthz` response shape). Keep it that way.
