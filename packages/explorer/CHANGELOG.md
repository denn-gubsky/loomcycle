# @loomcycle/explorer changelog

Releases before 0.20.0 have no entry here: their notes are in the release
commits (`git log -- packages/explorer/package.json`) and in the runtime's
`REVISIONS.md`.

## 0.20.0

The RFC GZ redesign (#2854, #2859, #2868, #2972): one single-line row for the
Path tree and the document outline, a reading-first document page, collapsible
panes, side panels behind a rail, a hover annotation card, a breadcrumb, tree
search and filter, and folder counts with chunk briefs (RFC HA).

### Unchanged

- The props of `PathExplorer` and `DocumentViewer`: no prop was added, removed,
  renamed or retyped, and no default changed.
- The theme custom properties (`--bg`, `--bg-soft`, `--fg`, `--fg-soft`,
  `--fg-muted`, `--border`, `--accent`, `--danger`, `--running`, `--failed`,
  `--font-sans`, `--mono`, `--lc-font-mono`, `--lc-text-xs|sm|base`), the
  `.loomcycle-explorer` root class and `data-theme`.
- The `ExplorerDataLayer` methods. A custom data layer needs no change.

### Package exports

- Added: the type `PathLsFilters`.
- Nothing removed or renamed. `Pane`, `AnnotationCard`, `Breadcrumb` and the
  other components are internal: they are in the bundle and have `.d.ts` files
  under `dist/components/`, but the package's `exports` map does not reach
  them.

### Data types (additive, all optional)

- `PathEntry`: `documents`, `documents_capped`, `matched`.
- `ChunkRow`: `summary` (`{ level, brief, abstract, stale }`).
- `PathLsPage`: `withCounts`, `filters`. `PathLsResult`: `filters`,
  `type_expanded_to`.
- `dataLayerFromClient` now sends `with_summary: true` on `query_chunks`, and
  `with_counts` / the filters on a one-level `ls` when the explorer asks for
  them. A runtime before v1.169.0 ignores them: folders then show no counts,
  and the filter covers only the folders already opened.

### Dependencies

- New runtime dependency `@loomcycle/code-editor` ^0.1.1 (CodeMirror 6), used
  by the chunk editor. It is loaded on demand (a dynamic import), but the
  bundle imports `@loomcycle/code-editor/styles.css` at the top, so the host's
  bundler must handle a CSS import from `node_modules` (Vite and webpack with a
  CSS loader do). The editor themes through its own `--lc-ce-*` properties;
  the explorer sets `--lc-ce-font-size` to its UI size.

### CSS

Every selector stays scoped under `.loomcycle-explorer`. A host that styled the
explorer's inner classes must move to the new ones:

| Removed | Now |
| --- | --- |
| `.path-row`, `.path-name`, `.path-kind-icon`, `.tree-caret`, `.children`, `.path-link`, `.path-brief` (Path tree) and `.chunk-title`, `.chunk-type-icon`, `.chunk-badge`, `.chunk-status`, `.chunk-rev`, `.chunk-link`, `.chunk-link-badge` (outline) | one shared row: `.etree`, `.etree-node`, `.etree-row`, `.etree-caret`, `.etree-icon`, `.etree-name`, `.etree-marks`, `.etree-mark-type`, `.etree-mark-status`, `.etree-mark-link`, `.etree-count`, `.etree-children`; a brief shows in the hover card (`.ex-card*`) |
| `.selected` on a row | `.etree-node[aria-selected="true"] > .etree-row` |
| `.doc-toolbar`, `.doc-toolbar-actions`, `.doc-mode-toggle`, `.doc-content`, `.doc-content-head`, `.doc-content-meta`, `.doc-content-controls`, `.doc-content-body` | `.doc-header`, `.doc-header-path`, `.doc-header-actions`, `.doc-header-note`, `.doc-page`, `.doc-article`, `.doc-body`, `.doc-chunk`, `.doc-chunk-bar`, `.doc-chunk-title`, `.doc-chunk-icon`, `.doc-chunk-rev`, `.doc-chunk-source`, `.doc-chunk-edit`, `.doc-edit` |
| `.doc-download-menu`, `.doc-download-list` | the "more" menu: `.doc-more-menu`, `.doc-more-button`, `.doc-more-list`, `.doc-more-group`, `.doc-more-rule` |
| `.chunk-ancestry`, `.chunk-ancestry-up` | the breadcrumb: `.doc-crumbs`, `.doc-crumbs-list`, `.doc-crumbs-item`, `.doc-crumbs-folder`, `.doc-crumbs-current` |
| `.id-copy`, `.id-copy-btn`, `.id-copy-label`, `.id-copy-value`, `.id-copy-copied`, `.id-copy-error` | none: ids are no longer printed; "copy id" is a button on the chunk bar |
| `.stack-splitter`, `.stack-splitter-handle`, `.stack-splitter-pane` | side panels behind a rail: `.doc-rail`, `.doc-rail-button`, `.doc-rail-count`, `.doc-rail-dot`, `.doc-side`, `.doc-side-body`, `.doc-panel-heading` |
| `.chunk-edit`, `.chunk-body-input` | `.chunk-editor-main`, `.chunk-editor-body`, `.chunk-editor-fields`, `.chunk-editor-preview`, `.chunk-fields-error` |
| `.paths-context`, `.paths-doc-head` | `.paths-console`, `.paths-tree-box`, `.paths-detail-card` |

Other new classes: collapsible panes (`.pane-head`, `.pane-head-label`,
`.pane-head-count`, `.pane-collapse`, `.pane-rail`, `.pane-rail-button`,
`.pane-rail-count`), the annotation card (`.ex-card`, `.ex-card-title`,
`.ex-card-path`, `.ex-card-meta`, `.ex-card-brief`, `.ex-card-abstract`,
`.ex-card-stale`, `.ex-card-loading`), tree search and filter (`.paths-find*`,
`.search-hit`, `.search-current`, `.filter-match`, `.filter-kept`), the create
menu (`.paths-create-menu`, `.paths-create-list`), and the state classes
`.narrow`, `.panel-over`, `.current`, `.capped`.

New custom properties a host may set (none was removed or renamed):

- Type scale: `--lc-text-md` (16px), `--lc-text-lg` (20px), and the
  explorer's five sizes `--ex-text-meta`, `--ex-text-ui`, `--ex-text-body`
  (15px), `--ex-text-h3`, `--ex-text-h2`.
- Tree: `--etree-indent` (16px per level), `--etree-dot` (the neutral status
  dot). `--etree-depth` and `--etree-tint` are set inline per row; don't set
  them.

### Browser storage

Layout is remembered per browser in `localStorage`: new keys
`loomcycle.explorer.collapsed.paths`, `loomcycle.explorer.collapsed.outline`
and `loomcycle.explorer.panel`. The `loomcycle.explorer.doc-links-height` key
is no longer read.
