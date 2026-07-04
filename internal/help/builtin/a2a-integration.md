---
name: a2a-integration
description: How loomcycle speaks Agent2Agent (A2A) — exposing loomcycle agents as an A2A server (well-known card + REST/JSON-RPC/gRPC bindings), calling remote A2A peers as synthetic tools, signed cards, multi-tenant routing, and the INPUT_REQUIRED ↔ Interruption bridge.
---

# A2A (Agent2Agent) interoperability

A2A is an open protocol for agents that live in different runtimes to
discover and call each other: a caller fetches a peer's **AgentCard**
(its identity + skills + transport endpoints), then sends it a message
and consumes the resulting **Task** event stream. loomcycle speaks A2A
on **both** sides:

- **Server surface** — loomcycle publishes an AgentCard and accepts
  inbound A2A calls, so an agent running in a Microsoft / Google / any
  A2A-capable stack can reach a loomcycle agent as a remote skill.
- **Client surface** — a loomcycle agent calls a remote A2A peer as an
  ordinary tool (`a2a__<peer>__<skill>`), so a loomcycle run can
  delegate to agents you don't own.

Both surfaces are off by default. The server is gated by an env var;
the client surface activates per peer the operator registers.

A2A is loomcycle's interop story to *other* runtimes. For loomcycle's
own multi-agent fan-out (parent agent spawns sub-agents) use the
`Agent` tool — see the `subagents` and `fan-out-patterns` topics. For
calling a typed HTTP API from a single peer, an MCP server is usually
the lighter choice (see `mcp-registry`). Reach for A2A when the other
side is an *agent* that already speaks the protocol.

## Enabling the server surface

```sh
LOOMCYCLE_A2A_ENABLED=1                       # turns the server surface on
LOOMCYCLE_A2A_SERVER_CARD=loomcycle-fleet     # the active A2AServerCardDef name (required)
LOOMCYCLE_A2A_PUBLIC_BASE_URL=https://agents.example   # externally-reachable origin advertised in the card
LOOMCYCLE_A2A_TENANCY_ROUTING=none            # none|host|path (default none)
```

When `LOOMCYCLE_A2A_ENABLED=1`, loomcycle mounts (additively — none of
this touches `/v1/*`, MCP, or `/ui`):

| Path | Binding |
|---|---|
| `/.well-known/agent-card.json` | The AgentCard discovery URI (unauthenticated). |
| `/a2a/v1` (+ subpaths) | REST (HTTP+JSON) binding. |
| `/a2a/jsonrpc` | JSON-RPC binding. |
| `/a2a/grpc` (the gRPC service) | gRPC binding, registered on loomcycle's shared gRPC server. |

The well-known card advertises all three interface URLs so an A2A
client picks whichever transport it prefers (the reference SDK defaults
to JSON-RPC, then REST). The card is served with `Cache-Control:
max-age=300`.

The binding endpoints are **not** wrapped by loomcycle's bearer
`authMiddleware`. A2A auth happens *inside* the protocol handler: a
per-request interceptor maps the peer's own credential to a run
principal. The `?extended=true` card variant — and the `/extended`
path — are the exception: they are gated behind admin bearer auth, and
an unauthenticated `?extended=true` silently serves the base card
rather than leaking the extended surface.

## `A2AServerCardDef` — exposing agents over A2A

The server publishes exactly one active card, declared in yaml
(`a2a_server_cards:`) or authored at runtime via the `A2AServerCardDef`
tool (yaml is the operator-blessed root; the substrate is the derived
fork layer). Shape:

```yaml
a2a_server_cards:
  loomcycle-fleet:
    name: loomcycle-fleet
    description: Acme's loomcycle agent fleet
    provider:
      organization: Acme
      url: https://acme.example
    capabilities:
      streaming: true
    sign_with_key_env: LOOMCYCLE_A2A_SIGNING_KEY   # optional; see "Signed cards"
    security_schemes:
      - kind: http        # http | apiKey advertised; oauth2/mtls not yet enforceable
        scheme: bearer
    exposed_agents:
      - agent_name: company-researcher   # a loomcycle agent name
        skill_id: research               # the A2A skill id a peer targets
        skill_name: Research
        description: Researches a company
        tags: [web, search]
        input_modes: [text/plain]
        output_modes: [text/plain]
      - agent_name: report-writer
        skill_id: write
        skill_name: Write
```

