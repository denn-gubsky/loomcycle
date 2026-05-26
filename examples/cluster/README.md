# loomcycle multi-replica cluster — quickstart demo

> One `docker compose up` away from a running 2-replica loomcycle cluster, with Postgres-backed cluster state and an nginx load balancer in front. Closes the loop on the v0.12.x multi-replica HA work — see [`docs/MULTI-REPLICA.md`](../../docs/MULTI-REPLICA.md) for the full operator runbook.

## What you'll get

```
                          ┌─────────────────┐
                          │nginx (port 18080)│
                          │ round-robin LB   │
                          └────────┬────────┘
                                   │
                ┌──────────────────┴──────────────────┐
                │                                     │
        ┌───────▼────────┐                  ┌────────▼───────┐
        │  loomcycle-a   │                  │  loomcycle-b   │
        │  REPLICA_ID=a  │                  │  REPLICA_ID=b  │
        │  :18787 (host) │                  │  :18788 (host) │
        └───────┬────────┘                  └────────┬───────┘
                │                                     │
                └──────────────────┬──────────────────┘
                                   │
                          ┌────────▼────────┐
                          │   postgres:16   │
                          │  cluster shared │
                          └─────────────────┘
```

- **Operators / consumers** hit `http://localhost:18080` (the nginx LB).
- **`verify.sh` / debugging** hits replicas directly on `:18787` and `:18788` so checks are deterministic.

> Ports are intentionally high (18080 / 18787 / 18788) so the demo doesn't clash with a local `loomcycle` dev binary or anything else commonly running on :8080 / :8787. Override via `REPLICA_A_URL` / `REPLICA_B_URL` / `LB_URL` env vars if you want to remap.

## Prerequisites

- **Docker Desktop** (macOS / Windows) or **Docker Engine + Compose v2** (Linux). `docker compose version` should report v2.x.
- **`jq`** for `verify.sh` output parsing. `brew install jq` (mac) / `apt-get install jq` (linux).
- **Optionally** a provider API key — at least one of `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `DEEPSEEK_API_KEY` — if you want to exercise actual agent runs through the cluster (verify checks 3 + 4 require this; checks 1 + 2 work without).

## Quickstart

From the loomcycle repo root:

```bash
# 1. Copy the env template + edit it
cp examples/cluster/.env.example examples/cluster/.env
$EDITOR examples/cluster/.env
#   - LOOMCYCLE_AUTH_TOKEN     → generate via: openssl rand -hex 32
#   - POSTGRES_PASSWORD        → anything (this is an ephemeral demo DB)
#   - One of the API keys      → optional, for verify checks 3 + 4

# 2. Bring up the cluster
docker compose -f docker-compose.cluster.yaml --env-file examples/cluster/.env up -d

# 3. Wait ~10s for both replicas to boot + heartbeat
docker compose -f docker-compose.cluster.yaml --env-file examples/cluster/.env logs -f loomcycle-a loomcycle-b
# Look for: "loomcycle listening on 0.0.0.0:8787" from BOTH services
# + "coord: cluster mode active — replica_id=..." from each
# Ctrl-C to stop following.

# 4. Verify
./examples/cluster/verify.sh
#   ✓ replica-a sees 2 replicas (replica-a,replica-b)
#   ✓ replica-b sees 2 replicas (replica-a,replica-b)
#   ✓ LB hit both replicas over 4 requests
#   (cross-replica run + cancel checks skip without an API key)
```

That's it — you have a working cluster.

## Common operations

**Hit the cluster like a real consumer (via the LB):**

```bash
TOKEN=$(grep '^LOOMCYCLE_AUTH_TOKEN=' examples/cluster/.env | cut -d= -f2)

# Healthz — should show both replicas, replica_id flips on repeat
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:18080/healthz | jq

# Run an agent (needs an API key in .env)
curl -s -H "Authorization: Bearer $TOKEN" -X POST http://localhost:18080/v1/runs \
     -H "Content-Type: application/json" \
     -d '{"agent":"default","user_input":[{"role":"user","content":"Say hi."}]}'

