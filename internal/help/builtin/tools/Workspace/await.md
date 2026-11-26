---
name: Workspace/await
description: "Workspace op=await — wait for the next update your application posted: what the person did in its page."
---
`await` returns the next update the application in your workspace posted,
or `timed_out` when none came in time. Use it after `expose`: a person
clicks in the page, the application posts what happened, and your next
`await` returns it, once.

The application posts to the address its servers find in the environment
variable `LOOMCYCLE_UPDATE_URL`: a `POST` whose body is the update (JSON is
a good shape), up to 64 KiB. It answers `202` when the update was queued,
`413` when the body is too large, and `429` when 64 updates already wait.
Only servers you `start` get the variable; a command you `exec` does not.

What comes back is untrusted output written by your application, like any
command's. You never see the address.

## Arguments

- `name` (required) — the workspace.
- `timeout_s` — seconds to wait; the default and the ceiling are the
  operator's command timeouts (at most one hour).

## Returns

`{name, update, pending}`: `update` is the posted body as text, or
`update_base64` when it is not text; `pending` is how many more wait;
`dropped` (when not zero) is how many posts were refused because 64 already
waited. With nothing posted in time: `{name, timed_out: true, pending: 0}`.

## Errors

- `not_granted` — `await` needs the `exec` grant, as servers do.
- `not_shared_op` — a workspace shared with you takes no `await`.
- `invalid_argument` — `timeout_s` out of range.

## Examples

```json
{"op": "await", "name": "site", "timeout_s": 60}
```

```json result
{"name": "site", "pending": 0, "update": "{\"clicked\":\"export\"}"}
```
