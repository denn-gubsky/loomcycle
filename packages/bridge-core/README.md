# @loomcycle/bridge-core

The shared core of the [loomcycle](https://github.com/denn-gubsky/loomcycle) bridges. A bridge connects one chat platform to a loomcycle interactive agent. Each person who talks to it is their own loomcycle user, with their own runs, memory, history, budget and rate limit. A group chat or channel is a loomcycle user of its own.

You install a platform package, not this one:

| Package | Platform |
|---|---|
| `@loomcycle/bridge-teams` | Microsoft Teams |
| `@loomcycle/bridge-slack` | Slack |
| `@loomcycle/bridge-telegram` | Telegram |
| `@loomcycle/bridge-discord` | Discord |

This package is for writing one more.

## What the core does

- **Per-user tokens.** The bridge holds one `substrate:tenant` token. It registers each person as a loomcycle user, `<platform>-<id>`. It mints each of them a short-lived token that can start only the configured agent, interactively, at a bounded rate. Tokens are kept in memory only; a restart mints fresh ones. A platform package can instead run the bridge as a loomcycle **gate** (below), which holds no loomcycle token at all.
- **Conversations.** One conversation drives one interactive run at a time. A second message steers the live run. After a restart, the next message continues the stored session in a new run, so nothing is replayed.
- **Turns.** Where the platform can stream, the answer shows as it is written. Elsewhere the bridge shows a typing indicator, then sends one message per turn, split at the platform's size limit.
- **Cards.** A question the agent asks, and an answer held for review, are sent as platform-neutral cards: a question with up to six options, or a review with approve and send-back. The platform package renders them. Each button has a short id, so a platform with a small button payload sends only the id.
- **Commands.** `/new`, `/stop`, `/help`, as the whole message. In a shared chat only the person who started the conversation may `/new`, `/stop` or answer a card. A platform package may give `/help` its own text.
- **Idle sweep.** A run nobody has written to for `BRIDGE_IDLE_MINUTES` is ended; its session is kept.
- **`GET /healthz`.** When the bridge last heard from the platform, sent to it, and called loomcycle. Times only.

## Trust

A platform package marks each speaker `trusted` or `untrusted`.

- **Trusted** text is part of the agent's prompt: a 1:1 chat, or a shared chat inside an allow-listed workspace.
- **Untrusted** text is sent as an untrusted block. The agent reads it as data, and it never reaches memory.

loomcycle's input to a live run takes only trusted text, so the core never steers a run with untrusted text. A parked run is ended and its session continued with the block. A run mid-turn gets the block when the turn ends.

An untrusted speaker may pick an option the agent offered, decline, approve or send back. They cannot type an answer or feedback, since those reach the agent as trusted text.

## Configuration

Every bridge reads these, then its platform's own:

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
| `BRIDGE_DEV` | `1` turns the platform's request authentication off, for local testing only | unset |
| `PORT` | Listen port | per platform |

### As a loomcycle gate

A platform package that opts in (`loadSharedConfig(reader, { port, gate: true })`) runs the bridge as an external gate of loomcycle. The bridge then holds no loomcycle token. It signs who is talking with its own key, and loomcycle checks the signature, provisions the user by the gate's `identity_map`, and issues the short-lived token. What a new user may reach, and which groups they join, is set in loomcycle, not in the bridge.

| Variable | Meaning |
|---|---|
| `BRIDGE_GATE` | The gate's name in loomcycle's `auth.gates`. A person is the loomcycle user `<gate>:<id>`. |
| `BRIDGE_GATE_AUDIENCE` | loomcycle's `LOOMCYCLE_PUBLIC_URL`, which every signed assertion names. `https://`, or `http://` to localhost only. |
| `BRIDGE_GATE_KEY` | The bridge's Ed25519 or ES256 private key, as a JWK. loomcycle holds only the public half, pinned in the gate's `public_key`. |
| `BRIDGE_GATE_SECRET` | Instead of a key pair, an HMAC secret shared with a co-located loomcycle (`hmac_secret_env` in the gate). Set one of the two. |
| `BRIDGE_GATE_KEY_ID` | The `kid` sent in each assertion's header; defaults to the JWK's own `kid`. When both sides name one, loomcycle refuses a mismatch. Optional. |

`LOOMCYCLE_TOKEN` and `BRIDGE_USER_ACCESS_MODE` are refused in this mode: the first would be an unused credential left deployed, and the second is the gate's `identity_map.new_user.access_mode` in loomcycle. The matching loomcycle side:

```yaml
auth:
  gates:
    telegram:
      kind: external
      public_key: { kty: OKP, crv: Ed25519, x: "<the bridge key's public x>" }
      issued_token: { ttl: 12h, agents: [helper], overrides: [interactive] }
      identity_map: { tenant: acme, provision: auto }
```

**Switching a bridge that ran with a token.** Its users are `<platform>-<id>`; as a gate the same people are `<gate>:<id>`. Set `identity_map.legacy_subject_prefix: <platform>-` on the gate, before the bridge is switched, and each person's first login links the user the bridge made, so their history, memory and documents stay theirs. The bridge's own store carries over too: a conversation stored under a `<platform>-<id>` subject moves to the gate's subject of the same identity the next time it is used, keeping its session and its owner. A conversation nobody writes to again keeps its old subject, and the bridge's idle sweep cannot end a run it left parked (it logs `could not end a stale run`) until the conversation is used again.

## Writing a platform package

A platform package supplies a `BridgeAdapter`:

```ts
import { createBridge, EnvReader, loadSharedConfig, type BridgeAdapter } from "@loomcycle/bridge-core";

const adapter: BridgeAdapter<MyRef> = {
  platform: { name: "myplatform", label: "MyPlatform" },
  capabilities: { streaming: "never", maxMessageChars: 4000 },
  port: { send: async (ref, activity) => { /* text, typing, or a CanonicalCard */ } },
  async start(ctx) {
    await ctx.serve({
      "POST /myplatform/events": async (req) => {
        // 1. Verify req.body (the raw bytes) and headers. Refuse first.
        // 2. Parse, drop the bot's own messages, apply the allow-list.
        // 3. ctx.router.onMessage(identified, ref, text)
        return { status: 200 };
      },
    });
  },
};
```

- **Verify before parsing.** Routes get the raw body. Compare secrets with `safeEqual`.
- **Drop the bot's own messages and other bots'** before calling the router. A bridge that answers itself loops on the operator's model keys.
- **Apply the allow-list in `identify`,** before anything touches loomcycle.
- **Help.** Set `platform.helpText` for a `/help` that lists your platform's own commands or setup. Build it on the exported `HELP_TEXT`, so the core's commands stay in the list:
  ```ts
  platform: { name: "myplatform", label: "MyPlatform", helpText: HELP_TEXT + "\n- **/link**: connect your account" },
  ```
  The core handles only `/new`, `/stop` and `/help`. Your route handles your own commands before calling the router. Unset, `/help` answers with `HELP_TEXT`.
- **Names.** A personal subject is `<platform>-<stable id>`. A shared conversation runs as `sharedSubject(platform, ...parts)`. Its parts name the memory: a channel's threads pass the channel alone.

Design: RFC EQ (Bridges), generalising RFC EN (the Teams bridge).
