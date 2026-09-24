---
name: Skill/list
description: "Skill op=list — the skills you may load, each with its description, optionally filtered by a /-glob pattern."
---
`list` shows the skills you are allowed to load, sorted by name, so you can
pick one by its description before calling `invoke`. It only shows skills your
agent's `skills:` allowlist admits.

## Arguments

- `pattern` — a `/`-glob filter. `*` matches one name segment (`doc/*`
  matches `doc/redactor`, not `doc/a/b`); `**` matches any number
  (`marketing/**`). A lone `*` matches everything. Omit it to list all.

## Returns

`{skills: [{name, description?}]}`. A skill created at runtime may have no
description in this list; `invoke` it to read it. An empty list means no skill
you may load matches.

## Errors

None in practice: a pattern that matches nothing returns an empty list.

## Examples

List every skill you may load:

```json
{"op": "list"}
```

```json result
{"skills": [
  {"name": "doc/redactor", "description": "Remove personal data from a document before it leaves the tenant."},
  {"name": "marketing/seo", "description": "Check a page against the SEO checklist."}]}
```

Only the skills in the `doc/` group:

```json
{"op": "list", "pattern": "doc/*"}
```