Each `exposed_agents` entry becomes one **AgentCard skill**. The
`skill_id` is the routing key: an inbound message carries it in
`Message.Metadata["skillId"]`, and the server dispatches to the mapped
loomcycle agent. A request that names an **unknown or unexposed skill**
is rejected (terminal FAILED) — a peer cannot reach an agent that isn't
in `exposed_agents` by omitting or guessing the skill id. A single-skill
card may route every request to its one agent.

Only `http` and `apiKey` security schemes are advertised on the served
card; `oauth2` / `mtls` entries are skipped rather than advertised,
so the card never claims a scheme the frontier can't enforce.

## `A2AAgentDef` — registering a remote peer

To let loomcycle agents *call* a peer, register it in `a2a_agents:`
(or via the `A2AAgentDef` tool):

```yaml
a2a_agents:
  partner-research:
    # Discovery is EITHER a well-known card URL …
    agent_card_url: https://partner.example/.well-known/agent-card.json
    # … XOR a direct endpoint + binding (skip card fetch):
    # endpoint: https://partner.example/a2a/jsonrpc
    # binding: jsonrpc
    verify_signed_card: false           # true ⇒ REQUIRE a valid card signature
    auth:
      scheme: http                      # http | apiKey | oauth2 | mtls
      bearer_credential_ref: partner_token   # key into the run's UserCredentials
    expected_skills:
      - id: research
        required: true
```

For each `(peer, expected_skill)` pair loomcycle synthesises one tool,
`a2a__<peer>__<skill>` (e.g. `a2a__partner-research__research`), at
boot — mirroring how an MCP server registers `mcp__<server>__<tool>`.
An agent reaches the peer **only** by listing that tool (or an
`a2a__<peer>__*` glob) in its `tools`; peer + skill identities
come solely from operator-registered sources, never from model text.
A peer with no `expected_skills` registers no tool (and logs why).

When the tool runs, the result maps back to the model:

- A peer reply (`Message`) or a `COMPLETED` task → the answer text.
- A `FAILED` / `REJECTED` task → surfaced as a tool error so the model
  can self-correct.

## Signed cards

A served card is signed (ES256 over the JCS canonicalisation of the
card, JWS detached) when `sign_with_key_env` names an env var that is
**on the operator's env allowlist** (the same
`LOOMCYCLE_*`-style allowlist the scheduler and RFC F credentials use)
**and** that var holds a usable P-256 PEM key. The signature embeds the
matching public key (a self-contained JWS) so a verifier needs no
separate key fetch.

Signing is best-effort and **never fails card serving**: no key
configured, a non-allowlisted env name, an unset var, or a malformed
key all serve the card **unsigned** with a single trace line (the key
value is never logged). This means a substrate-authored card cannot
name an arbitrary env var and exfiltrate it into a signature — the
allowlist is the floor.

Inbound verification is **tolerant by default**: when a registered peer
sets `verify_signed_card: false`, loomcycle accepts an unsigned or
unverifiable card. Set `verify_signed_card: true` to *require* a valid
signature on the fetched peer card before any call is made.

**What `verify_signed_card` does and does not prove.** The JWS is
self-contained: the signature verifies against a public key embedded in
the card's own protected header, with no external trust anchor. So
`verify_signed_card: true` proves the card's **integrity** — it was not
altered after signing — but **not** the peer's **identity**. Peer
identity rests on TLS: the card is fetched over HTTPS from the peer's own
well-known URI, so the transport authenticates the origin. Treat the flag
as tamper-evidence on top of TLS, not as a replacement for it; pinning a
peer's key out-of-band is a future enhancement.

**Auth-scheme support.** Outbound peer auth currently wires the
bearer-style schemes (`http`, `apiKey`) via a `bearer_credential_ref`
resolved from the run's credentials. `oauth2` and `mtls` are accepted in
config but not yet wired — a peer declaring them is not callable yet.

## Multi-tenant routing

`LOOMCYCLE_A2A_TENANCY_ROUTING` selects how the inbound tenant is
derived. The tenant is **host- or path-authoritative** — it comes only
from the request's host or URL path, never from the message body:

- `none` (default) — single-tenant. One card, no tenant.
- `host` — the tenant is the `tenant-<id>` subdomain label
  (`tenant-acme.agents.example` → tenant `acme`). A bare host root
  serves the single-tenant card.
- `path` — the tenant is a leading path segment
  (`/acme/.well-known/agent-card.json` → tenant `acme`); the served
  card's interface URLs are prefixed with `/acme` so the peer POSTs
  back to the same tenant-scoped binding. A bare root serves the
  un-prefixed card.

