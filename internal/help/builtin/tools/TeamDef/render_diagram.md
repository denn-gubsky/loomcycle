---
name: TeamDef/render_diagram
description: "TeamDef op=render_diagram — a Mermaid stateDiagram-v2 of a stored team, or a dry-run preview of an unsaved graph."
---
`render_diagram` draws a team's graph as a Mermaid `stateDiagram-v2`, with its
colours. Name a stored team (its active version, or one `def_id`), or pass an
`overlay` to preview a graph you have not saved: it is checked as `create`
would check it, and nothing is written.

## Arguments

- `name` — a stored team's active version; with `overlay`, only the title.
- `def_id` — one stored version instead.
- `overlay` — an unsaved graph to preview.
- `highlight_state` — a state to mark with a bold outline (for example a
  board's current state).
- `format` — `mermaid` (the default and only format).

## Returns

`{name, def_id, format: "mermaid", diagram}`; a preview has `def_id: ""` and
`preview: true`.

## Errors

- `render_diagram: provide name (active version) or def_id`.
- `render_diagram: team not found`.
- `render_diagram: ...` — the previewed graph was refused; the message says why.

## Examples

```json
{"op": "render_diagram", "name": "pr-review", "highlight_state": "review"}
```
