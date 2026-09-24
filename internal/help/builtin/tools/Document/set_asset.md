---
name: Document/set_asset
description: "Document op=set_asset — attach an image (base64 bytes) to an existing chunk, turning it into an image chunk."
---
`set_asset` stores an image on a chunk. The chunk must already exist — create
it first with `create_chunk`, giving it a title and, as its body, a caption
that says what the image shows (the caption is what makes the image findable by
`search`). The call sets the chunk's type to `image` and records the image's
media type and size in its fields. Calling it again replaces the image.

## Arguments

- `id` (required) — the CHUNK id, not the document id.
- `media_type` (required) — `image/png`, `image/jpeg`, `image/gif` or
  `image/webp`. SVG is not accepted.
- `data` (required) — the image bytes as standard base64. **No
  `data:image/png;base64,` prefix** — the bare base64 text only.
- `filename` — the original file name, kept as metadata.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

The image may be at most 8 MiB decoded unless the operator set another cap.

## Returns

The updated chunk, as `get_chunk` returns it: `{id, document_id, title, type:
"image", body, fields: {kind, media_type, size, filename?}, asset: {media_type,
size}, ...}`. The image bytes themselves are never returned by a Document op.

## Errors

- `set_asset: unsupported media_type ...` — convert the image to one of the
  four allowed types.
- `set_asset: data must be valid standard base64 (no data: prefix)` — strip
  the prefix and any non-base64 characters.
- `set_asset: image is ... bytes, exceeds the ...-byte cap` — shrink the image;
  retrying the same bytes is pointless.
- `set_asset: no such chunk: ...` — create the chunk first.

## Examples

Attach a PNG to a chunk made for it:

```json
{"op": "set_asset", "id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "media_type": "image/png", "data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==", "filename": "architecture.png"}
```

```json result
{"id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "document_id": "b4407b522dc11495d3de371311db17f0", "title": "Architecture diagram",
 "type": "image", "body": "The request path from the gateway to the workers.", "revision": 2, "position": 1,
 "fields": {"kind": "image", "media_type": "image/png", "size": 70, "filename": "architecture.png"},
 "asset": {"media_type": "image/png", "size": 70}}
```
