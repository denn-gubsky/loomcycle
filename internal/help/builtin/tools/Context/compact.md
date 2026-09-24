---
name: Context/compact
description: "Context op=compact — ask for your own conversation to be summarised at the next step, to free context-window space on a long run."
---
`compact` asks the runtime to shrink your own conversation: older turns are
replaced by a summary, and the most recent turns are kept as they are. It
takes effect **at the start of your next step**, not during this call, so you
will not see a difference in the reply to it.

Call it when `{"op":"self"}` shows `context.used_pct` high and you still have
work to do. How much is kept is set by the `compaction` settings `self`
reports. On a run whose `context_policy` is in recap mode, this triggers a
recap instead. For how compaction works, read
`{"op":"help","topic":"compaction"}`.

The one thing to get right: **if `self` shows `context_distill_declined`,
do not call `compact` again** — the last attempt did nothing, and the same
condition will make this one do nothing too. Report the reason instead.

## Arguments

None besides `op`.

## Returns

`{compaction: "scheduled", applies_at: "the next step"}`.

## Errors

- `context compaction is not available for this run` — this run cannot
  compact itself (for example a stateful run). Retrying is pointless.

## Examples

Free space before a long final synthesis:

```json
{"op": "compact"}
```

```json result
{"compaction": "scheduled", "applies_at": "the next step"}
```
