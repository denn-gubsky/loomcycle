---
name: Document/get_asset
description: "Document op=get_asset — an image chunk's asset metadata (media type, size, description state), never the bytes."
---
`get_asset` tells you about the image stored on a chunk: its media type, its
size, and whether a description of the image has been generated. It does not
return the image. Use it to check that an image chunk really has an image, or
to see why an image is not turning up in `search` (an image with no
description is findable only by its caption).

## Arguments

- `id` (required) — the chunk id.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{chunk_id, media_type, size, described, description?, described_at?, note?}`.
`described` is `true` once a description pass has run; `description` is its
text when it produced one. `note` explains, in words, when the image is
searchable only by its caption.

## Errors

- `get_asset: no asset on chunk: ...` — the chunk has no image (or is not in
  this scope). Attach one with `set_asset`.

## Examples

Check an image chunk:

```json
{"op": "get_asset", "id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f"}
```

```json result
{"chunk_id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "media_type": "image/png", "size": 48213, "described": false,
 "note": "no describe pass has run for this image yet; it is searchable only by its caption until one does"}
```
