---
name: File/download
description: "File op=download — GET a URL straight into the run's inbox or one of your volumes, so the file's bytes never pass through you."
---
`download` fetches one URL and keeps what it returns as a file. The runtime
moves the bytes; you get the file's reference. Use it for a link a tool handed
you — a pre-authenticated download URL from an MCP server, a document a page
links to — instead of reading the file through HTTP or WebFetch.

The request is an HTTP request: you need the HTTP tool too, and its hooks
judge the call. It goes out on the HTTP tool's rules: the host must be on the
operator's allowlist, private and loopback addresses are refused unless the
operator opted the host in, and a redirect is followed only to the same host.
A URL with a query string may be a credential, so results show its query as
`?…`.

## Arguments

- `url` — the absolute `http` or `https` URL to fetch.
- `to` — where to keep it. Omit it for the inbox (`documents/` or `images/` by
  type). `volume:inbox:<folder>/` picks an inbox folder,
  `volume:inbox:<folder>/<name>` a name there. `volume:<name>:<path>` saves
  into one of your read-write volumes and needs the Write tool: a path ending
  in `/`, or naming a folder that exists, keeps the server's file name.
- `overwrite` — replace a file already at `to` in a volume. Default false. (In
  the inbox a clashing name gets a numeric suffix instead.)
- `headers` — extra request headers, such as `Accept`. No credentials: an
  `Authorization`, a cookie, an API key or a bearer value is refused.
- `max_bytes` — refuse a file larger than this. Default and ceiling: the
  per-file limit.

## Returns

In the inbox: `{url, status, ref, media_type, size, sha256, origin, file}` —
`file` is the one-line reference other tools return for a kept file, and
`origin` is `download:<host>`. In a volume: `{url, status, ref, size, sha256,
media_type}`.

## Errors

- You do not hold the HTTP tool, or an HTTP hook refused the request.
- The host is not on the allowlist, or resolves to a private address the
  operator did not exempt.
- A redirect leads to another host, or from `https` to `http`.
- The server answered with an error status (a pre-authenticated URL may have
  expired).
- The file is larger than `max_bytes` or the per-file limit: nothing is kept.
- `to` is in a volume and you do not hold Write, the volume is read-only, its
  folder does not exist, or a file is there and `overwrite` is not set.
- A header looks like a credential.

## Examples

Download an attachment by its pre-authenticated URL into the inbox:

```json
{"op": "download", "url": "https://contoso.sharepoint.com/_layouts/15/download.aspx?UniqueId=7f3a&tempauth=…"}
```

```json result
{"url": "https://contoso.sharepoint.com/_layouts/15/download.aspx?…", "status": 200,
 "ref": "volume:inbox:documents/board-papers.pdf", "media_type": "application/pdf", "size": 1887436,
 "sha256": "3f9a1c2b4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8", "origin": "download:contoso.sharepoint.com",
 "file": "[file: volume:inbox:documents/board-papers.pdf · application/pdf · 1.8 MiB · sha256:3f9a1c2b4d5e… · from download:contoso.sharepoint.com]"}
```

Save a report straight into the `docs` volume:

```json
{"op": "download", "url": "https://example.com/reports/q3.pdf", "to": "volume:docs:reports/"}
```
