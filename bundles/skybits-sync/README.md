# skybits-sync bundle

Keeps tenant Documents under a Path root (`/skybits` by default) in step with
one Skybits account. Every doc, sheet and file becomes a Document with
provenance (`source_kind: skybits`, the item's link, its revision and update
time), named in the Path tree after its Skybits tags. Edits are re-imported
into the same document, items trashed or deleted in Skybits are marked on
their documents (never deleted), and copies are proposed as `duplicate_of`
statements for review. The sync only reads the account: no Skybits write tool
is on any agent.

| agent | provider | tools | role |
|---|---|---|---|
| `skybits/sync` | `code-js` | `find_documents`, `list_tags`, `list_jobs` (Skybits); `Document`, `Path`, `Memory`, `Agent`; `statement_methods: [rule]` | lists and diffs the account, dispatches the imports, owns names, tags, removals, duplicates and the report |
| `skybits/import` | `code-js` | `outline_document`, `document_history`, `start_export`, `get_job`, `list_jobs`, `get_file_url` (Skybits); `File`, `HTTP`, `Document`, `Path`, `Memory`, `Agent`; OCR and speech-to-text | imports or re-imports one item |
| `skybits/structure` | your `middle` tier | none | returns a document's heading outline as JSON; the import validates it |

Why code and not a prompt: every invariant the sync keeps — a document id never
changes, a name is never taken from another document, a removal is a mark and
not a delete, a short listing infers nothing — is a line of code a test runs.
The one model call answers one bounded question, and its reply is data.

## What it needs

- `LOOMCYCLE_CODE_AGENTS_ENABLED=1` and SQL Memory (`LOOMCYCLE_SQLMEM_ENABLED=1`).
- An MCP server named **`skybits`** in the tenant, connected to the account
  (an `oauth` credential and `Authorization: Bearer $oauth:<name>`). The bundle
  does not declare it: the URL and the credential are the deployment's.
- **Tenant grants, in an overlay.** The documents and the manifest live in the
  tenant scope, and a tenant Document grant is a tenant write grant, so the
  bundle gives none. Fork both code agents in the tenant (or add a config
  layer) with:

  ```yaml
  agents:
    skybits/sync:
      memory_scopes: [tenant]
      sql_scopes: [tenant]
    skybits/import:
      memory_scopes: [tenant]
      sql_scopes: [tenant]
  ```

- A `middle` tier for `skybits/structure` (select `base` alongside the
  bundle). Its prompt carries document text: route it to a local model in
  your tier policy if the text must stay on your hosts.
- For images: an OCR model (`transcription.ocr`). For recordings:
  speech-to-text (`transcription.stt`), and a media converter for recordings
  the engine cannot take whole. Without them those items become stubs and a
  full run retries them.
- HTTP access to the account's download hosts (File download uses the HTTP
  tool's allowlist).
- `LOOMCYCLE_FILE_MAX_BYTES` large enough for the largest file you want
  imported; a larger file becomes a `too_large` stub.
- For duplicate proposals in a tenant whose ontology is confirmed with a
  Relations section: nothing more (`duplicate_of` is a standard kind).

## Running it

Input is JSON in the run prompt (or the same keys on the run metadata):

