# circuit-stress — multi-agent load test

> **Why this, not the alternative.** We've shipped the v0.12.x cluster substrate and explicitly held v1.0 until that path has been exercised under real load. A unit test stresses one component at a time and a benchmark measures raw throughput, but neither one shows what happens when the Channel bus, Memory tool, Evaluation tool, and Anthropic OAuth-dev provider all engage **simultaneously** against the agent loop under N-way contention. This driver does. It also produces the first concrete data point for *"how far does the OAuth-dev MAX subscription path actually go?"* — a question we've been answering by intuition until now.

This package spawns `N` parallel "circuits", each a 3-agent pipeline:

```
researcher ──Memory.set + Channel.publish──► editor ──Memory.set + Channel.publish──► evaluator ──Memory.set + Evaluation.submit──► done
```

All three agents start at T+0 with the same `user_id`. The editor blocks on `Channel.subscribe` waiting for the researcher's signal; the evaluator blocks waiting for the editor. Memory and Channel CRUD operations across the pipeline exercise the substrate end-to-end.

Circuits are grouped `--circuits-per-user 10-20` so the Web UI's per-user agents tree gets exercised at scale too. With `--scale 100 --circuits-per-user 15`, the Users page shows ~7 distinct users each holding ~45 agents in the tree.

## Quickstart

```sh
# 1. Prereqs (one-time)
loomcycle anthropic login                              # populate OAuth token
export LOOMCYCLE_AUTH_TOKEN=$(openssl rand -hex 32)    # bearer for both server + driver

# 2. Smoke test (x1, ~1 min)
./test/load/circuit-stress/run.sh --scale 1

# 3. Web UI walkthrough at a scale that shows the agents tree filling up
./test/load/circuit-stress/run.sh --scale 30 --circuits-per-user 15
open "http://127.0.0.1:8787/ui?token=$LOOMCYCLE_AUTH_TOKEN"
# Users page shows 2 users · click either → 45 agents in tree
```

`run.sh` orchestrates: starts/reuses a Postgres container on `127.0.0.1:15432`, starts loomcycle on `127.0.0.1:8787` with the bundled test yaml, runs the driver, captures `/v1/_metrics/summary` snapshots bracketing the test, stops loomcycle on exit. Postgres stays running across invocations.

## What `run.sh` produces

Each invocation writes to `test/load/circuit-stress/results/<timestamp>/`:

| File | Content |
|---|---|
| `circuits.jsonl` | One JSON object per circuit: status, durations, tokens, score, rationale, error |
| `loomcycle.log` | Server stderr — grep here for `quota`, `subscribe`, `error` to find the wall |
| `metrics-summary-pre.json` | RSS / CPU / goroutine snapshot before the test |
| `metrics-summary-post.json` | Same, after — diff for resource growth |

The driver also prints a summary table to stdout:

```
─── Summary ────────────────────────────────────────────────
  Circuits: 100 total / 97 completed / 3 failed / 0 timeout / 0 skipped
  Duration: p50=2840ms  p95=4120ms  p99=5870ms  max=8200ms
  Tokens:   total_in=12340  total_out=18420  avg_per_circuit=127 in / 189 out
  Quality:  mean score=0.78 over 97 evaluations
  ⚠ Anthropic OAuth-dev quota exhausted at circuit 84    ← appears only when relevant
```

## Driver flags

| Flag | Default | Notes |
|---|---|---|
| `--scale N` | `1` | Total parallel circuits |
| `--circuits-per-user M` | `10` | Group circuits per user_id (10-20 recommended) |
| `--base-url` | `http://127.0.0.1:8787` | loomcycle endpoint |
| `--token` | `$LOOMCYCLE_AUTH_TOKEN` | Bearer |
| `--prompts <path>` | bundled list | Custom prompts file (one question per line) |
| `--results-dir <path>` | auto-timestamped | Where to write artifacts |
| `--circuit-timeout` | `5m` | Per-circuit deadline before marking failed |
| `--no-cleanup` | `false` | Skip post-test sweep of channels + memory |
| `--cleanup-only` | `false` | Skip the test entirely; just sweep leftover circuit-stress memory entries via the admin API and exit. Useful after reviewing rows from a previous `--no-cleanup` run. |

## Ramp protocol

We've designed this for stepwise scale-up. Stop at the first scale that breaks; characterize the failure mode; iterate.

