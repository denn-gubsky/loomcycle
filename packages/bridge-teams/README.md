# @loomcycle/bridge-teams

A Microsoft Teams bot for a [loomcycle](https://github.com/denn-gubsky/loomcycle) interactive agent. People chat with the agent in Teams. In a 1:1 chat each person is their own loomcycle user, with their own runs, memory, history, budget and rate limit; a group chat or channel is a loomcycle user of its own.

- **Streamed answers** in 1:1 chats, with Teams' Stop button. Group chats and channels get one message per turn.
- **Steering:** a message sent while the agent is working reaches it mid-turn.
- **Questions and reviews** from the agent arrive as cards with buttons.
- **Commands:** `/new` (start over), `/stop` (stop the current answer), `/help`.
- **Restarts lose nothing:** the next message continues the conversation, with the agent's full history.

Built on [`@loomcycle/bridge-core`](../bridge-core), the core every loomcycle bridge shares (Slack, Telegram, Discord). Design and decisions: RFC EN (`/loomcycle/rfcs/rfc-en-teams-bridge`) and RFC EQ (`/loomcycle/rfcs/bridges`) in the loomcycle document store.

## Why a bridge, and not Copilot Studio or A2A

loomcycle's A2A server treats each message as a new run, so a Copilot Studio agent calling it loses the conversation, steering and streaming, and Copilot's own model writes the reply. Calling loomcycle's MCP server from Copilot makes loomcycle a tool rather than the agent. This bridge keeps the loomcycle agent in charge of the conversation and translates only the Teams side.

## How it works

```
Teams ──▶ Bot Connector ──HTTPS /api/messages──▶ teams-bridge ──private network──▶ loomcycle
```

- **Only the bridge is public.** loomcycle stays on your private network.
- **One interactive run per Teams conversation.** The bridge acknowledges each message at once and renders the reply in the background, so a long agent turn never times out a Teams request.
- **Per-user tokens.** loomcycle makes a token's subject authoritative over a request's `user_id`, so one service token cannot act for many users. The bridge holds a tenant-operator token and uses it for two things only: registering each Teams user (`teams-<entra object id>`) and minting that user a short-lived token. A user's token can start only the configured agent, may set no per-run override except `interactive`, and is rate-limited. Tokens live in memory only. As a loomcycle gate (below) the bridge holds no token at all.

## Setup

### 1. loomcycle

Run loomcycle with authentication on and a persistent store. Then:

1. Pick or define the agent the bot runs, for example the `chat` agent from `LOOMCYCLE_PRESETS=base,chat`.
2. Mint the bridge's token, a tenant operator in the tenant the Teams users will live in:

   ```bash
   loomcycle operator-token create --name teams-bridge --tenant acme --subject teams-bridge --scopes substrate:tenant
   ```

   Or declare it in YAML as a principal (`principals:` with `scopes: [substrate:tenant]` and a `token_env`).

### 2. Microsoft Entra and Azure Bot

1. Register a **single-tenant** app in Microsoft Entra ID and create a client secret.
2. Create an **Azure Bot** resource using that app. Set its messaging endpoint to `https://<bridge-host>/api/messages` and enable the **Microsoft Teams** channel.

### 3. Run the bridge

```bash
npx @loomcycle/bridge-teams
```

Or run the container image, `denngubsky/loomcycle-bridge-teams` (also `ghcr.io/denn-gubsky/loomcycle-bridge-teams`), tagged with the package version. It takes the same environment variables. Mount a volume at `/data` and set `BRIDGE_STORE=file:/data/teams.json` to keep each conversation's session across restarts:

```bash
docker run --env-file teams-bridge.env -p 3978:3978 -v teams-bridge-data:/data denngubsky/loomcycle-bridge-teams:<version>
```

Configuration is by environment:

| Variable | Meaning | Default |
|---|---|---|
| `CLIENT_ID`, `CLIENT_SECRET`, `TENANT_ID` | The bot's Entra app | required |
| `PORT` | Listen port | `3978` |
| `LOOMCYCLE_BASE_URL` | loomcycle's private URL | required |
| `LOOMCYCLE_TOKEN` | The bridge's `substrate:tenant` token | required, unless the bridge is a gate (below) |
| `BRIDGE_AGENT` | The loomcycle agent every conversation runs | required |
| `TEAMS_BRIDGE_ALLOWED_TENANTS` | Comma list of Entra tenant ids allowed to use the bot | required |
| `BRIDGE_TOKEN_TTL_SECONDS` | Lifetime of a user's token | `43200` |
| `BRIDGE_RATE_PER_MINUTE`, `BRIDGE_RATE_BURST` | Runs a user may start per minute, and the burst | `6`, `10` |
| `BRIDGE_USER_ACCESS_MODE` | `isolated` (a user sees only their own data) or `tenant` | `isolated` |
| `BRIDGE_IDLE_MINUTES` | End a conversation's run after this long without a message; the next message continues it | `1440` |
| `BRIDGE_STORE` | `memory`, or `file:<path>` to survive restarts on one instance | `memory` |
| `BRIDGE_DEV` | `1` accepts unauthenticated requests, for local testing only | unset |

With `BRIDGE_STORE=memory`, a restart forgets which loomcycle session each Teams conversation was in, and the next message starts a fresh one. Use `file:<path>` (one instance) or your own `ConversationStore` (several).

### 4. The Teams app

1. In `appPackage/manifest.json`, replace the `${{...}}` placeholders. `BOT_ID` is the Entra app's client id.
2. Replace `color.png` (192×192) and `outline.png` (32×32, white on transparent) with your own icons if you like.
3. Zip the three files (no folder) and upload the zip in Teams (**Apps → Manage your apps → Upload an app**) or through the Teams admin center.

## As a loomcycle gate

Since 0.3 the bridge can be a loomcycle gate instead of holding a token: it signs who is talking with its own key, and loomcycle provisions the user and issues the token. A stolen bridge can then only speak for Teams users, never act as a tenant operator. Set `BRIDGE_GATE` and the bridge runs this way; without it, it runs with `LOOMCYCLE_TOKEN` as before.

1. Make a key pair. The private half goes to the bridge only:

   ```bash
   node -e 'const {privateKey,publicKey}=require("node:crypto").generateKeyPairSync("ed25519");
   console.log("BRIDGE_GATE_KEY=" + JSON.stringify(privateKey.export({format:"jwk"})));
   console.log("public_key: " + JSON.stringify(publicKey.export({format:"jwk"})))'
   ```

2. Register the gate in loomcycle, in the tenant the bridge used, **before** you switch the bridge:

   ```yaml
   auth:
     gates:
       teams:
         kind: external
         public_key: { kty: OKP, crv: Ed25519, x: "<the public x>" }
         issued_token: { ttl: 12h, agents: [chat], overrides: [interactive], rate: { per_minute: 6, burst: 10 } }
         identity_map: { tenant: acme, provision: auto, legacy_subject_prefix: teams- }
   ```

   `legacy_subject_prefix: teams-` keeps the people the bridge already served: each person's first message through the gate links the user the bridge made (`teams-<entra object id>`, a group chat's or channel's `teams-conv-<hash>`), so their history, memory and documents stay theirs. Without it they would start again as new users `teams:<entra object id>`. New people are `teams:<entra object id>` either way.

3. Reconfigure the bridge: set `BRIDGE_GATE=teams`, `BRIDGE_GATE_AUDIENCE` (loomcycle's `LOOMCYCLE_PUBLIC_URL`) and `BRIDGE_GATE_KEY`, and remove `LOOMCYCLE_TOKEN` and `BRIDGE_USER_ACCESS_MODE`, which a gate bridge refuses. Then retire the old operator token. The other `BRIDGE_GATE_*` settings are in `@loomcycle/bridge-core`'s README.

   With `BRIDGE_STORE=file:<path>`, start the gate bridge on a new, empty store file: the old one remembers each conversation under its token-mode subject, which a gate cannot speak for. Only the session pointer is lost; the next message starts a fresh session as the same loomcycle user.

As a gate the bridge also tells loomcycle about groups and leavers. Every person, group chat and channel is in the loomcycle group `teams:teams-tenant-<entra tenant id>` of its organisation, which an operator can bind permissions to; it takes `trusted: true` on the gate and an `identity_map.groups` block. (A channel gets no team group: the bridge keeps no team id.) A person who uninstalls the app from their 1:1 chat is revoked in loomcycle: disabled at once, then removed by the gate's removal policy. Until loomcycle has taken the revoke the bridge answers Teams with an error, so the Bot Connector delivers the uninstall again.

## Group chats and channels

- **A shared chat runs as itself, never as one of its members.** A group chat, or a channel, is its own loomcycle user (`teams-conv-<hash>`), with its own memory and history. Anyone in it may talk to the agent, so no single person's private data or tools are reachable from it. A channel's threads share the channel's identity; each thread is its own run.
- Every message reaches the agent labelled with the speaker's name, so it knows who is talking.
- The person who started the conversation answers the agent's question and review cards, and only they can `/new` or `/stop`.
- Teams sends the bot channel messages only when it is @mentioned.

## Security

- **Inbound:** the Teams SDK validates the Bot Connector's signature on every request. The bridge always sets the SDK's unauthenticated mode explicitly from `BRIDGE_DEV`, so a stray `DANGEROUSLY_ALLOW_UNAUTHENTICATED_REQUESTS` cannot turn validation off.
- **Tenant fence:** activities from Entra tenants not in `TEAMS_BRIDGE_ALLOWED_TENANTS` get one refusal and never reach loomcycle.
- **Least privilege:** the bridge's own token never runs agents. A user's token is limited to one agent, every per-run override except `interactive` is locked, it is rate-limited, and it expires.
- **Nothing at rest:** tokens are kept in memory. Logs carry conversation and run ids, never tokens, message text or agent output.
- **Output:** tool inputs and outputs are never sent to Teams; while the agent works, Teams sees the tool's name only.

## As a library

```ts
import { createTeamsBridge, loadConfig } from "@loomcycle/bridge-teams";

const bridge = createTeamsBridge(loadConfig(), { store: myRedisStore });
await bridge.start();
```

`ConversationStore` (from `@loomcycle/bridge-core`) is four methods (`get`, `put`, `delete`, `list`); implement it over Redis or Postgres to run several instances.

Unlike the webhook bridges, the Teams bridge serves no `GET /healthz`: the Teams SDK's server registers POST routes only.

## Development

```bash
(cd ../bridge-core && npm ci && npm run build)   # the core, depended on by path while unpublished
npm ci
npm run typecheck
npm test                          # unit tests
./test/integration/run.sh         # builds loomcycle from this repo and runs against it (Go required)
```

The integration suite boots loomcycle on the mock provider, runs conversations through the bridge with real per-user tokens, and drives the real Teams SDK app with posted activities and a fake Bot Connector.

For a real Teams test, expose the bridge with a tunnel (for example Dev Tunnels), point the Azure Bot's messaging endpoint at it, and sideload the app package.