In every mode, distinct tenants get distinct cards / binding URLs, and
the routed tenant flows into run attribution — a cross-tenant request
cannot borrow another tenant's identity.

## INPUT_REQUIRED ↔ Interruption

loomcycle's `Interruption` tool is the human-in-the-loop primitive (see
the `interruption` topic). Over A2A it maps onto the protocol's
`INPUT_REQUIRED` task state:

1. A run that parks on `Interruption.ask` surfaces a terminal-looking
   `TASK_STATE_INPUT_REQUIRED` event carrying the question text. The
   loomcycle run stays alive in the background, blocked on the bus.
2. The peer answers by sending a **follow-up message on the same
   taskId**. loomcycle resolves the pending interruption with that
   answer and resumes the *same* run to its real terminal state — A2A
   resume and HTTP resume converge on one mechanism.
3. A follow-up on an **unknown** taskId starts a fresh run instead.

This resume path requires the server to be wired to the same
notification bus the Interruption tool waits on (loomcycle's default
wiring does this). Without it, a parked run still surfaces
INPUT_REQUIRED but a follow-up starts a new run rather than resuming.

`AUTH_REQUIRED` is treated as a **terminal FAILED** outcome — loomcycle
does not implement an interactive auth-elevation round-trip; the
operator supplies peer credentials up front (see below).

## Per-run credentials for outbound peer auth

A peer whose `auth.scheme` declares a `bearer_credential_ref` resolves
the bearer from the **run's `UserCredentials` map** (the RFC F
per-run-credentials seam — see the `per-run-credentials` topic), keyed
by the ref name. The credential is supplied by the caller on the run
request (or a schedule fork's `user_credentials`), never hardcoded in
the agent or the Def. A missing/empty ref at call time is an error, not
a silent unauthenticated call. Sub-agents inherit the same credential
map as their parent.

## Worked example

**Expose a loomcycle agent, then have an external client call it.**

```yaml
# loomcycle.yaml
a2a_server_cards:
  loomcycle-fleet:
    name: loomcycle-fleet
    capabilities: { streaming: true }
    exposed_agents:
      - { agent_name: company-researcher, skill_id: research, skill_name: Research }
```

```sh
LOOMCYCLE_A2A_ENABLED=1 \
LOOMCYCLE_A2A_SERVER_CARD=loomcycle-fleet \
LOOMCYCLE_A2A_PUBLIC_BASE_URL=https://agents.example \
  ./bin/loomcycle --config loomcycle.yaml
```

An external A2A client fetches
`https://agents.example/.well-known/agent-card.json`, sees the
`research` skill, and sends a message with
`Message.Metadata["skillId"] = "research"`. loomcycle routes it to the
`company-researcher` agent and streams the run back as A2A Task events,
ending in `COMPLETED`.

**Register a remote peer, then call it from an agent.**

```yaml
# loomcycle.yaml
a2a_agents:
  partner-research:
    agent_card_url: https://partner.example/.well-known/agent-card.json
    auth: { scheme: http, bearer_credential_ref: partner_token }
    expected_skills:
      - { id: research, required: true }

agents:
  orchestrator:
    tools: [Read, a2a__partner-research__research]
```

The `orchestrator` agent can now call
`a2a__partner-research__research`; loomcycle fetches the peer card,
resolves the `partner_token` bearer from the run's `UserCredentials`,
sends the message, and returns the peer's answer to the model.

## Troubleshooting

- **"active server card not found"** — `LOOMCYCLE_A2A_SERVER_CARD`
  names a card that isn't in `a2a_server_cards:` and has no active
  `A2AServerCardDef` version. Check the name and that the substrate
  version is *active* (not draft/retired).
- **"card exposes no usable agents"** — every `exposed_agents` entry
  needs both `agent_name` and `skill_id`.
- **Card served unsigned despite `sign_with_key_env`** — the env var
  isn't on the operator allowlist, is unset, or holds a malformed key.
  Look for the single trace line naming the reason; the key value is
  never logged.
- **Peer tool not registered** — the peer has no `expected_skills`, or
  its name only has draft/retired `A2AAgentDef` versions (no active
  def ⇒ no callable tool). A boot triage line names which peer was
  skipped.
- **Peer call fails with "no bearer_credential_ref"** — the peer
  declares an auth scheme but the run didn't supply the referenced
  credential in `UserCredentials`. Supply it on the run request.
- **`LOOMCYCLE_A2A_TENANCY_ROUTING` rejected** — it must be exactly
  one of `none`, `host`, or `path` when the server is enabled.
