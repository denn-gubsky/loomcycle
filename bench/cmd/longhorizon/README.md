# longhorizon — RFC CS long-horizon benchmark (P1)

Measures the RFC CR context-retention regimes on a synthetic, horizon-scalable,
oracle-graded task, to decide (measure-first) whether RFC CR is worth building into
the loop. It drives the model **through loomcycle's OpenAI-compatible gateway**
(`POST /v1/chat/completions`), so every arm shares the same provider routing and the
RFC AV cost ledger; token counts are provider-reported `usage`.

## The arms (RFC CR regimes, as harness-side context assemblers)

| Arm | Regime | Per-step context | Cost |
|-----|--------|------------------|------|
| **A0** | append (today) | system + **full growing history** + step | O(T²) |
| **A1** | recap (RFC CR L1) | system + a **model-maintained running recap** + last-N verbatim + step | O(T) |
| **A2** | stateful (RFC CR L2 / SKILL.state) | system + an **explicit JSON state** the model patches; no history | O(T) |

These are faithful stand-ins for what the loomcycle loop would do server-side — the
point is to measure the *idea* before changing the loop.

## The task

A deterministic **counter-tracking** stream: T instructions (`SET`/`ADD`/`SUB`/`RESET`),
optional distractor `NOTE` lines (`-noise`) and one external `CORRECTION` (`-drift`),
followed by queries (`GET` a counter, `SUM`, `MAX`) graded against an oracle. It is
state-tracking with light arithmetic — losing an early mutation gives a wrong answer,
which is exactly what separates the arms at long horizons.

> ⚠️ **RFC CS Q3.** This task is *maximally schema-able*, so it likely **overstates the
> A2 win**. It is the clean-curve instrument (cost-vs-horizon, the local A0-vs-A1
> gate). The go/no-go weights the controlled **marketing-research team run** (RFC CS
> P2) higher — that is the reality check.

## Run

Needs a running loomcycle with the OpenAI-compat gateway enabled and a routable model.

```sh
go run ./bench/cmd/longhorizon \
  -base http://127.0.0.1:8787 -bearer "$LOOMCYCLE_AUTH_TOKEN" \
  -model claude-frontier -arm all -horizon 100 -keys 5 -seeds 3 -out results.jsonl
```

Key flags: `-arm A0|A1|A2|all`, `-horizon T`, `-keys N`, `-seed`/`-seeds`,
`-noise <pct>`, `-drift`, `-keep-last-n` (A1 window), `-out <jsonl>`.

Output is a per-arm table averaged over seeds:

```
longhorizon  T=100  (avg over seeds)
arm            total_tok  peak_prompt   accuracy    steps   recaps       ms
A0-append         ...          ...        ...        100        0      ...
A1-recap          ...          ...        ...        100       ..      ...
A2-stateful       ...          ...        ...        100        0      ...
```

`peak_prompt` is the O(T)-flatness witness: A0 grows with T, A1/A2 stay roughly flat.

## The decision gate (RFC CS)

- **Local model:** A0 vs **A1** — greenlight L1 if A1 cuts cumulative tokens ≥ ~30% at
  the target horizon with accuracy within noise of A0.
- **Frontier model:** A0 vs **A2** (+ budget-matched controls, a later addition) —
  greenlight L2 if A2 beats A0 and the controls at equal-or-lower cost.

## Status / next

P1 is the synthetic instrument + A0/A1/A2. Not yet included (RFC CS later phases):
budget-matched controls (sliding-window, prose-summary), the noise/drift robustness
sweeps as first-class report rows, and the marketing-research team-run workload (P2).
A2 currently drops an unparseable patch (a no-op); the RFC CR rollback-retry is a
follow-up.
