# @loomcycle/bridge-core

The shared core of the [loomcycle](https://github.com/denn-gubsky/loomcycle) bridges. A bridge connects one chat platform to a loomcycle interactive agent. Each person who talks to it is their own loomcycle user, with their own runs, memory, history and rate limit. A group chat or channel is a loomcycle user of its own. A person has a token budget of their own only when loomcycle holds one for them: a per-user token limit, which a gate's `identity_map.new_user.budget` writes for every user it creates (below).

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
| `LOOMCYCLE_BASE_URL` | loomcycle's URL: `https://`, or `http://` to localhost | required |
| `BRIDGE_ALLOW_INSECURE_LOOMCYCLE` | `1` accepts an `http://` `LOOMCYCLE_BASE_URL` to another host, when the bridge reaches loomcycle over a private network (`http://loomcycle:8787` in a compose file). The bridge's tokens travel in clear on that link | unset |
| `LOOMCYCLE_TOKEN` | The bridge's `substrate:tenant` token | required |
| `BRIDGE_AGENT` | The loomcycle agent every conversation runs | required |
| `BRIDGE_TOKEN_TTL_SECONDS` | Lifetime of a user's token | `43200` |
| `BRIDGE_RATE_PER_MINUTE`, `BRIDGE_RATE_BURST` | Runs a user may start per minute, and the burst | `6`, `10` |
| `BRIDGE_USER_ACCESS_MODE` | `isolated` (a user sees only their own data) or `tenant` | `isolated` |
| `BRIDGE_IDLE_MINUTES` | End a conversation's run after this long without a message; the next message continues it | `1440` |
| `BRIDGE_STORE` | `memory`, or `file:<path>` to survive restarts on one instance | `memory` |
| `BRIDGE_DEV` | `1` turns the platform's request authentication off, for local testing only. The bridge refuses to start with it on a gate (`BRIDGE_GATE`) or bound beyond loopback | unset |
| `PORT` | Listen port | per platform |
| `BRIDGE_BIND` | The address the listener binds. Unset, every interface (a container publishing its port needs that); with `BRIDGE_DEV=1`, `127.0.0.1`, and only a loopback address is accepted | unset |

### As a loomcycle gate

A platform package that opts in (`loadSharedConfig(reader, { port, gate: true })`) runs the bridge as an external gate of loomcycle. The bridge then holds no loomcycle token. It signs who is talking with its own key, and loomcycle checks the signature, provisions the user by the gate's `identity_map`, and issues the short-lived token. What a new user may reach, and which groups they join, is set in loomcycle, not in the bridge.

| Variable | Meaning |
|---|---|
| `BRIDGE_GATE` | The gate's name in loomcycle's `auth.gates`. A person is the loomcycle user `<gate>:<id>`. |
| `BRIDGE_GATE_AUDIENCE` | loomcycle's `LOOMCYCLE_PUBLIC_URL`, which every signed assertion names. `https://`, or `http://` to localhost only. |
| `BRIDGE_GATE_KEY` | The bridge's Ed25519 or ES256 private key, as a JWK. loomcycle holds only the public half, pinned in the gate's `public_key`. |
| `BRIDGE_GATE_SECRET` | Instead of a key pair, an HMAC secret shared with a co-located loomcycle (`hmac_secret_env` in the gate). Set one of the two. |
| `BRIDGE_GATE_KEY_ID` | The `kid` sent in each assertion's header; defaults to the JWK's own `kid`. When both sides name one, loomcycle refuses a mismatch. Optional. |
| `BRIDGE_GATE_SYNC_HOURS` | How often the bridge reconciles its users with loomcycle (below), in hours; `0` turns it off. Default `6`. |
| `BRIDGE_RECONCILE_MAX_REVOKES` | The most people one reconciliation round may revoke; a round over it revokes nobody. Default: 10% of the people the bridge serves, at least five. |

`LOOMCYCLE_TOKEN` and `BRIDGE_USER_ACCESS_MODE` are refused in this mode: the first would be an unused credential left deployed, and the second is the gate's `identity_map.new_user.access_mode` in loomcycle. The matching loomcycle side:

```yaml
auth:
  gates:
    telegram:
      kind: external
      public_key: { kty: OKP, crv: Ed25519, x: "<the bridge key's public x>" }
      issued_token: { ttl: 12h, agents: [helper], overrides: [interactive] }
      identity_map:
        tenant: acme
        provision: auto
        new_user: { budget: { soft_limit: 500000, hard_limit: 1000000 } }   # tokens a month, per user
```

`issued_token` is required: it is the most any token loomcycle issues this gate's users may do, and whoever holds the gate's key can ask for exactly that much. A gate without it does not load; write `issued_token: unrestricted` to give the tokens their user's whole member reach. `new_user.budget` is written as each new user's token limit (soft warns, hard refuses new runs), so an open bot cannot be used to make uncapped users; a limit an operator set for the user first is kept.

**Leavers.** When the platform says a person is gone (they blocked the bot, were deactivated, uninstalled the app), the bridge revokes them in loomcycle: disabled at once, then removed by the gate's removal policy. A revoke loomcycle does not take is answered to the platform with an error, so it delivers the event again, and it is also kept beside the store (`<path>.revokes.json` with `BRIDGE_STORE=file:<path>`, else in memory) and retried by the bridge itself, from a minute up to hourly, after a restart too. A kept revoke is never dropped because loomcycle refuses it: loomcycle answers an unknown or already-removed person with success, so a refusal means a key, gate name or setting is wrong, and every retry that stays refused is logged at error. It is dropped only when that person writes to the bot again (they came back).

**Reconciliation.** A platform that can list its people (Slack) also reconciles, a few minutes after start and then every `BRIDGE_GATE_SYNC_HOURS`: each person the bridge served 1:1 (a person who is a conversation's run identity in its store) whom the platform no longer lists is revoked, the same way as above, and their conversations leave the store. It acts only on that evidence: a user the store does not know is never touched, so a lost or replaced store revokes nobody, and a shared conversation's own user is never revoked. People who only spoke in shared conversations are not loomcycle users of this gate (the conversation is) and are not part of it. A round that would revoke more than `BRIDGE_RECONCILE_MAX_REVOKES` (default: 10% of the people served, at least five) revokes nobody and says so at error every round: a member list read wrongly looks exactly like everyone leaving. Raise the setting to apply a real mass departure. With the memory store the bridge starts empty after every restart, so reconciliation has nothing to go on until people write again; use `BRIDGE_STORE=file:<path>`.

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
- **Bind where the core says.** A platform package that brings its own server (the Teams SDK does) binds it to `ctx.config.bindHost`, so dev mode stays on loopback.
- **Reconciliation.** A gate bridge whose platform can list its members sets `members()`: the platform ids of every person who is a member now. Return the whole list or throw; never a first page. The core revokes only people it served whom the list does not have, and an unreadable or empty list revokes nobody.
- **Help.** Set `platform.helpText` for a `/help` that lists your platform's own commands or setup. Build it on the exported `HELP_TEXT`, so the core's commands stay in the list:
  ```ts
  platform: { name: "myplatform", label: "MyPlatform", helpText: HELP_TEXT + "\n- **/link**: connect your account" },
  ```
  The core handles only `/new`, `/stop` and `/help`. Your route handles your own commands before calling the router. Unset, `/help` answers with `HELP_TEXT`.
- **Names.** A personal subject is `<platform>-<stable id>`. A shared conversation runs as `sharedSubject(platform, ...parts)`. Its parts name the memory: a channel's threads pass the channel alone.

Design: RFC EQ (Bridges), generalising RFC EN (the Teams bridge).
