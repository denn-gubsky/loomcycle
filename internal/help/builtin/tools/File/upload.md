---
name: File/upload
description: "File op=upload — send a file from the inbox or one of your volumes to a URL, in one request or in ranges for an upload session."
---
`upload` sends a file's bytes to a URL without them passing through you: the
runtime reads the file and makes the request. It is how a file reaches a
service that hands out an upload URL — a Microsoft Graph upload session, a
pre-signed storage URL.

The request goes out on the HTTP tool's rules: the host must be on the
operator's allowlist, and private and loopback addresses are refused unless
the operator opted the host in. A redirect is not followed. Results show the
URL's query, and any URL's query in the server's reply, as `?…`.

## Arguments

- `from` — the file: `volume:inbox:<path>` (or a bare path in the inbox), or
  `volume:<name>:<path>` in one of your volumes, which needs the Read tool.
- `url` — the absolute `http` or `https` URL to send it to.
- `method` — `PUT` (default) or `POST`.
- `content_type` — the Content-Type to send. Default: the file's type.
- `headers` — extra request headers. No credentials: an upload URL carries its
  own authorisation, and an `Authorization`, a cookie or an API key is refused.
- `ranged` — send the file in chunks, each with
  `Content-Range: bytes <first>-<last>/<total>`, as an upload session takes
  it. After each chunk the server's `nextExpectedRanges` says where the next
  one starts; a 200 or 201 ends the upload. Default false: one request.
- `chunk_bytes` — the chunk size with `ranged`, rounded down to a multiple of
  327680 (320 KiB, as Microsoft Graph requires). Default 3932160 (3.75 MiB).

## Returns

`{url, from, status, bytes_sent, size, response}` — `response` is the start of
the server's reply. `bytes_sent` can exceed `size` when the server asked for a
range again.

## Errors

- The host is not on the allowlist, or resolves to a private address the
  operator did not exempt.
- `from` is in a volume and you do not hold Read, or there is no file there.
- The server answered with an error status (the result says which, with its
  reply): an upload URL may have expired.
- A ranged upload did not finish within a bounded number of requests.
- A header looks like a credential.

## Examples

Upload the extracted minutes to a Graph upload session, in ranges:

```json
{"op": "upload", "from": "volume:inbox:documents/minutes.docx", "url": "https://contoso.sharepoint.com/_api/v2.0/drives/b!x/items/01AB/uploadSession?guid=…&tempauth=…", "ranged": true}
```

```json result
{"url": "https://contoso.sharepoint.com/_api/v2.0/drives/b!x/items/01AB/uploadSession?…", "from": "volume:inbox:documents/minutes.docx",
 "status": 201, "bytes_sent": 48213, "size": 48213, "response": "{\"id\":\"01AB…\",\"name\":\"minutes.docx\",\"size\":48213}"}
```

Send a small file in one PUT:

```json
{"op": "upload", "from": "volume:docs:notes.txt", "url": "https://storage.example.com/bucket/notes.txt?X-Amz-Signature=…", "content_type": "text/plain"}
```
