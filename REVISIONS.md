# LoomCycle release history

Per-version release notes from v0.4.0 onward. The current and immediately previous releases are also summarised in the main [`README.md`](README.md); older releases live here.

For the **public roadmap** (planned v0.8.16 through v1.0 work — Question tool, Pause / Resume / Snapshot, distribution, operator postures), see [`docs/PLAN.md`](docs/PLAN.md).

For pre-v0.4 history (single-tool runtime, library milestone, security patch), see the same `docs/PLAN.md` under the per-version sections.

---

## What's in v0.11.3

OpenAI Chat Completions compatibility shim. New `POST /v1/chat/completions` endpoint that translates the OpenAI wire shape onto loomcycle's native LLM gateway. Every existing OpenAI-SDK consumer can route through loomcycle by changing only the base URL + auth token — zero per-consumer integration work.

### Motivation

v0.11.0's `/v1/_llm/chat` gives consumers loomcycle's routing benefits but requires writing loomcycle-specific client code. Every OpenAI-SDK tool out there (Aider, Goose, Continue, Cursor, Cody, custom Python/TypeScript, every "use OpenAI as your LLM" tutorial) hardcodes OpenAI's URL + request shape. v0.11.3 closes that gap: point any OpenAI client at loomcycle and it Just Works.

The shim is the highest-leverage follow-up to the v0.11.x gateway-product line — it picks up the entire OpenAI ecosystem with one ~600 LOC translator.

### What ships

**Endpoint:** `POST /v1/chat/completions` (no underscore — OpenAI SDKs hardcode this path; the whole point is consumers change only the base URL).

**Wire-format translation:**