| Step | Command | Pass criteria |
|---|---|---|
| 1. Smoke | `run.sh --scale 1` | 3 memory rows + 1 Evaluation row land; sweep wipes cleanly |
| 2. Functional | `run.sh --scale 10 --circuits-per-user 10` | All 10 complete; Web UI shows the 30-agent tree under one user |
| 3. Sustained | `run.sh --scale 100 --circuits-per-user 15` | p95 within 2-3× the smoke baseline; queue depth healthy |
| 4. Stress | `run.sh --scale 1000 --circuits-per-user 20` | Either complete OR characterize the wall |

Between scales: capture `/v1/_metrics/summary` snapshots (the `run.sh` does this automatically) and run quick `psql` row counts to confirm the previous scale's sweep was thorough.

## Functional validation (skip sweep to inspect rows)

Use `--no-cleanup` to keep the memory + channel state for inspection:

```sh
./test/load/circuit-stress/run.sh --scale 1 --no-cleanup

PG_DSN="postgres://postgres:loomcycle@127.0.0.1:15432/postgres?sslmode=disable"
psql "$PG_DSN" -c "SELECT scope_id, key FROM memory WHERE scope='user' ORDER BY scope_id, key"
# Expect three rows under user-001: c1-research, c1-research-edited, c1-research-scored

psql "$PG_DSN" -c "SELECT run_id, score, emitter_role FROM evaluations LIMIT 5"
# Expect one row with emitter_role='unrelated' (evaluator is a sibling of editor)
```

The next `run.sh` invocation (without `--no-cleanup`) will sweep automatically before running, so leftovers from inspection don't pollute the next test.

To sweep WITHOUT running a new test (e.g. after reviewing rows in psql):

```sh
./test/load/circuit-stress/run.sh --cleanup-only
```

Discovers all `user-NNN` scope_ids under `scope=user` and deletes their `c*-*` memory keys via the admin API. Idempotent; safe to run repeatedly. Loomcycle still boots briefly to serve the admin requests, then shuts down cleanly.

## How the agents stay coordinated

Per-circuit, three things make the pipeline work:

1. **Shared `user_id`** — all three agents in a circuit run under the same `user_id`. Memory's `scope=user` resolves `scope_id` from the run identity, so reads/writes from any of the three agents land in the same namespace automatically.
2. **Circuit-namespaced channel names** — `research-done/c{N}` and `editing-done/c{N}` use the loomcycle channel ACL's `/*` prefix-wildcard (config validates that the prefix matches at least one declared channel; the driver pre-creates the per-circuit children via `POST /v1/_channels` before any agent spawns).
3. **Circuit-namespaced memory keys** — `c{N}-research`, `c{N}-research-edited`, `c{N}-research-scored`. Multiple circuits under the same user_id stay isolated by key suffix.

The evaluator gets the editor's `run_id` (needed by `Evaluation.submit`) via the channel message body — the editor publishes `{"editor_run_id": <its run_id>}` and the evaluator parses it from `Channel.subscribe`'s response.

## What we're measuring

Functional:
- Do all three agents in a circuit reach `completed` status?
- Are the three expected memory rows present per circuit (research, research-edited, research-scored)?
- Does each completed circuit produce an Evaluation row?
- Does the sanity sweep return the DB to its pre-test state?

Performance:
- Per-circuit duration distribution (p50/p95/p99)
- Token totals (inflated under contention if models retry)
- The first circuit ID where a quota / queue saturation / channel-bus error occurs
- Resource growth between pre/post `/v1/_metrics/summary` snapshots

## Known limitations

- **Single-replica only.** This is a single-binary stress test. The same shape against the multi-replica cluster compose is a follow-up plan.
- **OAuth-dev provider only.** API-key Anthropic is reachable via the same provider family but would obscure the OAuth quota measurement.
- **No tier fallback.** `loomcycle.yaml` sets `fallback_on_error: false` deliberately. When quota dies, runs fail and the driver halts — that's the signal we're after.

## Files in this directory

- `main.go` — the driver
- `loomcycle.yaml` — minimal test config (Postgres, OAuth-dev, 3 agents, channel ACLs)
- `prompts.txt` — 30 tiny factual questions (driver round-robins through them)
- `run.sh` — convenience wrapper
- `README.md` — you're reading it
