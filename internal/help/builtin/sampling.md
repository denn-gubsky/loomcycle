---
name: sampling
description: Per-agent LLM sampling params (v0.28.0) — temperature, top_p, top_k, frequency/presence_penalty, seed, stop. Set via static yaml, AgentDef create/fork, or per-run on /v1/runs (per-run wins per field). Reported back via Context op=self.
---
v0.28.0 added a per-agent **`sampling`** block so you can tune how an
agent's model samples — most importantly **temperature** (exploration vs.
exploitation), plus `top_p`, `top_k`, `frequency_penalty`,
`presence_penalty`, `seed` (reproducibility), and `stop` sequences.

## Where you set it (three surfaces, same shape)

1. **Static yaml** — on the agent:
   ```yaml
   agents:
     explorer:
       model: claude-sonnet-4-6
       sampling:
         temperature: 0.9
         top_p: 0.95
         seed: 7
   ```
2. **AgentDef substrate** — `create`/`fork` overlay (the self-evolving
   "breeder" pattern mints variants this way):
   ```json
   {"op":"fork","name":"explorer","overlay":{"sampling":{"temperature":0.2}}}
   ```
3. **Per run** — on `POST /v1/runs` (A/B a single agent without forking):
   ```json
   {"agent":"explorer","sampling":{"temperature":1.0}, "segments":[...]}
   ```

## Merge semantics — per field

A fork overlay or a per-run `sampling` block merges **field by field** over
the agent's own sampling: a value you set wins; a field you leave out
inherits the agent's. So a fork that sets only `temperature` keeps the
parent's `top_p`, and a per-run `temperature` override doesn't blow away the
agent's `seed`. Precedence: **per-run > per-agent > provider default**.

`temperature: 0.0` is a real, deterministic value — distinct from "unset"
(which means "use the provider/model default").

## Per-provider support (each driver applies what it supports, drops the rest)

| param              | Anthropic | OpenAI / DeepSeek | Gemini | Ollama |
|--------------------|:--------:|:-----------------:|:------:|:------:|
| temperature        | ✅       | ✅                | ✅     | ✅     |
| top_p              | ✅       | ✅                | ✅     | ✅     |
| top_k              | ✅       | —                 | ✅     | ✅     |
| frequency_penalty  | —        | ✅                | —      | (model) |
| presence_penalty   | —        | ✅                | —      | (model) |
| seed               | —        | ✅                | ✅     | ✅     |
| stop               | ✅       | ✅                | ✅     | ✅     |

Unsupported params are silently dropped per provider (the same
translate-or-drop contract as `effort`).

**Anthropic + thinking:** Anthropic rejects a non-default `temperature`
(and `top_p`) when an extended-**thinking** block is attached. Since
`effort: high|medium` engages thinking on reasoning models (sonnet/opus),
an agent that sets BOTH `effort` and a custom `temperature` would 400 — so
the driver **drops** temperature/top_p for that call (thinking wins) and
logs it. Rule of thumb: set `effort` OR `temperature`, not both, on
Anthropic reasoning models.

## Introspection

`Context op=self` reports the resolved sampling for the current run under a
`sampling` object (alongside `provider` and `model`), so a self-evolving
agent can read how it's being sampled and reason about its own
exploration/exploitation. The key is omitted when no sampling is configured.

## Validation

Light bounds are checked at config-load and AgentDef create/fork
(`0 ≤ temperature ≤ 2`, `0 ≤ top_p ≤ 1`, `top_k ≥ 1`, penalties `-2..2`,
≤ 8 stop sequences); the provider API is the final authority on per-model
ranges.
