# Migrating from Bash and Bashbox to workspaces

The `Bash` tool, Bashbox (with its host-command fallback), the builder sidecar's `mcp__sandbox__*` tools and the `shell_credentials` agent field are deprecated. They still work, and a future release removes them (RFC GW §9). Workspaces replace them: an agent runs commands, scripts and servers in a workspace on a workspace worker, a process outside the core that holds no credential.

Why they go:

- `Bash` is not a sandbox. It reaches any file the runtime's user can, and any address.
- The Bashbox host fallback runs real host binaries with the same reach.
- A credential handed to either is readable by the model.
- Bashbox's interpreter runs inside the core's own memory.

A workspace keeps commands off the core's machine, and a credential never enters it: the core adds it to the requests that leave.

loomcycle warns about each use at boot, and `loomcycle doctor` lists them under its "Deprecated" check. Each warning names the item, says what replaces it, and links a section below.

Agent definitions stored at run time (`AgentDef`, `register_agent`) are seen only when a run is given one. Each such run is counted in `loomcycle_deprecated_use_total{kind,name}`. The first run per tenant and agent in a process also logs one warning, which names the agent and the items, never a credential.

An admin sees both in the Web UI, under Settings → Health: the lines boot printed and the runs this replica gave each item. An agent reads its own in `Context op=self`, whose `deprecations` lists each deprecated tool or field it was given, with the replacement.

The workspaces guide (`docs/WORKSPACES.md`) covers turning workspaces on (`workspace.mode`), the agent's `workspace` grant, and egress routes.

## Environment variables

| Variable | What replaces it |
|---|---|
| `LOOMCYCLE_BASH_ENABLED` | Turn on workspaces (`workspace.mode`) and give the agent the `Workspace` tool with a `workspace` grant. |
| `LOOMCYCLE_BASH_ALLOWED_CREDS` | An egress route with a credential (`workspace.egress.routes`, or a tenant's `EgressRouteDef`). The core adds the credential to the request; no shell sees it. |
| `LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS` | Run the commands in a workspace, on a worker that carries their toolchain. |
| `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_ENV` | The same: a workspace's session has its own environment, and no secret is in it. |
| `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS` | An egress route with a credential, or, for git, a git workspace opened with the credential. |
| `LOOMCYCLE_BASHBOX_OVERLAY_MAX_BYTES` | Nothing. Bashbox's in-process sandbox goes; a workspace has a disk quota on its worker instead. |

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

The `dev-workspace` bundle replaces the `sandbox` + `dev-exec` pair. Its `dev/exec` takes the same command envelope and returns the same result shape, running it in a workspace, and its `dev/exec-usage` skill replaces `dev/sandbox-usage`. Select `LOOMCYCLE_PRESETS=…,dev-workspace` in place of `sandbox,dev-exec`: both define `dev/exec`, and the later one wins. Envelope fields a workspace has no setting for (`session_id`, `keep_open`, `close`, `network`, `expose`, `cpu`, `mem_mb`, `pids`, `tmpfs_mb`, `git_setup`, `max_output_bytes`) are reported as ignored in `final_text`. A workspace is released when dev/exec's run ends; to keep files between calls, give dev/exec a fork of your own workspace (`Agent op=spawn … workspaces=[{fork: "<name>"}]`) and name it in the envelope's `workspace`.

## shell_credentials

`shell_credentials` narrowed which operator credentials a shell received. A workspace's session receives none. A script reaches an API through an egress route, and the core adds the route's credential on the way out. Remove the field, and give the agent the routes it needs in `workspace.egress`.