- **Request side (OpenAI → loomcycle):**
  - `messages[].content` polymorphic field (string OR `[{type:"text", text:"..."}]` array OR null) → flat string. Multimodal image/audio parts silently skipped in v1.
  - `messages[].tool_calls[].function.arguments` (JSON string per OpenAI) → parsed object for loomcycle's native shape.
  - `tools[]` (OpenAI's `{type:"function", function:{name, description, parameters}}` envelope) → flat `{name, description, input_schema}`.
  - `model`, `messages`, `tools`, `max_tokens`, `temperature`, `stream` — pass-through.

- **Response side (loomcycle → OpenAI):**
  - Native content blocks → `choices[0].message.content` (text concatenated) + `choices[0].message.tool_calls` (tool_use blocks re-wrapped in OpenAI's function envelope).
  - `stop_reason` → `finish_reason`: `end_turn` / `stop_sequence` → `"stop"`; `max_tokens` → `"length"`; `tool_use` → `"tool_calls"`.
  - `usage` → `{prompt_tokens, completion_tokens, total_tokens}` shape.
  - Streaming: bare `data: <json>` SSE frames in the `chat.completion.chunk` shape, terminated by literal `data: [DONE]` — NO named SSE events (matches OpenAI's protocol; differs from native `/v1/_llm/chat` which uses `event: name\ndata: payload`).

### Drop-in usage

Python (OpenAI SDK):

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8787/v1",
    api_key="<your-LOOMCYCLE_AUTH_TOKEN>",
)
resp = client.chat.completions.create(
    model="claude-sonnet-4-6",
    messages=[{"role": "user", "content": "What is 2+2?"}],
)
```

TypeScript (OpenAI SDK, streaming):

```typescript
import OpenAI from "openai";
const client = new OpenAI({
  baseURL: "http://127.0.0.1:8787/v1",
  apiKey: process.env.LOOMCYCLE_AUTH_TOKEN,
});
const stream = await client.chat.completions.create({
  model: "claude-sonnet-4-6",
  messages: [{ role: "user", content: "Count to 5." }],
  stream: true,
});
for await (const chunk of stream) {
  process.stdout.write(chunk.choices[0]?.delta?.content ?? "");
}
```

### Routing extensions (namespaced)

Four loomcycle-specific fields in the request body for resolver / tier / quota control:

- `loomcycle_provider` — pin to a specific provider (overrides model-based resolution).
- `loomcycle_tier` — tier for the resolver dispatch.
- `loomcycle_user_id` — per-user quota tracking + audit log key.
- `loomcycle_user_tier` — per-user tier overlay; takes precedence over `loomcycle_tier`.

The OpenAI standard `user` field auto-maps to `loomcycle_user_id` when the explicit field isn't set — SDK callers passing `user: "alice"` get per-user quota tracking for free.

### Accepted-but-ignored OpenAI fields

`n`, `presence_penalty`, `frequency_penalty`, `top_p`, `seed`, `response_format`, `logit_bias`, `tool_choice`, `top_logprobs`, `stop`. Accepted so SDKs don't trip validation errors; ignored because loomcycle doesn't apply them today. When the providers package gains support for any of these, the shim's translator picks them up automatically.

### Refactor: shared dispatch path

`handleLLMChat` was refactored to extract `prepareGatewayDispatch` (~70 LOC moved): validation → resolver call → semaphore acquire → providers.Request build, all returning a `gatewayDispatch` handle. Both `handleLLMChat` and the new `handleOpenAICompatChat` call it; the shim handles wire-format translation only. Security policy (per-user quota, resolver pin precedence, audit logging) lives in one place. A bug in routing / quota / retry surfaces in both paths; a bug in the shim is a translation bug.

### Files changed

| File | Change |
|---|---|
| `internal/api/http/openai_compat.go` *(new, ~310 LOC)* | `handleOpenAICompatChat` + request translator + non-streaming response translator + streaming response translator. |
| `internal/api/http/openai_compat_types.go` *(new, ~150 LOC)* | OpenAI wire-shape structs (request / response / chunk / tool envelopes). |
| `internal/api/http/openai_compat_test.go` *(new, ~310 LOC)* | 11 tests: happy path text + tool_call + streaming + array-content + ignored-fields + user-id mapping + finish-reason matrix + auth + validation. |
| `internal/api/http/llm_gateway.go` | Extracted `prepareGatewayDispatch` + `gatewayDispatch` type; `handleLLMChat` shrunk to parse-then-delegate. Native semantics unchanged (verified via existing LLMGateway tests still pass). |
| `internal/api/http/sse.go` | Added `sendOpenAIData` (bare `data:` frames) + `sendOpenAIDone` (`data: [DONE]` terminator). |
| `internal/api/http/server.go` | Registered `POST /v1/chat/completions` next to the native gateway route. |
| `internal/help/builtin/openai-compat.md` *(new, ~140 lines)* | Bundled `Context.help` topic with Python + TypeScript SDK examples. |
| `adapters/ts/package.json` | Version 0.11.2 → 0.11.3 (lockstep; no method changes). |
| `README.md` + `REVISIONS.md` | Release notes. |

### Wire-surface delta vs v0.11.2

| Surface | v0.11.2 | v0.11.3 |
|---|---|---|
| Go HTTP endpoints | n | n + 1 (`POST /v1/chat/completions`) |
| Go tests | n | n + 11 |
| Bundled `Context.help` topics | n | n + 1 (`openai-compat`) |
| TS adapter methods | 41 | 41 (no change) |
| Distribution channels | 3 | 3 (unchanged) |

### Migration notes

- **Purely additive.** Existing `/v1/_llm/chat` semantics unchanged; the refactor preserved every passing test in the gateway suite.
- **No schema migrations.** Shim uses no persistent storage.
- **No new substrate logic.** Zero changes to `internal/providers/`, `internal/resolve/`, `internal/loop/`.
- **TS adapter consumers** bump to 0.11.3 for lockstep parity; consumers using `@loomcycle/client` should still prefer `llmChat()` / `llmStream()` over the OpenAI SDK shim because the native adapter has richer typing (per-frame discriminated unions, generic `LLMChatStreamItem`, etc.).
- **Operators with OpenAI-SDK consumers** point the SDK's `base_url` at `http://localhost:8787/v1` + set `api_key` to the loomcycle bearer; everything else stays the same.

### Versioning

v0.11.3 — additive feature; patch bump. `@loomcycle/client` 0.11.2 → 0.11.3.

### Deferred

- `tool_choice` field handling — currently accepted-but-ignored. Lands when the providers package wires its semantics through.
- Multi-modal `content` parts (image_url / input_audio) — currently silently dropped to text-only. Lands when native gateway grows multi-modal.
- Embeddings (`/v1/embeddings`) compatibility — separate RFC.
- `logprobs` / `top_logprobs` — currently silently ignored.
- Stream usage on intermediate chunks (operator-controllable via `stream_options.include_usage`) — v1 always emits on the final chunk only.

### Downloads

Assets attached: `loomcycle-{darwin,linux}-{amd64,arm64}.tar.gz` + `SHA256SUMS`. Docker images at `docker.io/denngubsky/loomcycle:v0.11.3` + `:latest`. Adapter via `npm install @loomcycle/client@0.11.3`.

---

## What's in v0.11.2

Distribution-pipeline polish — closes the install-path loop opened by v0.11.1. Adds a multi-arch Docker image, refreshes the Homebrew formula caveats to point at the new init/doctor flow, and ships a docker-compose example. Zero Go code changes; pure release-pipeline + docs.

### Docker image

Published to `docker.io/denngubsky/loomcycle` on every release tag (multi-arch: `linux/amd64` + `linux/arm64`, single manifest). Built from `gcr.io/distroless/static:nonroot` — ~6 MB total image, no shell, no package manager, runs as uid 65532. Matches loomcycle's pure-Go static binary (CGO_ENABLED=0).

Tags shipped per release:
- `vX.Y.Z` — exact pin (recommended for production)
- `latest` — most recent stable

No `vX` or `vX.Y` floating tags during v0.11.x — too early for major-version stability promises.

First-run flow:

```sh
mkdir -p ./config ./data
docker run --rm -v $(pwd)/config:/home/nonroot/.config/loomcycle \
  denngubsky/loomcycle:v0.11.2 init --no-interactive

docker run -d --name loomcycle \
  -p 127.0.0.1:8787:8787 \
  -v $(pwd)/config:/home/nonroot/.config/loomcycle:ro \
  -v $(pwd)/data:/home/nonroot/.local/share/loomcycle \
  -e LOOMCYCLE_AUTH_TOKEN=$(openssl rand -hex 32) \
  -e ANTHROPIC_API_KEY=$YOUR_KEY \
  -e LOOMCYCLE_LISTEN_ADDR=0.0.0.0:8787 \
  denngubsky/loomcycle:v0.11.2
```

For declarative setups, `docker-compose.example.yaml` at the repo root carries mount + env-var + port-mapping defaults plus a commented-out Postgres upgrade block.

**Registry naming:** Docker Hub strips hyphens from usernames. The GitHub org `denn-gubsky` becomes `denngubsky` on Docker Hub. The first-time confusion is intentional context — pin against `denngubsky/loomcycle`, not `denn-gubsky/loomcycle`.

**Image security posture:** distroless means no `/bin/sh`. `docker exec ... sh` won't work for debugging — use `docker logs` instead. The OCI labels carry the canonical source URL, version, commit SHA, and Apache-2.0 license so registry tooling (image scanners, SBOM generators, supply-chain inspectors) sees the right metadata.

### CI stubbing

The Docker steps in `release.yml` are gated behind a repo VARIABLE (not secret) named `DOCKER_PUBLISH_ENABLED`. When unset or any value other than `"true"`, the pipeline:
- Skips `docker/setup-qemu-action`, `docker/setup-buildx-action`, and `docker/login-action`.
- Runs goreleaser with `--skip=docker,docker_manifest` so the dockers stage doesn't try to push.
- Still ships the four platform tarballs + brew formula bump.

When the operator is ready to enable Docker publish:
1. Create a Docker Hub access token at `hub.docker.com/settings/security` scoped to `docker.io/denngubsky/loomcycle` with `Read, Write, Delete` perms.
2. Add secrets `DOCKER_USERNAME` (= `denngubsky`) + `DOCKER_PASSWORD` (= the token) under repo Settings → Secrets.
3. Add a repo VARIABLE `DOCKER_PUBLISH_ENABLED` set to `"true"` under Settings → Variables.

The same release tag that runs without the gate also runs with it — no workflow changes needed to flip the switch.

Why a var (not a secret) for the toggle: secrets are masked in logs and conditionals can't easily distinguish "secret unset" from "secret empty"; vars are visible and the gate is operator-explicit. Docker credentials themselves remain secrets.

### Homebrew formula caveats refresh

The auto-generated `Formula/loomcycle.rb` (via goreleaser's `brews:` block) used to print this on `brew install`:

```
loomcycle ships as a single Go binary that reads configuration from
a YAML file. Quick start:

  mkdir -p ~/.config/loomcycle
  # Drop your loomcycle.yaml into ~/.config/loomcycle/
  loomcycle --config ~/.config/loomcycle/loomcycle.yaml
```

That instructional flow is exactly what `loomcycle init` automates as of v0.11.1. The caveats now read:

```
Quick start (v0.11.1+):

  loomcycle init       # bootstrap ~/.config/loomcycle/loomcycle.yaml
  # set $LOOMCYCLE_AUTH_TOKEN and at least one provider key
  loomcycle doctor     # verify your setup
  loomcycle            # start the server on 127.0.0.1:8787

For Docker-based deployment, pull from
docker.io/denngubsky/loomcycle (v0.11.2+).
```

The change applies automatically to the next `brew upgrade` run.

### Files changed

| File | Change |
|---|---|
| `Dockerfile` *(new)* | Multi-stage local build: node:20-alpine builds web/ → golang:1.26-alpine builds static binary → distroless/static:nonroot runtime. |
| `Dockerfile.release` *(new)* | goreleaser-specific variant. Pre-built binary copied in; ~2 minutes faster per release vs running go build inside Docker. |
| `.goreleaser.yaml` | Added `dockers:` (2 entries, one per arch) + `docker_manifests:` (2 manifests: version-pinned + latest). Updated `brews.caveats` to reference init/doctor. |
| `.github/workflows/release.yml` | Added QEMU + buildx + Docker login steps (all gated on `vars.DOCKER_PUBLISH_ENABLED`). Updated goreleaser args to `--skip=docker,docker_manifest` when the var isn't `"true"`. Added documentation block for the 3-step Docker Hub setup. |
| `docker-compose.example.yaml` *(new)* | Operator-friendly compose: loomcycle service + volume mount + env passthrough + port mapping + commented-out Postgres block. |
| `internal/help/builtin/installation.md` *(new)* | Bundled help topic covering all four install paths + verification. |
| `README.md` | New "Install" section above "Quick start" listing all four paths. New v0.11.2 entry. Quick-start section rewritten around init/doctor (was build-from-source). |
| `adapters/ts/package.json` | Version 0.11.1 → 0.11.2 (lockstep; no method changes). |

### Wire-surface delta vs v0.11.1

| Surface | v0.11.1 | v0.11.2 |
|---|---|---|
| Go HTTP endpoints | n | n (unchanged) |
| CLI subcommands | 15 | 15 (unchanged) |
| Bundled `Context.help` topics | n | n + 1 (`installation`) |
| Distribution channels | 2 (brew + tarball) | 3 (brew + tarball + docker) |
| TS adapter methods | 41 | 41 (no change) |

### Migration notes

- **Purely additive.** Existing tarball + brew install paths are unchanged. Existing operators on `brew upgrade` see the updated caveats text on next install.
- **No Go code changes.** No HTTP surface changes. No schema changes.
- **TS adapter consumers** bump to 0.11.2 for lockstep parity; no code changes.
- **Operator action required before Docker images appear**: configure the three repo settings (DOCKER_USERNAME / DOCKER_PASSWORD secrets + DOCKER_PUBLISH_ENABLED variable). Without these the release pipeline still ships tarballs + brew formula but skips Docker push.

### Versioning

v0.11.2 — small additive release. `@loomcycle/client` 0.11.1 → 0.11.2.

### Deferred

- **GHCR mirror** (`ghcr.io/denn-gubsky/loomcycle`) — one extra goreleaser `dockers:` entry; ship when an operator requests it.
- **`homebrew_casks:` migration** — goreleaser deprecation warning notes brews is being phased out; cask migration is non-trivial (casks target GUI apps) and current brews still works.
- **Helm chart** — Kubernetes deployment pattern is a tiny audience today.
- **Image hardening pass** (rootless user variant, healthcheck, multi-distro variants).

### Downloads

Assets attached: `loomcycle-{darwin,linux}-{amd64,arm64}.tar.gz` + `SHA256SUMS`. Docker images at `docker.io/denngubsky/loomcycle:v0.11.2` + `:latest` (when DOCKER_PUBLISH_ENABLED is set).

---

## What's in v0.11.1

First-run UX overhaul. A bare `loomcycle` install via `brew` (or `go install` from a tagged tarball) used to fail with `failed to load config: open loomcycle.yaml: no such file or directory` and no obvious next step. v0.11.1 closes that gap with three pieces: a new `init` subcommand to bootstrap the config tree, a new `doctor` subcommand to verify the setup, and auto-discovery so the bare binary finds a generated config in `~/.config/loomcycle/`.

### `loomcycle init`

Writes `~/.config/loomcycle/loomcycle.yaml` (the bundled heavily-commented example) + `~/.config/loomcycle/README.md` (a new per-machine quickstart covering file layout, env vars, yaml structure, troubleshooting). The repo's `docs/CONFIGURATION.md` remains the provider-routing deep-dive — they're complementary. Two modes:

- **Non-interactive** (default in CI / Docker / scripted): drops the embedded example yaml verbatim. The operator edits it later.
- **Interactive** (auto-on when stdin is a TTY; `--no-interactive` to force off): minimal 3-question wizard — which provider key do you have (anthropic / openai / deepseek / skip), what env var to read it from, what HTTP listen address. Everything else stays as the commented sections of the generated yaml.

Flags: `--path <dir>` (override the default `~/.config/loomcycle/` destination), `--force` (overwrite existing files), `--interactive` / `--no-interactive` (force the mode).

**Security:** the wizard never writes secrets to disk. It prints the env-var lines for the operator to paste into their shell rc themselves (CLAUDE.md security rule §2). Generated wizard output:

```
Wrote /Users/denn/.config/loomcycle/loomcycle.yaml
Wrote /Users/denn/.config/loomcycle/README.md

Add these to your shell rc (e.g. ~/.zshrc):
    export LOOMCYCLE_AUTH_TOKEN=$(openssl rand -hex 32)
    export ANTHROPIC_API_KEY=<your-key-here>

Then read /Users/denn/.config/loomcycle/README.md and run `loomcycle doctor` to verify.
```

### `loomcycle doctor`

Runs six checks in order, prints `[PASS]` / `[WARN]` / `[FAIL]` per check, exits 0 if no FAILs.

1. **Config found** — auto-discovers in the same order the server uses (`./loomcycle.yaml` → `$XDG_CONFIG_HOME/loomcycle/loomcycle.yaml` → `~/.config/loomcycle/loomcycle.yaml`).
2. **Config parses** — reuses `config.Load`.
3. **`LOOMCYCLE_AUTH_TOKEN` set** — WARN when empty (server boots fine but every `/v1/*` request is allowed unauthenticated).
4. **Per-configured provider** — checks the canonical API-key env var per `providers:` block (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `DEEPSEEK_API_KEY`, `GEMINI_API_KEY`, `OLLAMA_API_KEY`). Local providers (`ollama-local`) need no key — PASS unconditionally. WARN on missing, not FAIL — operators may run the binary intentionally without a given provider's key.
5. **Storage backend** — SQLite: data dir creatable + writable. Postgres: DSN non-empty (full `Open()` connectivity check deferred to v0.11.2+ to keep doctor fast).
6. **Listen address** — try-listen-then-close on the configured `ListenAddr`. FAIL when another process owns the port.

Sample output:

```
loomcycle doctor — system health check

[PASS]  Config found          : /Users/denn/.config/loomcycle/loomcycle.yaml
[PASS]  Config parses
[PASS]  LOOMCYCLE_AUTH_TOKEN set
[PASS]  Provider anthropic    : ANTHROPIC_API_KEY set
[WARN]  Provider openai       : OPENAI_API_KEY not set
[PASS]  Storage backend       : sqlite at /Users/denn/.local/share/loomcycle (writable)
[PASS]  Listen address        : 127.0.0.1:8787 (bindable)

1 warning, 0 failures.
```

### Config auto-discovery

When `loomcycle` is run without `--config`, the binary walks the same three paths doctor uses and picks the first one that exists. Explicit `--config /any/path.yaml` is unchanged — auto-discovery only kicks in when the flag is left at its default AND `./loomcycle.yaml` is absent.

When no config is found anywhere, the binary prints a friendly first-run hint and exits with code 1 (instead of the old confusing "open loomcycle.yaml" error):

```
loomcycle: no config found at any of:
    ./loomcycle.yaml
    /Users/denn/.config/loomcycle/loomcycle.yaml

Run `loomcycle init` to create one, or pass --config <path> to use an existing file.
```

### Bundled documentation

`loomcycle.example.yaml` moved into `cmd/loomcycle/embedded/` and is now `//go:embed`'d alongside the new `cmd/loomcycle/embedded/README.md`. A symlink at the repo root keeps every existing reference working (config tests, GitHub raw-URL docs). The yaml is the same 737-line heavily-commented schema reference; the new per-machine `README.md` (~150 lines) covers file layout + the full env-var reference + troubleshooting. Both ship with the binary; both are written to `~/.config/loomcycle/` by `init`. (Distinct from the repo's existing `docs/CONFIGURATION.md` — that's the conceptual provider-routing deep-dive; `~/.config/loomcycle/README.md` is the per-machine quickstart.)

The bundled `Context.help` registry also picks up the new `getting-started` topic (~80 lines). Agents asked "how do I set up loomcycle" can read it directly via `GET /v1/_help/getting-started` or `Context.help getting-started`.

### Files changed

| File | Change |
|---|---|
| `cmd/loomcycle/embedded/` *(new package)* | Houses the embedded `loomcycle.example.yaml` (moved from repo root, symlinked back) + the new `README.md`. The `embedded.go` package exposes `ExampleYAML()` / `LocalReadme()` byte accessors. |
| `cmd/loomcycle/main.go` | Add `case "init"` / `case "doctor"` to subcommand switch. Replace the bare `config.Load(*cfgPath)` call with `resolveConfigPath(*cfgPath)` auto-discovery + first-run hint. |
| `cmd/loomcycle/autodiscover.go` *(new)* | `resolveConfigPath` + `configAutoDiscoveryPaths` + `userOverrodeConfigFlag` helpers. |
| `internal/cli/init.go` *(new, ~250 LOC)* | `RunInit` + minimal 3-question wizard. |
| `internal/cli/doctor.go` *(new, ~280 LOC)* | `RunDoctor` + 6 checks. Narrow `configForDoctor` interface for testability. |
| `internal/cli/doctor_adapters.go` *(new)* | `realConfig` adapter wrapping `config.Config` into the narrow interface. |
| `internal/cli/init_test.go` *(new, 7 tests)* | Non-interactive write, --force, wizard with bytes.Buffer stdin, validator reprompt, mutually-exclusive flags. |
| `internal/cli/doctor_test.go` *(new, 10 tests)* | All-pass, missing-config, parse-error, auth WARN, provider WARN, sqlite-unwritable FAIL, port-bound FAIL, ollama-local no-key, postgres-empty-DSN, postgres-with-DSN. |
| `internal/cli/cli.go` | New FIRST-RUN section in PrintHelp + package-doc subcommand listing updated. |
| `internal/help/builtin/getting-started.md` *(new, ~80 lines)* | Bundled help topic. |
| `go.mod` | `golang.org/x/term` direct dep for `IsTerminal` (already transitively pulled). |
| `loomcycle.example.yaml` | Now a symlink to `cmd/loomcycle/embedded/loomcycle.example.yaml` (canonical location). |
| `adapters/ts/package.json` | Version 0.11.0 → 0.11.1 (lockstep; no method changes). |

### Wire-surface delta vs v0.11.0

| Surface | v0.11.0 | v0.11.1 |
|---|---|---|
| Go HTTP endpoints | n | n (unchanged) |
| CLI subcommands | 13 | 15 (+`init`, +`doctor`) |
| Bundled `Context.help` topics | n | n + 1 (`getting-started`) |
| Embedded assets | 0 | 2 (example yaml + README.md) |
| TS adapter methods | 41 | 41 (no change) |

### Migration notes

- **Purely additive.** Existing `--config /path/to/yaml` invocations keep their exact semantics; auto-discovery only kicks in when the operator omits the flag AND `./loomcycle.yaml` is absent.
- **No yaml schema changes** — `init` always writes the latest example. Operators with hand-edited yaml see no difference until they run `init --force`.
- **No new HTTP endpoints, no wire-protocol changes** — pure CLI + auto-discovery feature.
- **TS adapter consumers** bump to 0.11.1 for lockstep parity; no code changes.

### Versioning

v0.11.1 — small additive feature; patch bump. `@loomcycle/client` 0.11.0 → 0.11.1.

### Downloads

Assets attached: `loomcycle-{darwin,linux}-{amd64,arm64}.tar.gz` + `SHA256SUMS`.

---

## What's in v0.11.0

First slice of the v0.11.x line — exposes loomcycle's resolver + provider auth + retry layer as a **direct LLM gateway wire surface** that bypasses the agent loop. Same binary, second product positioning: alongside the agent runtime, loomcycle is now a LiteLLM/Portkey-class gateway any LangChain-compatible consumer can hit.

### Motivation

`@loomcycle/n8n-nodes-loomcycle` needs a `LoomCycleChatModel` cluster sub-node that plugs into n8n's AI Agent **Chat Model slot** — so the AI Agent's reasoning turns are powered by loomcycle's resolver instead of n8n's per-provider nodes. Today the only way to do this is a "passthrough agent" hack (declare an agent with `system_prompt: ""` + `allowed_tools: []`, spawn `runStreaming` per turn). That works but costs ~50-200 ms per reasoning turn × 10-50 turns per workflow = 0.5-10 s of pure overhead. The gateway closes this gap.

The broader product positioning competes with **LiteLLM / Portkey / Helicone** in the "one credential + one quota + one observability surface across providers" market — loomcycle already has the resolver + provider auth + retry + host allowlist + tier policy infrastructure; this release exposes it as a first-class wire surface.

### New endpoint: `POST /v1/_llm/chat`

Bearer-authed admin surface, same `LOOMCYCLE_AUTH_TOKEN` as every `/v1/_*` route. Both `stream: false` (single JSON response) and `stream: true` (SSE) selected by the request body.

```jsonc
// Request
{
  "messages": [
    { "role": "system", "content": "You are helpful." },
    { "role": "user", "content": "What is 2+2?" }
  ],
  "tools": [{"name":"calc", "description":"math", "input_schema":{...}}],
  "max_tokens": 4096,
  "stream": false,
  "provider": "anthropic",  // optional — see routing precedence
  "model": "claude-sonnet-4-6",
  "tier": "default",
  "user_id": "alice"        // per-user quota tracking
}
```

```jsonc
// Non-streaming response
{
  "id": "llm_abc",
  "request_id": "req_xyz",
  "provider": "anthropic",     // what the resolver actually picked
  "model": "claude-sonnet-4-6",
  "content": [
    {"type":"text", "text":"5 * 7 = 35"}
    // OR a tool call:
    // {"type":"tool_use", "id":"call_x", "name":"calc", "input":{"expr":"5*7"}}
  ],
  "stop_reason": "end_turn",
  "usage": {"input_tokens":1234, "output_tokens":56, "cache_read_input_tokens":0}
}
```

Streaming SSE mirrors Anthropic's event names — `provider_chosen` (gateway-specific; emitted first), `content_block_start`, `content_block_delta`, `content_block_stop`, `message_delta`, `done`, `error`. Operator-familiar; consumers (the TS adapter's `llmStream` method, LangChain BaseChatModel implementations) map cleanly.

### Routing precedence

The resolver applies request hints in this order:

1. **Both `provider` AND `model` set** — explicit pin; resolver short-circuits. Useful when the consumer knows the answer.
2. **`provider` only** — resolver picks the best model within that provider for `tier` / `user_tier`.
3. **`model` only** — resolver picks the provider hosting that model.
4. **Neither** — full resolver pick using `tier` (defaults to "default") / `user_tier`.

The chosen `provider` + `model` are echoed in the response + the `provider_chosen` SSE frame so consumers can log / display the decision.

### Tool calling — zero new translation code

The substrate's existing per-provider `buildRequestBody()` helpers (Anthropic pass-through, OpenAI `{type:"function", function:{parameters:input_schema}}` wrap, Gemini `sanitizeGeminiSchema` + `function_declarations[]` nesting) consume `providers.ToolSpec{Name, Description, InputSchema}` directly. The gateway forwards every wire-shape `tools[]` entry to the chosen driver as a `ToolSpec` — drivers translate to provider-native shapes inside the existing per-driver code. **No new shared translation package; no provider-interface changes.**

For tool results — gateway accepts `role:"tool"` messages with `tool_call_id`; the translator maps these to `{type:"tool_result", tool_use_id, text}` ContentBlocks before handing to the driver.

### Authentication + quotas

Bearer-authed admin scope. When `user_id` is in the request, the existing `concurrency.Semaphore.AcquireForUser` per-user cap applies (see `help fairness` for the policy). Anonymous calls bypass the per-user cap but still count against the global semaphore.

### Audit + observability (v0.11.0 posture)

Each gateway call emits a structured log line on completion:

```
llm_gateway: request_id=req_abc provider=anthropic model=claude-sonnet-4-6 \
  tier="default" user_id="alice" input_tokens=1234 output_tokens=56 \
  stop_reason=end_turn latency_ms=842 status=ok err=""
```

Scrape via `journalctl` / log shippers. When OTEL is configured (see `help observability`), the `provider.Call` path already emits `loomcycle.provider.call` spans with the standard attributes — gateway calls show up there alongside agent runs.

**Why no `/v1/_events` audit row in v0.11.0:** the events table has a NOT NULL FK to runs, and we don't want to fake phantom run rows per gateway call (would pollute the runs table; n8n workflows fire dozens of gateway calls per execution). v0.11.1 follow-up will add a dedicated `gateway_events` table with its own `GET /v1/_gateway_events` query surface.

### Architectural decisions (locked)

- **No `runs`-table row per gateway call** — gateway is too high-cardinality.
- **No cross-provider mid-call fallback in v1** — single-shot per call; same-provider rate-limit retry inside the driver still applies. Cross-provider on a fresh-call retry can land in v0.11.1+ if demand emerges.
- **No new shared `internal/llm/` package** — the gateway handler lives in `internal/api/http/` and calls existing providers + resolver directly. Adding a service-layer abstraction now would be speculative.

### TS adapter

`@loomcycle/client@0.11.0` adds two methods:

```typescript
async llmChat(opts: LLMChatOptions): Promise<LLMChatResponse>
async *llmStream(opts: LLMChatOptions): AsyncIterable<LLMChatStreamItem>
```

Plus 8 new exported types (LLMChatMessage, LLMTool, LLMChatOptions, LLMChatResponse, LLMChatContent, LLMChatUsage, LLMChatStreamItem, LLMChatStreamDelta, LLMChatToolCall). 10 new vitest tests covering happy-path, tool-call round-trip, streaming, bearer auth, AuthError on 401, UnavailableError on 503, AbortSignal propagation, error frame, SSE keepalive ignore.

### Wire-surface delta vs v0.10.4

| Surface | v0.10.4 | v0.11.0 |
|---|---|---|
| Go HTTP endpoints | n | n + 1 (`POST /v1/_llm/chat`) |
| Go tests | n | n + 8 |
| TS adapter methods | 39 | 41 |
| TS adapter exported types | n | n + 8 |
| Bundled `Context.help` topics | n | n + 1 (`llm-gateway`) |

### Migration notes

- **Purely additive.** Existing `/v1/runs`, `/v1/_channels`, `/v1/_*def`, `/v1/_memory/*` surfaces unchanged.
- **No schema migrations.** Gateway uses no persistent storage in v0.11.0.
- **No yaml changes required.** The gateway uses the same resolver / providers / concurrency wiring the agent runtime already does.
- **TS adapter consumers** bump to 0.11.0 to access `llmChat` / `llmStream`. Existing methods byte-identical; no breaking changes.

### Deferred (per the RFC's out-of-scope list)

- gRPC mirror RPC (v0.11.1+).
- LoomCycle MCP server `llm_chat` meta-tool (v0.11.1+).
- OpenAI-compat shim (`POST /v1/chat/completions` translating to gateway) (v0.11.1+).
- `tool_choice` field — LangChain consumers default to `auto`.
- Multi-modal content (image / audio inputs) — Anthropic + OpenAI + Gemini all have it; their shapes differ; defer to v0.11.x.
- Embeddings endpoint — separate RFC.
- Bearer-level rate limiting — operator bearer is operator-trust scope; per-user quotas cover the workflow-storm case.

### Downloads

Assets attached: `loomcycle-{darwin,linux}-{amd64,arm64}.tar.gz` + `SHA256SUMS`. Adapter via `npm install @loomcycle/client@0.11.0`.

---

## What's in v0.10.4

Web UI–only release. Adds **manual CRUD on the agent / skill / MCP-server library** to the `/ui/library` page. The HTTP mutation surface already existed since v0.8.22 (AgentDef + SkillDef substrate tools) and v0.9.x (MCPServerDef); this release wires it to the Web UI so operators don't have to curl bearer-authed endpoints to register or edit substrate entries.

### Motivation

Two concrete use cases drove this:

1. **n8n integration testing.** Test workflows need to set up specific agents / skills / MCP servers via the API as part of fixture prep — formerly that meant writing curl scripts or editing yaml + restarting. Now: open `/ui/library`, click "+ New Agent", fill the form.
2. **Docker container deployments.** Running loomcycle as a container without a writable yaml mount left operators without any way to declare substrate entries except hitting the bearer-authed HTTP endpoints directly. The Web UI now covers the gap end-to-end.

The substrate's existing op grammar (`create` / `fork` / `promote` / `retire` + MCP-only `rediscover`) covers every CRUD operation the UI needs — zero new substrate logic, zero new HTTP endpoints, zero new wire-protocol shape.

### New UX

**Per-row controls** (added to each entry's lineage tree row):
- `Edit ✎` — opens the fork modal pre-filled with the active definition body. For static-only entries the button label is `Edit (forks from yaml)`; the substrate's existing v0.8.22 bootstrap-on-first-fork mechanism auto-creates a v1 lineage root from yaml before attaching the new fork as v2.
- `Retire ⊘` — confirm prompt → `retire` op. The retired flag stays in lineage; agents stop seeing the version as active.
- `Promote ▲` — per non-active version, sets the active pointer for the name to this row.

**Page-level controls** (right-pane header per tab):
- `+ New <Flavor>` — opens the create modal.
- `Rediscover tools 🔄` (MCP servers only) — prompts for a server name, runs the `rediscover` op, refreshes the cached `discovered_tools` snapshot.

**Hybrid form per substrate flavor** (the form-style fork the user picked):
- **AgentDef** — structured: name, description, provider, model, tier, effort. JSON textarea (collapsible) for everything else with a "show schema" toggle that reveals the full field reference (system_prompt, allowed_tools, skills, providers, models, memory_*, max_*).
- **SkillDef** — structured: name, description, allowed_tools (comma-separated). Plus a large markdown body textarea (the substrate refuses empty bodies; this is the required field).
- **MCPServerDef** — structured: name, description, transport (radio: streamable-http / http; stdio refused at the substrate boundary), URL, headers (key-value row repeater). No JSON textarea — the structured fields exhaustively cover the substrate's accepted shape.

**Promote-on-save checkbox:**
- Default ON for create (operator created it, they want it active).
- Default OFF for fork (review the new version before activating).
- Matches the substrate tool defaults exactly.

**Refusal mapping (`explainServerError`):** Substrate refusals come back as HTTP 422 + `{"code":"tool_refused","error":"<human text>","tool":"..."}`. The modal pattern-matches the inner text against the substrate's stable error phrases (e.g. `matches a static cfg.`, `not allowed for dynamic registration`) and surfaces a UI-friendly message. Falls back to the raw text when the pattern doesn't match — so new server-side error text doesn't break the UI silently.

### Static-vs-dynamic posture

- **Yaml-static entries stay immutable.** The UI doesn't edit yaml files. Static-only rows expose "Edit (forks from yaml)" which creates a substrate row mirroring the yaml + a child fork — the substrate is the canonical mutable surface; yaml remains operator-managed offline.
- **Retire button only shows on non-static substrate rows.** Static synthetic rows (def_id starts with `static:`) can't be retired; the button is hidden.
- **Promote button only shows on non-active non-static non-retired substrate rows.** No-op affordances stay hidden.

### Files changed

| File | Change |
|---|---|
| `web/src/api.ts` | 5 new mutation wrappers: `createDef` / `forkDef` / `promoteDef` / `retireDef` + `rediscoverMcpServerDef`. Each one-liner over `jsonFetch` (same pattern as the v0.9.3 `listDefVersionsByName` POST helper). Cookie auth via `credentials: "same-origin"` — no token plumbing. |
| `web/src/components/LibraryEditModal.tsx` *(new)* | Generalized hybrid-form modal. Discriminated on `(kind, mode)`; per-flavor field clusters; `explainServerError` for refusal-text mapping; ESC-to-close; submit-time blocking. ~600 LOC. Visual structure mirrors `AnswerModal` in `InterruptInbox.tsx`. |
| `web/src/components/LineageTree.tsx` | Three new optional props (`onEditRow` / `onRetireRow` / `onPromoteRow`); per-row buttons with `stopPropagation` so they don't toggle content / selection on click. |
| `web/src/components/LineagePanel.tsx` | Five new optional props (`onCreateNew` / `onEditRow` / `onRetireRow` / `onPromoteRow` / `onRediscover`); `+ New <Flavor>` CTA in the right-pane header + MCP-only `Rediscover tools` button; threads row callbacks through to `LineageTree`. Empty-state also gets the create CTA so a fresh installation isn't a dead-end. |
| `web/src/pages/LibraryView.tsx` | Modal state management; per-tab `tabKind` / `tabSubstrate` / `tabEntries` derivation; handlers for create / fork / promote / retire / rediscover; refresh-on-mutation via `refreshKey` bump; wires the callbacks down to each `LineagePanel`. |
| `web/src/styles.css` | ~160 lines of CSS extending the existing `.modal-*` anchors with `.library-modal`, `.library-form-row`, `.library-json-textarea`, `.library-schema-hint`, `.library-radio-group`, `.library-headers-grid`, `.lineage-row-actions`, `.lineage-header-actions`. |
| `adapters/ts/package.json` | Version 0.10.3 → 0.10.4 (lockstep with binary tag; no method additions). |

### Wire-surface delta vs v0.10.3

| Surface | v0.10.3 | v0.10.4 |
|---|---|---|
| Go HTTP endpoints | unchanged | unchanged |
| Go tests | unchanged | unchanged |
| TS adapter methods | 39 | 39 (no new public surface) |
| Web UI pages with mutation | 4 (Memory, Snapshots, Interrupts, Hooks) | 5 (+ Library) |
| Web UI bundle size | n KB | n + ~12 KB (~3% increase) |

### Migration notes

- **No yaml changes required.** Existing deployments see zero behavior change on upgrade.
- **No wire-protocol changes.** The binary's HTTP surface is byte-identical to v0.10.2–v0.10.3 on the HTTP side; the only repo changes are under `web/` and the lockstep adapter version bump.
- **No schema migrations.** Mutations route through the already-shipped POST endpoints + substrate tables.
- **Operators with the v0.10.x web bundle already deployed** see the new buttons immediately on next page load (the embedded `internal/webui/dist/` is rebuilt with the new bundle on `make build-ui`).
- **TS adapter consumers** bump to 0.10.4 for lockstep version parity; no new methods.

### Why not yaml editing in the UI

Yaml stays operator-managed offline — file-mounted, git-tracked, ground truth. The substrate is the mutable surface; the Web UI surfaces substrate mutations only. Static-only entries get explicit "Edit (forks from yaml)" labeling so operators know the new version is a substrate row, not a yaml change.

### Downloads

No new binary tarballs (binary HTTP surface unchanged from v0.10.2). The shipped `loomcycle` binary embeds the new Web UI bundle on `make build-ui`; consumers building from v0.10.4 source pick up the new UI for free. Adapter via `npm install @loomcycle/client@0.10.4`.

---

## What's in v0.10.3

Adapter-only release. Adds three typed enumeration methods to `@loomcycle/client` that wrap the v0.9.3 Library v2 HTTP endpoints — no Go code changes, no wire-protocol changes, no schema migrations.

### Motivation

While integrating loomcycle with n8n (the workflow editor), the operator needed to populate a dropdown of "which agent should this workflow node dispatch?" — covering both yaml-static agents AND dynamically-registered AgentDefs. The HTTP endpoint to enumerate this (`GET /v1/_library/agents`, shipped in v0.9.3 as part of Library v2) already merges both sources into one envelope. But the npm-published TypeScript adapter had no typed wrapper for it — external consumers would have to drop to raw `fetch` against a path string with no IntelliSense over the response shape. This release closes that gap.

The same gap existed for skills + MCP servers (also enumerable via `/v1/_library/skills` and `/v1/_library/mcp-servers` since v0.9.3); ship all three together since the pattern is identical.

### New methods

```typescript
async listLibraryAgents(opts?): Promise<LibraryListResponse<LibraryAgentDefinition>>
async listLibrarySkills(opts?): Promise<LibraryListResponse<LibrarySkillDefinition>>
async listLibraryMcpServers(opts?): Promise<LibraryListResponse<LibraryMcpServerDefinition>>
```

Each returns the canonical Library v2 envelope: a list of entries tagged with `source: "static-only" | "dynamic-only" | "both"`, `in_static` / `in_substrate` booleans, substrate-version metadata (`version_count`, `active_def_id`, `latest_version`, `last_updated`), and the typed `static_definition` body inlined when the entry has a yaml source. Deterministic alphabetical sort by name.

### Typed `static_definition` per endpoint

The Web UI's `LibraryEntry` shape (`web/src/api.ts:825`) types `static_definition` as `unknown` because one renderer component handles all three flavors. The adapter does the opposite: each method returns a typed `LibraryEntry<T>` where `T` is the per-endpoint definition shape, so external consumers get full IntelliSense.

- `LibraryAgentDefinition` — provider, model, tier, effort, max_tokens, max_iterations, system_prompt[_base], allowed_tools, skills, providers, models (kept opaque as Record<string, unknown> for forward-compat), memory_scopes, memory_quota_bytes.
- `LibrarySkillDefinition` — body, description, allowed_tools.
- `LibraryMcpServerDefinition` — transport, url, headers, command, args, env, pool_size, allowed_tools, discovered_tools (cached PeekTools snapshot; omitted when the pool inspector returns nil).

### Test coverage

8 new tests in `adapters/ts/tests/library.test.ts` (mirrors `substrate.test.ts` pattern): GET shape + multi-source envelope, bearer-auth header forwarding, AuthError on 401, UnavailableError on 503, empty-entries (the n8n empty-dropdown case), AbortSignal propagation. Plus one happy-path each for skills + mcp-servers covering the per-flavor `static_definition` typing.

### Wire-surface counts

| Surface | v0.10.2 | v0.10.3 |
|---|---|---|
| TS adapter methods | 36 | 39 (+3 listLibrary*) |
| TS adapter exported types | n | n + 5 (LibraryEntry, LibraryListResponse, LibraryAgentDefinition, LibrarySkillDefinition, LibraryMcpServerDefinition) |
| Go HTTP endpoints | unchanged | unchanged |
| Go test count | unchanged | unchanged |

### Migration notes

- **No yaml changes required.** Existing deployments see zero behavior change on upgrade.
- **No wire-protocol changes.** v0.10.3 binary is byte-identical to v0.10.2 binary on the HTTP surface (the only repo changes are under `adapters/ts/`). Operators who don't use the TypeScript adapter can stay on v0.10.2 indefinitely.
- **TS adapter consumers** bump `@loomcycle/client` to 0.10.3 to pick up the new methods. Existing methods are byte-identical; no breaking changes.
- **n8n-nodes-loomcycle** bumps its `@loomcycle/client` pin to 0.10.3 + swaps any `loadRecentAgentNames` lookup to `client.listLibraryAgents()` whose response is source-tagged for richer dropdown rendering. The n8n-side change ships in a separate PR on the n8n-nodes-loomcycle repo.

### Why not a new `GET /v1/_agents` endpoint

The original proposal was a new slim Go endpoint. After exploring the codebase, the existing `/v1/_library/agents` (v0.9.3 Library v2) already returns exactly the proposed shape — name + source + static_definition + version metadata — for both yaml-static AND dynamic AgentDefs. Adding a second endpoint would create two surfaces that look almost identical, with source-of-truth ambiguity. The cleaner fix is the adapter-side wrapper that lets external consumers call the existing endpoint with typing.

### Downloads

No new binary tarballs (no Go changes). The `loomcycle` binary stays on v0.10.2. Pull this adapter via `npm install @loomcycle/client@0.10.3`.

---

## What's in v0.10.2

Three independent items closing v0.9.x loose ends. Bundled as v0.10.2 to keep the v0.10.x roadmap clean before the larger remaining slices (multi-replica HA via Redis cancel pubsub, in-memory run-status cache).

### What's new

**Voyage AI embedder** — replaces the `provider: anthropic` stub that returned `embedder_not_implemented` in v0.9.0–v0.10.1. Anthropic has no native embedding API and explicitly recommends Voyage AI; the operator yaml stays `provider: anthropic` for ergonomics, but the underlying HTTP calls now go to Voyage's `/v1/embeddings` endpoint with `Authorization: Bearer $VOYAGE_API_KEY`. Same wire shape as OpenAI's embedder (Voyage's API is deliberately OpenAI-compatible).

```yaml
memory:
  embedder:
    provider: anthropic           # routes to Voyage AI under the hood
    model: voyage-3               # see model menu below
    batch_size: 128               # Voyage caps voyage-3 family at 128
```

```sh
export VOYAGE_API_KEY=...         # NEW (separate from ANTHROPIC_API_KEY)
```

`voyageEmbeddingDims` covers the canonical model menu:

| Model family | Models | Default dim |
|---|---|---|
| Current (voyage-4) | voyage-4, voyage-4-large, voyage-4-lite, voyage-4-nano | 1024 |
| Domain-specific | voyage-code-3, voyage-finance-2, voyage-law-2 | 1024 |
| Legacy (back-compat) | voyage-3, voyage-3-large, voyage-multilingual-2 | 1024 |

Per-attempt timeout (not per-batch) — each retry attempt gets a fresh deadline so a `Retry-After: 30s` from a 429 doesn't silently neuter retries even when `timeout: 10s`. The outer ctx still applies as the absolute ceiling.

7 new unit tests against a synthetic `httptest.Server`: happy path, auth header, batching across calls, index-based reorder, dimension mismatch detection, HTTP 5xx surface, missing model construction refusal, `providers.NewEmbedder("anthropic", ...)` registration round-trip.

**sqlite-vec build mechanism** — architectural opt-in for SQLite Vector Memory. Default build is unchanged: pure-Go `modernc.org/sqlite`, no CGO, single static binary, vector ops refuse with `vector_unsupported`. Operators wanting vectors on SQLite build with:

```sh
brew install sqlite-vec                 # or apt install libsqlite3-mod-vec
export LOOMCYCLE_SQLITE_VEC_PATH=$(brew --prefix sqlite-vec)/lib/vec0
CGO_ENABLED=1 go install -tags=sqlite_vec github.com/denn-gubsky/loomcycle/cmd/loomcycle@v0.10.2
```

The tag swaps the driver to `github.com/mattn/go-sqlite3` (CGO) and registers a custom `sqlite3_loomcycle_vec` driver with a `ConnectHook` that calls `LoadExtension(LOOMCYCLE_SQLITE_VEC_PATH, "")` on every new connection. Boot log confirms the build-tag choice:

```
sqlite: sqlite_vec build active — extension path=/opt/homebrew/opt/sqlite-vec/lib/vec0 (MemoryEmbed* implementation lands in v0.10.3; SupportsVectors() still false until then)
```

**The actual `MemoryEmbed*` methods are still stubbed in v0.10.2** — `SupportsVectors()` returns `false` regardless of build tag. The build mechanism is the architectural commitment; the full vec0 virtual-table schema design (per-dimension partitioning vs single-table-with-aux-columns) lands in v0.10.3 after benchmarking against real workloads. Operators selecting `-tags=sqlite_vec` today get a CGO binary that LOADS the extension but doesn't USE it for vector ops yet.

Release tarballs stay at 4 (default-only). Operators wanting sqlite-vec build locally — adding cross-platform CGO compilation to the goreleaser pipeline is a separate (substantial) infrastructure change.

File factoring:

- `internal/store/sqlite/sqlite.go` — driver-agnostic; `Open()` calls `openDB()`.
- `internal/store/sqlite/driver_default.go` (`//go:build !sqlite_vec`) — modernc import + `sql.Open("sqlite", ...)`.
- `internal/store/sqlite/driver_vec.go` (`//go:build sqlite_vec`) — mattn import + `sql.Register("sqlite3_loomcycle_vec", ...)` with the ConnectHook.
- `internal/store/sqlite/memory_embeddings.go` (`//go:build !sqlite_vec`) — existing refusal stubs.
- `internal/store/sqlite/memory_embeddings_vec.go` (`//go:build sqlite_vec`) — new file with `SupportsVectors()=false` + `errVecImplPending` returned from all MemoryEmbed* methods.

**Heartbeat-sweeper test flake fix** — `TestSweeperRun_LogsResults` in `internal/heartbeat/sweeper_test.go` used an 80ms fixed sleep to wait for ≥2 sweeper ticks at a 10ms interval. Under `-race` (CI), the scheduler's 2-5x slowdown can push past the budget — flaked once on PR #190's CI run. New `waitForLogContaining` helper polls the captured-log slice under the existing mutex with a 2-second deadline. Same pattern as PR #195's `waitForActive` helper. 10 race iterations clean (`go test -race -count=10`).

### Adapter releases

- **`@loomcycle/client` 0.10.1 → 0.10.2** (npm) — version bump for binary-tag-to-adapter-version lockstep enforced by `publish-ts-adapter.yml`. No method changes.
- **`loomcycle` Python** held at 0.7.0.

### Wire-surface counts

| Surface | v0.10.1 | v0.10.2 |
|---|---|---|
| Embedder drivers | 3 (openai, gemini, anthropic-stub) | 3 (openai, gemini, **anthropic→Voyage**) |
| Env vars | b | b + 1 (`VOYAGE_API_KEY`) |
| Build tags | platform-only | platform-only + **`sqlite_vec`** |
| Bundled help topics | n | n + 2 (`voyage-embedder`, `sqlite-vec`) |
| MCP meta-tools | 33 | 33 (no change) |
| gRPC RPCs | n | n (no change) |
| TS adapter methods | 36 | 36 (no change) |

### Migration notes

- **No schema migrations required.** Purely additive.
- **No yaml changes required for back-compat.** The Anthropic embedder slot was non-functional in v0.10.1; operators who had `provider: anthropic` set but were getting refusals now get working Voyage embeddings as long as `VOYAGE_API_KEY` is set.
- **Operators newly setting `provider: anthropic`** need to set `VOYAGE_API_KEY` separately from `ANTHROPIC_API_KEY` (the latter stays for chat completions).
- **SQLite operators wanting vector ops** continue to get `vector_unsupported` on the default build. The `-tags=sqlite_vec` opt-in is the only path; full functionality lands in v0.10.3.
- **TS adapter consumers**: bump `@loomcycle/client` to 0.10.2 if you tag the binary to v0.10.2 (lockstep). No code changes required.

### Code review fixes

A parallel code-reviewer agent run caught 4 findings, all fixed in commit `458238a` before merge:
1. **Critical** — Voyage timeout wrapped the entire `ratelimit.Do` call rather than each attempt; long `Retry-After` would silently neuter retries. Fixed by moving timeout INSIDE the attempt closure.
2. **Critical** — `memory_embeddings_vec.go` originally returned `SupportsVectors()=true` which routed the storetest contract suite into round-trip tests that would fail with `errVecImplPending` rather than the expected `ErrVectorUnsupported`. Fixed by returning false until v0.10.3 wires the real implementation; added a boot log line so operators still see the build-tag confirmation.
3. **Important** — `voyageEmbeddingDims` only had legacy voyage-3 models; operators following Voyage's current recommendations and configuring `voyage-4` got `Dimension()=0` (silently skipping the in-response sanity check). Added voyage-4 family + domain models.
4. **Important** — `vecDriverRegErr` dead var with linter-silencing `_ = vecDriverRegErr`. Removed.

### Downloads

Assets attached: `loomcycle-{darwin,linux}-{amd64,arm64}.tar.gz` + `SHA256SUMS`.

---

## What's in v0.10.1

Per-tenant fairness on the run-admitting semaphore. Second slice of the v0.10.x production-grade-ops sweep. Closes the multi-tenant starvation case operators hit when one user submits a burst large enough to fill the global queue — without fairness, every other user's run waits behind the burst even when the noisy user is plainly hogging the substrate.

The cap is **off by default** — existing deployments see zero behavior change on upgrade. Operators opt in by setting `LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER=4` (or yaml `concurrency.max_concurrent_runs_per_user: 4`).

### What's new

**The cap measures active + queued, not just active.** Load-bearing semantic — without including the queued count, a noisy user could fill the queue with their own runs while at active-cap and starve everyone else for the queue's lifetime. With active+queued counted, the queue stays available for other users.

**Check order at run admission:**

1. Per-user cap. If the user is at cap, return 429 immediately (no queue, no wait).
2. Global active. Take the slot if there's an open one.
3. Global queue. Enqueue if there's room.
4. Backpressure. Both queue full → 429 with `code: "backpressure"`.

The two 429 flavors share status but distinguish via the JSON body's `code` field:

| Code | When | Retry strategy |
|---|---|---|
| `per_user_quota_exhausted` | This specific user is at cap | Wait `Retry-After` seconds (server hint: 5), then retry. Deterministic — your in-flight runs complete on a schedule. |
| `backpressure` | Whole substrate is overloaded | Exponential backoff with jitter. The wait depends on system-wide load. |

**Anonymous calls bypass the check.** Requests without `user_id` (system-initiated, background ops, yaml callers that omit it) skip per-user accounting by design. The counter is keyed on non-empty user_id.

**Sub-agents don't double-count.** Sub-runs spawned via the Agent tool share the parent's semaphore slot AND the parent's user_id count. A parent run by `user_a` that spawns 5 cv-adapter children consumes 1 slot + 1 per-user count.

**New `Semaphore.WithPerUserCap(n)` fluent setter** — chained after the existing `concurrency.New(...)` call. Back-compat for the 58 existing `New(...)` callers (mostly tests).

**New typed errors:**

- `concurrency.ErrPerUserQuotaExhausted` (Go) — fields: `UserID`, `Cap`. Implements `Code() string = "per_user_quota_exhausted"`.
- `runner.ErrPerUserQuotaExhausted` (Go, wire-agnostic sentinel) — for the connector boundary; mirrors `ErrBackpressure`.
- `PerUserQuotaExhaustedError` (TS adapter) — `userId`, `cap`, `retryAfterMs` fields populated from the JSON body + `Retry-After` header.

**New `GET /v1/_concurrency/stats` admin endpoint** — bearer-authed sister of the `/v1/_metrics/*` family. Returns:

```json
{
  "active": 5,
  "queued": 0,
  "per_user": {"user_a": 4, "user_b": 1}
}
```

`per_user` is omitempty — the field is absent when no per-user activity has happened. Operators check liveness with a single curl. When the semaphore isn't wired (test embeds), 503 + `code:"concurrency_not_wired"` so probes distinguish "not configured" from "broken."

**New `loomcycle.queue_wait_ms` span attribute** on the top-level `loomcycle.run` span. Measured around each `AcquireForUser` call at the three run-creation sites. With OTEL traces from v0.10.0, operators graphing this attribute by `loomcycle.user_id` in Jaeger / Tempo / Honeycomb / DataDog validate that fairness is engaging — queue waits should distribute across users instead of all landing on whoever's behind a noisy tenant.

**New bundled `Context.help` topic `fairness`** (~180 lines). Covers the JobEmber starvation case, active+queued semantics, retry-strategy guidance, validation via `/v1/_concurrency/stats` + OTEL, choosing a cap value, and the explicit non-goals.

### gRPC mapping

Both `ErrBackpressure` and `ErrPerUserQuotaExhausted` map to `codes.ResourceExhausted` on the gRPC wire. HTTP distinguishes the two via the JSON body's `code` field + `Retry-After` header; gRPC consumers branch on the error message string if they need to distinguish.

### Adapter releases

- **`@loomcycle/client` 0.10.0 → 0.10.1** (npm) — adds `PerUserQuotaExhaustedError` typed class. Two new tests confirm 429 + `code:"per_user_quota_exhausted"` body routes to the new class with populated `userId`/`cap`/`retryAfterMs`, and that the existing `code:"backpressure"` shape still routes to `BackpressureError`. 110 → 112 tests.
- **`loomcycle` Python** held at 0.7.0.

### Wire-surface counts

| Surface | v0.10.0 | v0.10.1 |
|---|---|---|
| HTTP endpoints | n | n + 1 (`/v1/_concurrency/stats`) |
| Typed errors (HTTP) | k | k + 1 (`per_user_quota_exhausted` JSON code) |
| Typed errors (TS adapter) | m | m + 1 (`PerUserQuotaExhaustedError`) |
| Span attributes | a | a + 1 (`loomcycle.queue_wait_ms`) |
| Env vars | b | b + 1 (`LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER`) |
| Yaml fields | c | c + 1 (`concurrency.max_concurrent_runs_per_user`) |
| MCP meta-tools | 33 | 33 (no change) |
| gRPC RPCs | n | n (no change — gRPC maps to existing `ResourceExhausted`) |
| TS adapter methods | 36 | 36 (no change — new error class is wire-side only) |

### Migration notes

- **No schema migrations required.** No new tables, no envelope sections. The fairness surface is purely additive.
- **No yaml changes required for back-compat.** The new `concurrency.max_concurrent_runs_per_user` key defaults to 0 (= disabled). Existing yaml files work as-is.
- **TS adapter consumers**: bump `@loomcycle/client` to 0.10.1 if you tag your loomcycle binary to v0.10.1 (release lockstep enforced by `publish-ts-adapter.yml`). Existing consumers catching `BackpressureError` for 429s keep working — the new typed error is for consumers wanting to branch retry strategies.
- **gRPC consumers** see no change: both backpressure flavors share `codes.ResourceExhausted`. Branch on error message string when distinguishing is needed.
- **Choosing a cap**: pragmatic starting point is `MaxConcurrentRunsPerUser ≈ MaxConcurrentRuns / 2`. See `help fairness` for the full guidance.

### What this slice deliberately doesn't include (deferred)

- **Queue-reorder fairness.** When the global queue is non-empty, FIFO order applies regardless of per-user counts. Hard cap solves the starvation case; reorder is a smaller follow-up win not worth bundling.
- **Per-tier fairness.** The `user_tier` field on `RunIdentity` could drive a tier-aware quota (free=2, paid=10). Defer until a consumer asks.
- **Dynamic cap updates without restart.** Cap is read at boot; `POST /v1/_concurrency/limits` would close this gap; defer.

### Downloads

Assets attached to this release: `loomcycle-{darwin,linux}-{amd64,arm64}.tar.gz` + `SHA256SUMS`.

---

## What's in v0.10.0

Production-grade observability lands. Loomcycle emits OpenTelemetry distributed traces for every agent run — operators see latency p99s, cost-per-provider attribution, and span-level error rates within seconds of opening Jaeger / Grafana Tempo / Honeycomb / DataDog APM. The v1.0 release gate moves from "implementation complete" to "instrumented + observable under load."

This is the first of the v0.10.x production-sweep slices. Per-tenant fairness on the semaphore, multi-replica HA via Redis cancel pubsub, and the in-memory run-status cache follow in subsequent v0.10.x releases.

### What's new

**Span tree per agent run.** Every run emits a hierarchical trace:

```
loomcycle.run
├─ loomcycle.iteration (one per loop turn)
│  ├─ loomcycle.provider.call (provider, model, tier, effort)
│  └─ loomcycle.tool.call (tool name)
│     └─ loomcycle.mcp.call (mcp_server, mcp_tool) — when applicable
└─ done attrs (input_tokens, output_tokens, cache_read_tokens, stop_reason)
```

Sub-agent spawns nest as children of the parent's iteration span via context propagation — operators see the full `cv-batch-adapter → cv-adapter → ...` tree in one Jaeger view, no per-replica linking needed.

**Default OFF.** When `LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT` is unset, the global tracer is a no-op. Every `tracer.Start(ctx, name)` call across the codebase returns the SDK's no-op span singleton — zero allocations, zero goroutines, zero atomic operations in the hot path beyond a single `otel.globalTracer` pointer load. The load-bearing assumption is that operators who haven't opted in pay nothing.

**Five env vars** to opt in:

| Variable | Default | Purpose |
|---|---|---|
| `LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT` | empty | Empty disables. `host:port` or `http(s)://host:port` — `http://` triggers `WithInsecure` automatically. |
| `LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS` | empty | Comma-separated `key=value` list for collector auth (e.g. `x-honeycomb-team=$KEY`). |
| `LOOMCYCLE_OTEL_SERVICE_NAME` | `loomcycle` | `service.name` resource attribute. Override per replica for multi-replica HA grouping. |
| `LOOMCYCLE_OTEL_TRACES_SAMPLER_RATIO` | `1.0` | Head-based sampling ratio, clamped to `[0,1]`. `ParentBased(TraceIDRatioBased(r))` so sampled parents always produce sampled children — traces stay whole. |

**Provider driver instrumentation.** Each of the four cloud providers (Anthropic, OpenAI, Gemini, Ollama — both ollama-cloud and ollama-local) opens one `loomcycle.provider.call` span per HTTP attempt inside its `attempt` closure. The closure runs under `ratelimit.Do` which retries on 429/5xx — each retry surfaces as a sibling span, giving operators clean retry-latency visibility. Spans stamp `Error` status on Go-level errors AND HTTP non-2xx responses.

**DeepSeek wrapping** uses a ctx-key provider-override (`lcotel.WithProviderOverride`) instead of a wrapping span. The wrapping driver-call returns before the streaming channel is consumed, so a span there would mismeasure latency. Instead, the inner OpenAI driver reads the override from ctx and stamps `loomcycle.provider="deepseek"` on its per-attempt span. Operators filtering by provider see DeepSeek calls distinctly with correct streaming-attempt durations.

**Tool dispatcher + MCP nesting.** `Dispatcher.Execute` is the single canonical wrap-point for every dispatched tool (built-in + MCP + sub-agent). One `loomcycle.tool.call` span per dispatch. MCP tools open a nested `loomcycle.mcp.call` inside the outer tool span — operators see two nested spans for any MCP-tool invocation, with `mcp_server` + `mcp_tool` attributes parsed from the dispatched name. Errors at all three sites (unknown tool, Go-level Execute error, IsError=true result) mark the span Error.

**What spans deliberately don't carry.** No transcript bodies, no tool inputs, no tool results, no system prompts, no user prompts, no API keys, no header values. Span attributes go to operator-side telemetry endpoints (Honeycomb, DataDog, etc.) which have different trust postures than loomcycle's bearer-auth — keeping secrets out of spans means opting into tracing doesn't widen the secret surface. The transcript view at `/ui/agents/<id>` stays the authoritative record of agent content.

**UTF-8-safe truncation** for span error messages. Loomcycle's `SetSpanError` and `SetSpanErrorMessage` cap error text at 500 bytes; the truncate helper backs the cut to the nearest preceding rune boundary so non-ASCII provider error messages (DeepSeek's Chinese status messages, Anthropic's Unicode JSON error bodies) never split mid-rune.

### Operator setup walkthroughs

The new bundled `Context.help` topic `observability` covers four concrete setups:

- **Local Jaeger via Docker** — `docker run jaegertracing/all-in-one:latest`, set the endpoint env var, open `http://localhost:16686`. Fastest path to first trace.
- **Grafana Tempo + Grafana** — production-grade open-source alternative. Includes minimum docker-compose and tempo.yaml.
- **Honeycomb** — hosted SaaS option. Free tier (~20M events/month) covers a JobEmber-class workload at ~0.05 sampling. Uses the `x-honeycomb-team` header.
- **DataDog APM** — set the local DataDog Agent's `otlp_config` and point loomcycle at `127.0.0.1:4318`. Same OTLP/HTTP wire.

### Adapter releases

- **`@loomcycle/client` 0.9.3 → 0.10.0** (npm) — no method additions; no wire-shape additions. Version bump for binary-tag-to-adapter-version lockstep. The OTEL surface is server-side telemetry; consumers don't interact with it through the adapter.
- **`loomcycle` Python** held at 0.7.0 — no Python-side surface change.

### Wire-surface counts

| Surface | v0.9.3 | v0.10.0 |
|---|---|---|
| HTTP endpoints | n | n (no change) |
| MCP meta-tools | 33 | 33 (no change) |
| gRPC RPCs | n | n (no change) |
| TS adapter methods | 36 | 36 (no change) |
| Env vars | n | n + 4 (OTEL exporter / headers / service-name / sampler-ratio) |

### Migration notes

- **No schema migrations are required.** No new tables, no envelope sections. The OTEL surface is purely additive.
- **No yaml changes required.** The `agents:` / `mcp_servers:` / `memory:` blocks are unchanged. A new commented-out OTEL section in `loomcycle.example.yaml` documents the env vars.
- **Default-OFF posture means existing deployments see zero behavior change** on upgrade. To enable, set the endpoint env var and restart.
- **Trace tree breaks across replicas** for sub-runs spawned on a different loomcycle instance. This is expected — multi-replica HA ships in a later v0.10.x slice. Single-replica deployments see the full tree.
- **TS adapter consumers**: bump `@loomcycle/client` to `0.10.0` if you tag your loomcycle binary to v0.10.0 (release lockstep enforced by `publish-ts-adapter.yml`). No code changes required.

### Downloads

Assets attached to this release: `loomcycle-{darwin,linux}-{amd64,arm64}.tar.gz` + `SHA256SUMS`.

---

## What's in v0.9.3

Two coordinated themes plus four follow-up fixes. The headline is **Web UI Library v2** — the `/ui/library` surface stops being substrate-only and shows every agent / skill / MCP server the runtime knows about, with STATIC / DYNAMIC source chips and inline content expansion. The second theme is the **static-vs-dynamic resolver consolidation** that turned PRs #184/#185/#186 into a canonical `internal/lookup` package + a four-rule contract for future substrates. The follow-ups close a latent UI dead-body limitation, fix sub-agent spawn name resolution, and disambiguate the transcript USER/SYSTEM cards that PR #171 (v0.9.1) shipped duplicating content.

### What's new

**Web UI Library v2** (PRs #191 + #193 + main commits 21b2e2e + 21cc512). The shipped `/ui/library` surface (PR #187) enumerated only substrate rows. Two operator complaints surfaced immediately: static yaml-only entities (the operator's bread and butter) were invisible, and static MCP servers' tool lists (cached in `Pool.entry.tools`, never persisted) had no path to the UI. v2 closes both:

- **Three new bearer-authed read-only endpoints**: `GET /v1/_library/agents`, `GET /v1/_library/skills`, `GET /v1/_library/mcp-servers` — each merges cfg-side + substrate-side views into one envelope per entry. The existing `/v1/_*def/names` endpoints stay byte-identical so external adapter consumers see no breakage.
- **Source taxonomy**: every entry carries `source` (`static-only` | `dynamic-only` | `both`) + `in_static` + `in_substrate` booleans + an optional `static_definition` payload. The UI renders STATIC and DYNAMIC chips at the name level; bootstrapped entities (existing in both) get both chips.
- **Whole-row click toggles content** (commit 21cc512): clicking anywhere on the row in the lineage tree expands or collapses the definition body inline. Multiple rows can be open simultaneously — operators inspecting a fork chain can visually diff v3 vs v4 without re-clicking. The tree caret keeps its own click handler with `stopPropagation`. Full keyboard a11y: `role="button"`, `tabIndex=0`, `aria-expanded`, Enter / Space.
- **Static MCP tools surface**: new `Pool.PeekTools(name) []ToolDescriptor` snapshot accessor on `internal/tools/mcp` + an `MCPPoolInspector func(name) json.RawMessage` typedef + `SetMCPPoolInspector` setter on the Server. `cmd/loomcycle/main.go` wires a closure that marshals into the substrate-mirror shape (`[{name, description, input_schema}]`), so the wire shape is uniform across static + dynamic MCP servers. Per-tool pill expansion shows the description + pretty-printed JSON schema.
- **Diagnostic empty-state**: when an MCP server's `discovered_tools` is absent on the wire (handshake failed, init pending, or `rediscover` not called for a substrate row), the UI renders a hint pointing operators at the loomcycle log for `mcp[<name>]: handshake failed` lines instead of silently omitting the section.
- **Static stdio MCP rendering**: stdio servers from `cfg.MCPServers` now render with `command` / `args` / `env` / `pool_size` alongside http servers' `url` / `headers`. The redactor widens from Authorization-only to env vars matching `*_TOKEN` / `*_KEY` / `*_SECRET` / `*_PASSWORD` / `*_CREDENTIAL` / `*_AUTH`.

**Static-vs-dynamic resolver consolidation** (PR #188 + PR #189 — full retrospective in [`doc-internal/static-vs-dynamic-equalization.md`](../loomcycle-internal/doc-internal/static-vs-dynamic-equalization.md)). Pre-v0.8 loomcycle had ONE load path for `config.AgentDef`: yaml → `config.LoadConfig` → `resolveSkills` / `resolveAgent` → `cfg.Agents`. v0.8.15 added `dynamic_agents` (`RegisterAgent` path) and v0.8.22 added the AgentDef substrate (`agent_defs` + `agent_def_active`). Both new READ paths skipped the boot-time normalizer chain, leaving `SystemPromptBase` empty on every runtime-resolved agent. The same drift class produced multiple symptoms patched piecemeal by PRs #184 (lookupAgent didn't consult `agent_def_active`; substrate-registered names 404 on `/v1/runs`) + PR #185 (a misleading "skills not loaded" error when substrate had the skill but not under that name) + PR #186 (`resolveSkillBodiesForRun` rebuilt `SystemPrompt = SystemPromptBase + skill bodies` and started from `""`, silently erasing the agent's instructions on every skill-enabled run).

PR #188 consolidates: new `internal/lookup` package with canonical `Agent` / `Skill` / `MCPServer` resolvers + `Substrate*` json-tagged adapter structs (the AgentDef-side close of the latent JSON-tag mismatch where `config.AgentDef` has yaml-only tags but the substrate persists snake_case via `mergedDef`) + a `NormalizeAgentDef` read-side normalizer + `mergedDef.normalize()` write-side fix + a `BackfillAgentDefSystemPromptBase` boot-time backfill for legacy rows. Equivalence test (`TestAgent_EquivalenceYamlVsSubstrate`) pins yaml-vs-substrate parity at CI time; reflection-based drift test pins json-tag coverage so a future field added to `mergedDef` without a matching `SubstrateAgentDef` entry fails CI rather than silently dropping. Documented in `internal/lookup/README.md`.

PR #189 closes the last missed call site: `runSubAgent` (the closure the Agent built-in tool calls for sub-agent spawns) was still doing `s.cfg.Agents[name]` directly, so a yaml parent could not spawn a child registered via `RegisterAgent`. Production symptom: `cv-batch-adapter` (yaml) trying to spawn N `cv-adapter` (dynamic) children — every spawn surfaced `unknown sub-agent` as an IsError tool_result and no CV adaptation happened end-to-end. The fix routes through `lookup.Agent`; a new regression test pins it.

**Transcript USER/SYSTEM card disambiguation** (PR #190). The v0.9.1 `user_input` event payload is the full `[]loop.PromptSegment` array. For sub-agent spawns + the run-creation paths that prepend a system segment for provider wire-shape reasons, that array contains `{role:"system", content: <agent.SystemPrompt>}` followed by `{role:"user", content: <actual prompt>}`. The Web UI's USER card mapped over ALL segments — so it led with the agent's system prompt before showing the actual user content, duplicating exactly what the SYSTEM card surfaces separately. The fix filters `role === "system"` segments out of the three `user_input` renderer branches in `AgentDetailPane.tsx` + the matching one in `TerminalTranscript.tsx`. Backend persistence stays as-is: replay (`server.go:2378` already filters role at message-reconstruction time) and external transcript consumers (TS adapter, snapshot/restore) need the full segs preservation.

**Substrate list-op completes** (PR #192). The three substrate `op:"list"` response builders (`rowResponseMap` / `skillDefRowResponseMap` / `mcpServerDefRowResponseMap`) omitted the persisted `definition` field — so `row.definition` was undefined on every wire response. The pre-existing UI side panel (PR #187) had the same dead-body problem; it only became user-visible with v2's inline content expansion, when operators explicitly click a row's content chevron and see "...nothing happens." The fix adds `"definition": row.Definition` to all three response maps. `json.RawMessage` marshals verbatim; no new round-trips. Same root cause produces the "MCP discovered_tools pills show no tool names" report — with `body.discovered_tools` undefined for substrate MCP rows, no pills rendered.

### Adapter releases

- **`@loomcycle/client` 0.9.2 → 0.9.3** (npm) — no method additions; no wire-shape additions. Version bump for binary-tag-to-adapter-version lockstep. The Library v2 surface is read-only and isn't routinely consumed from JS adapters today (it's a Web UI concern); when adapters need it, the existing `jsonFetch` pattern handles the new endpoints without typed wrappers.
- **`loomcycle` Python** held at 0.7.0 — no Python-side surface change in v0.9.3.

### Wire-surface counts

| Surface | v0.9.2 | v0.9.3 |
|---|---|---|
| HTTP endpoints (admin read) | n | n + 3 (`/v1/_library/{agents,skills,mcp-servers}`) |
| MCP meta-tools | 33 | 33 (no change) |
| gRPC RPCs | n | n (no change) |
| TS adapter methods | 36 | 36 (no change) |
| Substrate list-op response fields | 11 | 12 (+`definition`) |

### Migration notes

- **No schema migrations are required.** No new tables, no envelope sections. The substrate list-op now includes the `definition` field — additive, backwards-compatible for any consumer that ignored extra fields.
- **No yaml changes required.** The `agents:` / `mcp_servers:` blocks are unchanged. Library v2 enumerates existing yaml entries; no new keys.
- **MCP server cache surface**: operators noticing "no tools cached" in `/ui/library/mcp-servers` for static yaml MCP servers should check the loomcycle boot log for `mcp[<name>]: handshake failed` lines — the UI's empty-state hint now points there. Pool init is lazy + retried with backoff; if a server is slow to start, the cache populates after handshake succeeds.
- **TS adapter consumers**: bump `@loomcycle/client` to `0.9.3` if you tag your loomcycle binary to v0.9.3 (release lockstep enforced by `publish-ts-adapter.yml`). No code changes required.

### Downloads

Assets attached to this release: `loomcycle-{darwin,linux}-{amd64,arm64}.tar.gz` + `SHA256SUMS`.

---

## What's in v0.8.16

The v0.8.x substrate arc closes with the **Interruption** tool — the human-in-the-loop primitive. Memory + Channel + AgentDef + Evaluation + Context + LoomCycle MCP + **Interruption** is the full substrate operators promised back in v0.8.0's planning.

| Surface | Status |
|---|---|
| **Interruption tool — 3 ops** | ✅ `Interruption.ask` (blocks the loop until a human answers), `notify` (fire-and-forget), `cancel` (agent unblocks an unanswered question). Per-agent ACL via `interruption: {enabled, kinds, max_pending}` yaml (default-deny). v0.8.16 supports only `kind: question`; the schema's closed-enum `kind` discriminator is forward-compatible for future `pause` / `wait_until` / `approval` kinds without reopening the design. |
| **Three delivery surfaces, one tool interface** | ✅ Operator picks via `interruption.backend:`. `webui` (default — embedded React inbox at `/ui/interrupts`), `mcp_server:<name>` (consumer's own MCP server tool), `cli` (local-dev stdin/stdout). Agent-facing surface is identical across all three. |
| **Blocking via `channels.Bus`** | ✅ `ask` blocks on `bus.Wait` with key `intr:<id>`. The resolve HTTP handler writes the row + calls `bus.Notify`; the wait wakes in O(1). Same Bus instance the v0.8.4 Channel tool uses. |
| **During-block heartbeat** | ✅ Dedicated ticker fires `Store.UpdateHeartbeat` every 30s while blocked. Without it, the v0.5.0 sweeper (default `StaleAfter` 10 min) would reap a long-pending question as a crashed run. |
| **System channels for the signal flow** | ✅ Rides on v0.8.6 `_system/*` namespace — `_system/interrupts/pending` (publish on ask) + `_system/interrupts/resolved` (publish on resolve / timeout). No new SSE event-type proliferation. |
| **Storage — new `interrupts` table (migration 0011)** | ✅ Both sqlite + postgres. 17 columns including `kind` discriminator, denormalised `user_id`/`agent_id`/`agent_name`. 8 new Store methods + 12 cross-backend contract tests. |
| **HTTP endpoints** | ✅ `POST /v1/runs/{run_id}/interrupts/{interrupt_id}/resolve` (kind-discriminated; 422 / 409 / 410 errors); `GET /v1/runs/{run_id}/interrupts`; `GET /v1/users/{user_id}/interrupts`. |
| **21st LoomCycle MCP meta-tool** | ✅ `mcp__loomcycle__interruption_resolve` exposes the resolve op so external orchestrators (Claude Code) can act as the answerer regardless of backend. Closes the v0.8.15 LoomCycle MCP capstone loop. |
| **`EventInterruptionPending` SSE event** | ✅ Run's SSE stream carries `{interrupt_id, kind, question, options, context, priority, expires_at}`. Web UI renders modals in real-time without a follow-up fetch. |
| **Web UI `/ui/interrupts` inbox** | ✅ React route polling the user-scoped listing endpoint. Answer modal supports option-list (button choices) + free-text (textarea). |
| **Tests** | ✅ 12 storage contract tests + 11 tool unit tests + 1 sentinel-error test. `go test ./...` clean. PRs #119 + #120 + #121 + #122 + this PR. |

**Origin note.** Generalised from the previously-planned "Question tool" design with a broader option set. v0.8.16 ships `ask` / `notify` / `cancel` (the original Question shape); the schema's `kind` column + the `_system/interrupts/*` channel namespace are forward-compatible for future kinds (debug step-through, scheduled wakes, approval gates) without reopening the design.

---

## What's in v0.8.15

| Surface             | Status |
|---------------------|--------|
| **`connector.Connector` interface** | ✅ New `internal/connector/` package — 20-method Go interface that every wire transport translates into. `*lchttp.Server` IMPLEMENTS it (530 LOC of method implementations in `connector_impl.go`); `*lcmcp.Server` + `*loomgrpc.Server` CONSUME it via direct method dispatch (no HTTP round-trips). Compile-time interface assertion prevents drift. TS/Python adapters mirror the same operation surface in their own languages over the HTTP wire. (PR #99) |
| **MCP server — 20 tools** | ✅ New `internal/api/mcp/` package: stdio JSON-RPC I/O loop + handshake + 20 tool handlers. **Run lifecycle:** spawn_run, cancel_run, get_run, list_runs. **Agent management:** register_agent, unregister_agent, list_agents. **Builtin wrappers:** memory, channel, agentdef, evaluation, context (pass-through to tool.Execute via Connector). **Pause/Resume:** pause_runtime, resume_runtime, get_runtime_state (PREVIEW-mocked). **Snapshot:** create_snapshot, list_snapshots, export_snapshot, restore_snapshot, delete_snapshot (PREVIEW-mocked). (PR #99) |
| **Streaming via MCP notifications** | ✅ When the client opts in via `initialize.capabilities.loomcycle.runEvents=true`, `spawn_run` drives `runner.RunOnce` directly and emits `notifications/loomcycle/run_event` per provider event before returning. Wire-ordering invariant pinned: every notification lands on stdout BEFORE the final response. Adapters rendering live agent output depend on this. (PR #99) |
| **Dynamic agent registration** | ✅ New `dynamic_agents` table (SQLite + Postgres migration 0010) + 5 Store methods + TTL sweeper. `mcp__loomcycle__register_agent` persists agents at runtime; `dynamic_agents_by_expires_at` partial index drives the sweep. Privileged tools (Bash/Write/Edit) stripped from `allowed_tools` unless `LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS=1`. Name collisions with static yaml agents rejected. (PR #99) |
| **`loomcycle mcp --config Y` subcommand** | ✅ New entry point starts BOTH the HTTP listener AND the stdio MCP loop. Logs to stderr (stdout is the JSON-RPC wire). Companion `loomcycle-mcp.sh` wrapper at repo root sources `.env.local` before exec — required because Claude Code's MCP spawn inherits an empty env, missing the `LOOMCYCLE_*` keys upstream MCP server `${...}` placeholders expect. Without the wrapper, upstream handshakes block stdio readiness for ~32s. (PR #99) |
| **gRPC server dispatches through Connector** | ✅ `internal/api/grpc/server.go` now holds BOTH a `connector.Connector` field (used by `CancelAgent` and future proto handlers) AND the existing `runner.Runner` field (streaming Run/Continue — Connector.SpawnRun is blocking-only). Legacy direct path retained when Connector is nil for backwards compat with older test fixtures. (PR #99) |
| **`operatorCtx` policy enrichment** | ✅ Code-review catch: bare-ctx dispatch to `tool.Execute` from MCP would have failed every builtin wrapper with "no scope configured" because policy values weren't on ctx. New `internal/api/mcp/context.go operatorCtx()` enriches ctx with all 5 policy values (memory/channel/agentdef/evaluation/history) + synthetic RunIdentity + AgentName="mcp-operator" before each builtin wrapper invocation. Pinned by `TestOperatorCtx_AttachesAllRequiredPolicies`. (PR #99 review cycle) |
| **3 new env vars** | ✅ `LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS` (default 0 — strip Bash/Write/Edit from dynamic agent allowed_tools), `LOOMCYCLE_DYNAMIC_AGENT_DEFAULT_TTL_SECONDS` (default 86400 — TTL when register_agent omits ttl_seconds), `LOOMCYCLE_DYNAMIC_AGENT_SWEEP_INTERVAL_MS` (default 900000 — sweeper cadence; 0 disables). Documented in `.env.example`. (PR #99, PR #100) |
| **doc-internal migration** | ✅ Internal design docs (PLAN.md, RFCs, decision history) moved from `~/work/loomcycle/doc-internal/` (in-repo, always gitignored) to `~/work/loomcycle-internal/doc-internal/` (separate operator-side repo). `.gitignore` + `CLAUDE.md` updated in lockstep; the in-repo folder deleted in PR #100. Future RFC reads/edits use the external path. (PR #100) |
| **11 MCP unit tests** | ✅ Handshake, tools/list (20 tools), spawn_run blocking + streaming, notification-before-response ordering, register_agent dispatch, unknown tool → -32601, malformed frame → -32700, sequential dispatch (5 requests), pause_runtime PREVIEW shape, operatorCtx policy contract. Plus +1 gRPC regression `TestGrpcServer_CancelAgent_DispatchesThroughConnector`. `go test -race ./...` clean across 41 packages. |
| **Sharp edges (v0.8.16 follow-ups)** | ⚠️ Boot-time upstream MCP init can block stdio readiness for ~32s if an upstream is misconfigured (`loomcycle-mcp.sh` wrapper mitigates); `loomcycle mcp` binds 127.0.0.1:8787 alongside MCP (operators can't run daemon + mcp simultaneously); Pause/Resume/Snapshot ship as PREVIEW shapes (real impl in v0.8.16+, wire is locked). |

**Operator integration recipe** — project-root `.mcp.json`:

```json
{"mcpServers": {"loomcycle": {"command": "/abs/path/to/loomcycle/loomcycle-mcp.sh"}}}
```

Or via `claude mcp add loomcycle /path/to/loomcycle-mcp.sh` (writes to `~/.claude.json`). **Note:** `~/.claude/mcp.json` is NOT a discovered location.

## What's in v0.8.14

| Surface             | Status |
|---------------------|--------|
| **Per-run MCP bearer tokens (`${run.user_bearer}`)** | ✅ Operator yaml `mcp_servers.*.headers` can now reference `${run.user_bearer}` (strict) and `${run.user_bearer:-FALLBACK}` (POSIX-style default). The HTTP MCP transport substitutes per-request inside `Client.do()` reading a ctx-carried bearer from `tools.RunIdentityValue.UserBearer`. Pool construction is unchanged so the `Client` stays shared across runs without per-run instantiation — substitution happens against a per-call local map copy, never mutating `c.headers`. (PR #94) |
| **New `user_bearer` wire field** | ✅ Added to `runRequest` + `messagesRequest` (per-request, not session-bound — continuations may rotate). Charset `[A-Za-z0-9._\-+/=]{16,512}` → 400 otherwise. Empty is backwards compat (static-bearer setups unaffected). Plumbed through `tools.WithRunIdentity` at all four attach sites (root run, sub-agent dispatch, message continuation, gRPC RunOnce). Sub-agents inherit identically — NOT narrowed, unlike caller-host policy — since the sub-agent is acting on behalf of the same end-user. |
| **Drop-header-and-WARN on missing bearer** | ✅ When `${run.user_bearer}` (no fallback) appears in a header and ctx carries no bearer, the entire header is dropped and a WARN line emitted via `log.Printf` with `tokenPrefix(bearer)` (4-char prefix + `…`) — never the full token. Downstream MCP returns a clean 401, which the agent loop surfaces as a typed tool error. Better debug signal than a literal `Bearer ${run.user_bearer}` placeholder. |
| **Nested substitution composes naturally** | ✅ `Bearer ${run.user_bearer:-${LOOMCYCLE_STATIC_BEARER}}` works during soak-phase rollouts because the existing `expandEnv` regex (`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`) structurally cannot match `${run.*}` tokens (the `.` fails the `[A-Za-z0-9_]*` char class). Inner `${LOOMCYCLE_*}` resolves at yaml-load; outer `${run.user_bearer:-<resolved>}` flows to request-time. No allowlist extension needed, no precedence configuration. |
| **Auto-version from `runtime/debug` (`--version`)** | ✅ Output shape: `loomcycle version=<v> commit=<c> built=<t> go=<g>`. Derived automatically from Go's embedded VCS stamp (`runtime/debug.ReadBuildInfo()` since Go 1.18) — no ldflags or wrapper tooling required. Release scripts can still override per identifier via `-X main.buildVersion=...`. Boot-log line carries the same identifiers so an operator running stale code spots it immediately. Module-version path surfaces clean `v0.8.14` for tagged builds, pseudo-version (`v0.8.14-0.YYYYMMDD-HASH`) for commits past a tag, and a `-dirty` suffix when the working tree was modified at build time. (PR #95) |
| **Metrics sampler /proc-counter fix** | ✅ The v0.8.11 sampler conflated `/proc`-read errors with store-write errors in a single `s.failures` counter. CI on every main commit since v0.8.11 had been silently red on `TestSampler_GracefulStoreError` (`failures = 2, want 1`) because GitHub Actions Ubuntu runners' `/proc/self/status` lacks the `VmRSS:` line. Fix: new `Sampler.procReadFailureLogged bool` — proc errors log once per program lifetime via `log.Printf` (decoupled from `cfg.Logf` which tests use as the store-write counter); `s.failures` now means exclusively "consecutive store-write failures". (PR #96) |
| **gofmt drift cleanup** | ✅ Five files (`internal/api/http/metrics_handlers.go`, `internal/api/http/server.go`, `internal/loop/fallback_test.go`, `internal/metrics/proc_linux.go`, `internal/providers/gemini/driver.go`) had accumulated whitespace / import-order / struct-tag-alignment drift across prior PRs landed before the CI gofmt check was added. Folded into PR #96 so CI lands green in one go. |
| **23 new automated tests** | ✅ Per-run bearer: 7 substitute-helper unit tests + 3 MCP client integration tests (incl. concurrent-run isolation regression guard for R-2 per-run state bleed) + 10 validation cases at the HTTP boundary + sub-agent inheritance test + 2 expandEnv namespace regression guards. Auto-version: 7 format cases + smoke test. All tests on `go test -race ./...` clean. |

**Operator migration table:** see [docs/PLAN.md](docs/PLAN.md) for the soak-phase → strict-phase yaml progression. Existing operator yaml without `${run.user_bearer}` references continues to work unchanged.

## What's in v0.8.13

| Surface             | Status |
|---------------------|--------|
| **`FallbackPolicy.PinAfterSuccess bool`** | ✅ New field on `loop.FallbackPolicy`. When true, `tryProviderFallback` in `internal/loop/loop.go` suppresses fallback after any turn has succeeded (assistant message appended to conversation history). Initial-turn fallback still works (stale-probe safety net at run start); same-provider rate-limit retry continues to handle transient errors. Mid-conversation provider switches — the source of every DeepSeek 400 / Anthropic `cache_control` loss / Gemini `thoughtSignature` mismatch we'd discovered — stop happening. (PR #93) |
| **New typed event `EventFallbackSuppressed`** | ✅ Emitted whenever the pin policy intercepts a would-be fallback. Wire-stable for adapters; mirrors the v0.8.2 `EventCacheInvalidated` / v0.8.12 `EventReasoningInvalidated` event-on-policy pattern. `Text` field carries the cause error so operators can attribute the failure. Cost retros / dashboards consume this to distinguish "provider down" (run failed by design) from "fallback succeeded" (run survived). |
| **New env var `LOOMCYCLE_FALLBACK_PIN_AFTER_SUCCESS`** | ✅ Default **OFF** in v0.8.x (opt-in); default-on planned for v0.9.x once production-validated. Wired from `cfg.Env` through to `FallbackPolicy` via the HTTP server's `fallbackForRun`. |
| **v0.8.12 reasoning strip retained** | ✅ When a deployment opts back into mid-session fallback later, the `Message.Reasoning` strip in `tryProviderFallback` still works as a belt-and-suspenders safety net. Two complementary mechanisms for the same problem class — pin to AVOID the cross-provider transition; strip to SURVIVE one if it happens. |
| **3 regression tests** | ✅ `TestFallback_PinAfterSuccess_SuppressesPostTurn1Failure` (headline; turn 2 503 is suppressed when flag on), `TestFallback_PinAfterSuccess_InitialTurnFailureStillFallsBack` (turn 0 fallback still works — stale-probe safety preserved), `TestFallback_PinAfterSuccess_FlagOff_PreservesV082Behavior` (regression guard against accidental default-on flip). |
| **The trade** | ⚠️ A sustained mid-conversation provider outage now FAILS the run instead of cascading to alternates. Acceptable for most production deployments — provider outages are rare, clients retry, and mid-conversation transcript-translation bugs are subtle and silent (much worse failure mode). |

## What's in v0.8.12

| Surface             | Status |
|---------------------|--------|
| **Cross-provider `reasoning_content` strip on fallback** | ✅ When `tryProviderFallback` (`internal/loop/loop.go`) successfully switches providers mid-conversation, walks the in-flight `messages` slice and zeroes `Message.Reasoning` on every assistant turn. The new provider gets a clean history. Fixes the 2026-05-13 production bug where a `gemini-2.5-flash → deepseek-v4-flash` fallback 400'd with `"The reasoning_content in the thinking mode must be passed back to the API."` (PR #91) |
| **New typed event `EventReasoningInvalidated`** | ✅ Emitted when the strip pass cleared one or more assistant turns. Mirrors the v0.8.2 `EventCacheInvalidated` precedent. Wire-stable; consumed in the same way as other typed events. `Text` field carries: `"cleared reasoning_content from N assistant turn(s) on switch from <old> to <new>; cross-provider echo would 400"`. Cost retros should treat downstream iterations as reasoning-cold on the new provider. |
| **Safe across all current providers** | ✅ Anthropic uses typed content blocks for `extended_thinking` (not the Reasoning string field) → immune. Gemini's driver doesn't write Reasoning today → strip is a no-op unless populated via PriorMessages from a continuation. OpenAI o-series tolerates missing `reasoning_content` (treats as no prior thinking). DeepSeek + OpenAI o-series within their own family continue to round-trip correctly because the strip only fires on cross-family fallback. Tool calls in the same turn unaffected: strip only touches the `Reasoning` string field, not `Content` (tool_use blocks + tool_use_id stay intact). |
| **3 regression tests** | ✅ `TestFallback_ReasoningStrippedOnProviderSwitch` (headline regression; verified to fail on pre-fix code with the exact production failure mode), `TestFallback_NoReasoningStrip_NothingToStrip` (guards against spurious event emission when no Reasoning was set), `TestFallback_PartialStreamReasoning_NeverReachesMessages` (pins the drain-and-continue invariant for in-stream errors). New `recordingProvider` test wrapper captures the `providers.Request` the new provider receives so assertions can verify the strip happened on the wire. |
| **No env-var changes** | ✅ Existing fallback behavior preserved on same-family round-trips. No new config required. |
| **Adapter-side note** | ⚠️ TS adapter (`@loomcycle/client`) logs `[loomcycle: unknown event "reasoning_invalidated"]` until a handler is added. Cosmetic — doesn't affect run outcomes. Separate adapter PR. |

## What's in v0.8.11

| Surface             | Status |
|---------------------|--------|
| **`internal/metrics/` package** | ✅ New process-resource sampler. Periodic ticker (default 5s) reads `runtime.ReadMemStats` for Go heap + goroutine count, `/proc/self/status` for VmRSS, `/proc/self/stat` for utime+stime delta CPU%, and optionally `/proc/stat` + `/proc/meminfo` for system-wide CPU/mem. **Idle-gated on `concurrency.Semaphore.Stats().active > 0`** — when no agent runs are in-flight, no DB write, no `/proc` read. Sleep cost is one in-process atomic load per tick. |
| **`/v1/_metrics/*` HTTP API (3 endpoints)** | ✅ All bearer-authed, return 503 with `enable_hint` when sampler not configured: (1) `GET /v1/_metrics/samples?since=&until=&limit=&cursor=` — windowed raw samples with cursor pagination; (2) `GET /v1/_metrics/runs/{run_id}` — peak/mean RSS + max CPU% computed via SQL JOIN on `[started_at, COALESCE(completed_at, now)]`; (3) `GET /v1/_metrics/summary?period=1h\|24h\|7d` — aggregated buckets (mean/max RSS, p95 CPU%, max active_runs per bucket; in-Go aggregation acceptable at v0.8.x scale, ≤2016 rows for 7d/5min). |
| **Build-tag-split `/proc` readers** | ✅ `proc_linux.go` (`//go:build linux`) reads `/proc/self/status` VmRSS, `/proc/self/stat` utime+stime delta (USER_HZ=100, hard-coded), optionally `/proc/stat` + `/proc/meminfo`. `proc_other.go` (`//go:build !linux`) returns zero values + `ProcMetricsAvailable=false`. macOS/Windows dev workstations still record platform-independent fields (active_runs, goroutine count, Go heap) — RSS/CPU columns land as 0. Hardened containers (gVisor, kata) get soft-failure handling: log once, continue with zero fields. |
| **`process_samples` table** | ✅ Time-series, 12 columns. SQLite `CREATE TABLE IF NOT EXISTS` in `migrate.stmts`; index `process_samples_by_sampled_at` in `addIndexes` (defensive habit per the v0.8.6 lesson — future ALTER TABLE column adds can't break index creation order). Postgres migration `0009_process_samples.up.sql` with `TIMESTAMPTZ` + `BIGINT` types + same index. **No foreign keys to `runs`** — time-series correlation is a query-time JOIN, not a referential constraint. |
| **`MintSampleID` helper** | ✅ `smp_<16hex unixnano><8hex rand>` — mirrors `MintChannelMessageID`. Sortable lexicographically by sample time; collision-safe within a single nanosecond via the 4-byte random suffix. |
| **Bounded retention** | ✅ Sweeper goroutine deletes rows older than `LOOMCYCLE_METRICS_RETENTION_DAYS` (default 7) at `LOOMCYCLE_METRICS_SWEEP_INTERVAL_MS` cadence (default 15 min). Set retention=0 OR sweep interval=0 to disable (table grows unbounded). |
| **Consecutive-failure rate-limited logging** | ✅ Sampler tracks a failure counter. Logs loudly on the first store-write error or `/proc` read error, then every 10th. Prevents log flood on a wedged disk / disconnected Postgres pool / hardened-container `/proc` filter. Successful write resets the counter + emits a recovery log line. |
| **5 new env vars** | ✅ `LOOMCYCLE_METRICS_ENABLED` (default OFF; default-on planned for v0.9.x), `LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS` (default 5000; min-clamp 1000 to prevent write-storms from a typo'd `=50`), `LOOMCYCLE_METRICS_RETENTION_DAYS` (default 7), `LOOMCYCLE_METRICS_COLLECT_SYSTEM` (default OFF — Linux only), `LOOMCYCLE_METRICS_SWEEP_INTERVAL_MS` (default 900000). Documented in `.env.example` with storage estimate (~210 MB/week steady-state at defaults). |
| **`cancel.Registry.ListAll()`** | ✅ General-purpose accessor returning a snapshot of every live entry regardless of user. **Not consumed by the sampler in v0.8.x** (the sampler uses `Semaphore.Stats()` for its active-runs gate); shipped as a forward-compat addition for future cross-cutting consumers with its own test coverage. |
| **Test coverage** | ✅ 28 new tests: 6 storetest contract tests (auto-run on sqlite + postgres — write+query round-trip, sweep idempotency, run-summary empty/with-samples/in-flight/not-found), 5 sampler unit tests (idle skip, write on active, graceful store error with rate-limited log, nil store, recovery counter reset; uses embedded-`store.Store`-interface fake for forward-compat against future Store additions), 8 `/proc` parser unit tests (fixture-based so they run on macOS CI too), 9 HTTP handler tests (503-when-disabled, samples round-trip + cursor, run-summary 404 + happy path, summary period bucketing, validation errors), 2 cancel registry tests. 37 packages green; race-detector clean on the 5 changed packages. |
| **Production-validated** | ✅ Deployed to operator's TrueNAS VM 2026-05-13. First exercised by an employer-profiler run that spawned company-researcher + 2 injection-judge sub-agents; captured 31 samples revealing loomcycle's per-process footprint at 21–33 MB RSS across the entire 3-way concurrent run tree. Per-run peak RSS for the 154-second orchestrator: 33 MB. |

## What's in v0.8.10

| Surface             | Status |
|---------------------|--------|
| **Gemini schema sanitizer (`$ref` + combinators)** | ✅ `sanitizeGeminiSchema` rewritten in `internal/providers/gemini/driver.go`. Inlines `$ref` (cycle-safe via per-path visited-set; diamond refs each inline independently; cycles emit `{}`; unresolved refs emit `{}`). Collapses `allOf` / `oneOf` / `anyOf` by **merging** ALL variants' `properties` + `required` into the parent (an earlier first-variant-wins draft was caught in code review — it silently dropped every discriminated-union variant past the first, which was exactly the bug the fix targeted). Type-conflict defense skips structural fields of variants with conflicting `type:` (e.g. `oneOf[object, array]` would otherwise produce a schema MORE broken than the input). Fixes `400 INVALID_ARGUMENT` rejection of Zod-shape MCP tool schemas. (PR #86) |
| **Realistic-MCP regression test** | ✅ `TestSanitizeGeminiSchema_RealisticMcpSchema` mirrors a Zod-generated `discriminatedUnion` + nested `$defs` + `additionalProperties` at multiple levels. Asserts NO banned key (`$ref`, `$defs`, `definitions`, `oneOf`, `anyOf`, `allOf`, `additionalProperties`, `$schema`, `$id`) leaks through AND both discriminated-union variants' payload properties survive. |
| **SQLite migration ordering fix** | ✅ `internal/store/sqlite/sqlite.go migrate()`. The v0.8.6 migration created `channel_messages_by_visible` index in the first `stmts` loop, BEFORE the `addColumns` ALTER block. Fresh deploys worked because the `CREATE TABLE IF NOT EXISTS channel_messages (...visible_at...)` declared the column up front; on an UPGRADE from v0.8.4/v0.8.5 the existing table had no `visible_at` and the CREATE INDEX failed with `SQL logic error: no such column: visible_at`. CI never caught this (every test run uses a fresh DB). Fix: moved the CREATE INDEX into `addIndexes` (which runs AFTER `addColumns`). Postgres unaffected. (PR #87) |
| **Upgrade-path regression test** | ✅ `TestMigrate_UpgradeFromV084ChannelMessages` simulates the upgrade path by hand-creating a v0.8.4 schema, then re-opening through `migrate()`. Pre-fix fails with the exact production error message; post-fix asserts both columns added, by_visible index created, and legacy `visible_at` backfilled from `published_at`. |
| **Both fixes consolidated** | ✅ v0.8.9 shipped the schema sanitizer; v0.8.10 added the sqlite migration fix that became necessary when deploying v0.8.9 from a v0.8.4 schema. Effectively v0.8.10 is the first release that's deployable to existing v0.8.4 / v0.8.5 sqlite-backed installations. |

## What's in v0.8.9

| Surface             | Status |
|---------------------|--------|
| **Gemini schema sanitizer (initial pass)** | ✅ See v0.8.10 above — v0.8.10 ships the consolidated description because v0.8.9 was followed by v0.8.10's sqlite migration fix within hours and the two are typically discussed together. v0.8.9 alone is deployable on a fresh (no prior `channel_messages` table) install. (PR #86) |

## What's in v0.8.8

| Surface             | Status |
|---------------------|--------|
| **`Context.help` op (tenth op on the Context tool)** | ✅ Returns a topic index when called without `topic` (`{topics: [{name, description, source}], count, hint}`); returns the full markdown body when called with `topic=<name>` (`{name, description, content, source}`). Unknown topic surfaces the available list in the error so the model can self-correct in one round-trip. |
| **Five bundled topics** (embedded via `//go:embed`) | ✅ `loomcycle` (intro to runtime + tool surface), `scopes` (agent/user/global isolation model across Memory + Channel), `subagents` (Agent sync spawn vs Channel async handoff; recursion cap; `def_id` pinning; cross-name pinning refusal), `experimentation` (the v0.8.5 fork → spawn → submit → aggregate → promote/retire/rollback loop), `system-channels` (the v0.8.6 `_system/*` namespace, admin endpoint, deferred publish). |
| **Filesystem overlay** | ✅ `LOOMCYCLE_HELP_ROOT` points at a directory of `<name>.md` files. Files with names matching bundled topics REPLACE them; new names extend the set. Symlinks under the help root are **refused** with a log line (trust-boundary protection — a stray `escape.md` symlink would otherwise let an operator exfiltrate any file the loomcycle process can read into the topic body the model sees). Per-file parse errors are **soft-skipped** so one malformed operator topic doesn't kill the runtime — bundled defaults remain intact. |
| **Frontmatter contract** | ✅ Standard Claude-Code-compatible YAML frontmatter. `name:` (must match filename stem) + `description:` (the one-liner shown in the index) are required; everything after the closing `---` is the body. Missing/mismatched name, missing description, or empty body refuses the topic at load time (bundled = fatal; operator = soft-skip). |
| **Wiring + tests** | ✅ New `internal/help/` package (loader + bundled FS + 16 unit tests). `Help *help.Set` field on Context built in `cmd/loomcycle/main.go` at boot; boot log emits `help: loaded N bundled topics (no LOOMCYCLE_HELP_ROOT overlay)` or `help: loaded N topics (filesystem overlay at <path>)`. 4 unit tests for `execHelp` (nil refusal, index mode, detail mode, unknown topic). Race-detector clean. Runtime smoke at `test/runtime/context-help/` passes against `gemini-2.5-flash` — the agent reads the index, calls back with `topic=scopes`, and quotes a phrase from the body. |
| **Schema update** | ✅ Context tool's op enum is now: `self` / `tools` / `doc` / `permissions` / `agents` / `lineage` / `evaluations` / `channels` / `history` / `help` (ten total). New top-level `topic` string field on the input schema. The default-add behaviour from v0.8.7 still applies — every agent gets Context auto-attached at config-load. |
| **`.env.example`** | ✅ Documents `LOOMCYCLE_HELP_ROOT` with the frontmatter contract + override semantics. |

## What's in v0.8.7

| Surface             | Status |
|---------------------|--------|
| **`Context` built-in tool — runtime introspection** | ✅ Read-only; no mutations, no network, no side effects. Nine ops on a single discriminated `op` field (same shape as Memory / Channel / AgentDef / Evaluation): `self` (identity bundle from `RunIdentity` + `AgentName` ctx-keys), `tools` (post-filter tool catalog with closed-set side-effect classifier — `pure` / `state` / `network` / `filesystem` / `privileged` / `unknown`), `doc` (input schema + description for one tool by name; refuses outside the per-run allowlist — no doc leak), `permissions` (bundle of every policy ctx-key — `allowed_tools`, `host_policy`, `memory`, `channels`, `agent_def_scopes`, `evaluation_scopes`, `history_scope`), `agents` (operator-declared agents from `cfg.Agents` with active `def_id` from the v0.8.5 substrate; optional `prefix` filter), `lineage` (walks ancestors via `parent_def_id` chain + descendants BFS; `depth` default 10, cap 100; **total-node cap 500** with `truncated` flag), `evaluations` (v0.8.5 `EvaluationAggregate` output — mean/median/min/max/latest + per-dimension + per-emitter-role; optional `include_lineage` walks ancestors), `channels` (operator-declared channels with per-caller publish/subscribe bools; wildcards surface separately in `publish_wildcards` / `subscribe_wildcards`), `history` (transcript events for the target agent — default caller's own; optional `event_types[]` filter + `limit` default 100/cap 1000; `truncated` is **honest under filter** by counting post-filter matches; gated by yaml `history_scope`). |
| **Default-add behaviour** | ✅ Every agent's `allowed_tools` gets `Context` auto-appended at config-load — missing introspection is a footgun for self-evolving agents. Opt-out is a single yaml line: `disable_context: true`. Duplicate-check is **case-insensitive** so `[context, Context]` doesn't sneak through. |
| **`history_scope` yaml gate (closed set, default-deny)** | ✅ `self` (caller's own run — practical default), `siblings` / `descendants` / `named:<n>` (reserved for v0.8.x — need `RunIdentityValue.ParentAgentID` plumbing), `any` (UNRESTRICTED — operator-trust grant for admin/debug agents). Default-deny: an agent without `history_scope` in its yaml cannot call `history` at all. |
| **Wire-protocol stability** | ✅ Schema enum locked at the ten ops listed above; v0.8.8 added `help` as the tenth. Adapters/SSE consumers can pattern-match on op names. |
| **Test coverage** | ✅ 30+ unit tests covering all nine ops (validation, allowlist filtering, ctx-key bundle assembly, lineage walk + truncation, history filter + truncation correctness). Runtime smoke at `test/runtime/context/` exercises four ops in one chained run against Gemini 2.5 Flash. |
| **PRs in v0.8.7** | ✅ #79 (self / tools / doc / permissions), #80 (agents / lineage / evaluations), #82 / #83 (channels / history + default-add + runtime smoke). |

## What's in v0.8.6

| Surface             | Status |
|---------------------|--------|
| **System channels (`_system/*` namespace)** | ✅ Operator-declared channels published by loomcycle-authoritative paths only. Three categories: (1) **Cadence** — `_system/heartbeat-1m`/`-5m`/`-1h` publish `{ts, version, uptime_s}` at fixed intervals via a dedicated `HeartbeatRunner` goroutine (skip-on-pause via shared `bgCtx`). (2) **Event-driven** — `_system/runtime-state` (pause/resume/restore transitions), `_system/provider-events` (fallback / cache-invalidated) — fire from internal subsystem hooks; no `period:` needed. (3) **Agent-publishable system channels** — `_system/alarms/critical`/`/warning`/`/info` are reserved-by-convention; operators publish via the admin endpoint or future alarm tools. |
| **`SystemPublisher` interface + `StorePublisher` impl** | ✅ Loomcycle-authoritative publish path that bypasses agent-tool ACL gates. Used by the heartbeat ticker AND the admin endpoint. Stamps `published_by_user_id` as `"_system"` (internal) or `"_admin"` (admin-endpoint). |
| **Tool-layer refusals** | ✅ Agents can NEVER publish to (a) channels with `publisher: system` OR (b) anything with the `_system/` prefix — even if an operator forgets to set `publisher: system` on a `_system/...` channel, the prefix itself is the defense-in-depth gate. |
| **Admin endpoint** | ✅ `POST /v1/_channels/_system/{name…}` — bearer-authed, accepts `{payload, deliver_at?}` body. Stamps `published_by_user_id="_admin"`. Use cases: external monitoring webhooks pushing alerts, ops dashboards, operators debugging from `curl`. |
| **Deferred publish (general — any channel)** | ✅ `Channel.publish` accepts optional RFC3339 `deliver_at`. Message stored immediately with `visible_at = deliver_at`; subscribers + `peek` filter `WHERE visible_at <= now()`. In-process `time.AfterFunc(visible_at)` scheduler wakes long-poll subscribers exactly at delivery time; bounded by `LOOMCYCLE_CHANNELS_MAX_PENDING_DEFERRED` (default 10000). If the scheduler is over-cap or the process restarts mid-defer, deferred messages still get delivered on the next periodic poll — the scheduler is a latency optimisation, not a correctness mechanism. **TTL counts from `published_at`, NOT from `deliver_at`** — a 1-hour deferral with a 30-minute TTL means the message expires before becoming visible; size your TTL to cover both windows. |
| **Tuple cursor `(visible_at, msg_id)`** | ✅ Cursor format changes from `msg_<hex>` to `cur_<vh>_<msg_<…>>`. Pure msg_id ordering would silently skip deferred messages once a subscriber progressed past their publish-time id; the tuple ordering aligns the read path with delivery order. **Clean cursor break** — the 0005 migration truncates `channel_cursors` (v0.8.4 only shipped two weeks earlier, no production cursor state worth preserving). Subscribers replay from oldest on first subscribe after upgrade. |
| **Audit column** | ✅ New `channel_messages.published_by_user_id` populated from `RunIdentity` for agent publishes, `"_system"` for internal publishes, `"_admin"` for admin-endpoint publishes. Audit queries can distinguish operator + system + agent activity without grepping logs. |
| **Config validation** | ✅ `publisher: system` + `period:` rules enforced at config-load. `_system/` prefix is reserved (operator-only declaration; agents can never publish regardless of `publisher:` setting). |
| **Standard yaml** | ✅ `loomcycle.example.yaml` ships with the canonical heartbeat / alarm / runtime-state / provider-events channel set commented for operators to uncomment. |
| **PRs in v0.8.6** | ✅ #74 (deferred publish + tuple cursor), #75 (system publisher + admin endpoint), #76 / #78 (heartbeat ticker + runtime smoke). |

## What's in v0.8.5

| Surface             | Status |
|---------------------|--------|
| **`AgentDef` built-in tool — 6 ops** | ✅ `create` / `fork` / `get` / `list` / `promote` / `retire`. Single discriminated `op` field. Static `cfg.Agents` names are inviolate — must `fork`, never `create`. **AllowedTools ceiling is non-negotiable**: forks may NARROW the tool set, never widen; operator-blessed root is the permanent capability ceiling enforced via 100-hop cycle-guarded lineage walks. Per-agent yaml `agent_def_scopes` gates `self` / `descendants` / `named:[...]` / `any`, default-deny. |
| **`Evaluation` built-in tool — 5 ops** | ✅ `submit` / `get` / `list_for_run` / `list_for_def` / `aggregate`. Score model: required scalar (RL lingua franca) + optional `dimensions` map + optional `judgement` JSON + optional `rationale` text. **`emitter_role` derived server-side** from caller's `RunIdentity` vs target run's identity (`self` / `parent` / `external` / `unrelated`) — the model can't lie about who scored what. `sibling` collapses to `unrelated` today (RunIdentityValue lacks emitter ParentAgentID); `submit_siblings` scope is reserved-but-inert; `submit_any` is the escape hatch. Per-agent yaml `evaluation_scopes` gates submit roles + read ops. |
| **Versioned `agent_defs` + lineage** | ✅ Append-only `agent_defs` (UUID `def_id`, monotonic `version` per `name`, `parent_def_id` for lineage, `bootstrapped_from_static` flag). `agent_def_active` pointer table for "which version a name resolves to." Promote/retire flip pointers — they never rewrite definition rows. Postgres `pg_advisory_xact_lock(hashtextextended('agent_def:' || name, 0))` serialises version allocation per name; sqlite uses pinned-conn + `BEGIN IMMEDIATE`. Tested under contention: 250 parallel forks → exactly versions 1..250 with no gaps or duplicates on both backends. |
| **Sub-agent `def_id` pinning** | ✅ Optional `def_id` on the `Agent` tool input. `runSubAgent` overlays the row onto static `cfg.Agents` for that one sub-run (Model/Tier/Provider/Effort apply correctly via `resolveAgentDef`). `agent_def_id` persisted on the sub-run row + denormalised onto evaluations at submit time — aggregate queries downstream automatically partition by def. **Substrate policy fields are NEVER in the overlay surface** so forks can't widen their own gates. **Cross-name pinning refused** — passing a `def_id` whose row was created for a different agent name returns "cross-name pinning refused"; prevents namespace hijack. |
| **Selection stays policy** | ✅ Loomcycle does NOT auto-promote based on score. Agents (or operator orchestrators) call `Evaluation.aggregate` + `AgentDef.promote` per their own policy — max, GA, PPO, RLHF, whatever. Keeping policy out of the runtime is what lets it host arbitrary selection strategies. |
| **Migrations (additive)** | ✅ Postgres: `0006_agent_defs`, `0007_runs_agent_def`, `0008_evaluations`. SQLite: idempotent CREATE TABLE + `ALTER TABLE runs ADD COLUMN agent_def_id TEXT`. |
| **PRs in v0.8.5** | ✅ #65 (storage + locks + aggregate kernel), #66 (config + ctx-key plumbing), #67 (AgentDef tool), #68 (Evaluation tool), #71 (runtime smoke), #72 (sub-agent def_id pinning). |

## What's in v0.8.4

| Surface             | Status |
|---------------------|--------|
| **`Channel` built-in tool** | ✅ Persistent inter-agent message bus. Five ops on a discriminated `op` field: `publish` (append JSON payload to a named channel; ACL-gated), `subscribe` (drain up to N new messages + return a cursor; optional `wait_ms` long-poll), `ack` (explicitly commit a cursor; rejects regressions via `ErrChannelCursorRegression`), `peek` (non-consuming debug read), `list_channels` (informational ACL dump). Subscribe is at-most-once-by-default (commits `next_cursor` on return); agents wanting at-least-once / crash safety use `peek` → process → `ack`. Same single-discriminated-`op` shape as Memory. Sub-agents inherit the parent's ACL via ctx (mirror of `WithMemoryPolicy` / `WithHostPolicy`). |
| **Storage-layered backend** | ✅ Messages persist to `store.Store` via two new tables: `channel_messages` (TEXT id ULID-style `msg_<unixnano><rand>`, payload JSONB on Postgres / TEXT on SQLite, expires_at) and `channel_cursors` (per-subscriber committed position). Cursor scope mirrors Memory: `agent` (one cursor per agent name), `user` (per user_id), `global` (one shared cursor). Additive `0004_channels.up.sql` Postgres migration; idempotent CREATE TABLE on SQLite. Storetest contract suite: 11 subtests run on both backends — publish/subscribe ordering, cursor monotonicity, TTL filter at read, max_messages trim, scope isolation, replay via `cur_0`, ack-regression rejection. |
| **In-process notification Bus** | ✅ New `internal/channels/` package. `Bus.Notify(channel)` wakes any in-process subscribers blocked in `Bus.Wait(ctx, channel, timeout)`. Subscribe with `wait_ms > 0` queries storage, then blocks on the bus until a publish lands or the timeout fires — sub-millisecond latency for same-process consumers; cross-process subscribers fall back to polling. 7 race-detector-clean tests (notify wakes, timeout returns false, ctx cancel returns early, fan-out, channel isolation, no-timeout-no-wait, stress under concurrent notify+wait). |
| **Operator-yaml ACL** | ✅ New top-level `channels:` block declares the namespace (per-channel `scope` / `default_ttl` / `max_messages` / `semantic`). Per-agent `channels: {publish: [...], subscribe: [...]}` allowlists name channels with optional trailing `/*` wildcard (`findings/*` matches `findings/alpha` but NOT `findings`; mid-string globs rejected at config-load so an operator typo can't grant `*` access). Same trust model as `allowed_tools` + `memory_scopes`. Validation: every ACL entry must reference a declared channel; wildcards with no matches at load time are rejected. |
| **Lossy-on-overflow bounded storage** | ✅ Each channel declares `max_messages`; publishes that push the per-(channel, scope, scope_id) count past trim OLDEST rows inside the same txn. Publisher never blocks — the v0.8.4 RFC's central trade-off (cost cap → never starve the producer). The publish result includes `dropped_oldest: N` so the tool layer (and future audit events) sees the overflow signal. 0 = unbounded. |
| **Three new env vars** | ✅ `LOOMCYCLE_CHANNELS_MAX_VALUE_BYTES` (per-publish payload cap, default 64 KB), `LOOMCYCLE_CHANNELS_SWEEP_MS` (TTL reaper cadence, default 15 min), `LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS` (max `wait_ms` allowed on subscribe, default 30 s). All have sensible defaults; zero disables. |
| **Operator visibility** | ✅ Boot log emits `channels: configured N — channel-a / channel-b / ...` (mirror of `user_tiers:` line shape). Sweeper goroutine logs per-sweep delete count when > 0. `loomcycle.example.yaml` ships with two canonical channels — `findings` (scope: agent, semantic: queue, 24h TTL, 10k max) and `alerts` (scope: global, semantic: broadcast, 1h TTL, 1k max) — plus two example agents (`researcher` publishes, `analyst` subscribes) demonstrating the canonical handoff pattern. |

## What's in v0.8.3

| Surface             | Status |
|---------------------|--------|
| **Provider split: `ollama` + `ollama-local`** | ✅ Hosted ollama.com (Bearer auth via `OLLAMA_API_KEY`) is now `ollama`; local-network Ollama (no auth, default `http://localhost:11434`) is now `ollama-local`. One driver package serves both — same `/api/chat` wire shape; only the auth header + base URL differ. Existing deploys with `OLLAMA_BASE_URL=http://localhost:11434` keep working unchanged (the env var now drives `ollama-local`). Two new env vars: `OLLAMA_API_KEY` + optional `OLLAMA_CLOUD_BASE_URL`. Library `defaultLibraryPriority` becomes `[ollama-local, deepseek, openai, anthropic, ollama]` — workstation at the floor, hosted ollama after the paid clouds. (PR #55) |

## What's in v0.8.2

| Surface             | Status |
|---------------------|--------|
| **`user_tier` policy + resolver overlay** | ✅ Operator-defined named user-tier policies in `loomcycle.yaml` (`user_tiers:` block) — each tier carries its own `provider_priority`, per-task-tier `tiers`, `fallback_on_error` switch, and `max_fallback_attempts` cap. Runs carry `user_tier` per-request via `POST /v1/runs` (and `POST /v1/sessions/{id}/messages`); empty falls through to the required `default` entry; unknown name → 400. The resolver overlays the tier's policy between library defaults and per-agent overrides; `agent.providers ∩ user_tier.provider_priority` empty → `ErrTierAgentNotAvailable` (distinct from outage so clients render "upgrade required"). Sub-agents inherit the parent's `user_tier` via ctx. New `runs.user_tier` column (additive migration on both SQLite + Postgres) drives cost retros + compliance audit. (PR #52) |
| **Runtime provider fallback** | ✅ When a provider call returns a retryable error (429/5xx/network/v0.8.1 stream-idle), the loop swaps to the next-in-queue provider within the user_tier's candidate list and continues the iteration. Five-bucket error classifier in `internal/providers/errclass.go` distinguishes retryable from permanent (400/401/403/422) so config errors don't cascade through every provider's quota. Cumulative 3-attempt budget per run; per-tier `fallback_on_error: false` opts free tiers out of the cascade (cost-cap semantic — 429 returns error to client, no climb to paid providers). New typed events `EventProviderFallback` (with structured `FallbackInfo` payload) and `EventCacheInvalidated` (fired only on `anthropic → other` since Anthropic is the only provider with operator-controlled `cache_control` today). (PR #53) |
| **Per-tier policy in operator yaml** | ✅ `user_tiers:` block ships with five canonical tiers in `loomcycle.example.yaml`: `default` (back-compat for v0.7.x clients — mirrors the library defaults), `free` (ollama-only, no cascade — cost-cap shape), `low` (deepseek + anthropic, cascade on), `medium` (openai + anthropic + deepseek, cascade on), `high` (anthropic-only, no cascade — premium SLA). Each tier carries its own `fallback_on_error` posture. The "default" entry is required when the block is populated; validation rejects unknown providers/tiers and negative `max_fallback_attempts`. |
| **Per-run audit marker** | ✅ `runs.user_tier` column on both backends with the additive `0003_user_tier.up.sql` Postgres migration. Compliance + cost-retrospective queries facet by tier without grepping logs. The boot log emits `user_tiers: configured N — default / free / low / medium / high` so operators see what's available at startup. |

## What's in v0.8.1

| Surface             | Status |
|---------------------|--------|
| **Provider streaming timeouts** | ✅ Replaced the 5-min wall-clock `http.Client.Timeout` with a header + per-byte idle pair. `Transport.ResponseHeaderTimeout` caps time-to-first-byte (default 60 s); a body wrap resets a timer on each Read and cancels the request context on stall (default 90 s). Long but actively-emitting final-turn responses (e.g. job-searcher emitting a 25-position ingest payload) now complete instead of getting cut mid-stream. Two operator knobs: `LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS` / `LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS`. All five provider drivers updated; `streamhttp` package + 8 unit tests; `-race` clean. (PR #47) |
| **Lazy MCP retry on first agent call** | ✅ MCP servers that failed initial handshake at boot (peer down, slow to start, or broken at the time loomcycle started) used to stay marked `skipped` for the lifetime of the loomcycle process — operators had to restart loomcycle by hand once the peer recovered. Now the dispatcher carries an optional `FallbackFunc` (set in `cmd/loomcycle/main.go`); a tool name matching `mcp__<server>__<tool>` for a configured-but-skipped server triggers one fresh `pool.Get` for that server on the agent's call path. On success, the server's tools are memoised and dispatched; the operator-visible log line is `mcp[<server>]: lazy-registered N tool(s) on first agent call (was skipped at boot)`. Subsequent calls hit the cache without re-handshaking. The pool's existing `entry/ready` channel coalesces concurrent first-touches to a single underlying handshake (50-way concurrency test pinned). Peer restarts no longer require a loomcycle restart — addresses the "components restart independently in a server environment" failure mode. (PR #48) |
| **Agent directory discovery** | ✅ New `LOOMCYCLE_AGENTS_ROOT` points at a directory of flat `<name>.md` files. Each file's YAML frontmatter is the base `AgentDef`; the body becomes `system_prompt`. The yaml `agents:` map remains an OPTIONAL override layer — yaml entries with the same name override discovered fields per-field (yaml-as-override). Mixed-mode, MDs-only, and yaml-only deployments all supported. Frontmatter is flat top-level keys (`name` / `description` / `tools` / `model` / `tier` / `models` / `effort` / `max_tokens` / `skills` / `memory_scopes` / `memory_quota_bytes` / `providers` / `allowed_tools` / `system_prompt_file`); accepts both Claude Code's `tools: A, B, C` (comma-string) and loomcycle's `allowed_tools: [A, B, C]` (yaml list); `allowed_tools` wins when both present. Single source of truth for operators maintaining `.claude/agents/*.md` for Claude Code AND a corresponding loomcycle `agents:` block. (PR #49) |

## What's in v0.8.0

| Surface             | Status |
|---------------------|--------|
| **`Memory` built-in tool** | ✅ Persistent agent-scoped key/value storage that survives across runs and sessions. Five ops behind one tool: `get` / `set` / `delete` / `list` / **`incr`** (atomic counter). Two scopes: `agent` (yaml-keyed; cross-run, shared across users) and `user` (user_id-keyed; cross-agent, per end-user). Backed by a new `memory` table on both SQLite and Postgres adapters. (PR #45) |
| **Per-agent yaml policy** | ✅ `memory_scopes: [agent, user]` is a default-deny allowlist — `Memory` in `allowed_tools` is necessary but not sufficient. Optional `memory_quota_bytes` per-agent override of the global `LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES` cap. Sub-agents get their OWN policy from yaml — the parent's `memory_scopes` does NOT cascade. (PR #45) |
| **Web UI Memory page** | ✅ `/ui/memory` — three-pane browser: scope picker → scope_id list with key counts and byte totals → keys with prefix filter → entry detail with pretty-printed JSON, timestamps, and TTL. Polls the new `/v1/_memory/*` admin endpoints on a 5 s tick. (PR #45) |
| **Admin API for Memory** | ✅ Four read-only routes — `GET /v1/_memory/scopes`, `/scopes/{scope}`, `/scopes/{scope}/{scope_id}/keys`, `/scopes/{scope}/{scope_id}/keys/{key...}`. Bearer-authed via the existing middleware. The `{key...}` multi-segment route handles slashed keys like `events/2026-05-09T10:00`. (PR #45) |
| **Concurrency hardening** | ✅ Atomic increment correctness verified by a 100-goroutine regression test on both backends. Caught and fixed two real lost-update races at review time: SQLite `BeginTx(nil)` is DEFERRED (fix: pinned connection + raw `BEGIN IMMEDIATE`); Postgres `SELECT FOR UPDATE` doesn't lock absent rows (fix: `pg_advisory_xact_lock` keyed by hash of the (scope, scope_id, key) tuple). (PR #45) |
| **Pre-existing host-policy fix** | ✅ `handleMessages` (session continuation path) had been missing `tools.WithHostPolicy` on its loop ctx since v0.4.0 — sub-agents from continuations fell back to the operator's static allowlist instead of the caller's narrowed list. Fixed alongside the new Memory ctx values. (PR #45) |

## What's in v0.7.4

| Surface             | Status |
|---------------------|--------|
| **Web UI agent name + content fixes** | ✅ Run list now shows the YAML-declared agent name (`qa-agent`, `company-researcher`) instead of just the UUID. Agent detail header reads from the corrected wire shape (model + tokens + duration). Transcript event panels now render actual content (text, tool calls, tool results, errors) — collapsed-by-default with a one-line summary, click to expand for full text + tool params + pretty-printed JSON. (PRs #41, #42) |
| **User picker dropdown** | ✅ New `GET /v1/_users` admin endpoint surfaces distinct user_ids that have runs in the store, with running / total counts + last-active timestamp. The Web UI top bar swaps the freeform user_id input for a dropdown — operators no longer need to know the UUID up front. Manual override (✎ button) preserved for picking a user who has no runs yet. (PR #40) |
| **Gemini config validation hotfix** | ✅ v0.7.2 wired the Gemini driver into the resolver but missed adding `gemini` to the config validator's allowlist; operators with `provider: gemini` rows in their yaml saw startup fail. Fixed. (PR #39) |

## What's in v0.7.3

| Surface             | Status |
|---------------------|--------|
| **Embedded read-only Web UI** | ✅ React 19 + Vite 7 + TypeScript SPA at `/ui`. Two pages: run list (parent → children tree, status filter, auto-refresh every 3 s) and per-agent detail (event log: text / thinking / tool_call / tool_result / error / retry / done; auto-refresh every 1.5 s for active runs; cancel button). No new wire endpoints — the SPA reuses the existing `/v1/users/{user_id}/agents`, `/v1/agents/{agent_id}`, `/v1/sessions/{id}/transcript`, `/v1/agents/{agent_id}/cancel` routes. |
| **Bearer-in-cookie auth** | ✅ Operator visits `/ui?token=<bearer>` once; server sets a `loomcycle_session` HttpOnly cookie and 302s back. Subsequent /v1 calls authenticate via the cookie (same-origin fetch). The existing `Authorization: Bearer …` header path keeps working unchanged for adapters / curl / SDKs — bearer wins on precedence so a stale cookie can't mask a deliberate request. |
| **Build pipeline** | ✅ `make build-ui` runs `npm install + npm run build` and writes the production bundle to `internal/webui/dist/` (embedded via `go:embed`). `make build-all` does both. A fresh checkout without npm toolchain still compiles via Go alone (a committed `.gitkeep` placeholder); `/ui` then returns 503 with a `ui_not_built` code as the diagnostic. |

## What's in v0.7.2

| Surface             | Status |
|---------------------|--------|
| **Google Gemini provider** | ✅ Fifth backend driver in `internal/providers/gemini/`. Speaks Gemini's `generateContent` API: model name in URL path (not body), `x-goog-api-key` header auth, SSE streaming via `?alt=sse`. Tool dispatch maps loomcycle's `tool_use` / `tool_result` to Gemini's `functionCall` / `functionResponse` content parts. |
| **Effort hint translation** | ✅ `effort: low \| medium \| high` maps to `generationConfig.thinkingConfig.thinkingBudget` on gemini-2.5-flash / gemini-2.5-pro: `low` → 0 (disable), `medium` → 2048, `high` → 8192 (clamped to `max_tokens - 1024` when needed). Same vocabulary as Anthropic / OpenAI — no per-provider effort dialect. |
| **Resolver matrix integration** | ✅ Excluded when `GEMINI_API_KEY` is unset; probed at startup and on the periodic re-probe with the same 5 s deadline as the others. Per-agent yaml: `provider: gemini` and `model: gemini-2.5-flash` (or any model the wire `/v1beta/models` returns). |
| **Vertex AI deployments** | ✅ Optional `GEMINI_BASE_URL` overrides for Vertex AI Gemini gateways (production deployments routing through GCP project quotas instead of the public AI Studio API). |

## What's in v0.7.1

| Surface             | Status |
|---------------------|--------|
| **`EventThinking` event type** | ✅ Live streaming of model reasoning as a typed event distinct from `EventText`. Anthropic from `thinking_delta` content blocks; OpenAI / DeepSeek from `delta.reasoning_content`; Ollama from `message.thinking`. `EventDone.Reasoning` still carries the consolidated trace for next-turn echo (DeepSeek roundtrip). |
| **Tool-use hooks** | ✅ Operator-supplied middleware around tool dispatch via `POST /v1/hooks`. Selectors filter by `(agent, tool, phase)`; per-`(owner, name)` idempotent registration prevents cascading on app restart. Fail-open default (telemetry hooks don't block); fail-closed available for security-shaped hooks. See [`docs/TOOLS.md`](docs/TOOLS.md). |
| **Resolver Snapshot endpoint** | ✅ `GET /v1/_resolver` exposes the in-process availability matrix as JSON, bearer-authed. 503 with `resolver_unavailable` in the brief degraded-startup window so dashboards distinguish "matrix not available" from "matrix is empty". |
| **Parallel tool dispatch** | ✅ The agent loop dispatched a turn's tool_calls serially — `Agent` fan-outs queued. New `executePendingTools` runs each in its own goroutine, default 8 concurrent, `LOOMCYCLE_TOOL_PARALLELISM` to override. |
| **SSE wire-level keepalive** | ✅ Long-lived agent streams emit `: keepalive\n\n` comment frames every 20 s by default. Closes the opaque `TypeError: terminated` undici reports when networks with idle-connection timeouts (Tailscale, NAT routers) drop a silent stream. `LOOMCYCLE_SSE_KEEPALIVE_MS` to override; 0 disables. |
| **Per-token text coalescing** | ✅ OpenAI / DeepSeek streaming text deltas accumulate into 64-byte chunks. Closes the "every word on its own line" cosmetic noise DeepSeek's tokenizer produced. |
| **Ollama qwen3 tool-call recovery** | ✅ Both JSON-shape (`{"name":"...","arguments":{...}}`) and bracketed-markdown (`[tool_use: name]\n{args}`) forms now synthesize `EventToolCall` so the loop iterates instead of terminating with the markup as the final answer. |
| **DeepSeek thinking-mode roundtrip** | ✅ DeepSeek V4 Pro / deepseek-reasoner returns `reasoning_content` alongside `content`; the API requires it echoed back on subsequent turns. The OpenAI driver captures it on `EventDone.Reasoning`; the request builder serialises it back when the assistant Message carries one. |

## What's in v0.7.0

| Surface             | Status |
|---------------------|--------|
| **Tier-based resolution** | ✅ Agents declare `tier: low \| middle \| high` instead of pinning a specific model. Resolver picks `(provider, model)` against a live availability matrix. Per-agent `providers:` and `models:` overrides cover asymmetric pinning. Explicit pins from v0.6.x continue to work. |
| **Live `/v1/models` probes** | ✅ Each driver implements `Probe` + `ListModels`. Startup probes run in parallel with a 5s deadline; periodic re-probe runs every 15 min (configurable up to 1h via `LOOMCYCLE_RESOLVE_PROBE_INTERVAL_MS`). |
| **`Excluded` flag**  | ✅ Providers without API keys are explicitly marked excluded in the matrix — distinct from "probe attempted, failed". Visible in `Resolver.Snapshot()` for dashboards. Startup logs surface the state. |
| **Reactive stall feedback** | ✅ Loop calls `MarkStalled` on driver errors (5xx after retry, mid-stream errors). Resolver skips stalled `(provider, model)` pairs until next probe revives. `ctx.Err()` guards prevent user-cancellations from polluting the matrix. |
| **Per-driver effort hint** | ✅ Agent yaml: `effort: low \| medium \| high`. Anthropic → `thinking.budget_tokens` (haiku always skips); OpenAI → `reasoning_effort`; DeepSeek inherits OpenAI; Ollama is a no-op. Loop logs once per Run when effort is dropped. |

## What's in v0.6.0

| Surface             | Status |
|---------------------|--------|
| **DeepSeek provider** | ✅ Wraps the OpenAI driver with the DeepSeek base URL pre-baked. Per-agent yaml: `provider: deepseek`. Set `DEEPSEEK_API_KEY`; optional `DEEPSEEK_BASE_URL` for self-hosted OpenAI-compatible mirrors (vLLM, etc.). |
| **OpenAI `Usage.Model` fix** | ✅ Driver now captures the wire-resolved model alias from the streamed chunk envelope, so `runs.model` populates for every OpenAI-compatible run (OpenAI itself, DeepSeek, vLLM). Same regression class as the v0.4 anthropic fix; latent until the DeepSeek live test surfaced it. |
| **Ollama live integration tests** | ✅ Three tests (probe, chat, tool call) gated by `OLLAMA_TEST_BASE_URL`. Validated against qwen3:14b on RTX 5080 (16GB VRAM) end-to-end as the offline / cost-floor backend. |
| **Constant-time bearer compare** | ✅ New `internal/auth.CompareBearer` (sha256+CTC) replaces raw `subtle.ConstantTimeCompare` on both HTTP and gRPC. Closes a length-leak side channel that the stdlib documents but doesn't fix. |

**Provider routing intent (jobs-search-agent first):** Anthropic for user-sensitive paths · DeepSeek for high-volume public data · Ollama (local llama) for offline / cost floor · OpenAI for general use / prototyping. See [`docs/PLAN.md`](docs/PLAN.md#v060--earlier) for the full rationale and rollout history.

## What's in v0.5.5

| Surface             | Status |
|---------------------|--------|
| **gRPC server**      | ✅ Opt-in via `LOOMCYCLE_GRPC_ADDR`. All seven RPCs mirror the HTTP+SSE surface 1:1 (`Run`, `Continue`, `GetAgent`, `CancelAgent`, `ListUserAgents`, `GetTranscript`, `Health`). Coexists with HTTP — same store, same cancel registry, same semaphore. See [`docs/GRPC.md`](docs/GRPC.md). |
| **Python adapter**   | ✅ `pip install loomcycle`. Async `LoomcycleClient` over `grpc.aio` covering all seven RPCs. PEP-561 `py.typed`. |
| **`internal/runner/`** | ✅ Wire-agnostic seam — HTTP server satisfies `runner.Runner`, gRPC server delegates to the same instance. |
| **Synthetic registration frames** | ✅ Wire-stable `session` + `agent` frame pair at the head of every Run/Continue stream so adapters capture `(agent_id, run_id, session_id, parent_agent_id)` without re-decoding the transcript. |

## What's in v0.5.0

| Surface             | Status |
|---------------------|--------|
| **Postgres backend** | ✅ Full `Store` adapter over `pgx/v5` + embedded `golang-migrate`. Same interface as SQLite; adapters share a contract suite so they can't drift. See [`docs/POSTGRES.md`](docs/POSTGRES.md). |
| **SQLite stays first-class** | ✅ Default backend; both adapters tested against the same behavioural contract suite. |
| **Heartbeat sweeper** | ✅ Periodic background goroutine marks runs whose process crashed mid-loop as `failed`. Default-on, env-tunable. |
| **Session-lock map GC** | ✅ Refcounted + idle-pruned; closes the v0.3.2 leak where `sessionLocks` grew monotonically. |
| **CLI subcommands**  | ✅ `loomcycle validate` · `agents list` · `health` · `migrate up\|down\|status` · `migrate sqlite-to-postgres` |
| **`make pg-up` / `pg-down`** | ✅ Local Postgres fixture for tests + dev. |

The bulk of v0.5.0 is operational: backbone you'll need before scaling past one replica. SQLite stays the default for compact installs.

## What's in v0.4.0

| Surface             | Status |
|---------------------|--------|
| **Providers**       | Anthropic ✅ · OpenAI ✅ · Ollama ✅ (tool-tuned models only). DeepSeek added in v0.6.0; Gemini in v0.7.2; Ollama-local split out in v0.8.3. |
| **Built-in tools**  | Read · Write · Edit · HTTP · WebFetch · WebSearch · Bash · **Agent** · **Skill** (Memory added in v0.8.0) |
| **MCP transports**  | stdio (pooled, auto-respawn) · HTTP (Streamable, SSE-aware) |
| **MCP startup retry** | Exponential backoff handshake on boot — handles peer-still-starting races |
| **LocalAPI gateway** | ⏳ scaffolded — useful for consumers that have an OpenAPI spec but don't want to stand up an MCP server. Not the v0.4 integration vehicle (jobs-search-agent migrated to the MCP-server pattern instead). |
| **Sub-agents**      | Agent built-in spawns child runs; depth-capped; parent host policy + identity inherit via ctx |
| **Skills**          | Approach A: static bundling at config-load (skill body concatenated into agent system prompt) |
| **Storage**         | SQLite (modernc.org, pure Go); sessions / runs / events tables; partial indexes for v0.4 sub-agent columns |
| **Concurrency**     | Global semaphore + bounded FIFO queue; backpressure → HTTP 429 |
| **Cancellation**    | Registry-based cancel API; cascades from parent to all children via `parent_agent_id` walk |
| **Adapters**        | TypeScript (`@loomcycle/client`) ✅ · Python ⏳ deferred (shipped in v0.5.5) |

> **v0.4.0 — released after end-to-end MCP integration with jobs-search-agent.** Two agents (`ats-filter`, `qa-agent`) now fetch context — and `qa-agent` also persists results — through typed `mcp__jobs__*` tools served by jobs-search-agent's own MCP server. This validates the runtime's MCP HTTP transport, host-policy inheritance, sub-agent retry, SSE response decoding, and Streamable-HTTP `Accept` handling against a real consumer. Per-agent migration in the consumer continues incrementally; the loomcycle surface is stable.
