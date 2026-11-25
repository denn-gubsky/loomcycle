---
name: Workspace/expose
description: "Workspace op=expose — show a page your workspace's server makes to your user, in their chat."
---
`expose` publishes a port of your workspace to your user: the page a server
of the workspace serves on that port is shown in their chat (in loomboard),
as a page or a panel. Use it for a chart to explore, a form, a small app you
built for the question at hand.

Start the server first, with `start ... port`, then expose that port. The
page is reached only while a server of the workspace listens on the port
itself.

## Arguments

- `name` (required) — the workspace.
- `port` (required) — the port your server listens on.
- `title` — the page's title, one line, up to 200 bytes.
- `kind` — `page` (the default) or `panel`.

## Returns

`{name, exposure_id, port, title, kind, base_path}`. `base_path` is where the
page is served, `/v1/workspaces/exposures/<id>/`. Your application receives
paths without it (`/`, `/data.json`). A redirect it sends to `/x` is moved
under `base_path` for it; links inside its pages are its own, so make them
relative (`data.json`, not `/data.json`) or start them with `base_path`.

## What the page may do

- Only your user opens it, with their own credential; nobody else, an
  operator included.
- It runs scripts and forms in a sandbox: it cannot read the chat's storage,
  fetch another site, open popups or navigate away. No cookie reaches it or
  leaves it; keep state in the page or its address.
- Nothing a user does in the page enters your run. Read what happened with
  `request` to your own application, or the files it writes.

## Limits

- Needs the `expose` grant and the operator's `workspace.expose`.
- At most 2 ports of one workspace exposed at once (the operator's
  `max_per_workspace`).
- A page no one opened for 15 minutes closes (`idle_timeout_s`); so does
  every page of the workspace when it is released or your run ends.

## Errors

- `not_granted` — this agent may not expose ports.
- `expose_off` — the operator has not turned pages on.
- `exists` — the port is exposed already.
- `limit` — the workspace exposes the most ports it may; `unexpose` one.

## Examples

```json
{"op": "expose", "name": "site", "port": 8000, "title": "Sales by region"}
```

```json result
{"name": "site", "exposure_id": "4f1c0a9e2b7d3c5e6a8b9d0f", "port": 8000, "title": "Sales by region", "kind": "page", "base_path": "/v1/workspaces/exposures/4f1c0a9e2b7d3c5e6a8b9d0f/"}
```
