---
name: Channel/list_channels
description: "Channel op=list_channels — show this agent's publish and subscribe allowlists (the patterns, not the declared channels)."
---
`list_channels` reports which channel names your agent may publish to and
subscribe to. It returns the **allowlist patterns** from your agent
definition, which may include wildcards like `findings/*`. It does not list
the channels that exist; for that, with each channel's scope, TTL and whether
you may use it, call `Context {"op":"channels"}`.

## Arguments

None besides `op`.

## Returns

`{publish: [patterns], subscribe: [patterns]}`. A list may be empty (or
`null`), meaning you may not do that at all. When your session may use every
channel on one side, the result adds `publish_unrestricted: true` or
`subscribe_unrestricted: true` instead.

## Errors

None in practice: this op reads only your own policy.

## Examples

See what you may publish and subscribe to:

```json
{"op": "list_channels"}
```

```json result
{"publish": ["findings/*", "jobs/cv-results"], "subscribe": ["jobs/cv-requests"]}
```
