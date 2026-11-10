# Code-scanning triage (2026-11-06)

Every code-scanning alert open on `main` at 92875d45f (v1.141.0) was read
and given one of three outcomes: fixed in code, annotated where the use is
deliberate (gosec `#nosec <rule> -- <why>` at the line, which drops it from
the next scan), or listed below for dismissal with its reason. This is the
RFC FZ Part G item 3 triage; after it, the gosec gate in
`.github/workflows/security.yml` fails on medium severity, not only high.

Counts at the start: CodeQL 89 open (7 critical, 74 high, 8 medium); gosec
138 (all medium; 0 high). A local run of the pinned
gosec v2.29.0 with the workflow's flags (Go 1.26, `GOOS=linux`) over the root,
`deploy/builder`, `internal/errkind`, `internal/otel` and `internal/providers`
modules found 138 before and 0 after.

## Why most CodeQL alerts here are dismissals, not fixes

CodeQL follows taint into a sink but does not credit a guard that lives in a
helper (a fence function, a clamp in the caller, a charset gate) or a value
that is a request input by design (the command the Bash tool exists to run,
the SQL that SQL Memory exists to execute). Where restating the guard next to
the sink was cheap and clear, the branch did that (see "Fixed in code"). The
rest are below; the rationale column is written to be pasted into the
dismissal.

## Dismiss: CodeQL

