---
name: Workspace/screenshot
description: "Workspace op=screenshot — load a page your workspace's server serves in a headless browser and look at the picture."
---
`screenshot` loads a page that a running server of your own workspace
serves in a headless browser, and returns the picture to you as an image,
so you can check what a web page you built looks like. It needs both the
`exec` and the `connect` grant, and a workspace on a worker whose sessions
carry the browser.

The browser runs inside the workspace and reaches only the workspace's own
server: it loads `http://127.0.0.1:<port><path>`, and everything else the
page asks for, another port or an address included, goes nowhere. It
carries no cookie or sign-in of yours, and starts with a fresh profile
each time.

## Arguments

- `name` (required) — the workspace.
- `port` (required) — the port of a running server of this workspace, as
  you gave it to `start`.
- `path` — the page to load, starting with `/` (default `/`). A path
  only, never a URL; percent-encode spaces and anything outside ASCII.
- `width`, `height` — the browser's window in pixels (default 1280 by
  800, at most 1920 by 1080).

## Returns

`{name, port, path, width, height, bytes}`, then the picture itself. A
picture over 1568 pixels on its long side is scaled down first.

## Errors

- `not_granted` — a screenshot needs both the `exec` and the `connect`
  grant.
- `browser_unavailable` — the workspace's worker has no browser: it runs
  the host engine, or its sessions use the plain session image.
- `port_not_served` — no running server of this workspace declared that
  port; `start` one with it.
- `invalid_argument` — the path is a URL, does not start with `/`, or the
  window is too large.
- `browser_busy` — a screenshot of this workspace is being taken already.
- `browser_timed_out` — the page did not load and draw within 30 seconds.
- `too_large` — the picture is over 20 MiB; try a smaller window.
- `not_shared_op` — a screenshot is your own workspace's, never one shared
  with you.

## Examples

```json
{"op": "screenshot", "name": "site", "port": 8000, "path": "/report", "width": 1280, "height": 800}
```

```json result
{"bytes": 61204, "height": 800, "name": "site", "path": "/report", "port": 8000, "width": 1280}
```
