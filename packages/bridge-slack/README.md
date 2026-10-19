# @loomcycle/bridge-slack

A Slack bot for a [loomcycle](https://github.com/denn-gubsky/loomcycle) interactive agent. People chat with the agent in Slack. In a DM each person is their own loomcycle user, with their own runs, memory, history, budget and rate limit; a channel or group DM is a loomcycle user of its own.

- **Streamed answers** in DMs, with Slack's native AI streaming. Channels get one message per turn, in the thread.
- **Steering:** a message sent while the agent is working reaches it mid-turn.
- **Questions and reviews** from the agent arrive as Block Kit cards with buttons.
- **Commands:** `/new` (start over), `/stop` (stop the current answer), `/help`, typed as the whole message.
- **Restarts lose nothing:** the next message continues the conversation, with the agent's full history.

Built on [`@loomcycle/bridge-core`](../bridge-core). Design: RFC EQ (Bridges) in the loomcycle document store.

## How it works

```
Slack ──HTTPS /slack/events, /slack/actions──▶ bridge-slack ──private network──▶ loomcycle
  ◀──────────── Web API (chat.postMessage, chat.*Stream) ────┘
```

- **Only the bridge is public.** loomcycle stays on your private network.
- **No Slack SDK.** The bridge checks Slack's request signature itself and calls the Web API with `fetch`.
- **Ack first.** Slack wants an answer within three seconds, so the bridge answers at once and works after. A retried event (`X-Slack-Retry-Num`) is recognised by its `event_id` and taken once.
- **Per-user tokens.** The bridge holds a tenant-operator token and uses it only to register each Slack user (`slack-<team id>-<user id>`) and mint them a short-lived token that can start only the configured agent.

## Setup

### 1. loomcycle

Run loomcycle with authentication on and a persistent store. Pick the agent the bot runs (for example `chat` from `LOOMCYCLE_PRESETS=base,chat`) and mint the bridge's token, a tenant operator in the tenant your Slack users will live in:

```bash
loomcycle operator-token create --name slack-bridge --tenant acme --subject slack-bridge --scopes substrate:tenant
```

### 2. The Slack app

1. At [api.slack.com/apps](https://api.slack.com/apps), **Create New App → From scratch**, in your workspace.
2. **OAuth & Permissions → Bot Token Scopes:** add `chat:write`, `app_mentions:read` and `im:history`. Add `users:read` too if you want the agent to see people's display names; without it, people are named by their Slack id.
3. **App Home:** turn on the **Messages Tab** and allow users to send messages from it, so people can DM the bot.
4. **Install to Workspace.** Copy the **Bot User OAuth Token** (`xoxb-…`) into `SLACK_BOT_TOKEN`.
5. **Basic Information → App Credentials:** copy the **Signing Secret** into `SLACK_SIGNING_SECRET`.
6. Start the bridge (step 3), reachable over HTTPS. Then:
   - **Event Subscriptions:** enable, set the Request URL to `https://<bridge-host>/slack/events` (Slack verifies it at once), and subscribe to the bot events `message.im` and `app_mention`. Don't subscribe to `message.channels`: mentions arrive as `app_mention`, and the bridge ignores plain channel messages.
   - **Interactivity & Shortcuts:** enable, and set the Request URL to `https://<bridge-host>/slack/actions`.
7. Invite the bot to the channels it should answer in (`/invite @your-bot`).

Your workspace's team id (`T…`) goes in `SLACK_BRIDGE_ALLOWED_TEAMS`. The bridge logs a warning at start if its own workspace is not in the list.

### 3. Run the bridge

```bash
npx @loomcycle/bridge-slack
```

Configuration is by environment. The shared variables (every bridge reads them):

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
| `BRIDGE_DEV` | `1` turns request signature checks off, for local testing only | unset |
| `PORT` | Listen port | `3000` |

Slack's own:

| Variable | Meaning | Default |
|---|---|---|
| `SLACK_BOT_TOKEN` | The Bot User OAuth Token (`xoxb-…`) | required |
| `SLACK_SIGNING_SECRET` | The app's signing secret | required |
| `SLACK_BRIDGE_ALLOWED_TEAMS` | Comma list of workspace (team) ids the bot answers in | required |
| `SLACK_BRIDGE_SHARED_TRUST` | How a channel's or group DM's speakers are marked: `trusted` or `untrusted` | `trusted` |
| `SLACK_BRIDGE_STREAMING` | `1` streams DM answers; `0` sends one message per turn | `1` |
| `SLACK_BRIDGE_API_BASE_URL` | Slack's Web API (https; plain http only to localhost, for a local fake) | `https://slack.com/api` |

## Conversations

- **A DM** is one conversation, one loomcycle user: the person.
- **A channel thread** is one conversation. Mention the bot (`@your-bot …`) to start or continue one; it answers in the thread. All threads of a channel share one loomcycle user (`slack-conv-<hash>`), so the channel's memory spans its threads while each thread is its own run. A group DM works the same way.
- In a shared conversation the agent sees each message labelled with the speaker's name. The person who started it answers the agent's cards, and only they can `/new` or `/stop`.
- **Streaming.** Slack streams a message only as a thread reply, so a streamed DM answer appears in the thread of the message it answers. If Slack refuses to start a stream (the workspace or the app's settings may not allow it), the bridge logs Slack's error code and sends the turn as ordinary messages. If Slack reports that a person stopped a streamed answer (`stopped_by_user`), the agent's turn is stopped too.
- **Cards.** A question with options shows one button per option and Decline; a free-text question asks for a reply in the conversation, which the bridge takes as the answer. A review shows Approve and Send back. A used card is replaced by its result.
- **Typing.** Slack has no typing indicator for a bot outside its assistant threads, so none is shown.

## Trust

- **DMs are trusted:** the person's text is part of the agent's prompt.
- **Channels and group DMs** are trusted by default, as the speakers are members of an allow-listed workspace. Set `SLACK_BRIDGE_SHARED_TRUST=untrusted` to send what they write as an untrusted block: the agent reads it as data, and it never reaches memory. An untrusted speaker may press a card's buttons but not type an answer.

## Security

- **Inbound:** every request's `X-Slack-Signature` is checked against the raw body before anything is parsed, compared in constant time, and refused when its timestamp is more than five minutes off. `url_verification` is answered only after the check.
- **Workspace fence:** the workspace a request is delivered for must be in `SLACK_BRIDGE_ALLOWED_TEAMS`. A refused conversation is told once and never reaches loomcycle.
- **Slack Connect guests:** a speaker whose own team is not allowed is a guest. In a shared channel or group DM a guest is admitted as **untrusted**, whatever `SLACK_BRIDGE_SHARED_TRUST` says: the run stays the conversation's, and the guest's text is data. A guest's DM is refused, since there they would run as themselves.
- **No loops:** edits, joins and other subtyped events, every bot's messages, and the bot's own (learned with `auth.test`) are dropped before anything else.
- **Outbound text is inert.** Nothing the agent writes can page a channel or disguise a link: every `<`, `&` and `]` it writes outside a code block is broken with an invisible word joiner, so no mention (`<!channel>`, `<!here>`, `<@U…>`), Slack link (`<url|label>`), Markdown link, image or entity can form, and a link shows its real target as text. A bare `@here`, `@channel` or `@everyone` is broken the same way. Inside a ```` ``` ```` code block, code stays copyable and only what could still form a link or mention is broken. Fields Slack reads as mrkdwn (a card's notification text, a private reply) are escaped instead.
- **No unfurls:** every message is posted with `unfurl_links` and `unfurl_media` off, so a link the agent writes is never fetched by Slack. Streamed messages are not unfurled by Slack. The bridge never builds an image or attachment from a URL.
- **Least privilege:** the bridge's own token never runs agents. A user's token is limited to one agent, its per-run overrides are locked except `interactive`, it is rate-limited, and it expires. A card's result goes only to Slack's own `response_url`, never with the bot token.
- **Nothing at rest:** tokens are kept in memory. Logs carry ids and Slack error codes, never tokens, signatures, message text or agent output.

## Limits

- A message is at most 10,000 characters; a longer answer is split. Slack takes 12,000 in `markdown_text`, and the margin covers its counting, the word joiners and the bridge's notices.
- A streamed answer is appended at most about once a second (`chat.appendStream` allows over 100 a minute), and continues in a new message past 10,000 characters.
- One bot token serves one workspace. To serve several, run one bridge per workspace.
- With `BRIDGE_STORE=memory` a restart forgets which loomcycle session each conversation was in; use `file:<path>` (one instance) or your own `ConversationStore` (several).
- A rate-limited Slack call is retried after Slack's `Retry-After`, up to three times and 30 seconds a wait.

## As a library

```ts
import { createSlackBridge, loadConfig } from "@loomcycle/bridge-slack";

const bridge = createSlackBridge(loadConfig(), { store: myRedisStore });
await bridge.start();
```

`createSlackAdapter(config).routes(...)` gives the two routes for a server of your own.

## Development

```bash
npm ci
npm run typecheck
npm test                          # unit and end-to-end tests, with fake Slack and loomcycle
./test/integration/run.sh         # builds loomcycle from this repo (Go required), boots it on the mock
                                  # provider, and drives the built bridge with signed Slack requests
```
