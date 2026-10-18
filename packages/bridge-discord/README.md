# @loomcycle/bridge-discord

A Discord bot for a [loomcycle](https://github.com/denn-gubsky/loomcycle) interactive agent. People talk to the agent with slash commands. In a DM each person is their own loomcycle user, with their own runs, memory, history, budget and rate limit; a server channel is a loomcycle user of its own.

- **Slash commands:** `/ask prompt:<text>` talks to the agent, `/new` starts over, `/stop` stops the current answer, `/help` lists them.
- **Steering:** an `/ask` sent while the agent is working reaches it mid-turn.
- **Questions and reviews** from the agent arrive as messages with buttons.
- **Restarts lose nothing:** the next `/ask` continues the conversation, with the agent's full history.

It is built on `@loomcycle/bridge-core`, which holds the per-user tokens, conversations and cards every bridge shares. Design: RFC EQ (Bridges) in the loomcycle document store.

## What v1 does not do

The bridge takes Discord **interactions over HTTP** only. It does not connect to the Gateway, so it never sees ordinary messages: people use `/ask`, in a server or in a DM. Free-text messages need the Gateway and are a later phase.

## How it works

```
Discord ──HTTPS POST /discord/interactions──▶ bridge-discord ──private network──▶ loomcycle
        ◀──── interaction webhooks, REST ─────┘
```

- **Only the bridge is public.** loomcycle stays on your private network.
- **Every command is answered within Discord's 3 seconds** with a deferred response ("thinking…"). The agent's reply replaces it. A long reply continues in follow-up messages, 2,000 characters each.
- **Replies travel back through the interaction.** Its token lasts 15 minutes. A turn that ends later is posted with the bot token as an ordinary channel message, which needs the bot in that channel.
- **One interactive run per conversation:** per DM, and per server channel or thread.

## Setup

### 1. loomcycle

Run loomcycle with authentication on and a persistent store. Then:

1. Pick or define the agent the bot runs, for example the `chat` agent from `LOOMCYCLE_PRESETS=base,chat`.
2. Mint the bridge's token, a tenant operator in the tenant the Discord users will live in:

   ```bash
   loomcycle operator-token create --name discord-bridge --tenant acme --subject discord-bridge --scopes substrate:tenant
   ```

   Or declare it in YAML as a principal (`principals:` with `scopes: [substrate:tenant]` and a `token_env`).

### 2. The Discord application

1. In the [Developer Portal](https://discord.com/developers/applications), create an application.
2. **General Information:** copy the **Application ID** (`DISCORD_APPLICATION_ID`) and the **Public Key** (`DISCORD_PUBLIC_KEY`).
3. **Bot:** reset and copy the token (`DISCORD_BOT_TOKEN`). The bot needs no privileged intents.
4. **Installation:** keep **Guild Install** only. For the install link, add the scopes `bot` and `applications.commands`, and the bot permissions *Send Messages* and *View Channels*. Install it in your server.
5. Start the bridge (below), then set **Interactions Endpoint URL** to `https://<bridge-host>/discord/interactions`. Discord checks the URL with a PING when you save it, so the bridge must be running and reachable; it also probes the endpoint with bad signatures from time to time.

### 3. Run the bridge

```bash
npx @loomcycle/bridge-discord
```

With `DISCORD_BRIDGE_REGISTER_COMMANDS=1` the bridge registers its four commands when it starts:

- **in each allowed server, as that server's commands.** They take effect at once and show only where the bot answers.
- **globally, limited to the bot's DMs,** when DMs are on. Commands in DMs can only be global, and limiting them to DMs keeps a server from listing them twice.

Registration replaces the application's commands in those places. The bridge owns its application's commands; use a separate application for anything else. It does not remove global commands when you turn DMs off later.

### Configuration

The shared settings, as every bridge reads them:

| Variable | Meaning | Default |
|---|---|---|
| `LOOMCYCLE_BASE_URL` | loomcycle's private URL | required |
| `LOOMCYCLE_TOKEN` | The bridge's `substrate:tenant` token | required |
| `BRIDGE_AGENT` | The loomcycle agent every conversation runs | required |
| `BRIDGE_TOKEN_TTL_SECONDS` | Lifetime of a user's token | `43200` |
| `BRIDGE_RATE_PER_MINUTE`, `BRIDGE_RATE_BURST` | Runs a user may start per minute, and the burst | `6`, `10` |
| `BRIDGE_USER_ACCESS_MODE` | `isolated` (a user sees only their own data) or `tenant` | `isolated` |
| `BRIDGE_IDLE_MINUTES` | End a conversation's run after this long without a message; the next message continues it | `1440` |
| `BRIDGE_STORE` | `memory`, or `file:<path>` to survive restarts on one instance | `memory` |
| `BRIDGE_DEV` | `1` turns signature verification off, for local testing only | unset |
| `PORT` | Listen port | `8080` |

Discord's:

| Variable | Meaning | Default |
|---|---|---|
| `DISCORD_APPLICATION_ID` | The application's id | required |
| `DISCORD_PUBLIC_KEY` | The application's public key (64 hex characters) | required |
| `DISCORD_BOT_TOKEN` | The bot's token | required |
| `DISCORD_BRIDGE_ALLOWED_GUILDS` | Comma list of server ids the bot answers in | none |
| `DISCORD_BRIDGE_ALLOW_DMS` | `1` lets anyone who can reach the bot use it in a DM | off |
| `DISCORD_BRIDGE_TRUSTED_GUILDS` | Allowed servers whose members are trusted (see below) | none |
| `DISCORD_BRIDGE_REGISTER_COMMANDS` | `1` registers the commands at start | off |
| `DISCORD_BRIDGE_API_BASE_URL` | Discord's REST API (`https`, or `http` to localhost for a test fake) | `https://discord.com/api/v10` |

**The bridge is closed by default.** It refuses to start unless `DISCORD_BRIDGE_ALLOWED_GUILDS` or `DISCORD_BRIDGE_ALLOW_DMS=1` opens something: anyone on the internet can reach a Discord bot, and every message it answers runs on your model keys. `DISCORD_BRIDGE_ALLOW_DMS=1` means exactly that, so pair it with the rate limit and loomcycle's token budgets.

## Servers, channels and trust

- **A server channel runs as itself, never as one of its members.** It is its own loomcycle user (`discord-conv-<hash>`), with its own memory and history. A thread is its own conversation and shares its parent channel's memory.
- Every message reaches the agent labelled with the speaker's name.
- The person who started the conversation answers the agent's question and review cards, and only they can `/new` or `/stop`.
- **Trust.** A DM is trusted. In a server, what members write is **untrusted** unless the server is in `DISCORD_BRIDGE_TRUSTED_GUILDS`. The agent reads untrusted text as data, never as instructions, and it never reaches memory. An untrusted member can pick one of the agent's options, decline, approve or send back, but cannot type an answer to a question.
- A server not on the list, a DM when DMs are off, and a group DM each get a private reply saying the bot isn't available there, and never reach loomcycle.

## Questions and reviews

- A question with options shows one button per option (up to six) and **Decline**.
- A question without options asks to be answered with `/ask <your answer>`: a Discord message has no text field. The conversation owner's next `/ask` is taken as the answer.
- A review shows **Approve** and **Send back**. To send it back with notes, press **Send back**, then `/ask` the notes.
- A pressed card is replaced by its result, so it cannot be answered twice. A card from before a restart says it is no longer open.

## Limits

- **2,000 characters a message.** A longer answer arrives as several.
- **A turn longer than 15 minutes** finishes with the bot token. In a DM, or in a channel the bot cannot post in, that last message can be lost: the bridge logs the refusal. Another `/ask` gives the bridge a fresh interaction to reply through.
- **No streaming.** Without the Gateway there is no live message to grow; each turn arrives when it ends, with Discord's "thinking…" until then.
- A `/stop` sent while the agent waits for its next message has nothing to stop; its reply settles after ten seconds.

## Security

- **Inbound:** every request's `X-Signature-Ed25519` is checked over the timestamp and the raw body with the application's public key, before the body is parsed. A timestamp more than five minutes off, and an interaction id already seen, are refused. A failed check is a bare `401`.
- **Bots are dropped,** the bot itself included, before anything reaches loomcycle.
- **Least privilege:** the bridge's own token never runs agents. A user's token is limited to one agent, every per-run override except `interactive` is locked, it is rate-limited, and it expires.
- **Credentials stay in memory.** Interaction tokens are never written to the conversation store, so `BRIDGE_STORE=file:` holds no credential; after a restart, replies use the bot token until the next command. The bot token is sent only to Discord's REST API, never to an interaction webhook. Logs carry ids, never a token, message text or agent output.
- **Output:** agent text can be steered by whoever writes in a channel, so nothing the bridge posts can ping anyone (`allowed_mentions` parses nothing, so `@everyone` and role mentions are inert), shows a link preview (`SUPPRESS_EMBEDS` on every message: Discord would fetch the link to build one, with no click, and hand its query string to whoever runs that server), or hides a link's target (a masked link `[label](url)` is rewritten as `label (<url>)`). Tool inputs and outputs are never sent.

## As a library

```ts
import { createDiscordBridge, loadConfig } from "@loomcycle/bridge-discord";

const bridge = createDiscordBridge(loadConfig(), { store: myRedisStore });
await bridge.start();
```

`ConversationStore` is four methods (`get`, `put`, `delete`, `list`); implement it over Redis or Postgres to run several instances. Interaction tokens are per process, so with several instances a reply goes by the bot token unless the instance that took the command sends it.

## Development

```bash
npm ci
npm run typecheck
npm test                          # unit and HTTP tests
./test/integration/run.sh         # builds loomcycle from this repo and runs the CLI against it (Go required)
```

The integration run boots loomcycle on the mock provider, starts the built CLI against it with a fake Discord API on localhost, and posts signed interactions.

For a real Discord test, expose the bridge with a tunnel and set it as the application's Interactions Endpoint URL.