# Open the Web UI through the LB
open http://localhost:18080/ui/    # macOS
xdg-open http://localhost:18080/ui/  # linux
```

**Scale to 3 replicas:** add a `loomcycle-c` service in `docker-compose.cluster.yaml` (copy the `loomcycle-b` block, change `REPLICA_ID` to `replica-c`, bind to `127.0.0.1:18789:8787`), then add `server loomcycle-c:8787;` to the nginx upstream in `examples/cluster/nginx.conf`, then `docker compose ... up -d` again.

**Tear down (keeps the Postgres volume so a re-up reuses the data):**

```bash
docker compose -f docker-compose.cluster.yaml --env-file examples/cluster/.env down
```

**Tear down + wipe the Postgres volume (fresh-start on next up):**

```bash
docker compose -f docker-compose.cluster.yaml --env-file examples/cluster/.env down -v
```

## Pin to a specific version (for reproducibility)

`docker-compose.cluster.yaml` uses `denngubsky/loomcycle:latest`. That's fine for "play now". When you want to share a reproducible demo (a video, a screenshot in a deck, a test against a known binary), pin to a tagged version:

```yaml
# In docker-compose.cluster.yaml, change:
#   image: denngubsky/loomcycle:latest
# to:
#   image: denngubsky/loomcycle:v0.12.6
```

Tag list: https://hub.docker.com/r/denngubsky/loomcycle/tags

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `verify.sh` step 1 fails: "replica-a /healthz unreachable" | The replica is still booting (Postgres healthcheck takes ~10s; replicas wait for it). | Wait, retry. `docker compose logs loomcycle-a` to confirm it's running. |
| `verify.sh` step 1 fails: "expected 2 replicas, got 1" | The two replicas use different `LOOMCYCLE_AUTH_TOKEN` values OR one of them isn't reaching Postgres. | Check both env blocks share the same auth token. Check `docker compose logs postgres` for connection errors. |
| `verify.sh` step 2 fails: "LB only hit: replica-a" (or just one) | nginx's "prefer healthy backends" sent all traffic to whichever started first. | Both replicas need to be visible in `/healthz` first. Wait + re-run. |
| `verify.sh` step 3 fails: "POST /v1/runs ... returned empty" | No valid provider API key set. | Set `ANTHROPIC_API_KEY` (or another) in `.env`, `down -v && up -d` to pick it up. |
| nginx returns 502 from `http://localhost:18080/` | Both upstreams unhealthy. | `docker compose logs loomcycle-a loomcycle-b` — look for boot errors. |
| `docker compose up` errors: "LOOMCYCLE_AUTH_TOKEN required" | `.env` not loaded. | Pass `--env-file examples/cluster/.env` to every `docker compose ...` invocation OR `cd` into `examples/cluster/` first. |
| Web UI on :8080/ui/ loads but the topbar shows "?" replicas | `/healthz` is bearer-authed; the UI uses the operator session token cookie. | Log in with your `LOOMCYCLE_AUTH_TOKEN` via the UI's login screen. |

## Next steps

- **Production deployment shape** — see [`docs/MULTI-REPLICA.md`](../../docs/MULTI-REPLICA.md) for the full operator runbook: connection pool sizing, rolling upgrade procedure, crashed-replica auto-recovery (90s TTL sweep), pool budget, sharp edges.
- **Swap in your own `loomcycle.yaml`** — the demo config has one trivial agent. Replace `examples/cluster/loomcycle.yaml` with your real config (or mount a different path in the compose file). The cluster behavior doesn't change.
- **Add observability** — wire OTEL by setting `LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT` in `.env`. The cluster's spans pass through the LB and aggregate cleanly across both replicas. See [`docs/MULTI-REPLICA.md`](../../docs/MULTI-REPLICA.md#operational-concerns) for sampling guidance.
- **Talk to a real load balancer** — the nginx in this demo is the cheapest LB that works. Caddy / Traefik / HAProxy / your cloud LB all work the same way: round-robin between the replica hosts, no sticky sessions needed.