| Alert | Rule | Location (at scan) | Dismiss as | Rationale |
|---|---|---|---|---|
| 276 | go/command-injection | internal/tools/builtin/bash.go:153 | won't fix | Running the model's shell command is the Bash tool's purpose. It is off unless the operator sets LOOMCYCLE_BASH_ENABLED, runs in the bound volume's directory with a scrubbed environment, bounded output and a timeout, and is documented as not a sandbox. |
| 277 | go/command-injection | internal/tools/builtin/bash.go:153 | won't fix | Same sink as 276 (a second source path). |
| 154 | go/command-injection | deploy/builder/engine.go:35 | false positive | argv[0] is the operator's SANDBOX_PODMAN_BIN at every call site and runs without a shell; the caller's command is an argument to `podman exec`, so it runs inside the session container, which is the builder's purpose. |
| 146 | go/request-forgery | internal/memory/backends/remote/remote.go:339 | false positive | The base URL comes from the MemoryBackend definition; the key path segments are url.PathEscape'd, and the client dials through netguard with pinning. |
| 147 | go/request-forgery | internal/tools/builtin/httptool.go:221 | false positive | The HTTP tool's purpose. The host must pass the operator's allowlist (narrowed by the caller, never widened by the model) before the request and on every redirect, and the dialer enforces the IP-level guard. |
| 232 | go/allocation-size-overflow | internal/api/http/meminject.go:281 | false positive | Capacity is the sum of two in-memory slice lengths; each is bounded by addressable memory, so the sum cannot overflow a 64-bit int. |
| 233 | go/allocation-size-overflow | internal/api/http/meminject.go:282 | false positive | As 232 (map size hint from the same two lengths). |
| 234 | go/allocation-size-overflow | internal/config/config.go:1240 | false positive | len(p.Lanes)+1 over an in-memory config map; cannot overflow. |
| 235 | go/allocation-size-overflow | internal/config/config.go:7859 | false positive | Sum of two in-memory map lengths (built-in provider ids and configured providers); cannot overflow. |
| 237 | go/allocation-size-overflow | internal/hooks/dispatcher.go:372 | false positive | Sum of two in-memory hook-chain lengths; cannot overflow. |
| 238 | go/allocation-size-overflow | internal/hooks/dispatcher.go:372 | false positive | As 237. |
| 239 | go/allocation-size-overflow | internal/sigv1/sigv1.go:103 | false positive | len(ts)+1+len(body) for a body already held in memory (and bounded by the request size cap); cannot overflow. |
| 240 | go/allocation-size-overflow | internal/statepatch/statepatch.go:27 | false positive | Map size hint from two in-memory map lengths; cannot overflow. |
| 241 | go/allocation-size-overflow | internal/statepatch/statepatch.go:27 | false positive | As 240. |
| 272 | go/allocation-size-overflow | internal/api/http/server.go:5629 | false positive | Sum of two in-memory segment-slice lengths; cannot overflow. |
| 186 | go/uncontrolled-allocation-size | internal/api/webhook/recent.go:58 | false positive | limit is clamped to the ring's length (at most recentBufferCap) two lines above the make. |
| 188 | go/uncontrolled-allocation-size | internal/store/postgres/postgres.go:1578 | false positive | ListSessions clamps limit to 500 at the top of the function. |
| 189 | go/uncontrolled-allocation-size | internal/store/postgres/postgres.go:1905 | false positive | ListEvents clamps limit to 500 at the top of the function. |
| 190 | go/uncontrolled-allocation-size | internal/store/postgres/postgres.go:3883 | false positive | MetricsSampleWindow clamps limit to 1000 at the top of the function. |
| 191 | go/uncontrolled-allocation-size | internal/store/sqlite/sqlite.go:3409 | false positive | ListSessions clamps limit to 500 at the top of the function. |
| 192 | go/uncontrolled-allocation-size | internal/store/sqlite/sqlite.go:3727 | false positive | ListEvents clamps limit to 500 at the top of the function. |
| 193 | go/uncontrolled-allocation-size | internal/store/sqlite/sqlite.go:12095 | false positive | MetricsSampleWindow clamps limit to 1000 at the top of the function. |
| 194 | go/uncontrolled-allocation-size | internal/tools/builtin/context.go:1071 | false positive | depth is clamped to 1..100 before the make. |
| 195 | go/uncontrolled-allocation-size | internal/tools/builtin/document.go:4682 | false positive | limit is clamped to 200 before the make. |
| 371 | go/uncontrolled-allocation-size | internal/tools/builtin/identity.go:558 | false positive | limit comes from pageLimit, which refuses anything outside 1..identityMaxLimit (200). |
| 372 | go/uncontrolled-allocation-size | internal/tools/builtin/identity.go:859 | false positive | As 371. |
| 257 | go/clear-text-logging | internal/help/index.go:431 | false positive | The logged value is the hybrid search's error. CodeQL taints it because the config struct that also holds the search-provider keys reaches the call; no key value is in the message. |
| 261 | go/clear-text-logging | internal/memory/backends/inprocess/inprocess.go:589 | false positive | Logs the memory key and the embedder's error; the embedders send their key in a header, never the URL, so the error does not quote it. |
| 262 | go/clear-text-logging | internal/sqlmem/sqlite.go:269 | false positive | Logs a scope database file path and its Close error. |
| 264 | go/clear-text-logging | internal/providerbuild/providerbuild.go:371 | false positive | Logs the NAME of the env var (api_key_env) that is set but empty, so the operator can fix it; the value is not logged (and is empty). |
| 283 | go/clear-text-logging | internal/wiring/embedder.go:162 | false positive | As 264, for the embedder's api_key_env. |
| 338 | go/clear-text-logging | internal/api/http/server.go:6835 | false positive | Logs run_id, iteration and the store's RecordCallUsage error; the usage row holds token counts and cost, not credentials. |
| 339 | go/clear-text-logging | internal/api/http/server.go:6835 | false positive | As 338. |
| 366 | go/clear-text-logging | internal/tools/builtin/document_review.go:1141 | false positive | Logs a ruling id, the tenant and a store write error. |
| 397 | go/clear-text-logging | internal/tools/builtin/document_annotation_search.go:94 | false positive | Logs an annotation's chunk id, level and the embedder's error, as #303 does for a chunk body; the embedders send their key in a header, never the URL, and build errors from the status and a response preview, so the error does not quote it. |
| 177 | go/incorrect-integer-conversion | cmd/loomcycle/gitsource.go:72 | won't fix | int(int64) of an operator env value; release builds are amd64/arm64 only, where int is 64 bits, so the conversion is lossless. |
| 178 | go/incorrect-integer-conversion | cmd/loomcycle/gitsource.go:87 | won't fix | As 177. |
| 179 | go/incorrect-integer-conversion | deploy/builder/config.go:100 | won't fix | As 177 (builder image is amd64/arm64). |
| 180 | go/incorrect-integer-conversion | deploy/builder/config.go:101 | won't fix | As 179. |
| 181 | go/incorrect-integer-conversion | deploy/builder/config.go:113 | won't fix | As 179. |
| 256 | go/incorrect-integer-conversion | cmd/loomcycle/boot_tools.go:347 | won't fix | As 177 (LOOMCYCLE max document asset bytes). |
| 207 | go/path-injection | internal/gitsource/tree.go:99 | false positive | dest is the volume directory the runtime provisioned under the fenced dynamic root, not a request path; tree entries written below it are checked for plain relative names, reserved names and links that leave the root. |
| 211 | go/path-injection | internal/gitsource/tree.go:294 | false positive | As 207; this is the check that a written link stays inside the resolved root. |
| 231 | go/path-injection | internal/tools/builtin/volumedef_gitsource.go:379 | false positive | path comes from dynvol.ProvisionEphemeral (charset-validated name, fenced under the resolved dynamic root, symlinks refused); the temp dir is a sibling inside the run's own directory. |
| 336 | go/path-injection | internal/transcribe/helper/helper.go:208 | false positive | workDir refuses a dir outside the root (filepath.Rel), resolves only the operator's root, and walks each component below it with Lstat, refusing links. |
| 337 | go/path-injection | internal/transcribe/helper/helper.go:216 | false positive | As 336. |
| 196 | go/sql-injection | internal/sqlmem/sqlite.go:304 | won't fix | SQL Memory executes the caller's SQL by design, against the scope's own database (one SQLite file per scope in defensive mode, or a per-scope Postgres schema and login role), behind the statement guard and quotas. |
| 197 | go/sql-injection | internal/sqlmem/sqlite.go:345 | won't fix | As 196. |
| 198 | go/sql-injection | internal/sqlmem/postgres.go:593 | won't fix | As 196 (runs under the scope's role, in a transaction). |
| 199 | go/sql-injection | internal/sqlmem/postgres.go:636 | won't fix | As 198. |
| 200 | go/sql-injection | internal/sqlmem/txn.go:332 | won't fix | As 196 (multi-statement transaction on the scope's connection). |
| 201 | go/sql-injection | internal/sqlmem/txn.go:363 | won't fix | As 200. |
| 390 | go/sql-injection | internal/sqlmem/sqlite.go:449 | won't fix | As 197: the same agent read, now on the scope connection taken with the value length limit set (opConn). |
| 391 | go/sql-injection | internal/sqlmem/sqlite.go:503 | won't fix | As 196: the same agent write, on the length-limited scope connection. |
| 151 | go/weak-sensitive-data-hashing | internal/credential/crypto.go:71 | false positive | SHA-256 of a 32-byte random KEK, truncated to a key id. The input is a high-entropy key, not a password; the hash is an identifier. |
| 152 | go/weak-sensitive-data-hashing | internal/tools/builtin/document_derive_units.go:196 | false positive | Content hash of a chunk's text for change detection; no password is involved. |
| 153 | go/weak-sensitive-data-hashing | internal/tools/repeat.go:170 | false positive | Hash of a tool result's text to detect a repeated call; no password is involved. |
| 316 | go/weak-sensitive-data-hashing | internal/authgate/password/hash.go:56 | false positive | SHA-256 of the pepper key (derived from LOOMCYCLE_SECRET_KEY) to name it; passwords themselves are hashed with Argon2id in the same file. |
| 364 | go/weak-sensitive-data-hashing | internal/tools/builtin/crypto_kms.go:117 | false positive | A cache key over a stored key's identity and external reference, not a password. |
| 359 | go/cookie-secure-not-set | internal/api/http/ui_session.go:521 | won't fix | Secure follows the request's transport: over HTTPS the cookie is the Secure __Host- one; a plain cookie is issued only to a plain-HTTP request, which strict mode refuses unless the operator sets LOOMCYCLE_UI_INSECURE_COOKIES (documented), and the server warns when it happens. |
| 312 | go/cookie-secure-not-set | internal/api/http/ui_session.go:524 | won't fix | Deleting the legacy bearer cookie; Secure as for 359. |
| 313 | go/cookie-secure-not-set | internal/api/http/ui_session.go:815 | won't fix | Sign-out deletes each cookie; a __Host- one is always deleted with Secure. |
| 325 | go/cookie-secure-not-set | internal/api/http/authgate_oidc.go:198 | won't fix | The login-state cookie: Secure follows the transport as for 359; HttpOnly, SameSite=Lax (the IdP's redirect back is a cross-site top-level GET), 10-minute TTL. |
| 326 | go/cookie-secure-not-set | internal/api/http/authgate_oidc.go:247 | won't fix | Clearing the cookie of 325. |
| 357 | go/cookie-secure-not-set | internal/api/http/authgate_passkey.go:366 | won't fix | The passkey ceremony cookie, through transportCookie: Secure as for 359, HttpOnly, SameSite=Strict. |
| 358 | go/cookie-secure-not-set | internal/api/http/authgate_passkey.go:399 | won't fix | Clearing the cookie of 357. |
| 324 | go/unvalidated-url-redirection | internal/authgate/oidc/oidctest/provider.go:178 | used in tests | A fake identity provider for tests; redirecting to the client's redirect_uri is what it emulates. |
| 135 | js/incomplete-multi-character-sanitization | packages/bridge-telegram/test/security.test.ts:99 | used in tests | A test helper that strips tags to compare what a reader sees; its output is never rendered. |
| 136 | js/incomplete-url-substring-sanitization | packages/bridge-slack/test/bridge.test.ts:503 | used in tests | A test assertion that no call went to the attacker host; not a URL check in shipped code. |

