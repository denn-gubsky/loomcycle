---
name: Workspace/toolchains
description: "Workspace op=toolchains — the toolchains a workspace you open may ask for, each with its engines."
---
`toolchains` lists what you may pass to `open` as `toolchain` or
`toolchains`: every toolchain carried by an attached worker that could take
a workspace of yours, under your grant's engine requirement, with the
engines it is available on. A worker reports a toolchain only when its image
has it, so this is what the deployment can really build with.

It names toolchains only, never workers or how many there are. Needs your
grant's `workspace.open`.

## Arguments

None.

## Returns

`{toolchains: [{name, engines}], general, required?}`, sorted by name.
`general` is what an `open` naming no toolchain asks for; `required`, when
your grant names toolchains, is what every open adds. An empty list means no worker
serves you now.

## Examples

```json
{"op": "toolchains"}
```

```json result
{"toolchains": [{"name": "cmake", "engines": ["container"]}, {"name": "cpp", "engines": ["container"]}, {"name": "node", "engines": ["container", "host"]}, {"name": "python", "engines": ["container", "host"]}], "general": ["python", "node"]}
```

Then `open` with `"toolchains": ["cpp", "cmake"]` gets a workspace that can
build C++ (see Workspace/open).

## Errors

- `not_granted` — your grant does not allow `open`.
- `workspaces_off` — this runtime serves no workspaces.
