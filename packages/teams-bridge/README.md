# @loomcycle/teams-bridge

A Microsoft Teams bot for a [loomcycle](https://github.com/denn-gubsky/loomcycle) interactive agent. People chat with the agent in Teams; each person is their own loomcycle user, with their own runs, memory, history, budget and rate limit.

- **Streamed answers** in 1:1 chats, with Teams' Stop button. Group chats and channels get one message per turn.
- **Steering:** a message sent while the agent is working reaches it mid-turn.
- **Questions and reviews** from the agent arrive as cards with buttons.
- **Commands:** `/new` (start over), `/stop` (stop the current answer), `/help`.
- **Restarts lose nothing:** the next message continues the conversation, with the agent's full history.

Design and decisions: RFC EN in the loomcycle document store (`/loomcycle/rfcs/rfc-en-teams-bridge`).

## Why a bridge, and not Copilot Studio or A2A

loomcycle's A2A server treats each message as a new run, so a Copilot Studio agent calling it loses the conversation, steering and streaming, and Copilot's own model writes the reply. Calling loomcycle's MCP server from Copilot makes loomcycle a tool rather than the agent. This bridge keeps the loomcycle agent in charge of the conversation and translates only the Teams side.

## How it works

```
Teams ──▶ Bot Connector ──HTTPS /api/messages──▶ teams-bridge ──private network──▶ loomcycle
```

- **Only the bridge is public.** loomcycle stays on your private network.
- **One interactive run per Teams conversation.** The bridge acknowledges each message at once and renders the reply in the background, so a long agent turn never times out a Teams request.
- **Per-user tokens.** loomcycle makes a token's subject authoritative over a request's `user_id`, so one service token cannot act for many users. The bridge holds a tenant-operator token and uses it for two things only: registering each Teams user (`teams-<entra object id>`) and minting that user a short-lived token. A user's token can start only the configured agent, may set no per-run override except `interactive`, and is rate-limited. Tokens live in memory only.

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
npx @loomcycle/teams-bridge
```

Configuration is by environment:

| Variable | Meaning | Default |
|---|---|---|
| `CLIENT_ID`, `CLIENT_SECRET`, `TENANT_ID` | The bot's Entra app | required |
| `PORT` | Listen port | `3978` |
| `LOOMCYCLE_BASE_URL` | loomcycle's private URL | required |
| `LOOMCYCLE_TOKEN` | The bridge's `substrate:tenant` token | required |
| `TEAMS_BRIDGE_AGENT` | The loomcycle agent every conversation runs | required |
| `TEAMS_BRIDGE_ALLOWED_TENANTS` | Comma list of Entra tenant ids allowed to use the bot | required |
| `TEAMS_BRIDGE_TOKEN_TTL_SECONDS` | Lifetime of a user's token | `43200` |
| `TEAMS_BRIDGE_RATE_PER_MINUTE`, `TEAMS_BRIDGE_RATE_BURST` | Runs a user may start per minute, and the burst | `6`, `10` |
| `TEAMS_BRIDGE_USER_ACCESS_MODE` | `isolated` (a user sees only their own data) or `tenant` | `isolated` |
| `TEAMS_BRIDGE_IDLE_MINUTES` | End a conversation's run after this long without a message; the next message continues it | `1440` |
| `TEAMS_BRIDGE_STORE` | `memory`, or `file:<path>` to survive restarts on one instance | `memory` |
| `TEAMS_BRIDGE_DEV` | `1` accepts unauthenticated requests, for local testing only | unset |

With `TEAMS_BRIDGE_STORE=memory`, a restart forgets which loomcycle session each Teams conversation was in, and the next message starts a fresh one. Use `file:<path>` (one instance) or your own `ConversationStore` (several).

### 4. The Teams app

1. In `appPackage/manifest.json`, replace the `${{...}}` placeholders. `BOT_ID` is the Entra app's client id.
2. Replace `color.png` (192×192) and `outline.png` (32×32, white on transparent) with your own icons if you like.
3. Zip the three files (no folder) and upload the zip in Teams (**Apps → Manage your apps → Upload an app**) or through the Teams admin center.

## Group chats and channels

- The conversation's run belongs to the person who started it. Its data stays in their scope, and its tools act with their token.
- Anyone in the chat may talk to the agent. A message from someone else reaches the agent marked with that person's name, so the agent knows who is speaking.
- Only the owner can answer the agent's question and review cards, and only the owner can `/new` or `/stop`.
- Teams sends the bot channel messages only when it is @mentioned.

## Security

- **Inbound:** the Teams SDK validates the Bot Connector's signature on every request. The bridge always sets the SDK's unauthenticated mode explicitly from `TEAMS_BRIDGE_DEV`, so a stray `DANGEROUSLY_ALLOW_UNAUTHENTICATED_REQUESTS` cannot turn validation off.
- **Tenant fence:** activities from Entra tenants not in `TEAMS_BRIDGE_ALLOWED_TENANTS` get one refusal and never reach loomcycle.
- **Least privilege:** the bridge's own token never runs agents. A user's token is limited to one agent, every per-run override except `interactive` is locked, it is rate-limited, and it expires.
- **Nothing at rest:** tokens are kept in memory. Logs carry conversation and run ids, never tokens, message text or agent output.
- **Output:** tool inputs and outputs are never sent to Teams; while the agent works, Teams sees the tool's name only.

## As a library

```ts
import { createTeamsBridge, loadConfig } from "@loomcycle/teams-bridge";

const bridge = createTeamsBridge(loadConfig(), { store: myRedisStore });
await bridge.start();
```

`ConversationStore` is four methods (`get`, `put`, `delete`, `list`); implement it over Redis or Postgres to run several instances.

## Development

```bash
npm ci
npm run typecheck
npm test                          # unit tests
./test/integration/run.sh         # builds loomcycle from this repo and runs against it (Go required)
```

The integration suite boots loomcycle on the mock provider, runs conversations through the bridge with real per-user tokens, and drives the real Teams SDK app with posted activities and a fake Bot Connector.

For a real Teams test, expose the bridge with a tunnel (for example Dev Tunnels), point the Azure Bot's messaging endpoint at it, and sideload the app package.
