# @loomcycle/bridge-telegram

A Telegram bot for a [loomcycle](https://github.com/denn-gubsky/loomcycle) interactive agent. People message the bot in Telegram and talk to the agent. In a private chat each person is their own loomcycle user, with their own runs, memory, history and rate limit, and a token budget of their own when loomcycle holds one for them (the gate's `new_user.budget`, below). A group is a loomcycle user of its own.

- **Private chats and groups.** In a group the bot answers commands, mentions of it and replies to it. A forum topic is a conversation of its own.
- **Steering:** a message sent while the agent is working reaches it mid-turn.
- **Questions and reviews** from the agent arrive as messages with buttons.
- **Commands:** `/new` (start over), `/stop` (stop the current answer), `/help`. In a group, `/new@YourBot` works too.
- **Optional streaming** in private chats: the answer grows in one message as it is written.
- **Optional voice messages** (`BRIDGE_VOICE=1`): send a voice note, hear the answer as one (see Voice messages). `/voice on|off|auto` sets when replies are spoken.
- **Restarts lose nothing:** the next message continues the conversation, with the agent's full history.

It is built on [`@loomcycle/bridge-core`](../bridge-core), which holds everything the bridges share. Design: RFC EQ (Bridges) in the loomcycle document store.

## How it works

```
Telegram ──HTTPS webhook /telegram/webhook──▶ bridge-telegram ──private network──▶ loomcycle
                ◀──────── Bot API (sendMessage, …) ────────
```

- **Only the bridge is public.** loomcycle stays on your private network.
- **Webhook, not polling.** Telegram posts each update to the bridge with the secret you chose. The bridge checks the secret, answers at once and does the work after, so a long agent turn never times out a webhook.
- **Per-user tokens.** The bridge holds one tenant-operator token. It uses it only to register each Telegram user (`telegram-<user id>`) and mint them a short-lived token. That token can start only the configured agent, interactively, at a bounded rate. As a loomcycle gate (below) the bridge holds no token at all.
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

Or run the container image, `denngubsky/loomcycle-bridge-telegram` (also `ghcr.io/denn-gubsky/loomcycle-bridge-telegram`), tagged with the package version. It takes the same environment variables. Mount a volume at `/data` and set `BRIDGE_STORE=file:/data/telegram.json` to keep each conversation's session across restarts:

```bash
docker run --env-file telegram-bridge.env -p 8443:8443 -v telegram-bridge-data:/data denngubsky/loomcycle-bridge-telegram:<version>
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
| `LOOMCYCLE_BASE_URL` | loomcycle's URL. From `@loomcycle/bridge-core` 0.4.0: `https://`, or `http://` to localhost; over a private network set `BRIDGE_ALLOW_INSECURE_LOOMCYCLE=1` | required |
| `LOOMCYCLE_TOKEN` | The bridge's `substrate:tenant` token | required, unless the bridge is a gate (below) |
| `BRIDGE_AGENT` | The loomcycle agent every conversation runs | required |
| `BRIDGE_TOKEN_TTL_SECONDS` | Lifetime of a user's token | `43200` |
| `BRIDGE_RATE_PER_MINUTE`, `BRIDGE_RATE_BURST` | Runs a user may start per minute, and the burst | `6`, `10` |
| `BRIDGE_USER_ACCESS_MODE` | `isolated` (a user sees only their own data) or `tenant` | `isolated` |
| `BRIDGE_IDLE_MINUTES` | End a conversation's run after this long without a message; the next message continues it | `1440` |
| `BRIDGE_STORE` | `memory`, or `file:<path>` to survive restarts on one instance | `memory` |
| `BRIDGE_DEV` | `1` accepts webhooks without the secret, for local testing only. From `@loomcycle/bridge-core` 0.4.0 it is refused on a gate, and the listener binds loopback only | unset |
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
| `TELEGRAM_BRIDGE_SET_WEBHOOK` | `1` calls `setWebhook` at start, with the secret and the update types the bridge handles | unset |
| `TELEGRAM_BRIDGE_API_BASE_URL` | The Bot API server, for a [local Bot API server](https://github.com/tdlib/telegram-bot-api). `http://` is accepted only to localhost | `https://api.telegram.org` |
| `TELEGRAM_BRIDGE_VOICE_AUDIO` | `1`: with `BRIDGE_VOICE=1`, also take audio files (usually music) as voice messages (see Voice messages) | unset |

**The bot is closed by default.** With no allow-list and no `TELEGRAM_BRIDGE_ALLOW_ANYONE=1`, the bridge refuses to start: a bot anyone can find must not run on your model keys unless you say so. A private chat passes when its user is in `TELEGRAM_BRIDGE_ALLOWED_USERS`; a group passes when the group is in `TELEGRAM_BRIDGE_ALLOWED_CHATS`, for anyone in it, or with `TELEGRAM_BRIDGE_GROUP_REQUIRE_USERS=1` only for its members who are in `TELEGRAM_BRIDGE_ALLOWED_USERS`. A refused chat hears why, once.

`TELEGRAM_BRIDGE_ALLOW_ANYONE=1` makes every Telegram account that writes a loomcycle user. Pair it with loomcycle's token budgets: a tenant budget, and, as a gate, `new_user.budget` (or `provision: known`, or `new_user.status: pending` to approve each person).

With `BRIDGE_STORE=memory`, a restart forgets which loomcycle session each chat was in, and the next message starts a fresh one. Use `file:<path>` (one instance) or your own `ConversationStore` (several).

## As a loomcycle gate

Since 0.3 the bridge can be a loomcycle gate instead of holding a token: it signs who is talking with its own key, and loomcycle provisions the user and issues the token. A stolen bridge can then only speak for Telegram users, never act as a tenant operator. Set `BRIDGE_GATE` and the bridge runs this way; without it, it runs with `LOOMCYCLE_TOKEN` as before.

1. Make a key pair. The private half goes to the bridge only:

   ```bash
   node -e 'const {privateKey,publicKey}=require("node:crypto").generateKeyPairSync("ed25519");
   console.log("BRIDGE_GATE_KEY=" + JSON.stringify(privateKey.export({format:"jwk"})));
   console.log("public_key: " + JSON.stringify(publicKey.export({format:"jwk"})))'
   ```

2. Register the gate in loomcycle, in the tenant the bridge used:

   ```yaml
   auth:
     gates:
       telegram:
         kind: external
         public_key: { kty: OKP, crv: Ed25519, x: "<the public x>" }
         issued_token: { ttl: 12h, agents: [chat], overrides: [interactive], rate: { per_minute: 6, burst: 10 } }
         identity_map:
           tenant: acme
           provision: auto
           legacy_subject_prefix: telegram-
           new_user: { budget: { soft_limit: 500000, hard_limit: 1000000 } }   # tokens a month, per user
   ```

   `issued_token` is required, or the gate does not load (write `issued_token: unrestricted` for tokens with the user's whole member reach). `new_user.budget` becomes each new user's token limit, so people the bot admits are capped without a limit written for each; leave it out and they have none.

   `legacy_subject_prefix: telegram-` keeps the people the bridge already served: each person's first message through the gate links the user the bridge made (`telegram-42`, a group's `telegram-conv-<hash>`), so their history, memory and documents stay theirs. Without it they would start again as new users `telegram:42`. New people are `telegram:<id>` either way.

3. Reconfigure the bridge: set `BRIDGE_GATE=telegram`, `BRIDGE_GATE_AUDIENCE` (loomcycle's `LOOMCYCLE_PUBLIC_URL`) and `BRIDGE_GATE_KEY`, and remove `LOOMCYCLE_TOKEN` and `BRIDGE_USER_ACCESS_MODE`, which a gate bridge refuses. Then retire the old operator token. The other `BRIDGE_GATE_*` settings are in `@loomcycle/bridge-core`'s README.

As a gate the bridge also tells loomcycle about groups and leavers. A person is in the loomcycle group `telegram:tg-chat-<id>` of each trusted chat they belong to (`telegram:tg-chat-<id>-admins` when they administer it), which an operator can bind permissions to; it takes `trusted: true` on the gate and an `identity_map.groups` block. A person who blocks the bot is revoked in loomcycle: disabled at once, then removed by the gate's removal policy. For that Telegram must send `my_chat_member` updates: `TELEGRAM_BRIDGE_SET_WEBHOOK=1` asks for them, or add it to `allowed_updates` when you set the webhook by hand. From `@loomcycle/bridge-core` 0.4.0 a revoke loomcycle does not take is also kept and retried by the bridge itself, so a loomcycle outage longer than the platform's redelivery no longer loses it. Telegram lists no members the bridge can read, so there is no reconciliation: a person removed without an event reaching the bridge keeps access until an operator disables them in loomcycle.

## Groups

- **A group runs as itself, never as one of its members.** It is its own loomcycle user (`telegram-conv-<hash>`), with its own memory and history. A forum topic is its own conversation inside the group's memory.
- Every message reaches the agent labelled with the speaker's name.
- The person who started the conversation answers the agent's question and review buttons, and only they can `/new`, `/stop` or `/voice on|off|auto`.
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

## Voice messages

With `BRIDGE_VOICE=1` a person can send the bot a voice note instead of typing, and hear the answer as a voice note. The bridge downloads the recording, has loomcycle transcribe it with the conversation's own token, and gives the agent the text, marked `(voice message)`, the way it gives typed text. When the turn ends the text answer goes out as always and, when the reply should be spoken, a voice note follows it, replying to the answer and captioned "AI voice". Nothing is kept: the audio lives in memory for one request. The shared settings (`BRIDGE_VOICE_REPLY`, `BRIDGE_VOICE_MAX_SECONDS`, `BRIDGE_VOICE_ECHO`, …) and the `/voice on|off|auto` command are in `@loomcycle/bridge-core`'s README.

- **What is taken:** voice notes and round video notes. Audio files (`message.audio`, which is mostly music) only with `TELEGRAM_BRIDGE_VOICE_AUDIO=1`. Photos, stickers, documents and the rest still get "I can only read text for now."
- **Where:** in a private chat, any voice note. In a group, only a voice note that **replies to one of the bot's messages**: a voice note cannot mention the bot, and a reply to it is what Telegram's privacy mode delivers. The allow-lists and trust rules are the same as for text; in an untrusted group the transcript reaches the agent as an untrusted block.
- **Language:** the sender's Telegram language is passed to loomcycle as a hint.
- **Download:** `getFile`, then the file from the Bot API's own file host (`TELEGRAM_BRIDGE_API_BASE_URL`) only. A redirect is refused, and a download larger than `BRIDGE_VOICE_MAX_BYTES` is cut off while it streams in (Telegram lets a bot download at most 20 MB). The bot token is in the file URL, so the URL is never logged and never in an error.

**What loomcycle needs.**

- The `speech:` and `transcription.stt:` engines.
- For spoken replies, `speech.encode` (an ffmpeg Opus command) with `speech.format: ogg`. Telegram shows OGG/Opus (or MP3) as a voice note; loomcycle's default `wav` is not, so without an encoder replies stay text.
- The speech surfaces on each user token. As a gate, add them to the gate's issued token, or the bridge stays on text:

  ```yaml
  issued_token: { ttl: 12h, agents: [chat], overrides: [interactive], surfaces: [speech, transcription], rate: { per_minute: 12, burst: 20 } }
  ```

  With `LOOMCYCLE_TOKEN`, the bridge asks for them when it mints each user's token.
- **Rate.** A voice turn takes three starts from the user's rate: transcribe, run, speak. Raise `BRIDGE_RATE_PER_MINUTE` (and the gate's `rate.per_minute`) to at least `12` with voice on.

A person whose privacy settings forbid voice notes from others gets the text answer only, and spoken replies stop in that chat until someone sends `/voice on`.

## Security

- **Inbound:** every webhook must carry the `X-Telegram-Bot-Api-Secret-Token` header with your secret, compared in constant time before the body is read. Anything else gets `401`. Redelivered updates are handled once.
- **Loops:** messages from bots, this one included, are dropped before anything else.
- **Least privilege:** the bridge's own token never runs agents. A user's token is limited to one agent, every per-run override except `interactive` is locked, it is rate-limited, and it expires.
- **No link previews.** Every message and edit turns link previews off. To build a preview Telegram fetches the link, so a URL an attacker steered the agent into writing would leak to the attacker's server without a click.
- **Output:** the agent's Markdown becomes Telegram HTML; every other `<`, `>` and `&` is escaped, so agent text cannot add markup of its own. No link is ever hidden behind a label: `[label](url)` is shown as `label (url)`, the URL as plain text that Telegram makes clickable, so agent text cannot show one address while pointing at another. Only `http` and `https` addresses count: no `tg://` link, so agent text cannot mention a user or open a chat. Tool inputs and outputs are never sent; while the agent works, a streamed chat sees the tool's name only.
- **Secrets stay out of logs:** the bot token is in every Bot API URL, so the bridge never logs a URL and its errors name the API method only. Logs carry chat and run ids, never tokens, the secret or message text.

## Limits

- Messages are split at 4,000 characters (Telegram's limit is 4,096).
- Text, and voice notes with `BRIDGE_VOICE=1`. In a private chat a photo, file, sticker or other non-text message (and a voice note with voice off) is answered with "I can only read text for now."; in a group it is ignored.
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
