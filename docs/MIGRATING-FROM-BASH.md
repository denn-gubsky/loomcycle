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

## Before and after

An agent that ran tests through Bash, with a token for an API in its shell:

```yaml
# before
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

A script then calls `"${LOOMCYCLE_EGRESS}crm/deals"`, and no token is in its environment. On the local worker (the `host` engine), egress also needs `LOOMCYCLE_WORKSPACE_HOST_EGRESS=1`, and there it is a convenience, not a boundary (see the workspaces guide). Remove `LOOMCYCLE_BASH_ENABLED` and `LOOMCYCLE_BASH_ALLOWED_CREDS`; boot stops warning once nothing uses them.

## The bundles

| Bundle | Agent or skill | Now |
|---|---|---|
| `team-examples` | `sdlc/coder` | Edits the `work` volume, builds and tests in a workspace (`File op=copy` the checkout in, `Workspace op=exec`). Keeps Bashbox as a fallback. |
| `team-examples` | `sdlc/reviewer` | Reads the diff with the `Git` tool, runs the tests in its own workspace. Keeps Bashbox as a fallback. |
| `agent-teams` | `team/orchestrator`, `team/sqldata` | Runs a data team's scripts in a workspace; holds the grant its handlers open workspaces under. Keeps Bashbox. |
| `agent-teams` | `team/repo` | Still clones and opens the PR through Bashbox's host `git`/`gh`. It moves to git workspaces when their git operations ship (RFC GW P11). |
| `chat` | `chat/medium`, `chat/local`, `chat/local-small` | Run code in a workspace; keep Bash and Bashbox as a fallback. |
| `sandbox`, `dev-exec` | `dev/exec`, `dev/sandbox-usage` | Deprecated. Select `dev-workspace` instead (see "The builder sidecar"). |

Each moved agent's grant is `{ops: [read, write, exec], open: true}`, with no share, expose or templates. Its workspace path works only once `workspace.mode` is on, and until then it uses its old tool. The old tools go from the bundles in the release that removes them.

## What a workspace does not do yet

- **Git work with a forge token.** Cloning, committing and pushing from a workspace, with the credential held by the core, comes with git workspaces (RFC GW P11). Until then use the `Git` tool on a volume, or keep the Bashbox fallback for `git`/`gh`.
- **Your own machine as the worker.** A personal worker, a user's own machine attached for that user's runs, comes with RFC GW P13. The removal release is not cut before it ships, so a local install always keeps a supported way to run commands: `workspace.mode: local` today.
- **Isolation on the local worker.** The `host` engine runs commands as the worker's user with no isolation, as Bash did. For untrusted code, use a worker with the `container` engine.