If an alert listed under "Fixed in code" below is still open after the next
scan of `main` (CodeQL may not credit the restated guard), dismiss it as a
false positive with the reason given there.

## Fixed in code (branch `feature-fz-p6c-scanner-triage`)

| Alert(s) | Rule | Change | Commit |
|---|---|---|---|
| gosec 10 | G705 (XSS) | The dev OAuth callback page echoed `error_description` unescaped on a localhost origin; now html.EscapeString. Real; regression test `TestCallbackServer_ErrorPageEscapesErrorDescription`. | fix(oauth-dev): escape the callback error on the local login page |
| 183, 184 | go/incorrect-integer-conversion | LOOMCYCLE_PG_MAX_OPEN_CONNS / MIN_IDLE_CONNS parsed at int32 width; 2^32+5 used to become a pool of 5. Real; `TestStorageEnv_PoolSizeOutOfInt32RangeIsIgnored`. | fix(config): parse the PG pool sizes at int32 width |
| 185 | go/incorrect-integer-conversion | DecodeRunCursor refuses a timestamp past int64 instead of wrapping it before 1970. `TestDecodeRunCursor_RefusesTimestampPastInt64`. | fix(store): refuse a run cursor whose timestamp is past int64 |
| 141, 142, 143 | js/polynomial-redos | Telegram heading and fence lines read by hand; the heading pattern was cubic on a whitespace run (5,000 tabs: 104 s). Real (agent text is untrusted). Two new tests in `test/units.test.ts`. | fix(bridges): read headings, fences and URLs without backtracking |
| 139, 140 | js/polynomial-redos | Discord and Slack API base URL trimmed with a loop (operator config; hygiene). | fix(bridges): read headings, fences and URLs without backtracking |
| 137 | js/polynomial-redos | `goTrimSpace` in `@loomcycle/client` scans from both ends (40,000 inner tabs: 6 s). New test in `tests/def-overlays.test.ts`. | fix(ts-adapter): trim a hook description without backtracking |
| 370 | js/polynomial-redos | Universe view slug trims its dashes with two slices. Unreachable before (the collapse runs first); restructure only. | refactor(universe): trim the view slug's dashes without a regex |
| 202, 203 | go/path-injection | Builder `resolveWorkspaceDir` checks the principal/name part for `..` next to MkdirAll (the charset gates already excluded it). Restructure. | refactor(security): restate fenced checks where scanners can see them |
| 204, 205, 206, 209, 210 | go/path-injection | dynvol `mkdirFenced` / `RemoveEmptyFenced` check the root-relative part for `..` next to the filesystem calls (locateBelowRoot already confined it). Restructure. | refactor(security): restate fenced checks where scanners can see them |
| 145 | go/request-forgery | Real. A run request's hook additions resolve in the run's tenant, "" in open mode, for the legacy token and for an admin, and every tenant-"" webhook dialed unguarded, so POST /v1/runs gave a blind POST to loopback, private and metadata addresses. Only hooks from the operator's static config (a yaml agent's or yaml channel's) now dial unguarded; every other webhook goes through netguard. Tests `TestDispatcher_RunAddedHookCannotReachAPrivateAddressInSharedTenant`, `TestRunHooks_ARunRequestHookCannotReachAPrivateAddress`. If the alert stays open (the URL is still request data for a run's hooks), dismiss: "the dial goes through netguard unless the hook is in the operator's static config". | fix(hooks): dial only the operator's static-config hooks unguarded |
| 148 | go/request-forgery | Real for the path. `url.PathEscape` leaves `.` and `..` unchanged, so a path parameter of exactly `..` moved the call to another route; both values are now refused (`TestEndpointTool_DotSegmentPathParamIsRefused`). If the alert stays open, dismiss: "base URL and path template come from the operator's OpenAPI spec; path parameters are escaped into one segment and dot segments are refused; query values are url.Values-encoded". | fix(localapi): refuse "." and ".." as a path parameter |
| 258, 259, 260 | go/clear-text-logging | The hook-failure log line named the full callback URL (whose path or query can be the secret, as in an incoming-webhook URL) and quoted 1 KiB of the response body. It now names scheme://host and at most 256 redacted bytes of the body (`TestWebhookCallError_LogDetailHidesURLPathAndRedactsBody`). If the alerts stay open, dismiss: "the logged error names the callback's scheme and host only and a redacted, bounded body snippet". | fix(hooks): log a failed callback by host, with a redacted snippet |
| 144 | go/weak-crypto-key | `cryptoops.Generate` refuses an RSA size below 2048 before GenerateKey (the size list already started at 2048). Restructure. | refactor(security): restate fenced checks where scanners can see them |

