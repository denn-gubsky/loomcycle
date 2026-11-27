# Migrating from Bash and Bashbox to workspaces

The `Bash` tool, Bashbox (with its host-command fallback), the builder sidecar's `mcp__sandbox__*` tools and the `shell_credentials` agent field were removed in v1.172.0 (RFC GW P15). Workspaces replace them: an agent runs commands, scripts and servers in a workspace on a workspace worker, a process outside the core that holds no credential.

Why they went:

- `Bash` was not a sandbox. It reached any file the runtime's user could, and any address.
- The Bashbox host fallback ran real host binaries with the same reach.
- A credential handed to either was readable by the model.
- Bashbox's interpreter ran inside the core's own memory.

A workspace keeps commands off the core's machine, and a credential never enters it: the core adds it to the requests that leave.

**Operator config that still names a removed item does not start.** A yaml or markdown agent whose `tools` names `Bash` or `Bashbox`, any `shell_credentials` key (`[]` included), any of the variables below set to a non-empty value, and `LOOMCYCLE_PRESETS` naming `sandbox` or `dev-exec` each refuse the start. One error lists every use, each with the line to change and a link to its section below. `loomcycle doctor` shows the same lines under its "Removed" check.

**Stored definitions keep running without the item.** An agent definition stored at run time (`AgentDef`, `register_agent`, a TeamDef's inline agent) that names `Bash`, `Bashbox` or `shell_credentials` is not refused: its runs go on without the item, and each run starts with one `capability_inert` event, gate `removed`, naming it. Each such run is counted in `loomcycle_deprecated_use_total{kind,name}`, and the first run per tenant and agent in a process logs one warning that names the agent and the items, never a credential. An admin sees them in the Web UI under Settings → Health, and an agent reads its own in `Context op=self`, whose `deprecations` lists each dropped item with its replacement. Fork each such definition without the item, and with `Workspace`, so its runs can still run commands.

**New authoring is refused.** `AgentDef create`, `register_agent` (HTTP, gRPC, MCP) and an `AgentDef fork` overlay that name a removed item are refused with the line to change. A `fork` that only inherits one from its base has it stripped, and the result says so.

The workspaces guide (`docs/WORKSPACES.md`) covers turning workspaces on (`workspace.mode`), the agent's `workspace` grant, and egress routes.

## Upgrading to v1.172.0

Run `loomcycle doctor` on v1.171 first: its "Deprecated" check lists every use in your config, and `loomcycle_deprecated_use_total` lists the stored definitions in use. Then, for each item:

| If your deployment has | Change it to |
|---|---|
| `LOOMCYCLE_BASH_ENABLED=1` | Remove the line. Set `workspace.mode: local` (single binary) or attach workspace workers, and give each agent that ran commands `Workspace` and `workspace: {ops: [read, write, exec], open: true}`. |
| `LOOMCYCLE_BASH_ALLOWED_CREDS` | Remove the line. Declare an egress route with the credential (`workspace.egress.routes` in yaml, or an `EgressRouteDef`) and name it in the agent's `workspace.egress`. |
| `LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS`, `_ALLOWED_ENV` | Remove the lines. The commands run in a workspace whose worker image carries them (`loomcycle-workspace` has Python and Node; build on it for more). A session gets no host variable. |
| `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS` | Remove the line. Use an egress route with the credential, or for git `Workspace op=open {repo, credential}`. |
| `LOOMCYCLE_BASHBOX_OVERLAY_MAX_BYTES` | Remove the line. A workspace has a disk quota instead. |
| `LOOMCYCLE_PRESETS` with `sandbox` or `dev-exec` | Replace them with `dev-workspace`. |
| An `mcp_servers.sandbox` block that overlays the `sandbox` bundle's server | Delete it, and stop the `builder-sidecar` container, its `loom-dev` network and its `SANDBOX_*` secrets file. Callers of `dev/exec` keep working through `dev-workspace`. The name `sandbox` itself is not refused: a complete `mcp_servers.sandbox` of your own is an ordinary MCP server, but no new builder image is published. |
| A yaml or markdown agent whose `tools` names `Bash` or `Bashbox` | Replace them with `Workspace` and a `workspace:` grant. Drop any `mcp__sandbox__*` names once the sidecar is gone. |
| `shell_credentials:` on any agent | Delete the key. |
| The `loomcycle-toolbox` image | Use `denngubsky/loomcycle` and run commands on a workspace worker (see "Images"). |
| Stored `AgentDef`s naming a removed item | Nothing is required to start. Fork each without the item (and with `Workspace`) so its runs can still run commands. |
| ACL rules or hook definitions naming `tool:Bash` | Nothing: they stay valid and match nothing. Rewrite them for `tool:Workspace` if they still mean something. |

An operator with `LOOMCYCLE_BASH_ENABLED`, `LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS` and `_ALLOWED_ENV` set, `LOOMCYCLE_PRESETS=…,sandbox,dev-exec` and an `mcp_servers.sandbox` overlay changes: remove the three variables, replace the two presets with `dev-workspace`, delete the overlay and the sidecar service, set `workspace.mode` (or attach a `loomcycle-ws-worker`), and give the agents that ran commands `Workspace` with a grant. The variables and presets, left in place, stop the start with an error naming each; an overlay left alone without the bundle under it is refused by the ordinary MCP server validation if incomplete.

One side effect for authors: an `AgentDef create` whose body equals a stored row that still carries `shell_credentials` now makes a new version instead of returning the stored one. The new version is the clean one.

## Environment variables

Each of these, set to a non-empty value, refuses the start with the line to remove; set but empty, it is reported as a dead line. `LOOMCYCLE_BASHBOX_ENABLED`, Bashbox's old gate, refuses the same way.

| Variable | What replaces it |
|---|---|
| `LOOMCYCLE_BASH_ENABLED` | Turn on workspaces (`workspace.mode`) and give the agent the `Workspace` tool with a `workspace` grant. |
| `LOOMCYCLE_BASH_ALLOWED_CREDS` | An egress route with a credential (`workspace.egress.routes`, or a tenant's `EgressRouteDef`). The core adds the credential to the request; no shell sees it. |
| `LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS` | Run the commands in a workspace, on a worker that carries their toolchain. |
| `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_ENV` | The same: a workspace's session has its own environment, and no secret is in it. |
| `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS` | An egress route with a credential, or, for git, a git workspace opened with the credential. |
| `LOOMCYCLE_BASHBOX_OVERLAY_MAX_BYTES` | Nothing. Bashbox's in-process sandbox is gone; a workspace has a disk quota on its worker instead. |

## The Bash tool

Replace `Bash` in an agent's `tools` with `Workspace`, and give the agent a grant:

```yaml
agents:
  coder:
    tools: [Workspace]
    workspace:
      ops: [read, write, exec]
      open: true
      egress: [pypi, pypi-files, npm]   # only if it installs packages
```

The agent opens a workspace (`Workspace op=open`), runs commands in it (`op=exec`), and moves files in and out with the `File` tool.

## Bashbox

Bashbox's read-only interpreter and its host-command fallback are both replaced by a workspace. Read-only work becomes a grant of `ops: [read]`. Commands, including `git` and `gh` that the fallback ran on the host, run in a workspace, and git's network work goes through the typed git ops of a git workspace.

## The builder sidecar

The builder sidecar (`deploy/builder`, the `sandbox` and `dev-exec` bundles, the `mcp__sandbox__*` tools) is replaced by a workspace worker with the `container` engine, which runs each workspace in a container of its own. Its MCP tools map onto `Workspace` ops: open, exec, write, read, release.

The `dev-workspace` bundle replaces the `sandbox` + `dev-exec` pair. Its `dev/exec` takes the same command envelope and returns the same result shape, running it in a workspace, and its `dev/exec-usage` skill replaces `dev/sandbox-usage`. Select `LOOMCYCLE_PRESETS=…,dev-workspace` in place of `sandbox,dev-exec`; naming either removed bundle refuses the start. Envelope fields a workspace has no setting for (`session_id`, `keep_open`, `close`, `network`, `expose`, `cpu`, `mem_mb`, `pids`, `tmpfs_mb`, `git_setup`, `max_output_bytes`) are reported as ignored in `final_text`. A workspace is released when dev/exec's run ends; to keep files between calls, give dev/exec a fork of your own workspace (`Agent op=spawn … workspaces=[{fork: "<name>"}]`) and name it in the envelope's `workspace`.

## Images

The builder sidecar's images (`loomcycle-builder`, `loomcycle-builder-docker`, `loomcycle-sandbox-session`) and the `loomcycle-toolbox` image are no longer built. Their old tags stay published; no tag is pushed after v1.171.x.

- **In place of the builder sidecar:** a workspace worker on the `container` engine. `denngubsky/loomcycle-ws-worker` is a worker that drives the host's Docker, and `denngubsky/loomcycle-workspace` is the session image it starts for each workspace.
- **In place of the toolbox image:** the distroless `denngubsky/loomcycle`, with commands run on a workspace worker whose image carries the toolchain. `loomcycle-workspace` has Python and Node; build on it for more.

## shell_credentials

`shell_credentials` narrowed which operator credentials a shell received. A workspace's session receives none. A script reaches an API through an egress route, and the core adds the route's credential on the way out. Remove the field, and give the agent the routes it needs in `workspace.egress`.

## Before and after

An agent that ran tests through Bash, with a token for an API in its shell:

```yaml
# before (refuses the start on v1.172.0)
agents:
  coder:
    tools: [Read, Write, Edit, Bash]
    shell_credentials: [CRM_TOKEN]
# .env.insecure: LOOMCYCLE_BASH_ENABLED=1, LOOMCYCLE_BASH_ALLOWED_CREDS=CRM_TOKEN
```

The same agent on a workspace. The local worker suits a single trusted machine; elsewhere, attach workspace workers. The API is reached through a route whose credential the core adds:

```yaml
# after
workspace:
  mode: local
  egress:
    routes:
      crm:
        url: "https://api.crm.example/v2/"
        credential: "$cred:crm_token"   # restricted (allowed_urls) to the route's URL
agents:
  coder:
    tools: [Read, Write, Edit, Workspace, File]
    workspace: {ops: [read, write, exec], open: true, egress: [crm]}
```

A script then calls `"${LOOMCYCLE_EGRESS}crm/deals"`, and no token is in its environment. On the local worker (the `host` engine), egress also needs `LOOMCYCLE_WORKSPACE_HOST_EGRESS=1`, and there it is a convenience, not a boundary (see the workspaces guide). Remove `LOOMCYCLE_BASH_ENABLED` and `LOOMCYCLE_BASH_ALLOWED_CREDS`; the start is refused while either is set.

## The bundles

| Bundle | Agent or skill | Now |
|---|---|---|
| `team-examples` | `sdlc/coder` | Edits the `work` volume, builds and tests in a workspace (`File op=copy` the checkout in, `Workspace op=exec`). |
| `team-examples` | `sdlc/reviewer` | Reads the diff with the `Git` tool, runs the tests in its own workspace. |
| `agent-teams` | `team/orchestrator`, `team/sqldata` | Runs a data team's scripts in a workspace; holds the grant its handlers open workspaces under. |
| `agent-teams` | `team/repo` | Clones the repository into a git workspace, commits and pushes the team's branch, and opens the pull request, with the credential held by the core. |
| `chat` | `chat/medium`, `chat/local`, `chat/local-small` | Run code in a workspace. |
| `sandbox`, `dev-exec` | `dev/exec`, `dev/sandbox-usage` | Removed. Select `dev-workspace` instead (see "The builder sidecar"). |

Each moved agent's grant is `{ops: [read, write, exec], open: true}`, with no share, expose or templates. Its workspace path works only once `workspace.mode` is on or a workspace worker is attached; without one, these agents cannot run commands.

## What a workspace does not do

- **Isolation on the local worker.** The `host` engine runs commands as the worker's user with no isolation, as Bash did. On EC2 such a command can also read the instance role's credentials from the metadata service, and `loomcycle doctor` warns when `workspace.mode: local` runs beside a provider that uses AWS auth. For untrusted code, use a worker with the `container` engine.
