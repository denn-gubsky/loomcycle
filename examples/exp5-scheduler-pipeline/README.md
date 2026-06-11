# exp5 — scheduler-driven fan-out / fan-in news-digest pipeline

A real periodic agentic pipeline: every 5 minutes the **scheduler** fans out **5 RSS
collectors**; each picks AI/agentic items from its feed and writes per-item digests to
shared memory, then pings a channel. One minute later a **consolidator** runs, **waits**
for the collectors (native fan-in), dedups the cycle's items, writes a consolidated
digest, and pushes one **Telegram** message. It self-stops after **3 cycles**.

```
scheduler ──┬─▶ exp5-collect-hn        ┐
            ├─▶ exp5-collect-wired     │  5 collectors · "*/5 * * * *" · max_fires:3
            ├─▶ exp5-collect-engadget  ├─ each: Context op=time → cycle bucket;
            ├─▶ exp5-collect-ars       │       HTTP GET feed; pick 0..5 AI items →
            └─▶ exp5-collect-tc        ┘       Memory digest:<cycle>:<feed>:<n>;
                                               on_complete ─▶ ping exp5-pings (global)
                                                                     │
scheduler ────▶ exp5-consolidate   ("1-59/5 * * * *", :01 offset, max_fires:3)
     Channel op=await {exp5-pings, at_least, n=5, wait_ms=120000} ◀──┘   (5 pings OR timeout)
     → pick cycle with most feeds → dedup by URL → Memory consolidated:<cycle>
     → mcp__telegram__send_message (one message) → done
```

## What it demonstrates

The v0.25.0+ **"agentic-ensemble"** primitives, end-to-end:

| Primitive | Used for | Finding / RFC |
|---|---|---|
| **scheduler fan-out** (5 collector schedules) | one schedule = one agent/fire; 5 feeds = 5 defs | RFC E |
| **`Context op=time`** | agent clock → `cycle = floor(unix_ms/300000)` | F34 / RFC S |
| **`Channel op=await`** `{mode:at_least, n, wait_ms}` | native AT_LEAST_N fan-in barrier (returns on N pings **or** timeout) | F35 / RFC S |
| **schedule `max_fires`** | self-stop after exactly 3 cycles — no external watcher | F36 / RFC S |
| `on_complete: channel.publish` (scope-honoring) | each collector pings a **global** channel; `schedule_name` = distinct collector | F37 / RFC T |
| 3rd-party **stdio MCP** (Telegram) with `${ENV}` | the push leg; env interpolated at spawn | F33/F39 |

Routing: **Anthropic OAuth (primary) → deepseek-v4-pro (fallback)** via `tier: middle`.

> **Cadence note.** This is the **canonical 5-minute** form: collectors fire at `:00/:05/…`,
> the consolidator one minute later at `:01/:06/…` so it reads a settled cycle, all as
> **static `scheduled_runs`** — no manual driver step. (The sandbox live-run used an
> accelerated 1-minute cadence + a manually-spawned consolidator to finish in ~4 min; that's
> only needed when same-minute consolidators would straddle cycle buckets.) A full 3-cycle
> run here spans ~15 minutes.

## Prerequisites

1. **loomcycle ≥ v0.25.1** on PATH (or set `LOOMCYCLE_BIN`). v0.25.0 introduced the
   ensemble primitives; v0.25.1 fixed the scope-honoring `on_complete` hook (F37) this
   relies on for the **global** `exp5-pings` channel.
2. **A model provider** — either Anthropic OAuth (`loomcycle anthropic login`, kept
   enabled by `run.sh`) or `DEEPSEEK_API_KEY` in `.env.local`.
3. **A Telegram bot** — talk to @BotFather for the bot token; get your chat id (message
   the bot, then `https://api.telegram.org/bot<token>/getUpdates`).
4. **`python3`** (the bundled Telegram MCP is stdlib-only).
5. **Outbound network** to the 5 feed hosts (the `run.sh` HTTP allowlist lists them).

This example is **mostly self-contained** — the only external dependency is the Telegram
bot (without it, everything runs and consolidates; only the final send fails).

## Setup

Fill `.env.local` (run.sh creates it from the template on first launch):
`LOOMCYCLE_TELEGRAM_BOT_TOKEN`, `LOOMCYCLE_TELEGRAM_CHAT_ID` (+ `DEEPSEEK_API_KEY` or an
OAuth login). That's it.

## Run

> **`loomcycle validate` note:** tier-based config (for the OAuth→deepseek fallback) —
> `validate` reports `no provider resolved` because it doesn't probe providers. Not a
> config bug; verify by **running** and reading the boot `resolve probe:` + `scheduler:` lines.

