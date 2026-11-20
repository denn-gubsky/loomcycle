---
name: File/download
description: "File op=download — GET a URL straight into the run's inbox, one of your volumes or a workspace, so the file's bytes never pass through you."
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

- `url` — the absolute `http` or `https` URL to fetch, exactly as given; taken
  from JSON, the value only, without the quote marks around it.
- `to` — where to keep it. Omit it for the inbox (`documents/` or `images/` by
  type). `volume:inbox:<folder>/` picks an inbox folder,
  `volume:inbox:<folder>/<name>` a name there. `volume:<name>:<path>` saves
  into one of your read-write volumes and needs the Write tool: a path ending
  in `/`, or naming a folder that exists, keeps the server's file name.
  `workspace:<name>:<path>` saves into a workspace this run opened and needs
  your agent's `workspace` grant `write` (not Write); a folder keeps the
  server's file name the same way. The `Workspace` hooks judge the final path
  as `{op: "write", name, path, via: "File"}`, and the file counts against the
  workspace's disk quota: over it, nothing is kept.
- `overwrite` — replace a file already at `to` in a volume or a workspace.
  Default false. (In
  the inbox a clashing name gets a numeric suffix instead.)
- `headers` — extra request headers, such as `Accept`. No credentials: an
  `Authorization`, a cookie, an API key or a bearer value is refused.
- `max_bytes` — refuse a file larger than this. Default and ceiling: the
  per-file limit.

## Returns

In the inbox: `{url, status, ref, media_type, size, sha256, origin, file}` —
`file` is the one-line reference other tools return for a kept file, and
`origin` is `download:<host>`. In a volume: `{url, status, ref, size, sha256,
media_type}`, and the same in a workspace.

## Errors

- You do not hold the HTTP tool, or an HTTP hook refused the request.
- The host is not on the allowlist, or resolves to a private address the
  operator did not exempt.
- A redirect leads to another host, or from `https` to `http`.
- The server answered with an error status (a pre-authenticated URL may have
  expired).
- The file is larger than `max_bytes` or the per-file limit: nothing is kept.
- `to` is in a workspace and your grant lacks `write`, a `Workspace` hook
  refused, no workspace of that name is this run's, a file is there and
  `overwrite` is not set, or the workspace's quota would be passed
  (`workspace_over_quota`): nothing is kept.
- `to` is in a volume and you do not hold Write, the volume is read-only, its
  folder does not exist, or a file is there and `overwrite` is not set.
- A header looks like a credential.
- `url` holds a character a URL never holds unencoded (a quote, a space, `<`,
  `>`, a backslash or a backtick). Nothing was fetched: repeat the call with the
  URL corrected.

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

Fetch a data set into a workspace, for a script to work on:

```json
{"op": "download", "url": "https://data.example.org/2026/trades.csv", "to": "workspace:build:in/"}
```

```json result
{"url": "https://data.example.org/2026/trades.csv", "status": 200, "ref": "workspace:build:in/trades.csv", "size": 1048576,
 "sha256": "8d0f4a1c2b3e5f6a7b8c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c", "media_type": "text/csv"}
```
