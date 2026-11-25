---
name: Workspace/request
description: "Workspace op=request — send one HTTP request to a server running in your workspace and get its answer."
---
`request` sends one HTTP request to a port a server of your workspace
listens on, and returns the answer: its status, headers and body. Use it to
try what you built: an API, a page, a form's handler. It is a single
request and answer; nothing stays open.

Only a port a **running** server of this workspace declared with
`start ... port`, and that the server itself (or a process it started)
listens on, is reached. Start the server first, with `port` set to the port
it binds.

Through a workspace shared with you (`<name>@<its agent id>`) with
`connect`, `request` reaches only a port its owner **exposed** with
`expose`, and needs your `workspace.connect` grant instead of `exec`. The
same checks then apply to the port.

## Arguments

- `name` (required) — the workspace.
- `port` (required) — the port, as declared at `start`.
- `method` (required) — the HTTP method, upper case: `GET`, `POST`, ...
- `path` (required) — the request target, starting with `/`, with its query:
  `/api/items?page=2`. Escape spaces and the like (`%20`).
- `headers` — headers to send, name to value. `Host` defaults to
  `localhost:<port>`. `Content-Length`, `Transfer-Encoding`, `Connection`
  and the other framing headers are set by the runtime and refused here.
- `body` — the body to send, text, up to 1 MiB.

Nothing else is added: no credential of yours, your run's or your user's.

## Returns

`{status, headers, body}`. `body` is the answer's body when it is text;
otherwise `body_base64` carries it. A body over 1 MiB is cut there and
`truncated` is true. A redirect is returned as it came, not followed.

## Limits

- The request waits as long as a command does by default (the operator's
  `exec` default), then gives up with `timeout`.
- What a server answers is untrusted output, like any command's.
- On the `host` engine every workspace of a worker shares one loopback: the
  worker checks that the port is listened on by your server's own
  processes and refuses a port two workspaces' servers both declare, but
  this is a check, not isolation.

## Errors

- `not_granted` — `request` needs the `exec` grant, as servers do; through a
  share, your `workspace.connect` grant and a share made with `connect`.
- `not_exposed` — through a share: its owner has not exposed that port, or
  the page closed (unexposed, idle, or the operator turned pages off).
- `port_not_served` — no running server of this workspace declared that
  port, or the server that declared it does not listen on it itself
  (something else on the worker does, or the server moved out of its
  process group). `logs` lists your servers and their ports.
- `port_unreachable` — a server declared the port but nothing listens on it
  yet; it may still be starting, or it failed: read its `logs`.
- `bad_response` — what came back was not HTTP.

## Examples

```json
{"op": "request", "name": "site", "port": 8000, "method": "GET", "path": "/"}
```

```json result
{"status": 200, "headers": {"Content-Type": "text/html; charset=utf-8"}, "body": "<!DOCTYPE html>..."}
```

```json
{"op": "request", "name": "api", "port": 3000, "method": "POST", "path": "/items", "headers": {"Content-Type": "application/json"}, "body": "{\"title\": \"milk\"}"}
```
