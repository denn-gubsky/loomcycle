# Provider configuration

loomcycle dispatches LLM calls through a pluggable provider matrix. Each provider has its own compiled-in **driver** (`internal/providers/<id>/`); *which* providers exist — the built-ins below plus any 3rd-party or self-hosted ones — is **declared in config** via the `providers:` map (see [Config-driven providers](#config-driven-providers-providers-map)). Operators select a provider per-agent (yaml `provider:` field) or per-tier (`user_tiers.<tier>.tiers.<level>[].provider`).

This page covers operator-facing setup for each provider. The substrate, resolver, and tier-fallback design live in `docs/CONFIGURATION.md`.

## Supported providers

| ID | Auth | Endpoint | Status |
|---|---|---|---|
| `anthropic` | API key (`ANTHROPIC_API_KEY`) | `https://api.anthropic.com` | Production. The default choice. |
| `openai` | API key (`OPENAI_API_KEY`) | `https://api.openai.com` | Production. |
| `deepseek` | API key (`DEEPSEEK_API_KEY`) | `https://api.deepseek.com` | Production. |
| `gemini` | API key (`GEMINI_API_KEY`) | `https://generativelanguage.googleapis.com` | Production. |
| `ollama` | Bearer (`OLLAMA_API_KEY`) | `https://ollama.com` (configurable via `OLLAMA_CLOUD_BASE_URL`) | Production. Hosted ollama.com. |
| `ollama-local` | None (local trust) | `OLLAMA_BASE_URL` (default `http://localhost:11434`) | Production. Local-network Ollama. |
| `anthropic-oauth-dev` | OAuth subscription | `https://api.anthropic.com` | **Opt-in. Research/dev only.** Reverse-engineered Anthropic subscription billing via Claude Code's OAuth flow. See below. |

## Config-driven providers (`providers:` map)

Providers are declared in a top-level `providers:` map, keyed by the provider id agents reference in `provider:`. Each entry names the compiled-in **driver** that serves it plus how it's wired. This is what lets an operator point loomcycle at a **3rd-party or self-hosted** OpenAI-compatible endpoint (vLLM, groq, together, a second Ollama) **without a code change**.

The built-in providers in the table above ship as an **embedded default layer** that is prepended to every config, so a config with **no `providers:` block** resolves them exactly as before — you add a `providers:` entry only to introduce a new provider or override a built-in. Operator entries deep-merge over the defaults (last wins). Set `LOOMCYCLE_NO_DEFAULT_PROVIDERS=1` to drop the built-ins entirely and declare only your own.

### Entry fields

| Field | Meaning |
|---|---|
| `driver` | **Required.** The compiled-in driver family: `anthropic`, `openai`, `gemini`, `deepseek`, `ollama`, `mock`, `code-js`. (`loomcycle validate` lists the compiled-in set on a typo.) |
| `dialect` | Wire dialect when a driver speaks more than one; omit for the driver's default (today each has one: `anthropic-messages`, `openai-chat`, `gemini-v1beta`, `ollama-chat`). |
| `base_url` | Override the driver's default endpoint (a self-hosted mirror, Vertex Gemini, a LAN Ollama). Empty = driver default. |
| `api_key_env` | The **env-var NAME** (never the value) whose credential authenticates this provider — resolved server-side, and overridable per-tenant/user (a tenant's own key shadows the operator's — see [CREDENTIALS.md](CREDENTIALS.md)). **Omit for a keyless endpoint.** |
| `auth` | Authenticate with **AWS credentials** instead of a key: SigV4 signing or a minted short-term Bedrock key, for Amazon Bedrock. Exclusive with `api_key_env`. See [Amazon Bedrock with AWS credentials](#amazon-bedrock-with-aws-credentials-auth). |
| `max_concurrent` | Cap the model **calls** in flight to this provider (see below) — shorthand for `lanes.chat.max_concurrent`. `0` / unset = unbounded. |
| `lanes` | A queue per kind of model (`chat` / `decision` / `embedder`): its own `max_concurrent`, plus `affinity_hold_ms`, `quantum_calls` and `background_share` (see below). |
| `max_models_in_flight` | How many different models may compute on this provider at once, across all lanes. `0` / unset = unlimited. |
| `models_in_flight_exempt_kinds` | Model kinds `max_models_in_flight` leaves out, e.g. `[embedder]`. Only with `max_models_in_flight`. |
| `model_switch_max_wait_ms` | How long a waiting model can be held off by calls to the models in flight. Default `30000`; only with `max_models_in_flight`. |
| `options` | Driver-specific tuning, passed opaquely: `ollama` `num_ctx` / `num_gpu`; `llamacpp` `pin_slots` (see [Pinning llama.cpp slots](#pinning-llamacpp-slots)); `code-js` `code_root` / `deterministic` / `run_timeout_seconds`; `mock` `stable`. Unknown keys log a warning, never fatal. |
| `capabilities` | Override the driver's advertised capabilities (`supports_thinking` / `supports_vision` / `supports_effort` / `native_prompt_cache` / `parallel_tool_calls` / `max_context_tokens`). Only the coarse flag is overridable; per-model heuristics stay in the driver. |

### Add a 3rd-party OpenAI-compatible provider

```yaml
providers:
  # A self-hosted vLLM / llama.cpp / groq / together endpoint.
  # driver: openai speaks the OpenAI Chat Completions wire shape.
  my-vllm:
    driver: openai
    base_url: http://vllm.local:8000/v1
    api_key_env: MY_VLLM_KEY        # omit entirely for a no-auth endpoint
    max_concurrent: 4

models:
  qwen-big: { provider: my-vllm, model: qwen2.5-72b }
tiers:
  high: [qwen-big]
```

A **keyless** endpoint (no auth) is declared by simply omitting `api_key_env` — declaring the provider *is* the opt-in; loomcycle calls `base_url` with no key. (Before v1.21.0 a declared provider without `api_key_env` was silently treated as "not configured".)

### Per-provider concurrency (`max_concurrent`, `lanes`)

`max_concurrent` caps how many **model calls** are in flight to a provider at once. A slot is taken just before a call and given back when its response stream ends; nothing is held while a run executes a tool, waits for a sub-agent or waits for its operator. Calls over the cap **queue inside loomcycle** and never time out. The motivating case is a **local model on one GPU**: cap `ollama-local` so the host computes a fixed number of requests at a time.

```yaml
providers:
  ollama-local:
    driver: ollama
    max_concurrent: 2     # ≤2 model calls in flight, whoever makes them
```

Because the cap counts calls and not runs, eight agents on two slots take turns call by call: no agent waits for another's whole conversation.

- **Every call is counted.** Top-level runs, sub-agents, interactive runs, resumed runs, compaction summaries, the LLM gateway and the OpenAI-compatible routes, the image-describe route, the memory reranker (listwise) and the unit generator all go through the same queue. A fan-out parent and its same-provider children share the cap, and a run that fails over to another provider simply makes its next call in that provider's queue. Decision models and the embedder have lanes of their own (below).
- **A run's calls always wait**, however long the queue is: the run was admitted, so a busy provider never fails it. Time a sub-agent spends queued does not count against its `timeout_ms`.
- **A request sent directly, outside any run, is bounded.** That is the LLM gateway (`/v1/_llm/chat`, the OpenAI-compatible routes), `POST /v1/embeddings`, `POST /v1/_decide` and the image-describe route. With `concurrency.provider_queue_depth` (`LOOMCYCLE_PROVIDER_QUEUE_DEPTH`; defaults to the global `max_queue_depth`) calls already waiting on the provider, the next such request is refused instead of queueing: the gateway and `/v1/embeddings` answer `429 provider_concurrency_exhausted` (with `provider` and `cap`, so a client can try another tier at once), `/v1/_decide` reports the refusal as the call's failure, the describe route as that image's. A gateway or embeddings request waits for its global run slot in the run queue, taking turns with runs and with no timeout; waiting for its provider it holds no global run slot (it takes one only when the provider can take it) but stays counted against its caller's per-user cap, so requests piled onto a saturated provider cannot use the global cap up; if no global slot is free at the moment the provider frees up, the request is refused with `429 backpressure`. That is all `provider_queue_depth` bounds.
- **A run waiting for a provider slot lends its global run slot** (`concurrency.max_concurrent_runs`). While the run's own turn call waits for its lane, the slot goes to the next run waiting to start, so a saturated local provider does not keep runs bound for idle providers out. Before the call computes, the run takes a slot back, ahead of every new run.
  - **It never waits for a global slot holding a lane slot.** When the lane grants the call and no global slot is free, the run gives the lane slot back, waits for a global slot holding nothing, then queues for the lane again holding it. Calls that do not lend (a tool's decision or embedding, a compaction summary, a sub-agent's call) still wait holding the run's slot, and the two would otherwise wait on each other.
  - **A lent run still counts.** Lent runs count against `concurrency.max_queue_depth` with the queued ones, both for lending and for admitting a new run. So runs alive stay within `max_concurrent_runs + max_queue_depth`, with or without a per-user cap. At that bound a run simply waits holding its slot, as before.
  - **The cost:** while a run takes its slot back it waits for one to free. If every global slot is held by runs that are not computing (a parked interactive run, a long tool call), that wait lasts until one of them ends.
  - `GET /v1/_concurrency/stats` reports `lent`, the runs waiting with their slot lent; `active` is the runs holding one.

  So size the global cap for the runs you want computing at once, the provider cap for what the host can compute, and `max_queue_depth` for how many runs may be waiting at all.
- Cloud providers are vendor-rate-limited — leave them uncapped. An uncapped provider costs nothing.
- Advisory / per-replica (exact for the single-replica local case). It complements — does not replace — Ollama's own `OLLAMA_NUM_PARALLEL`.
- **A run sees its calls wait.** A model call of a run that has to wait for its provider puts `call_queued` on the run's stream and in its transcript before it waits, with `admission.reason` (`lane`: every slot of the lane is taken; `models_in_flight`: the provider has its cap of models in flight and this call's model is not among them), `admission.position` (its place when it queued, 1 = next), `admission.provider` and `admission.kind`; then `call_admitted` with `admission.wait_ms` once it has its slot. A call can report `call_queued` twice: for its model, then for a slot. A call that gets its slot at once reports nothing. The run's own model calls report, and so do the calls made on its behalf: a decision or an embedding one of its tools makes (`kind` `decision` / `embedder`), a compaction summary, a listwise rerank. Background work started from a run (a consolidation pass, an embedding fill) does not report on it, and neither does a request outside any run (the LLM gateway).
- **Watching it.** `GET /v1/_concurrency/stats` lists every provider that caps anything under `providers.<id>`: `active` / `queued` are its chat lane (as they always were), `lanes.<kind>` is each lane — `max_concurrent`, `active`, `reserved` (slots held for the run that last used them), `queued`, `queued_background`, `grants`, `preemptions`, `blocked_on_model` (the queued calls held back by their model's `model_caps`) — and `models_in_flight` / `model_waiters` are what `max_models_in_flight` is counting (`models_in_flight` maps each admitted model to its calls, including calls still queued in a lane, so the number of keys is the models in flight). An admin sees the same, refreshed every few seconds, in the Web UI under **Settings → Lanes**. Prometheus has the same per lane, labelled `provider` and `kind`: `loomcycle_provider_lane_max_concurrent`, `_lane_slots_in_use`, `_lane_slots_reserved`, `_lane_queue_depth`, `_lane_queue_depth_background`, `_lane_queue_depth_blocked_on_model`, `_lane_grants_total`, `_lane_preemptions_total`, plus `loomcycle_provider_models_in_flight` and `loomcycle_provider_model_waiters` per provider. The older `loomcycle_provider_slots_in_use` / `loomcycle_provider_queue_depth` gauges remain and are the chat lane alone. The built-in floor for decisions and reranks (below) is not a lane and is not shown.
- A config reload applies a changed cap to the next call.

#### Lanes: a queue per kind of model

`max_concurrent` is shorthand for the **chat** lane. A provider can cap each kind of model it serves (the `models:` `kind` tag — `chat`, `decision`, `embedder`) separately, and shape how the runs waiting in a lane take turns:

```yaml
providers:
  ollama-local:
    driver: ollama
    max_models_in_flight: 1
    model_switch_max_wait_ms: 30000
    lanes:
      chat:     { max_concurrent: 4, quantum_calls: 4, affinity_hold_ms: 5000 }
      decision: { max_concurrent: 2 }
      embedder: { max_concurrent: 2, background_share: 0.25 }
```

| Key | Meaning |
|---|---|
| `lanes.<kind>.max_concurrent` | Calls of that kind in flight. `0` / unset = that kind is not capped, and then none of the keys below may be set. Setting both `max_concurrent` and `lanes.chat.max_concurrent` to different values is refused at load. |
| `lanes.<kind>.affinity_hold_ms` | The **lease**: how long a slot stays reserved for the run that just used it, so the run's next call lands on the same slot — on a local host, on a warm prompt cache. `0` (default) = no reservation, a plain fair queue call by call. The lease belongs to the **run**: it is dropped as soon as the run ends, and a run gone for longer than the hold (a slow tool, a sub-agent, its operator) loses it. |
| `lanes.<kind>.quantum_calls` | How many consecutive calls a run may make on its reserved slot while other runs wait; after that it yields and queues again. `0` = unlimited (the lease is bounded by `affinity_hold_ms` alone). |
| `lanes.<kind>.background_share` | The least fraction of the lane's turns given to background work (see below) while both it and live runs wait, in `[0, 1)`. `0` (default) = live runs always go first, and background work on a lane that is never idle does not run at all. |
| `lanes.<kind>.model_caps` | Calls in flight **per model** inside the lane: `{ <model>: <n> }`, keyed by model name or a `models:` alias of this provider. A call whose model already has `n` calls in flight waits even while a slot is free; calls to other models are not held up behind it. A key covers the model at every context size (`qwen3.6:latest` covers `qwen3.6:latest@65536`). Each `n` is `>= 1` and at most the lane's `max_concurrent`; a key that is a `model_pattern` alias, an alias of another provider, or names the same model as another key is refused at load, as is `model_caps` on a lane with no `max_concurrent`. |
| `max_models_in_flight` | How many **different models** may compute on the provider at once, across all lanes. Some local hosts fail, or thrash loading and evicting weights, when several do. Any number of calls to a model already in flight are admitted; a call to another model waits until one drains. A run whose call waits here gives up the slots it holds reserved in the provider's lanes (`affinity_hold_ms`), so the model in flight is not kept from computing by a slot nobody can use. `0` / unset = unlimited. |
| `model_switch_max_wait_ms` | How long a waiting model can be held off by new calls to the models in flight: after this long those queue behind it, so it gets its turn. Default `30000`; only meaningful with `max_models_in_flight`. |
| `models_in_flight_exempt_kinds` | Kinds of model (`chat` / `decision` / `embedder`) that `max_models_in_flight` does not count and never holds back; their own lane still caps them. Setting it without `max_models_in_flight` is refused at load. |

Waiting calls are served round-robin across (tenant, user), so one user's fan-out cannot hold a lane against everyone else.

**Ollama: one model at two context sizes is two models.** Ollama loads a model at one `num_ctx`; the same model requested at another size is reloaded (about 10 s measured) and loses its prompt cache. `max_models_in_flight` counts `model@num_ctx`, so the two do not compute together — and the driver logs a warning, once per combination, when an agent's `max_context_tokens` differs from the provider's pinned `num_ctx`. Set them to the same value.

**Ollama: some models run one request at a time.** On Ollama 0.40.3 some model architectures compute ONE request at a time whatever `OLLAMA_NUM_PARALLEL` says — measured: `qwen3.6` and `ornith-1.5` (qwen35moe), `qwen3.8`, `nimble` and `clef` (qwen35) — while others (`gpt-oss`, `gemma4`) really run four. A lane of `max_concurrent: 4` shared by both kinds sends four calls to a one-slot model, and three of them wait *inside* Ollama: invisible to loomcycle's queue and stats, and counted against the driver's header timeout. A lane of `1` costs the parallel models their throughput instead. `model_caps` keeps the lane at 4 and holds the one-slot models to one call each, so their extra calls wait in loomcycle's queue while calls to the other models take the free slots:

```yaml
providers:
  ollama-local:
    lanes:
      chat:
        max_concurrent: 4
        model_caps: { qwen3.6:latest: 1, ornith-1.5:35b: 1 }
```

A call waiting for its model's cap reports `call_queued` with `admission.reason: lane`, and is counted in the lane's `queued` and also its `blocked_on_model` (`GET /v1/_concurrency/stats`; Prometheus `loomcycle_provider_lane_queue_depth_blocked_on_model`). With caps, a waiter's reported `position` is approximate: calls held back by their model are passed over. The lane stays fair: a freed slot goes to the first waiter in turn (round-robin across (tenant, user), oldest first) whose model is under its cap. A run that comes back to its reserved slot (`affinity_hold_ms`) while its model is at its cap gives the reservation up and queues, so the slot serves another model meanwhile. A changed `models:` alias is picked up on config reload.

**Leaving the embedder out.** On a host that fails when two LLMs compute at once, `max_models_in_flight: 1` keeps them apart, but it counts every model, so an embedding then waits for the chat model to finish. When the host runs a small embedding model beside an LLM without trouble (measured on the lab host: chat + embed passed, chat + decision did not), leave it out:

```yaml
providers:
  ollama-local:
    max_models_in_flight: 1
    models_in_flight_exempt_kinds: [embedder]   # embeddings do not wait for the chat model
    lanes:
      embedder: { max_concurrent: 2 }           # still capped by their lane
```

**Which lane a call takes.** Every model call the server makes is counted in a lane of the provider it goes to, and against that provider's `max_models_in_flight` unless its kind is exempt:

| Call | Lane |
|---|---|
| A run's own calls, compaction, the LLM gateway, the image-describe route | `chat` |
| `memory.reranker` with `kind: listwise`; `memory.unit_generator` | `chat` of the provider the block resolves to |
| The `decision:` block (the `Decision` tool, a team's decision states, `POST /v1/_decide`); `memory.reranker` with `kind: decision` | `decision` |
| The embedder (`memory.embedder`): Memory, Document and History searches and writes, trace indexing, `/v1/embeddings` | `embedder` |

- The lane is the **provider's**, even when a block points its calls at another endpoint with its own `base_url`. An embedder moved to a host of its own is capped by the provider's `embedder` lane, which no chat call takes.
- A call made by a tool inside a run queues **as that run**: it takes its turn by the run's (tenant, user), and a listwise rerank uses the slot the run's lease holds.
- A decision's `timeout_ms` starts when the call does; its wait for a lane slot is not counted. A rerank is different: `memory.reranker.timeout_ms` bounds the whole rerank, wait included, and a rerank that runs out of time keeps search's own order.
- The CLI commands that call a model themselves (`memory-eval-live`, `memory-calibrate`) run in their own process with no server scheduler, and are not queued.

**A built-in floor for decisions and reranks.** The `decision:` block and `memory.reranker` used to bound their own calls (`max_concurrent`, default 4 each), blind to each other and to the chat calls on the same host. Both keys are removed, and a config that still sets one fails to load, naming the lane that replaces it. The bound itself stays as a floor: while a provider has no lane for them, decisions to it are held to 4 in flight, and so are reranks. A lane you configure replaces the floor, higher or lower, and counts those calls together with everything else on the provider. Chat calls and embeddings have no floor: a provider with no cap sends every one of those at once.

**Background work.** These calls queue behind live runs on the same lane, with the share `background_share` gives them:

- a **memory consolidation** pass: the consolidator's run, the sub-agents it spawns, and the embeddings its writes make;
- the operator-run **sweeps**: `POST /v1/_document/reindex`, `/v1/_document/derive_units`, `/v1/_document/describe_images`, `/v1/_memory/backfill_embeddings`, `/v1/_memory/backfill_traces`, `/v1/_memory/reembed`;
- the part of a large Document re-index that continues after the edit that caused it has returned;
- indexing the built-in help and seeding `memory.entries` at boot.

Anything a live run asks for — an agent's Memory search, a `Decision` call, an ordinary scheduled run — is not background. A consolidation sweep still has its own time budget, so on a lane that live runs keep full give background work a share (`background_share: 0.25`) rather than leaving it at `0`.

#### Pinning llama.cpp slots

A lane's lease (`affinity_hold_ms`) keeps a slot for the run that last used it, so the run's next call can reuse its warm prompt cache. On Ollama loomcycle cannot choose which server slot a request lands on. llama.cpp can: `llama-server` keeps one prompt cache per slot (`--parallel N`) and takes `id_slot` on a request. With `pin_slots`, the `llamacpp` driver sends each call as `id_slot` = the number of the lane slot it was granted, with `cache_prompt: true`. A run's next call returns to the lane slot its reservation kept, and so to the server slot that still holds its prompt.

```yaml
providers:
  llama-local:
    driver: llamacpp
    base_url: http://gpu-box:8080/v1
    options:
      pin_slots: true
    lanes:
      chat: { max_concurrent: 4, affinity_hold_ms: 5000 }   # max_concurrent = llama-server --parallel
```

- Lane slots are numbered from 0, the lowest free one at each grant, and stay below `max_concurrent`. Set it to the server's `--parallel`: a number past the server's slots is refused by the server.
- Without `affinity_hold_ms` every call is still pinned to its lane slot, but a run is not kept on one between calls, so the prompt cache helps less.
- A call to the provider with no lane granted (no chat lane configured) is sent unpinned.

### Capability overrides

```yaml
providers:
  my-vllm:
    driver: openai
    base_url: http://vllm.local:8000/v1
    capabilities:
      supports_vision: true       # this mirror serves a multimodal model
      max_context_tokens: 131072
```

Only the flag you set changes; the rest keep the driver's defaults. `deepseek` gates vision off by default (its text models 400 on an image); an operator running a multimodal DeepSeek mirror behind `base_url` can re-enable it here.

## Amazon Bedrock with AWS credentials (`auth:`)

Bedrock serves Claude through the Anthropic Messages API and other models (gpt-oss, OpenAI's GPT models) through OpenAI Chat Completions, so the `anthropic` and `openai` drivers reach it as they are. What differs is authentication. A Bedrock API key in `api_key_env` works, but it is the wrong way in for the reason people choose Bedrock, which is running sensitive data inside their own AWS account:

- **Long-term keys:** AWS says they are for exploration only.
- **Short-term keys** expire within 12 hours.
- **Security-minded accounts** deny API keys outright in an SCP.

On EC2, ECS, EKS or Lambda the platform already gives the process short-lived, rotating IAM credentials. An `auth:` block lets a provider use them: no secret in any config, and every call attributed to the role in CloudTrail.

### Configuring a provider

```yaml
providers:
  bedrock-claude:
    driver: anthropic
    base_url: https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic
    auth:
      type: aws_sigv4
      region: eu-central-1
  bedrock-openai:
    driver: openai
    base_url: https://bedrock-runtime.eu-central-1.amazonaws.com/openai/v1
    auth:
      type: aws_sigv4
      region: eu-central-1

agents:
  analyst:
    provider: bedrock-claude
    model: eu.anthropic.claude-sonnet-5-5
```

Or select the embedded preset, which declares these two providers for `LOOMCYCLE_BEDROCK_REGION` and changes no routing: `LOOMCYCLE_PRESETS=base,bedrock`.

| Field | Meaning |
|---|---|
| `type` | `aws_sigv4`: sign every request (the recommended mode). `aws_bedrock_token`: mint a short-term Bedrock API key from the credentials and send it as the driver's key. |
| `region` | **Required.** The signing region. `base_url`'s host must name the same region, or the config is refused. |
| `service` | The SigV4 service. Defaults from the host: `bedrock-runtime…` → `bedrock`, `bedrock-mantle…` → `bedrock-mantle`. Required for any other host. |
| `role_arn` | Assume this role on top of the credentials found (a role in another account). |
| `external_id_env` | The env var holding the `ExternalId` for `role_arn`. |
| `session_name` | The role session name CloudTrail records. Default `loomcycle`. |

`auth` is accepted on the `anthropic` and `openai` drivers only, needs an `https` `base_url`, and cannot be combined with `api_key_env`.

**Two hosts.** Bedrock serves the same routes on two hosts. Either works.

| Host | `base_url` (Anthropic / OpenAI) | Model ids |
|---|---|---|
| runtime (AWS's recommended one) | `https://bedrock-runtime.<region>.amazonaws.com/anthropic` · `/openai/v1` | Claude by inference profile: `eu.anthropic.claude-sonnet-5-5`, `us.…`, `global.…`; gpt-oss as `openai.gpt-oss-120b-1:0` |
| mantle | `https://bedrock-mantle.<region>.api.aws/anthropic` · `/v1` | bare ids: `anthropic.claude-haiku-4-5`, `openai.gpt-oss-20b` |

Claude is served only on the Anthropic route; the Chat Completions route refuses a Claude id.

### Where the credentials come from

The AWS SDK's standard chain, first match wins:

1. `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` (+ `AWS_SESSION_TOKEN`).
2. Web identity (EKS IRSA): `AWS_WEB_IDENTITY_TOKEN_FILE` + `AWS_ROLE_ARN`.
3. The shared files `~/.aws/config` and `~/.aws/credentials`, profile from `AWS_PROFILE`. SSO and `credential_process` profiles work too.
4. The container endpoint: the ECS task role, or EKS Pod Identity.
5. The EC2 instance role, through **IMDSv2 only**. loomcycle never falls back to IMDSv1.

Temporary credentials are refreshed five minutes before they expire. If AWS answers that they expired anyway, the call is retried once with fresh ones. Nothing is fetched at boot: the routing probe's first model listing is the first use.

### IAM policy

The role (or user) needs to invoke the models it uses, and to **list** models. The routing probe lists them, and a model that is not listed is never routed to. For the runtime host:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "InvokeTheModelsLoomcycleUses",
      "Effect": "Allow",
      "Action": ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"],
      "Resource": [
        "arn:aws:bedrock:eu-central-1:123456789012:inference-profile/eu.anthropic.claude-sonnet-5-5",
        "arn:aws:bedrock:*::foundation-model/anthropic.claude-sonnet-5-5"
      ]
    },
    {
      "Sid": "ListModelsForRouting",
      "Effect": "Allow",
      "Action": ["bedrock:ListInferenceProfiles", "bedrock:ListFoundationModels"],
      "Resource": "*"
    },
    {
      "Sid": "NoBedrockApiKeys",
      "Effect": "Deny",
      "Action": ["bedrock:CallWithBearerToken", "bedrock-mantle:CallWithBearerToken"],
      "Resource": "*"
    }
  ]
}
```

- **Inference profiles:** an inference profile is invoked in every region it routes to, so the foundation-model statement names the model in all of them.
- **mantle:** inference is `bedrock-mantle:CreateInference` on `arn:aws:bedrock-mantle:<region>:<account>:project/default`.
- **Key minting:** the `Deny` keeps every Bedrock API key out of the account, which is right for `aws_sigv4`. With `aws_bedrock_token`, allow `bedrock:CallWithBearerToken` instead (on mantle, `bedrock-mantle:CallWithBearerToken`).

### Model ids and data residency

- **Geographic profiles stay in their geography:** `us.`, `eu.`, `apac.`, `jp.` and `au.` keep processing within it.
- **`global.` profiles go anywhere:** they may process data in any commercial AWS region, and need `aws:RequestedRegion = "unspecified"` allowed if an SCP restricts regions.

For sensitive data, name the geography you need; the `bedrock` preset never picks one for you. Some models require a data-retention mode to be set on the account before their first use. Bedrock's own error says so.

### What loomcycle does with the credentials

- **Never stored.** No secret key, session token or minted key is written to the database, a snapshot, a transcript, an event or a log. Each one is registered with the redactor, so a tool that ever printed one would have it masked in the transcript.
- **Restricted runs.** A run that may not use the operator's provider credentials (`providers:operator-key`) is refused before anything is signed. The AWS credentials are the operator's, as a host key is.
- **A tenant's own key** overrides the operator's AWS credentials when it is stored as a credential named `AWS_BEARER_TOKEN_BEDROCK` (a Bedrock API key), and is billed to that tenant. A tenant's `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` is never sent to Bedrock.
- **Service blocks.** A `memory.reranker`, `memory.unit_generator` or `decision:` block that points at another `base_url`, or names its own key, does not carry the provider's AWS auth there.
- **Agents cannot fetch the credentials.** The HTTP and WebFetch tools refuse the instance metadata service and the ECS / EKS credential endpoints. While any provider uses `auth`, the bashbox-fallback, scheduler and webhook env allowlists may not name `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_CONTAINER_AUTHORIZATION_TOKEN` or `AWS_BEARER_TOKEN_BEDROCK`.
- **Host commands can.** Bash and the Bashbox host fallback are not network-sandboxed, so on EC2 a host command can read the instance role's credentials. When loomcycle runs in a container, set the instance's IMDS hop limit to 1, and do not enable host commands where the role can reach sensitive data. `loomcycle doctor` warns about this combination.

### Checking it

- **`loomcycle doctor`** resolves each `auth` provider's credentials and asks STS whose they are. It prints the ARN, the account and the chain source, never the credentials.
- **The routing view** (`GET /v1/_routing`, Web UI → routing) labels the provider `aws_sigv4 (<region>)` for an admin. The provider is reachable once its model listing succeeds.

| Symptom | Cause |
|---|---|
| Provider unreachable, `bedrock model listing: … status 403` | The role lacks the list permissions above. |
| `provider authentication unavailable: aws credentials unavailable: …` | The chain found no credentials; the message says why. Permanent: no fallback cascade, no retry. |
| `… 403 … not authorized to perform: bedrock:InvokeModel` (`401` on runtime's OpenAI route) | The role may not invoke that model or profile. Permanent. |
| `(SigV4 signature refused: check this server's clock and that auth.region …)` | Clock skew, or a `region` / `service` that does not match the host. |

### Embeddings

The same credentials can serve `memory.embedder`. Bedrock serves embeddings through InvokeModel on the runtime host only, and `api: bedrock` makes the embedder speak it to the provider it names. Supported: Cohere Embed v4 and Cohere Embed v3 (multilingual and English).

```yaml
models:
  cohere-embed: { provider: bedrock-claude, model: eu.cohere.embed-v4:0, kind: embedder }
memory:
  embedder:
    model: cohere-embed
    api: bedrock
    dimensions: 1024
```

- **The provider** is any `auth:` provider on `https://bedrock-runtime.<region>.amazonaws.com`, whatever its chat route (`/anthropic` or `/openai/v1`): the embedder uses the host only. `bedrock-mantle` serves no embeddings and is refused. `aws_sigv4` and `aws_bedrock_token` both work.
- **`api: bedrock` is required** on an `auth:` provider. Without it the config is refused: no other embedder speaks AWS authentication.
- **No endpoint or key of its own.** `memory.embedder.base_url` and `api_key_env` are refused with `api: bedrock`, so the credentials go only to the provider's own host.
- **Queries and stored text.** Stored text is embedded as `search_document` and searches as `search_query` (TOOLS.md → "Vector / semantic search").
- **Calls.** At most 96 texts per call (`batch_size` lowers it). A throttle (429), a model timeout (408) and a 500 or 503 are tried up to 3 times within `timeout_ms`; other errors are not retried. An error names the model, the region, the status and AWS's message, never the texts.
- **Billing.** Embeddings always use the operator's AWS credentials (a tenant's `AWS_BEARER_TOKEN_BEDROCK` does not apply) and, as for every embedder, are not recorded in the usage ledger.

**Model ids and regions.**

| Model | id | Where it is served | Where text is processed |
|---|---|---|---|
| Cohere Embed v4 | `eu.cohere.embed-v4:0` (or `us.`, `apac.`, …) | through a geographic profile | any destination region of the profile; `eu.` today: eu-central-1, eu-north-1, eu-south-1, eu-south-2, eu-west-1, eu-west-3 |
| Cohere Embed v4 | `global.cohere.embed-v4:0` | any commercial region | anywhere, outside your geography too |
| Cohere Embed v4 | `cohere.embed-v4:0` | in-region where Bedrock serves it (not eu-central-1) | that region |
| Cohere Embed Multilingual v3 | `cohere.embed-multilingual-v3` | in-region, eu-central-1 included | that region |
| Cohere Embed English v3 | `cohere.embed-english-v3` | in-region | that region |

Prefer `eu.cohere.embed-v4:0` (or your geography's profile) for v4, and never `global.` for data with residency rules. Where text must stay in the calling region, use `cohere.embed-multilingual-v3`. Destination regions change: check AWS's inference-profile list. An application inference-profile ARN is refused, because its model cannot be read from the id.

**Dimensions.**

| Model | `dimensions` | Width stored |
|---|---|---|
| Cohere Embed v4 | `0` (default), `256`, `512`, `1024`, `1536` | 1536 for `0`, else the value |
| Cohere Embed v3 | `0` or `1024` | 1024 (fixed) |

Any other value is refused at load. pgvector indexes at most 2000 dimensions, and every Cohere width is under that.

**IAM.** A geographic profile is invoked in the calling region and served in any of its destination regions, so the role needs `bedrock:InvokeModel` on the profile ARN **and** on the foundation model in every destination region. Allowing `cohere.embed-*` by name is not enough:

```json
{
  "Sid": "EmbedWithCohere",
  "Effect": "Allow",
  "Action": "bedrock:InvokeModel",
  "Resource": [
    "arn:aws:bedrock:eu-central-1:123456789012:inference-profile/eu.cohere.embed-v4:0",
    "arn:aws:bedrock:*::foundation-model/cohere.embed-v4:0"
  ]
}
```

For v3 in the region: `arn:aws:bedrock:eu-central-1::foundation-model/cohere.embed-multilingual-v3`. `loomcycle doctor` prints the embedder (provider, model, width) and warns when the region does not list the model id; it uses the list permissions above.

**Switching embedders.** Vectors from different models (or widths) are not comparable, and the similarity bands are a property of the model. After switching:

1. On a populated store, `POST /v1/_memory/reembed` (also after changing `dimensions`).
2. `loomcycle memory-calibrate`, then set `memory.consolidation.merge_threshold` and `related_threshold` from its output.

A store with no vectors yet needs only step 2.

## `anthropic-oauth-dev` — Anthropic subscription billing (research/dev only)

> ### ⚠ NO GUARANTEES — READ BEFORE ENABLING
>
> The Anthropic OAuth flow used by this provider is **not an official integration**. It is the product of reverse-engineering done by the Pi agent team (github.com/earendil-works/pi, 51K stars, open-source) and replicated here by the loomcycle team. We do our best to mimic Claude Code's wire shape — tool names, system prompt format, headers, beta markers — so loomcycle's LLM calls pass through Anthropic's subscription-billing detection. **We cannot guarantee this will continue to work.** Anthropic can change the auth flow, the wire shape, the version pinning, or the detection heuristics at any time and the integration will break until a hotfix lands.
>
> **You are running this against your own Claude Pro/Max subscription. Anthropic's subscription terms historically restrict programmatic use outside the official Anthropic SDK. You — the operator — are solely responsible for any consequences with Anthropic if they ever object to this use, including account flagging, rate-limiting, or subscription revocation.** loomcycle (the project) and its maintainers carry no warranty, no liability, and provide no support guarantees for the OAuth-dev path. If your account is affected, the resolution path is between you and Anthropic; we cannot intervene.
>
> By setting `LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1` and running `loomcycle anthropic login`, you acknowledge that:
>
> 1. The OAuth flow is reverse-engineered, not officially endorsed by Anthropic.
> 2. Anthropic may flag, limit, or revoke your subscription if they object to non-Claude-Code clients using OAuth tokens.
> 3. The integration may stop working at any time as Anthropic updates their auth surface; loomcycle will ship hotfixes when feasible but offers no SLA.
> 4. All risk and any consequences with Anthropic are yours to manage. loomcycle disclaims responsibility for account, billing, or terms-of-service outcomes.
> 5. If you cannot accept these terms, **do not enable this provider** — use the production `anthropic` provider (API key) instead.
>
> This feature exists because it is genuinely useful for research workloads where API-key billing would be prohibitively expensive. It is not a free lunch.

### When to use it

Research workloads at scale: self-evolution experiments, agentic-team load testing, multi-iteration coding-task experiments. Concrete pain it solves: a 100-iteration self-evolution cycle costs $750-$3,750 at API-key Sonnet pricing; a $200/month MAX subscription absorbs the same workload.

### When NOT to use it

- **Production deployment of any kind.** Server-hosted instances stay on API-key billing.
- **Multi-tenant workloads.** One operator, one Claude subscription. Multi-tenant deployments must use API-key Anthropic.
- **Anything customer-facing.** The OAuth path can break at any time (Anthropic changes the auth surface; subscription revoked; quota exhausted). Customer-facing workloads need API-key SLAs.

### Prerequisites

1. **Active Claude Pro or Max subscription** (`claude.ai/settings/billing`). The OAuth scope set requests `user:inference` — only subscriptions with inference quota work.
2. **`LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1`** in your environment. Without this, the provider is absent from the resolver matrix and the `loomcycle anthropic` CLI subcommands refuse to run.
3. **A browser on the same machine as loomcycle.** Cross-machine login is not supported in v0.11.9 — the callback server binds to `127.0.0.1` on the loomcycle host, so the browser must redirect to that same loopback. Headless environments (CI, server-without-display) can use `--manual`: loomcycle prints the authorize URL instead of auto-opening a browser, and the operator opens that URL in a browser **on the same machine** (e.g. via SSH X-forwarding, a VNC session, or a graphical session that doesn't expose a default-browser hook to `xdg-open`).

### One-time login

```sh
export LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1
loomcycle anthropic login
```

This:
1. Generates a PKCE pair (S256 challenge).
2. Starts a localhost callback server on `127.0.0.1:53692` (configurable via `LOOMCYCLE_ANTHROPIC_OAUTH_CALLBACK_PORT`).
3. Opens your browser at `https://claude.ai/oauth/authorize?...` — you authorize loomcycle's access to your subscription.
4. Receives the auth code via the callback, exchanges it for access + refresh tokens at `https://platform.claude.com/v1/oauth/token`.
5. Persists tokens at `~/.config/loomcycle/anthropic-oauth.json` (chmod 0600 enforced).

A background goroutine refreshes the access token every 30 seconds, rotating proactively 5 minutes before expiry. The refresh token can be long-lived (Anthropic rotates it when it pleases; loomcycle persists whatever comes back).

### Status check

```sh
loomcycle anthropic status            # local token-file metadata only
loomcycle anthropic status --probe    # ALSO verify server-side validity
```

Prints the token file path, whether tokens are loaded, the access token's expiry, the granted scope, and the obtainedAt timestamp. Also surfaces a warning if the token file's permissions have drifted from 0600.

By default every field is **local token-file metadata** — the token can read "valid / expires in 6h" while Anthropic has already revoked it server-side, so plain `status` prints `Server-side: not checked`. Pass **`--probe`** (alias `--verify`) to confirm against Anthropic: it attempts a token refresh (free — token endpoint, no inference billing), reporting `✓ valid` (exit 0) or `✗ INVALID` (exit 1) with the reason. A successful probe **rotates and persists** a fresh token, so it also heals/extends the session.

### Logout

```sh
loomcycle anthropic logout
```

Deletes the token file. Idempotent — safe to run when no tokens are stored.

### Agent configuration

```yaml
agents:
  research-coder:
    description: "Heavy-volume coding-task agent for load testing"
    provider: anthropic-oauth-dev
    model: claude-sonnet-4-6
    tools:
      - Read
      - Write
      - Edit
      - Bash
      - Grep
      - Glob
      - Memory       # exposed via the mcp__loomcycle__memory wire name
      - Channel      # exposed via the mcp__loomcycle__channel wire name
      - Agent        # exposed via the mcp__loomcycle__agent wire name (parallel_spawn etc.)
    max_tokens: 8192
    max_iterations: 64
```

**All loomcycle tools are admissible under OAuth-dev.** The 10-tool Claude-Code canonical overlap (`Read`, `Write`, `Edit`, `Bash`, `Grep`, `Glob`, `NotebookEdit`, `WebFetch`, `WebSearch`, `Skill`) and any real MCP tool (`mcp__<server>__<tool>` from `mcp_servers:` yaml or `MCPServerDef` substrate) pass through unchanged. Loomcycle's substrate primitives (`Memory`, `Channel`, `Agent`, `AgentDef`, `SkillDef`, `MCPServerDef`, `Evaluation`, `Interruption`, `Context`, `HTTP`) are exposed under the `mcp__loomcycle__*` wire mask — to Anthropic's subscription-billing layer they look like tools from an MCP server registered as "loomcycle"; to the loomcycle dispatcher they continue to work exactly as today.

### Tier configuration (with API-key fallback)

```yaml
user_tiers:
  research:
    provider_priority: [anthropic-oauth-dev, anthropic]
    tiers:
      low:    [{provider: anthropic-oauth-dev, model: claude-haiku-4-5-20251001}]
      middle: [{provider: anthropic-oauth-dev, model: claude-sonnet-4-6}]
      high:
        - {provider: anthropic-oauth-dev, model: claude-sonnet-4-6}
        - {provider: anthropic, model: claude-sonnet-4-6}    # API-key fallback when subscription quota is exhausted
    fallback_on_error: true
```

With `fallback_on_error: true`, subscription-quota-exhausted errors trigger a fallback to API-key Anthropic for the affected request. Research workloads can run primarily on subscription billing and bleed over to API credits only when the MAX quota hits its ceiling.

### Risk details

The headline disclaimer above is the contract. This section is the operational detail.

* **Reverse-engineered flow.** The Claude Code OAuth `client_id` constant in `internal/providers/anthropic_oauth_dev/client_id.go` is the same one Pi publishes; loomcycle did not negotiate this access path with Anthropic. The wire shape — beta markers, User-Agent string, system-prompt prepend, tool-name canonicalization, MCP-name masking — exists to look like Claude Code so Anthropic's subscription billing accepts the request. None of this is guaranteed to keep working.
* **Subscription-terms posture.** Anthropic's MAX/Pro subscription terms historically restrict programmatic use outside their official SDK. Pi has been operating publicly since 2025-08 without takedown, suggesting Anthropic tolerates this in practice — but tolerance is not endorsement, and tolerance can be withdrawn at any time without notice.
* **No SLA, no warranty, no liability.** loomcycle is Apache-2.0 software. The OAuth-dev path is opt-in and explicitly described as "research/dev only" precisely so this expectation is clear. If you need contractually-guaranteed access to Claude, you must use the official API-key path (`provider: anthropic`) — that's between you and Anthropic and they support it.
* **Single-machine deployment, by design.** Tokens live at `~/.config/loomcycle/anthropic-oauth.json` (chmod 0600). There is no support for shared token stores, multi-replica synchronization, or server-side token mounting. Multi-tenant or server-hosted deployments must use API-key Anthropic. This is enforced by design — to limit the blast radius if Anthropic's detection trips.
* **Drift exposure.** Anthropic can change the OAuth flow, the beta marker, the required User-Agent string, or the validation of any wire-shape element at any time. When that happens, OAuth-dev breaks until a hotfix lands. Operators with `LOOMCYCLE_CLAUDE_CODE_VERSION=<override>` can self-patch the User-Agent string for the duration; other drift may require a code update.

If any of these are unacceptable for your workload, use the `anthropic` provider (API key) instead.

### Drift detection

Auth-surface drift surfaces as one of two patterns:

- **Refresh failures**: the background goroutine logs `WARN anthropic-oauth-dev: refresh failed (will retry on next tick) err=...` on every 30s tick. Persistent failures across multiple ticks mean the refresh endpoint changed or the refresh token was revoked.
- **400 INVALID_REQUEST on Messages calls**: Anthropic's auth surface rejected the request. Most common cause: the User-Agent's pinned Claude Code version is too old.

When the second pattern hits:

```sh
# Self-patch until a hotfix lands:
export LOOMCYCLE_CLAUDE_CODE_VERSION=2.1.80   # whatever Pi's pinned to today
loomcycle ...
```

Check Pi's source (github.com/earendil-works/pi → `packages/ai/src/providers/anthropic.ts`) for their current pinned version.

When refresh fails persistently:

```sh
loomcycle anthropic logout
loomcycle anthropic login
```

### How it works (architecturally)

The OAuth-dev driver wraps the production `internal/providers/anthropic` driver. Two layers sit on top:

1. **HTTP transport** (`oauthTransport` in `driver.go`): strips `x-api-key` from outgoing requests, adds `Authorization: Bearer <current access token>`, appends `claude-code-20250219,oauth-2025-04-20` to `anthropic-beta`, sets `user-agent: claude-cli/<version>`.
2. **Tool-name mask** (`loomcycle_mask.go`): rewrites loomcycle-only built-in tool names (Memory / Channel / etc.) to `mcp__loomcycle__<name>` on outbound, reverses on inbound. To Anthropic this looks like an MCP server registered as "loomcycle"; to the loomcycle dispatcher the tools work exactly as today.

Everything else (SSE parsing, retry on 429, rate-limit honouring, cache_control breakpoint placement, effort hint translation) is inherited from the production driver. The OAuth-dev path is a thin shim — when Anthropic adds a Messages API feature, the production driver gains support first and the OAuth-dev path inherits it for free.

### Multi-replica HA — not supported

The OAuth-dev provider is single-operator, single-machine. There's no token-sync mechanism, no shared refresh state. Multi-replica deployments must use API-key Anthropic. This is enforced by the design (tokens in `~/.config/loomcycle/`), not by a runtime check — operators who attempt to mount the token file across replicas will get refresh-token-rotation races and inconsistent state.

### References

- Pi (`earendil-works/pi`, 51K stars): the source of the OAuth client_id + flow shape.
- The internal RFC at `~/work/loomcycle-internal/doc-internal/rfcs/anthropic-oauth-dev.md` (locked 2026-05-19) documents the full design + decisions.
- `Context.help(topic="loomcycle")` from inside an agent prompt for the in-substrate description.

## Image / vision input (RFC AT)

loomcycle accepts image input as a content block on a `user`-role segment, over
**every transport** (HTTP, gRPC, MCP, the TS + Python adapters). It is additive
and backward-compatible — text-only callers are unchanged, and there is no new
endpoint.

### Sending an image

A caller adds an `image` block alongside text blocks in a `user` segment of
`POST /v1/runs` (or `POST /v1/sessions/{id}/messages` for a continuation turn):

```json
{
  "agent": "vision-agent",
  "segments": [{
    "role": "user",
    "content": [
      {"type": "trusted-text", "text": "What's in this picture?"},
      {"type": "image", "media_type": "image/png", "data": "<base64 bytes, no data: prefix>"}
    ]
  }]
}
```

- **`media_type`** must be one of `image/png`, `image/jpeg`, `image/gif`,
  `image/webp` (the common denominator across all vision providers).
- **`data`** is the raw base64 of the image bytes — **no `data:` prefix**. There
  is deliberately **no URL form**: accepting a URL would make loomcycle fetch
  arbitrary hosts (SSRF). The OpenAI driver builds the `data:` URI internally.
- Images are valid only in `user`-role segments.

### Per-transport

The same `image` block is accepted on every transport; only the representation
of the bytes differs (JSON has no byte type, proto does):

| Transport | How to send an image |
|---|---|
| **HTTP** (`POST /v1/runs`, `/v1/sessions/{id}/messages`) | `data` = base64 string (as above) |
| **MCP server** (`spawn_run` / `spawn_runs`) | a `segments` arg with the same JSON block — `data` = base64 string |
| **TS** (`@loomcycle/client`) | `PromptContent` `image` variant: `{ type:"image", media_type, data }` (base64 string) on `runStreaming` / `continueSession` |
| **gRPC** (`Run` / `Continue`) | `PromptContentBlock.media_type` (string) + **`data` (bytes)** — raw image bytes, NOT base64; the server encodes at the boundary |
| **Python** (`pip install loomcycle`, gRPC) | a segment dict `{"type":"image","media_type":...,"data": b"...raw bytes..."}` |

gRPC/Python carry the raw bytes natively (no ~33% base64 inflation); HTTP/MCP/TS
carry base64 strings. All converge on the same internal representation.

### Per-provider serialization

Each driver serializes the block natively — no conversion to text:

| Provider | Wire form |
|---|---|
| `anthropic` (+ `anthropic-oauth-dev`) | `{"type":"image","source":{"type":"base64","media_type","data"}}` |
| `openai` | user message `content` becomes the array form with an `image_url` part holding a `data:` URI |
| `gemini` | a part with `inlineData: {mimeType, data}` |
| `ollama` / `ollama-local` | the user message's `images: [base64]` field |
| `deepseek` | **not supported** — DeepSeek text models reject images |

### The capability gate

A provider advertises `SupportsVision`; the loop refuses an image sent to a
text-only provider/model **before** the call, emitting a clear error event
("model X on provider Y does not support image input") rather than letting the
image be silently dropped or the provider return an opaque 400. This makes a
provider-fallback onto a text-only model fail loudly. Per-model nuance (a legacy
text-only model on an otherwise vision-capable provider — `claude-2`/
`claude-instant`, `gpt-3.5*`, the original `gpt-4`/`gpt-4-32k` snapshots) is
refined inside the driver; unknown models default to supported (a wrong guess
surfaces as a provider error, never a silent drop). For Ollama the model must be
a vision model (llava, llama3.2-vision, …) — that's the operator's choice.

### Request body cap

Inline base64 makes requests larger, so the run-ingest body cap (`POST /v1/runs`
and `POST /v1/sessions/{id}/messages`) defaults to **16 MiB** and is tunable via
`LOOMCYCLE_MAX_REQUEST_BYTES` (bytes). An over-cap request returns
`413 Request Entity Too Large`. Consumers should resample images to a sane max
edge (~1568px) before sending to keep them small and bound token cost.

### Caveats

- The gRPC `PromptContentBlock` carries `media_type` + `data` (bytes); its
  `kind` field (the untrusted-block source label) is the HTTP wire's `kind`.
- **No image output** (generation), no audio/video.
- A vision model can be influenced by text rendered *inside* an image
  (prompt-injection-via-image) — inherent to any vision system, not defended at
  the wire.
