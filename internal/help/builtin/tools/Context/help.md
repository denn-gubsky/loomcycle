---
name: Context/help
description: "Context op=help — list the help topics, read one topic or one tool operation (Tool/op), or search every topic's sections by text."
---
`help` is loomcycle's manual. With no argument it lists every topic. With
`topic` it returns that topic's full text. With `query` it searches all topics
and returns the best-matching sections. Read the article for an operation
**before your first call to it** — it shows the exact arguments with examples.

## Arguments

- `topic` — the topic to read. Three kinds:
  - a feature topic, e.g. `scopes`, `subagents`, `compaction`;
  - a tool's article, named after the tool, e.g. `Memory`, `Channel`;
  - one operation of a tool, as `<Tool>/<op>`, e.g. `Channel/subscribe`.
    `Channel.subscribe` and `Channel subscribe` work too, and case does not
    matter.
- `query` — search text. When set, `topic` is ignored.

With neither, you get the index.

## Returns

- **Index:** `{topics: [{name, description, source}], count, hint}`.
  Operation articles are not in the index; open the tool's article to find
  them.
- **Topic:** `{name, description, content, source}`. A tool's article also
  returns `operations: [{topic, description}]` — every `<Tool>/<op>` article
  you can read next.
- **Query:** `{query, mode, results: [{topic_slug, heading, snippet, score}],
  count, hint}`. Read a hit in full with `topic` set to its `topic_slug`.

## Errors

- `help: topic "X" not found (available: ...)` — the error lists every topic
  name; pick one from it.
- `help: topic "Tool/op" not found; Tool documents: ...` — that tool has no
  article for this operation. The error lists the ones it has.
- `help: not configured ...` — no help library on this deployment; retrying
  is pointless.

## Examples

List every topic:

```json
{"op": "help"}
```

Read the Channel tool's article, which lists its operations:

```json
{"op": "help", "topic": "Channel"}
```

Read one operation's article before calling it:

```json
{"op": "help", "topic": "Channel/subscribe"}
```

Search when you do not know which topic covers it:

```json
{"op": "help", "query": "wait for several workers to finish"}
```

```json result
{"query": "wait for several workers to finish", "mode": "hybrid", "count": 2, "results": [
  {"topic_slug": "fan-out-patterns", "heading": "When to use parallel_spawn", "snippet": "...", "score": 0.82},
  {"topic_slug": "Channel/await", "heading": "Arguments", "snippet": "...", "score": 0.71}]}
```
