---
name: Workspace/view
description: "Workspace op=view — look at a picture in a workspace (PNG, JPEG, GIF, WebP), e.g. a chart you drew."
---
`view` returns a picture from a workspace to you as an image, so you can
check what you made: a chart, a rendered page, a diagram. It needs the
`read` grant, and works on a workspace shared with you (`<name>@<agent
id>`) with either kind of share.

## Arguments

- `name` (required) — the workspace.
- `path` (required) — the picture, relative to the workspace.

## Returns

`{name, path, bytes}`, then the picture itself, and a line naming its type
and size. A picture over 1568 pixels on its long side is scaled down first.

## Errors

- `not_granted` — viewing needs the `read` grant.
- `too_large` — the file is over 20 MiB; make a smaller one with `exec`.
- The call also fails when the file is not a PNG, JPEG, GIF or WebP, when
  decoding it would take too much memory, or when your model cannot see
  images; the message says which.

## Examples

```json
{"op": "view", "name": "analysis", "path": "charts/sales.png"}
```

```json result
{"bytes": 48213, "name": "analysis", "path": "charts/sales.png"}
```
