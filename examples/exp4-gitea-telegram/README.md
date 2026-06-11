# exp4 — Gitea webhook dev loop + 3rd-party MCP + Telegram

A realistic closed-loop dev workflow:

```
publish task → exp4-tasks → CODER (writes code, pushes a branch, opens a PR via gitea-mcp)
   └▶ Gitea `pull_request` webhook ─▶ /v1/_webhooks/pr-opened ─▶ REVIEWER (reviews + squash-merges via gitea-mcp)
        └▶ Gitea merge/review webhook ─▶ /v1/_webhooks/review-done ─▶ ADVISOR ─▶ Telegram "✅ merged"
```

## What it demonstrates
- **Inbound webhooks** with HMAC verification (`X-Hub-Signature-256`), spawning an
  agent with the signed event as its prompt (`payload_mapping: {goal: "$"}`).
- A real **3rd-party stdio MCP** — `gitea-mcp` (53 tools: PR create/review/merge/read).
- A minimal **Telegram-sender MCP** (bundled, stdlib-only python).
- A **channel-driven multi-agent** dev loop (coder ↔ reviewer ↔ advisor).
- Routing fallback: Anthropic OAuth (sonnet) → deepseek-v4-pro.

> **This example is NOT fully self-contained** — it needs external infrastructure: a
> reachable **Gitea** instance + repo + token, the **gitea-mcp** binary, a **Telegram**
> bot, and network **ingress** so Gitea can POST to loomcycle. The other examples
> (exp1–exp3) need only loomcycle + a provider.

## Prerequisites

1. **loomcycle** on PATH (or `LOOMCYCLE_BIN`), and a model provider (OAuth login, or
   `DEEPSEEK_API_KEY`).
2. **gitea-mcp binary** → `work/bin/gitea-mcp` (chmod +x). From
   <https://gitea.com/gitea/gitea-mcp/releases> (v1.3.0+). e.g.:
   ```bash
   cd examples/exp4-gitea-telegram
   # pick your platform asset (Darwin_arm64 / Linux_x86_64 / …):
   curl -sSL -o /tmp/g.tgz https://gitea.com/gitea/gitea-mcp/releases/download/v1.3.0/gitea-mcp_Darwin_arm64.tar.gz
   tar -xzf /tmp/g.tgz -C work/bin gitea-mcp && chmod +x work/bin/gitea-mcp
   ```
3. **A Gitea instance + repo + access token** (token scope: `write:repository`; plus
   `write:user` if you create the repo via API). Put the host + token in `.env.local`.
4. **A webhook HMAC secret** — any random string, shared between the Gitea repo hook
   and loomcycle: `openssl rand -hex 32` → `LOOMCYCLE_GITEA_WEBHOOK_SECRET`.
5. **A Telegram bot** — talk to @BotFather, get the bot token; get your chat id (e.g.
   message the bot, then `https://api.telegram.org/bot<token>/getUpdates`).
6. **Ingress** — Gitea must reach loomcycle's `/v1/_webhooks/*`. If Gitea isn't on this
   host, set `LOOMCYCLE_LISTEN_ADDR` to a reachable IP (LAN/VPN/tailnet) and point the
   Gitea hook URLs there. (No public relay needed on a private network.)

## Setup

Fill `.env.local` (run.sh creates it from the template on first launch):
`LOOMCYCLE_GITEA_TOKEN`, `LOOMCYCLE_GITEA_WEBHOOK_SECRET`, `LOOMCYCLE_GITEA_HOST`,
`LOOMCYCLE_GITEA_USER`, `LOOMCYCLE_TELEGRAM_BOT_TOKEN`, `LOOMCYCLE_TELEGRAM_CHAT_ID`
(+ `DEEPSEEK_API_KEY` or OAuth). Set `LOOMCYCLE_GITEA_INSECURE=true` only for a
self-signed/expired cert.

**Clone the target repo into the coder's working tree** with push auth wired via the
credential helper (the coder just runs `git push`, never touches the token):
```bash
cd examples/exp4-gitea-telegram
set -a; source .env.local; set +a
INSEC=""; [ "${LOOMCYCLE_GITEA_INSECURE:-false}" = true ] && INSEC="-c http.sslVerify=false"
git $INSEC -c credential.helper="$PWD/gitcreds.sh" -c credential.useHttpPath=false \
    clone "$LOOMCYCLE_GITEA_HOST/$LOOMCYCLE_GITEA_USER/<your-repo>.git" work/exp4-workflow
cd work/exp4-workflow
git config credential.helper "$PWD/../../gitcreds.sh"; git config credential.useHttpPath false
[ "${LOOMCYCLE_GITEA_INSECURE:-false}" = true ] && git config http.sslVerify false
git config user.email "you@example.com"; git config user.name "$LOOMCYCLE_GITEA_USER"
```

