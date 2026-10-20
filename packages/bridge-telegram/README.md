# @loomcycle/bridge-telegram

A Telegram bot for a [loomcycle](https://github.com/denn-gubsky/loomcycle) interactive agent. People message the bot in Telegram and talk to the agent. In a private chat each person is their own loomcycle user, with their own runs, memory, history, budget and rate limit. A group is a loomcycle user of its own.

- **Private chats and groups.** In a group the bot answers commands, mentions of it and replies to it. A forum topic is a conversation of its own.
- **Steering:** a message sent while the agent is working reaches it mid-turn.
- **Questions and reviews** from the agent arrive as messages with buttons.
- **Commands:** `/new` (start over), `/stop` (stop the current answer), `/help`. In a group, `/new@YourBot` works too.
- **Optional streaming** in private chats: the answer grows in one message as it is written.
- **Restarts lose nothing:** the next message continues the conversation, with the agent's full history.

It is built on [`@loomcycle/bridge-core`](../bridge-core), which holds everything the bridges share. Design: RFC EQ (Bridges) in the loomcycle document store.

## How it works

```
Telegram ──HTTPS webhook /telegram/webhook──▶ bridge-telegram ──private network──▶ loomcycle
                ◀──────── Bot API (sendMessage, …) ────────
```

- **Only the bridge is public.** loomcycle stays on your private network.
- **Webhook, not polling.** Telegram posts each update to the bridge with the secret you chose. The bridge checks the secret, answers at once and does the work after, so a long agent turn never times out a webhook.
- **Per-user tokens.** The bridge holds one tenant-operator token. It uses it only to register each Telegram user (`telegram-<user id>`) and mint them a short-lived token. That token can start only the configured agent, interactively, at a bounded rate.
- **No Telegram SDK.** The bridge calls the Bot API with `fetch`.

## Setup

### 1. loomcycle

Run loomcycle with authentication on and a persistent store. Then:

1. Pick or define the agent the bot runs, for example the `chat` agent from `LOOMCYCLE_PRESETS=base,chat`.
2. Mint the bridge's token, a tenant operator in the tenant the Telegram users will live in:

   ```bash
   loomcycle operator-token create --name telegram-bridge --tenant acme --subject telegram-bridge --scopes substrate:tenant
   ```

   Or declare it in YAML as a principal (`principals:` with `scopes: [substrate:tenant]` and a `token_env`).

### 2. The bot

1. In Telegram, open [@BotFather](https://t.me/BotFather), send `/newbot` and follow the prompts. It gives you the bot's token: that is `TELEGRAM_BOT_TOKEN`.
2. Keep **privacy mode on** (the default; `/setprivacy` in BotFather). In a group the bot then sees only commands, mentions of it and replies to it. The bridge applies the same rule itself, but with privacy mode off Telegram sends it every message in the group. Do not make the bot a group admin: an admin bot receives every message regardless.
3. Optionally `/setcommands` with `new`, `stop` and `help`, so Telegram suggests them.
4. Find the ids you want to allow. A user's id is a number; a group's is a negative number (`-100…` for a supergroup). The bridge logs the conversation (`tg:<chat id>`) of every message it refuses, which is one way to learn them.

### 3. Run the bridge

```bash
npx @loomcycle/bridge-telegram
```

It must be reachable from Telegram over HTTPS. Telegram sends webhooks only to ports 443, 80, 88 and 8443, so either listen on 8443 (the default) with TLS in front, or put it behind a reverse proxy on 443.

Then point Telegram at it. Either let the bridge do it at start (`TELEGRAM_BRIDGE_SET_WEBHOOK=1` with `TELEGRAM_BRIDGE_PUBLIC_URL`), or once by hand:

```bash
curl -s "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/setWebhook" \
  -d url=https://bot.example.com/telegram/webhook \
  -d secret_token="$TELEGRAM_BRIDGE_WEBHOOK_SECRET" \
  -d 'allowed_updates=["message","callback_query"]'
```

### Configuration

Shared by every bridge:

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
| `BRIDGE_DEV` | `1` accepts webhooks without the secret, for local testing only | unset |
| `PORT` | Listen port | `8443` |

Telegram's own:

| Variable | Meaning | Default |
|---|---|---|
| `TELEGRAM_BOT_TOKEN` | The bot's token from BotFather (secret) | required |
| `TELEGRAM_BRIDGE_WEBHOOK_SECRET` | The webhook's `secret_token` (secret): 1–256 characters of `A-Z`, `a-z`, `0-9`, `_`, `-`. For example `openssl rand -hex 32` | required |
| `TELEGRAM_BRIDGE_ALLOWED_USERS` | Comma list of user ids who may use the bot in a private chat | |
| `TELEGRAM_BRIDGE_ALLOWED_CHATS` | Comma list of group ids where the bot answers | |
| `TELEGRAM_BRIDGE_ALLOW_ANYONE` | `1` lets anyone on Telegram use the bot, in any chat | unset |
| `TELEGRAM_BRIDGE_GROUP_REQUIRE_USERS` | `1`: in a group, only people in `TELEGRAM_BRIDGE_ALLOWED_USERS` may use the bot, even with `ALLOW_ANYONE`. Needs that list | unset |
| `TELEGRAM_BRIDGE_TRUSTED_CHATS` | Comma list of group ids whose members are trusted (see Trust) | |
| `TELEGRAM_BRIDGE_STREAMING` | `1` streams answers in private chats | `0` |
| `TELEGRAM_BRIDGE_PUBLIC_URL` | The bridge's public `https://` base URL; the webhook is `<url>/telegram/webhook` | |
| `TELEGRAM_BRIDGE_SET_WEBHOOK` | `1` calls `setWebhook` at start, with the secret and the two update types the bridge handles | unset |
| `TELEGRAM_BRIDGE_API_BASE_URL` | The Bot API server, for a [local Bot API server](https://github.com/tdlib/telegram-bot-api). `http://` is accepted only to localhost | `https://api.telegram.org` |

**The bot is closed by default.** With no allow-list and no `TELEGRAM_BRIDGE_ALLOW_ANYONE=1`, the bridge refuses to start: a bot anyone can find must not run on your model keys unless you say so. A private chat passes when its user is in `TELEGRAM_BRIDGE_ALLOWED_USERS`; a group passes when the group is in `TELEGRAM_BRIDGE_ALLOWED_CHATS`, for anyone in it, or with `TELEGRAM_BRIDGE_GROUP_REQUIRE_USERS=1` only for its members who are in `TELEGRAM_BRIDGE_ALLOWED_USERS`. A refused chat hears why, once.

With `BRIDGE_STORE=memory`, a restart forgets which loomcycle session each chat was in, and the next message starts a fresh one. Use `file:<path>` (one instance) or your own `ConversationStore` (several).

## Groups

- **A group runs as itself, never as one of its members.** It is its own loomcycle user (`telegram-conv-<hash>`), with its own memory and history. A forum topic is its own conversation inside the group's memory.
- Every message reaches the agent labelled with the speaker's name.
- The person who started the conversation answers the agent's question and review buttons, and only they can `/new` or `/stop`.
- A group that becomes a supergroup gets a new id. If the old id was allowed, the bot says so once in the new chat, with the new id: "This group was upgraded, so its id changed to … Ask the operator to allow it." The new id is never allowed for you. Once you allow it, the group starts a fresh conversation and a fresh memory: the old group's memory stays under the old id.
- Anonymous group admins, and channels, cannot use the bot.

## Trust

- **Private chats are trusted:** what the person writes is part of the agent's prompt.
- **Groups are untrusted** unless listed in `TELEGRAM_BRIDGE_TRUSTED_CHATS`. Anyone a group's admins let in can write there, so their text reaches the agent as an untrusted block: the agent reads it as data, never as instructions, and it never reaches memory. In an untrusted group a speaker can press an option the agent offered, decline, approve or send back, but cannot type an answer: a question that wants free text says "Only the buttons can answer here." and can only be declined.

List a group as trusted only if you would let every member of it instruct the agent.

## Questions, reviews and streaming

- A question with options shows one button per option, then **Decline**. A question that wants free text shows **Decline**, then a second message, "Reply to this message with your answer.", that opens Telegram's reply box: reply to either message with the answer. In a group the box opens only for the person who started the conversation, by replying to their latest message; if the bridge restarted since they last wrote, it opens for everyone, and a reply from anyone else reaches the agent as an ordinary message. In an untrusted group there is no reply prompt, only **Decline** (see Trust).
- A review shows **Approve** and **Send back**. Telegram buttons carry no text, so a send-back has no written feedback.
- A pressed card is replaced by its result, so it cannot be answered twice.
- With `TELEGRAM_BRIDGE_STREAMING=1`, a private chat's answer appears as soon as it starts and is edited as it grows, at most once a second. Groups always get one message per turn. Telegram's own draft streaming (`sendMessageDraft`) is not used: it has no option to turn link previews off (see Security).

## Security

- **Inbound:** every webhook must carry the `X-Telegram-Bot-Api-Secret-Token` header with your secret, compared in constant time before the body is read. Anything else gets `401`. Redelivered updates are handled once.
- **Loops:** messages from bots, this one included, are dropped before anything else.
- **Least privilege:** the bridge's own token never runs agents. A user's token is limited to one agent, every per-run override except `interactive` is locked, it is rate-limited, and it expires.
- **No link previews.** Every message and edit turns link previews off. To build a preview Telegram fetches the link, so a URL an attacker steered the agent into writing would leak to the attacker's server without a click.
- **Output:** the agent's Markdown becomes Telegram HTML; every other `<`, `>` and `&` is escaped, so agent text cannot add markup of its own. No link is ever hidden behind a label: `[label](url)` is shown as `label (url)`, the URL as plain text that Telegram makes clickable, so agent text cannot show one address while pointing at another. Only `http` and `https` addresses count: no `tg://` link, so agent text cannot mention a user or open a chat. Tool inputs and outputs are never sent; while the agent works, a streamed chat sees the tool's name only.
- **Secrets stay out of logs:** the bot token is in every Bot API URL, so the bridge never logs a URL and its errors name the API method only. Logs carry chat and run ids, never tokens, the secret or message text.

## Limits

- Messages are split at 4,000 characters (Telegram's limit is 4,096).
- Text messages only. In a private chat a photo, file, voice note, sticker or other non-text message is answered with "I can only read text for now."; in a group it is ignored.
- Telegram limits how fast a bot may send. On a `429` the bridge waits the `retry_after` Telegram asks for, up to 30 seconds.

## As a library

```ts
import { createTelegramBridge, loadConfig } from "@loomcycle/bridge-telegram";

const bridge = createTelegramBridge(loadConfig(), { store: myRedisStore });
await bridge.start();
```

## Development

```bash
npm ci
npm run typecheck
npm test                          # unit tests and the whole webhook-to-reply path, with fakes
./test/integration/run.sh         # builds loomcycle from this repo and runs the built bridge against it (Go required)
```

The integration run boots loomcycle on the mock provider, starts the bridge CLI with a fake Bot API on localhost, and posts webhook updates to it.
