# Customizing agents — adding skills, tools, and MCP servers

You start from a bundled agent (say `chat/medium`) and want to teach it something new: a skill, a tool, a whole MCP server. loomcycle is deliberately strict about *who* may widen an agent's tools — so a compromised or prompt-injected agent can't escalate its own privileges — which makes the *how* non-obvious. This guide is the correct algorithm.

The short version: **adding a skill that uses tools the agent already has is free; adding a new tool is an operator action (create a new agent with the wider tool list); and you never let an in-run agent widen its own tools.**

## The capability model in one minute

- **An agent's tool ceiling is its `tools:` list.** That is the hard boundary — an agent can only ever call tools in its own `tools:`, enforced by the dispatcher (see [`docs/TOOLS.md`](TOOLS.md), the two-layer default-deny model).
- **Skills can't widen tools.** A skill is *instructions*; when it loads, its declared `tools:` must be a subset of the agent's `tools:`. A skill can never grant a tool the agent doesn't already hold.
- **Sub-agents can have different tools.** The sanctioned way to *compose* a bigger capability without widening a chat agent is to delegate to a purpose-built sub-agent (each is operator-vetted). This is exactly how the `chat/*` agents reach the code sandbox — they hold the `Agent` tool and delegate to `dev/sandbox`, which holds the sandbox tools.
- **Only an operator may widen a ceiling.** Widening `tools:` is a human/operator action — the Web UI console or the API, over an admin/operator bearer. An **in-run LLM can never widen its own (or a child's) tools**: `fork`/`create` invoked from inside a run are subset-checked against the caller's own tools, so a prompt injection can't escalate. The operator bearer is the exemption (the bearer *is* the boundary).

## Which case are you in?

1. **A skill that uses only tools the agent already has** → create a SkillDef. No fork, same conversation. See [Case 1](#case-1--add-a-skill).
2. **A new tool, or a skill/MCP tool the agent doesn't have** → the tool ceiling must widen → create a new agent with the extended `tools:`. See [Case 2](#case-2--add-a-new-tool-or-mcp-server).

## Case 1 — Add a skill

A chat agent that ships with the `Skill` tool and no `skills:` restriction (like `chat/medium`) has an **allow-all** skill policy: it can discover and load *any* skill on demand, as long as that skill's `tools:` are a subset of the agent's tools.

So adding a skill is just authoring one:

- **Web UI:** Library → Skills → Create. Give it a `/`-grouped name (e.g. `research/summarize`), a body (the instructions), and the `tools:` it needs (must be ⊆ the agent's tools).
- **Or the `SkillDef` tool** (`op=create`) from an agent whose `skills:` allowlist permits the name.

The agent picks it up on its **next turn** — same conversation, no fork, no restart. If the skill declares a tool the agent lacks, loading it is refused (an `is_error` result) — that means you're actually in Case 2.

> Skills are on-demand: the body is loaded via the `Skill` tool at run time, not baked into the prompt. See the `skills:` pattern-allowlist model in [`docs/TOOLS.md`](TOOLS.md) and [`docs/CONFIGURATION.md`](CONFIGURATION.md).

## Case 2 — Add a new tool (or MCP server)

The tool ceiling can only be widened by an operator, and a **static (bundled) agent cannot be widened in place**. So the pattern is **create a new agent with the wider ceiling**:

1. **Create a new agent** with a new name (e.g. `chat/mine`), seeded from the bundled one's configuration, and add the tool(s) you want to its `tools:`.
   - **Web UI:** Library → Agents → Create. Set the name, system prompt, tier, and the `tools:` list including the new tool. Over an admin/operator bearer this is unbounded — the bearer is the authority.
   - **Or the `AgentDef` tool** `op=create` under a new name with the extended `tools:` list, over an operator bearer.
2. **Talk to the new agent.**

**Why not `fork`?** `fork` may only *narrow* tools — its ceiling is the lineage root — by design, so a fork can't escalate. And `create` refuses a name that matches a bundled/static agent, so a clone must take a new name.

**Why not edit `chat/medium` directly?** It's a static bundled agent (ground truth in the config); it's immutable at runtime. Derive your own agent from it instead.

> ⚠️ **Conversation history does not carry over.** A conversation is bound to its agent (the agent is a column on the session). The chat you had with `chat/medium` does **not** continue under `chat/mine` — the new agent starts fresh. (Carrying a conversation into a derived agent is a planned convenience — see [Planned improvements](#planned-improvements).)

### Adding a whole MCP server

To give an agent every tool of an MCP server:

1. **Declare the server** (once):
   - **Static:** add it to `mcp_servers:` in your config — see [`docs/MCP_INTEGRATION.md`](MCP_INTEGRATION.md).
   - **Runtime:** the `MCPServerDef` tool (`op=create`) or Web UI → Library → MCP.
2. **Grant its tools** in the (new) agent's `tools:`. You can name individual tools by their full name `mcp__<server>__<tool>`, or grant the **whole server with one prefix-glob entry**:

   ```yaml
   tools: [Read, Write, WebSearch, "mcp__slack__*"]   # all tools of the "slack" server
   ```

   Prefix globs (`mcp__<server>__*`) are matched at the agent-exposure layer (see [`docs/TOOLS.md`](TOOLS.md) → *Glob support*), so tools later added to that server are covered without editing the agent again.

Because that adds a new tool to the ceiling, use the Case 2 flow (create a new agent) to introduce it.

## Widening a *dynamic* agent in place

If the agent you want to widen is already a **dynamic** agent — one you created, not a bundled static one — an operator can widen it **in place** instead of making yet another name: re-run `create` under the **same name** with the extended `tools:` (over the operator bearer). It becomes a new active version of that name, and any ongoing conversation picks up the wider tools on its **next turn** — same identity, no lost history.

This is why the recommended pattern is: **derive your own dynamic agent from the bundled one once** (Case 2), then widen *that* in place whenever you need more — you only pay the "new identity / fresh conversation" cost a single time.

## Prefer delegation when you can

Widening a chat agent's own ceiling gives it — and anything that can prompt it — direct access to the new tool. When the new capability is a self-contained task (run code, hit a specific API, drive a browser), the safer composition is a **dedicated sub-agent**: give the sub-agent the powerful tools, keep your chat agent's ceiling narrow, and let it **delegate** via the `Agent` tool. The chat agent orchestrates; the sub-agent is the only thing holding the sharp tools, and it was vetted by you. (This is the model the `dev/sandbox` agent uses for code execution.)

## Security — why it works this way

- **The tool ceiling is a trust boundary.** An in-run LLM can never grant itself or a child a tool beyond its own set, so a prompt injection can't escalate. `fork`/`create` from inside a run are subset-checked; only the operator bearer is unbounded.
- **Skills are authority-limited** to the agent's tools — they add instructions, not capabilities.
- **Delegation** to an operator-vetted sub-agent is the safe way to compose a larger capability.

## Planned improvements

These manual steps are being made smoother:

- A one-click **Clone** action in the Library (derive a new agent from an existing one with tools pre-filled and editable).
- Wildcard MCP grants understood by the **fork/create ceiling check**, not just the exposure layer (so derived agents and meta-agents can carry a `mcp__<server>__*` grant).
- **Carrying a conversation** into a derived agent (optionally compacted), so widening a capability no longer means starting the chat over.

## See also

- **Agents themselves** can read the model-facing version of this algorithm at run time via `Context op=help topic=adding-capabilities` (why you can't widen your own tools; load a skill within your ceiling, delegate to a sub-agent, or ask the operator).
- [`docs/TOOLS.md`](TOOLS.md) — the two-layer default-deny model, every built-in tool, glob support, per-request narrowing.
- [`docs/CONFIGURATION.md`](CONFIGURATION.md) — agent frontmatter fields, tiers, the `skills:` allowlist.
- [`docs/MCP_INTEGRATION.md`](MCP_INTEGRATION.md) — declaring and consuming MCP servers.
- [`docs/HISTORY.md`](HISTORY.md) — browsing and resuming past chats (a chat = a session).