**Register two Gitea repo webhooks** (by URL) pointing at loomcycle, both with the
HMAC secret (sent from env via a helper so it never hits argv):
```bash
cd examples/exp4-gitea-telegram
LC_URL="http://<loomcycle-reachable-ip>:8787/v1/_webhooks"   # match LOOMCYCLE_LISTEN_ADDR
python3 - "$LC_URL" <<'PY'
import os, sys, json, ssl, urllib.request
host=os.environ["LOOMCYCLE_GITEA_HOST"]; tok=os.environ["LOOMCYCLE_GITEA_TOKEN"]
secret=os.environ["LOOMCYCLE_GITEA_WEBHOOK_SECRET"]; user=os.environ["LOOMCYCLE_GITEA_USER"]
repo=os.environ.get("EXP4_REPO","exp4-workflow"); lc=sys.argv[1]
ctx=ssl.create_default_context()
if os.environ.get("LOOMCYCLE_GITEA_INSECURE")=="true": ctx.check_hostname=False; ctx.verify_mode=ssl.CERT_NONE
api=f"{host}/api/v1/repos/{user}/{repo}/hooks"
def post(url,body):
    r=urllib.request.Request(url,data=json.dumps(body).encode(),method="POST",
        headers={"Authorization":"token "+tok,"Content-Type":"application/json"})
    try:
        with urllib.request.urlopen(r,context=ctx,timeout=20) as resp: return resp.status
    except urllib.error.HTTPError as e: return f"{e.code} {e.read().decode()[:120]}"
for name in ["pr-opened","review-done"]:
    print(name, "->", post(api, {"type":"gitea","active":True,"events":["pull_request"],
        "config":{"url":f"{lc}/{name}","content_type":"json","secret":secret,"http_method":"post"},
        "secret":secret}))
PY
```
> Set the secret in **both** `config.secret` and the top-level `secret` field — some
> Gitea versions persist the HMAC secret only via the top-level field. Both hooks
> subscribe to `pull_request` (Gitea sends all PR actions); the agents guard on
> `action`/`merged`.

## Run + drive
> **`loomcycle validate` note:** tier-based config (for the OAuth→deepseek fallback) —
> `validate` reports `no provider resolved` because it doesn't probe providers. Not a
> config bug; verify by **running** and reading the boot `resolve probe:` + `mcp[…]` lines.

```bash
cd examples/exp4-gitea-telegram
./run.sh                      # starts the server; boot log: gitea 53/53 tools, telegram 1/1, webhooks enabled
```
In a second terminal — spawn the coder (long-lived, subscribes `exp4-tasks`) and
publish a task:
```bash
cd examples/exp4-gitea-telegram
printf '{"agent":"exp4-code-guru","user_id":"exp4","segments":[{"role":"user","content":[{"type":"trusted-text","text":"Begin your loop. Wait for tasks on exp4-tasks."}]}]}' > /tmp/coder.json
( timeout 480 ./loomcurl.sh -N -X POST http://127.0.0.1:8787/v1/runs -H 'Content-Type: application/json' -d @/tmp/coder.json > /tmp/coder.sse 2>&1 ) &
sleep 6
./loomcurl.sh -X POST http://127.0.0.1:8787/v1/_channels/exp4-tasks/publish \
  -H 'Content-Type: application/json' \
  -d '{"payload":{"task":"Add an is_even(n) function in is_even.py returning True for even ints, with a minimal pytest. Tiny and correct.","pr":null,"branch":null}}'
```
Then, automatically: coder opens a PR → Gitea fires `pr-opened` → reviewer reviews +
squash-merges → Gitea fires `review-done` → advisor sends Telegram "✅ merged".

## Verify
- **Boot log:** `mcp[gitea]: ready, 53/53 tools` · `mcp[telegram]: ready, 1/1 tools` ·
  `webhooks: enabled (POST /v1/_webhooks/{name} …)`.
- **PR opened + merged** in Gitea (API or UI).
- **Webhook delivery:** loomcycle log shows the receiver accept (no `signature
  rejected`); a `peek` shows the spawn. Gitea's repo → Settings → Webhooks → Recent
  Deliveries shows `202`.
- **Telegram:** a "✅ exp4: PR #N merged" message arrives.
- The reviewer/advisor runs' `done.usage.provider` confirms the route.

## Caveats / gotchas
- **Ingress is the #1 blocker** — if `signature rejected` never appears AND no spawn
  happens, Gitea can't reach loomcycle: fix `LOOMCYCLE_LISTEN_ADDR` + the hook URL.
- **`signature rejected`** = the Gitea-stored hook secret ≠ `LOOMCYCLE_GITEA_WEBHOOK_SECRET`.
  Re-set it via PATCH with the secret in **both** `config.secret` and top-level `secret`.
- **Self-approval:** Gitea refuses approving your *own* PR, but **merging** your own PR
  is allowed — so the reviewer's APPROVE may fail while the squash-merge succeeds.
- **TLS / certs (prefer the proper fix):** use a properly-issued cert, or add your
  Gitea CA to the system trust store — don't disable verification in general (it allows
  MITM). `LOOMCYCLE_GITEA_INSECURE=true` is a **last-resort opt-in** (defaults `false`)
  for a self-signed/expired cert on a **trusted private network you control** only; the
  wrapper, `giteapi.sh`, and the clone snippet honor it but it should not be left on.
- **Secret hygiene:** loomcycle redacts secret values from persisted transcripts at
  rest (v0.23.4+); still, the helper scripts keep tokens off argv (env/stdin only).

## Teardown
Ctrl-C the server; `kill` the coder SSE curl; delete `./data`. Optionally remove the
Gitea repo webhooks. The clone lives at `work/exp4-workflow`.

## Files
| File | Purpose |
|---|---|
| `loomcycle.yaml` | routing + coder/reviewer/advisor + `mcp_servers` (gitea/telegram) + `webhooks` |
| `run.sh` | launcher (Bash sandbox + webhooks receiver + the secret allowlist) |
| `.env.local.example` | secret + deployment-fill-in template (empty values) |
| `work/bin/gitea-mcp-stdio.sh` | token-safe wrapper loomcycle spawns as the `gitea` MCP server |
| `work/bin/telegram-bot-mcp.py` | minimal Telegram-sender MCP (stdlib-only) |
| `work/bin/gitea-mcp` | the gitea-mcp binary — **you download this** (gitignored) |
| `giteapi.sh` | token-safe Gitea API curl (repo + webhook setup) |
| `gitcreds.sh` | git credential helper (feeds the token to `git push`) |
| `loomcurl.sh` | token-safe loomcycle REST helper |