Also tightened while triaging gosec G301/G306 (no alert of its own beyond
the gosec ones): the data directory is created 0700; `snapshot export --out`,
imported configs and recipes, `mcp-registry` writes and memory eval reports
are written 0600 (`TestRunSnapshotsExport_FileIsOwnerOnly`). Commit:
fix(cli): write data, snapshots and configs owner-only.

## gosec: annotated, close on the next scan

These need no dismissal: each line now carries `#nosec <rule> -- <why>`, so
the next gosec upload from `main` no longer reports them and code scanning
closes them.

| Rule | Alerts (the open ones in these ranges) | Commit |
|---|---|---|
| G201, G202 (SQL built from fragments) | 20–23, 26–49, 273, 351, 355, 361, 362, 373 | chore(store): give each built SQL statement its #nosec reason |
| G204 (subprocess), G124 (cookies), G705 (XSS taint, except 10), G710 (redirect), G504/G505 (cgi, sha1) | 9, 11–17, 132–134, 274, 275, 314, 317, 327–330, 332, 350, 360 | chore(security): record why each deliberate exec, cookie and write is safe |
| G301, G302, G304, G306 (paths and modes) | 50–62, 64–131, 265, 266, 284–287, 289 | chore(security): give each deliberate file path and mode its #nosec; the modes that should be tighter were tightened in fix(cli): write data, snapshots and configs owner-only |
| G304 (Linux-only /proc reader) | 63 | ci(security): fail the gosec gate on medium severity |