```bash
cd examples/exp5-scheduler-pipeline
./run.sh        # first launch copies .env.local.example → .env.local; fill it in, re-run
```

Boot log should show: `scheduler: enabled (6 schedules)` · `mcp[telegram]: ready, 1/1
tools` · `resolve probe: anthropic-oauth-dev reachable` (and/or `deepseek reachable`).
Then just **wait** — at the next `:00/:05/:10…` boundary the 5 collectors fire; at `:01`
the consolidator runs and a Telegram message arrives. It repeats for 3 cycles, then the
schedules retire themselves.

## Verify

From a second terminal (the helper omits the bearer in dev open mode):

```bash
cd examples/exp5-scheduler-pipeline
BASE=http://127.0.0.1:8787

# collectors fired (expect 5 per cycle, up to 15 total over 3 cycles):
./loomcurl.sh "$BASE/v1/runs?user_id=exp5&limit=30" | python3 -m json.tool | grep -c '"agent": "exp5-collector"'

# the global fan-in channel saw the pings (distinct schedule_name = distinct collector):
./loomcurl.sh -X POST "$BASE/v1/_channels/exp5-pings/peek" -H 'Content-Type: application/json' -d '{"limit":20}'

# a consolidated digest exists for the cycle (scope=user, user_id=exp5):
./loomcurl.sh "$BASE/v1/_memory?scope=user&user_id=exp5&prefix=consolidated:"
```

Green run looks like:
- **Boot:** `scheduler: enabled (6 schedules)`, telegram `1/1 tools`.
- **Collectors ran:** ~5 `exp5-collector` runs per cycle; each `done.usage.provider`
  confirms the route (`anthropic-oauth-dev` or `deepseek`).
- **Fan-in:** the consolidator run's transcript shows `Channel op=await` →
  `{satisfied:true, total_messages:5, fired:["exp5-pings"]}` (or `timed_out:true` with a
  partial set if a feed was slow — it still consolidates what arrived).
- **Consolidated:** `consolidated:<cycle>` in memory; **independent re-derivation** holds —
  every `items[].url` traces back to a `digest:<cycle>:…` key, and `deduped ≤ total_seen`
  with no duplicate normalized URLs.
- **Telegram:** one `🗞️ AI digest — cycle … (k/5 sources, n_reached)` message arrives;
  the tool result is `{"ok": true, "message_id": <id>}`.
- **Self-stop:** after 3 cycles the log shows `reached max_fires=3 — retired def` ×6 and no
  further fires.

## Caveats / gotchas

- **HTTP allowlist is the #1 trap.** Scheduler-fired runs get **no per-run host allowlist**,
  so `LOOMCYCLE_HTTP_HOST_ALLOWLIST` (set by `run.sh`) is the *sole* egress policy — empty
  = every feed fetch refused. If collectors report `FEED_EMPTY`, check this list covers the
  feed host (incl. `www.`).
- **Scheduler must be enabled** — `run.sh` sets `LOOMCYCLE_SCHEDULER_ENABLED=1`; without it
  nothing fires.
- **Long-poll cap ≥ `wait_ms`.** The consolidator awaits up to 120 s; `run.sh` sets the cap
  to 180 s. A lower cap silently clamps the await.
- **Global channel cursor:** a `scope: global` channel has a single shared cursor and
  `await` advances it on read — so an admin `peek` *after* the consolidator's await may read
  0 (the messages remain at `global/""`). Fine for a single consumer; `peek` *before* the
  consolidator fires to see the pings.
- **Feeds change/rate-limit.** A flaky feed just yields fewer sources that cycle; the
  consolidator proceeds on timeout with whatever arrived (`exit=timeout`, `k<5`).
- **Secret hygiene:** the Telegram token is referenced by env name only and interpolated at
  spawn; loomcycle redacts secret values from persisted transcripts at rest (v0.23.4+).

## Teardown

Ctrl-C the server (or let `max_fires:3` retire all schedules on its own); delete `./data`
for a clean slate. No external resources to clean up beyond the Telegram bot (yours).

## Files

| File | Purpose |
|---|---|
| `loomcycle.yaml` | routing + collector/consolidator agents + `exp5-pings` channel + telegram MCP + 6 `scheduled_runs` |
| `run.sh` | launcher (scheduler on + HTTP feed-host allowlist + long-poll cap + OAuth) |
| `.env.local.example` | secret template (empty values) — Telegram token/chat id (+ provider) |
| `work/bin/telegram-bot-mcp.py` | minimal Telegram-sender MCP (stdlib-only python) |
| `loomcurl.sh` | token-safe loomcycle REST helper (omits the bearer in dev open mode) |