| key | default | meaning |
|---|---|---|
| `mode` | `plan` | `plan` lists, diffs and reports, and writes nothing; `apply` imports |
| `root` | `/skybits` | the Path root of the tree |
| `full` | `false` | also list the trash and mark removed and trashed items. Automatic when the last full run is missing or 7 or more days old |
| `max_items` | `50` | imports per apply run (at most 200) |
| `parallel` | `3` | imports at once (at most 3, Skybits' export limit) |
| `kinds`, `item_ids`, `tags` | all | narrow what this run imports (`tags`: raw tag names, a tag also selects its sub-tags) |
| `include_templates` | `false` | import template documents too |
| `defer` | `[]` | file extensions to leave for later, e.g. `["pptx"]` (a `skipped:deferred_<ext>` row) |
| `structure` | `always` | the heading pass: `always` on every doc and on files with a thin outline, `thin` only where the outline is thin, `never` |
| `stt_minutes` | `120` | speech-to-text minutes one run may spend |
| `root_fields` | none | extra fields stamped on every imported document's root (e.g. a colour scheme) |

Start with a plan, then apply until nothing is left:

```json
{"mode": "plan", "full": true}
{"mode": "apply", "full": true, "max_items": 50}
```

An apply takes the tenant lease `skybits-sync`; a second run while one holds it
reports `status: busy` and does nothing.

### Schedules

The bundle installs none. A deployment adds two tenant ScheduleDefs, both with
concurrency `forbid`, firing `skybits/sync`:

- every 4 hours, `{"mode": "apply"}`;
- weekly (e.g. Sunday 03:30), `{"mode": "apply", "full": true}`.

## What an apply does

1. Lists the whole account (`find_documents`, paged). If the rows read differ
   from `total_count`, it stops: nothing is inferred from a short listing. A
   full run also lists the trash.
2. Diffs each item against its manifest row: new → add; a changed
   `updated_at` or file version → update; a failure → retry (after 5
   failures, only on full runs); a stub whose reason is a missing capability
   (`ocr_unavailable`, `stt_needs_conversion`, `stt_unavailable`,
   `stt_over_cap`, `too_large`) → retried on full runs; moved tags → new names,
   no re-import.
3. Marks, on full runs: an item in the trash `trashed`, one in neither listing
   `removed` (Document `mark_source`; the document gets the tag
   `source/removed` and keeps everything else). An item back from the trash is
   marked `present`. If more than `max(3, 30%)` of the manifest would be marked
   removed, it marks none, writes the report and fails.
4. Imports up to `max_items` items, three at a time (recordings one at a
   time), each in a `skybits/import` child. Three Skybits failures in a row
   stop the dispatch; a passing fault is retried once at the end of the run.
5. Proposes duplicates (`find_duplicates`, method `rule:skybits-duplicates`)
   among the imported documents, after an apply that imported anything and on
   every full run. Nothing is merged or hidden.
6. Writes the report: a section per run in the `SyncReport` document
   `<root>/_sync/report-YYYY-MM`, and the same as JSON in the run's answer.

### Per kind

| item | import |
|---|---|
| doc | DOCX export → extract with headings recovered from formatting → the heading pass → import |
| sheet | XLSX export → extract. A tab with more than 20,000 populated cells (read from the outline) imports its first 200 rows and is listed in the report |
| pdf, pptx, docx, odt, csv, txt, md, html | download → extract (`ocr: never`; DOCX/HTML with heading recovery) → the heading pass when the outline is thin → import |
| png, jpg, gif, webp | download → extract with OCR → import; under 20 characters of text → stub `no_text`; no OCR → stub `ocr_unavailable` |
| mp4, m4a, mp3, wav, webm, ogg, flac | download → speech-to-text (a dry run first, against the minutes left) → import the timed transcript |
| canvas, unsupported files | stub `canvas_not_supported` / `unsupported_type:<ext>` |
| `Icon\r` artefacts, templates | a manifest row only (`skipped:icon_artefact`, `skipped:template`) |

Every import goes onto the work key `skybits:<item id>` with `reimport: true`:
the first creates the document, later ones reconcile it part by part (same
document id, unchanged parts keep their ids and links, provenance refreshed).
When the downloaded bytes hash as before, nothing is imported and only the row
moves.

A **stub** is a Document typed `SourceStub` with the item's title, provenance
and the reason (field `skybits_stub_reason`, tag `skybits/stub/<reason>`), no
body, at the item's names. Classification and the links agent skip it. When
the item later imports, the same document takes the text and loses the stub
type and tag. A stub never replaces a document that already holds text: the
row records the reason and the report lists it under `kept_previous_document`.

### The heading pass

Docs from Skybits mark headings by formatting. Extract recovers what it can
deterministically; then `skybits/structure` gets the numbered candidate lines,
the current headings and a sample, and returns the outline. The import accepts
it only when every line exists, each `text` is exactly that line, lines are in
order, levels are 2–6 and skip at most one level, and the count is sane. It
adds headings and re-levels existing ones (File import `headings`, `as:
"heading"` / `as: "level"`); it never removes a heading, so no line of text
is lost. A reply that fails, or an import that refuses the list, falls back to
the extract's own outline, and the report says why. A text longer than one
extract excerpt (30,000 characters) keeps the extract's outline.

## Names and tags

- A tag path `A/B` is the folder `<root>/<seg(A)>/<seg(B)>`; a shared tag
  `owner//path` is `<root>/_shared/<seg(owner)>/…`; no tag is
  `<root>/_untagged`. `seg` trims, transliterates Cyrillic, strips accents,
  turns `&` into `and`, drops brackets and quotes, replaces everything outside
  `[A-Za-z0-9._-]` with `-`, and cuts at 64 characters.
- The document's name is `seg(title)` in the folder of its first tag, and an
  extra name in each other tag's folder. A collision takes `-doc`, `-sheet`,
  `-pdf`, … then `-2`, `-3`, in creation order.
- Names are stored in the manifest and the tag → folder map in
  `skybits_state.tag_map_json`; both win over the rules, so a rule change never
  moves a document. Every name a manifest row holds is reserved, also for items
  gone from Skybits, and a name another document already holds is never taken.
- Document tags: `source/skybits`, `skybits/kind/<kind>`, and
  `skybits/tag/<raw tag path>` per tag; the raw tag names are in the root field
  `skybits_tags`, beside `skybits_id` and `skybits_kind`.
- A document under the root that the manifest does not know is not the sync's:
  a full run lists it in the report (`not_ours`) and never touches it.

## The manifest

Tenant SQL Memory, two tables:

- `skybits_manifest`: one row per item — `item_id`, `kind`, `title`, `url`,
  `tags_json`, `primary_path`, `paths_json`, `updated_at`, `revision`
  (`doc:r<n>`, `sheet:r<n>`, `file:v<n>`), `sha256`, `document_id`, `status`,
  `reason`, `last_synced_at`, `last_run_id`, and since schema 2 `fail_count`,
  `source_status` (`present` / `removed` / `trashed`) and `file_json` (the
  listing's view of the item, handed to the import).
- `skybits_state`: `tag_map_json`, `schema_version`, `last_full_at`,
  `high_water`.

Status is one of `imported`, `reimported`, `unchanged`, `stub:<reason>`,
`skipped:<reason>`, `failed:transient`, `failed:error`, `failed:persistent`.

**Adopting an earlier import.** The tables are created only if missing, with
the first schema's columns, so a manifest an earlier importer wrote (same
tables, work key `skybits:<id>`, the same names) is adopted as it stands: the
first apply adds the schema-2 columns, reclassifies `failed` rows by reason,
and imports nothing that is unchanged. Rows the earlier importer left as
`skipped:deferred_<ext>` or capability stubs without a document are imported
(or get their `SourceStub`) on that first apply.

## Costs

The two code agents spend no tokens. Each doc, and each file with a thin
outline, costs one `skybits/structure` call (about 4–10K tokens on your
`middle` tier; `structure: never` turns it off). OCR costs a model call per
image, speech-to-text the recording's minutes.
