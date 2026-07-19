import { useEffect, useMemo, useState } from "react";
import type { DefRow, LibraryEntry, SubstrateKind } from "../types";
import { useLibraryData } from "../lib/dataLayer";
import Splitter from "./Splitter";
import LineageTree, { buildLineageTree } from "./LineageTree";

// LineagePanel is the shared shape that backs each Library sub-tab
// (Agents / Skills / MCP Servers). Left pane lists entries with
// STATIC / DYNAMIC source chips; right pane shows the lineage tree
// for the selected entry + the selected version's definition JSON.
//
// v0.9.x Library v2: each entry can be source=static-only,
// dynamic-only, or both. Static-only entries appear as a synthetic
// v0 row in the tree (def_id="static:<name>") so the same LineageTree
// component renders them uniformly with substrate-backed rows.
//
// Polling cadence: entry list refresh is owned by the parent
// (Library calls listAgents / listSkills / etc.); version lineage
// refreshes on selection change via the injected data layer.

export interface LineagePanelProps {
  // Substrate kind used in the listDefVersionsByName data call. Covers
  // the original three plus the v0.24.0 Integrations families.
  kind: SubstrateKind;
  // Human-readable label for the empty-state copy.
  kindLabel: string;
  // Unified entries — fetched by the parent so the polling
  // strategy stays uniform with other admin pages.
  entries: LibraryEntry[];
  // Storage key for the Splitter's persisted width (per sub-tab so
  // the operator can size them independently).
  splitterStorageKey: string;
  // Kind-specific renderer for the version's Definition JSON shape.
  // Used by Agents (system_prompt + tools) / Skills (body) /
  // MCP Servers (url + headers + discovered_tools).
  renderDefinition: (row: DefRow) => React.ReactNode;
  // v0.10.4 — admin mutation callbacks. When wired, the right pane
  // gains a "+ New" CTA and rows gain Edit / Retire / Promote
  // buttons. Optional — backward compat for any future callsite
  // that wants the panel read-only.
  onCreateNew?: () => void;
  onEditRow?: (row: DefRow) => void;
  onCloneRow?: (row: DefRow) => void;
  onRetireRow?: (row: DefRow) => void;
  onPromoteRow?: (row: DefRow) => void;
  // v0.10.4 — MCP-only "Rediscover tools" button in the header.
  // Optional + only wired by Library for the mcp-servers tab.
  // The callback receives the currently-selected entry's name so the
  // caller doesn't have to re-derive it.
  onRediscover?: (name: string) => void;
  // RFC AU — "Import" CTA beside "+ New" (skills + mcp-servers tabs).
  // Opens the Claude Code import flow. Optional.
  onImport?: () => void;
  // Optional host error sink — called when the version-lineage load
  // fails (in addition to the inline error banner).
  onError?: (e: unknown) => void;
}

export default function LineagePanel({
  kind,
  kindLabel,
  entries,
  splitterStorageKey,
  renderDefinition,
  onCreateNew,
  onEditRow,
  onCloneRow,
  onRetireRow,
  onPromoteRow,
  onRediscover,
  onImport,
  onError,
}: LineagePanelProps) {
  const data = useLibraryData();
  const [selectedName, setSelectedName] = useState<string>(() =>
    entries.length > 0 ? entries[0]!.name : "",
  );

  // List controls (per-tab: each sub-tab renders its own LineagePanel, so the
  // filter/type/sort/hide-retired state is naturally independent per tab).
  const [filterText, setFilterText] = useState("");
  const [typeFilter, setTypeFilter] = useState<TypeFilter>("all");
  const [hideRetired, setHideRetired] = useState(false);
  const [sortMode, setSortMode] = useState<SortMode>("none");

  // The list actually rendered = entries ∘ name-filter ∘ type-filter ∘
  // hide-retired ∘ sort. Memoised so selecting a row (which changes
  // selectedName) doesn't re-run the whole chain. "none" preserves the
  // server's order (name ASC); the detail pane keeps showing a selected entry
  // even when a filter hides it from the list (selectedEntry reads full entries).
  const visibleEntries = useMemo(() => {
    let out = entries.filter((e) => {
      if (!nameMatches(e.name, filterText)) return false;
      if (typeFilter === "dynamic" && !e.in_substrate) return false;
      if (typeFilter === "static" && !e.in_static) return false;
      if (hideRetired && entryRetired(e)) return false;
      return true;
    });
    if (sortMode === "asc") {
      out = [...out].sort((a, b) => a.name.localeCompare(b.name));
    } else if (sortMode === "desc") {
      out = [...out].sort((a, b) => b.name.localeCompare(a.name));
    } else if (sortMode === "type") {
      // Group by source rank (static-only, both, dynamic-only), then name ASC.
      out = [...out].sort(
        (a, b) => sourceRank(a) - sourceRank(b) || a.name.localeCompare(b.name),
      );
    }
    return out;
  }, [entries, filterText, typeFilter, hideRetired, sortMode]);
  const [versions, setVersions] = useState<DefRow[]>([]);
  const [versionsErr, setVersionsErr] = useState<string | null>(null);
  const [versionsLoading, setVersionsLoading] = useState(false);
  const [selectedDefID, setSelectedDefID] = useState<string>("");

  const selectedEntry = useMemo(
    () => entries.find((e) => e.name === selectedName),
    [entries, selectedName],
  );

  // When the entries list updates and the currently-selected name is
  // gone (renamed / retired tree), fall back to the first available.
  useEffect(() => {
    if (selectedName && entries.find((e) => e.name === selectedName)) return;
    setSelectedName(entries.length > 0 ? entries[0]!.name : "");
  }, [entries, selectedName]);

  // Fetch the lineage for the selected entry.
  // - Static-only: synthesize a single v0 pseudo-row from
  //   entry.static_definition. Skip the network call entirely.
  // - dynamic-only / both: fetch the substrate lineage chain.
  useEffect(() => {
    if (!selectedName || !selectedEntry) {
      setVersions([]);
      setSelectedDefID("");
      return;
    }
    // The static base is shown as a synthetic "v0" row (def_id
    // "static:<name>") built from the entry's static_definition. We surface it
    // whenever the entry HAS a static base — not only when there are zero
    // dynamic versions — so it stays visible + fork-from-able, and (when no
    // dynamic version is active) it's the definition runs actually resolve to.
    // Without this, once any dynamic version exists (even all-retired / no
    // active pointer) the static base was buried and couldn't be edited/forked.
    const staticRow: DefRow | null = selectedEntry.in_static
      ? {
          def_id: `static:${selectedEntry.name}`,
          name: selectedEntry.name,
          version: 0,
          created_at: "",
          definition: selectedEntry.static_definition,
        }
      : null;

    if (!selectedEntry.in_substrate) {
      // Static-only — no network; the static row is the whole lineage.
      setVersions(staticRow ? [staticRow] : []);
      setSelectedDefID(staticRow?.def_id ?? "");
      setVersionsLoading(false);
      setVersionsErr(null);
      return;
    }
    let cancelled = false;
    setVersionsLoading(true);
    setVersionsErr(null);
    data
      .listDefVersionsByName(kind, selectedName)
      .then((r) => {
        if (cancelled) return;
        const dynVersions = r.versions ?? [];
        // Surface the static base alongside the dynamic lineage.
        setVersions(staticRow ? [staticRow, ...dynVersions] : dynVersions);
        setVersionsLoading(false);
        // Pre-select the EFFECTIVE definition — what a run resolves to: the
        // promoted dynamic version if one is active, else the static base
        // (runtime falls through to it when no dynamic is active), else the
        // newest dynamic row.
        if (selectedEntry.active_def_id) {
          setSelectedDefID(selectedEntry.active_def_id);
        } else if (staticRow) {
          setSelectedDefID(staticRow.def_id);
        } else if (dynVersions.length > 0) {
          const top = [...dynVersions].sort((a, b) => b.version - a.version)[0]!;
          setSelectedDefID(top.def_id);
        } else {
          setSelectedDefID("");
        }
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        setVersionsErr(e instanceof Error ? e.message : String(e));
        setVersionsLoading(false);
        onError?.(e);
      });
    return () => {
      cancelled = true;
    };
  }, [data, kind, selectedName, selectedEntry, onError]);

  const tree = useMemo(() => buildLineageTree(versions), [versions]);
  const activeDefID = selectedEntry?.active_def_id ?? "";
  // When NO dynamic version is active, the static base is the definition a run
  // resolves to — flag it "effective" in the tree. (When a dynamic version is
  // active, activeDefID already badges it "active ★".)
  const effectiveDefID =
    selectedEntry && !activeDefID && selectedEntry.in_static
      ? `static:${selectedEntry.name}`
      : "";

  if (entries.length === 0) {
    return (
      <div className="empty-state">
        <p>
          No {kindLabel} declared yet. Use the substrate admin API
          (POST /v1/_{kind}) or add one to loomcycle.yaml.
        </p>
        {onCreateNew && (
          <button
            type="button"
            className="primary"
            onClick={onCreateNew}
            style={{ marginTop: 12 }}
          >
            + New {newCtaLabel(kind)}
          </button>
        )}
        {onImport && (
          <button
            type="button"
            className="lineage-row-action"
            onClick={onImport}
            style={{ marginTop: 12, marginLeft: 8 }}
          >
            Import…
          </button>
        )}
      </div>
    );
  }

  return (
    <Splitter
      storageKey={splitterStorageKey}
      defaultLeftWidth={320}
      minLeftWidth={220}
      minRightWidth={320}
    >
      <div className="lineage-left">
        <div className="lineage-toolbar">
          <input
            type="text"
            className="lineage-filter-text"
            placeholder="filter (doc/ = doc/*)…"
            value={filterText}
            onChange={(e) => setFilterText(e.target.value)}
            aria-label="Filter by name"
          />
          <div className="lineage-toolbar-row">
            <select
              className="lineage-toolbar-select"
              value={typeFilter}
              onChange={(e) => setTypeFilter(e.target.value as TypeFilter)}
              aria-label="Filter by type"
              title="Show all, only dynamic (substrate), or only static entries"
            >
              <option value="all">All</option>
              <option value="dynamic">Dynamic</option>
              <option value="static">Static</option>
            </select>
            <select
              className="lineage-toolbar-select"
              value={sortMode}
              onChange={(e) => setSortMode(e.target.value as SortMode)}
              aria-label="Sort"
              title="Sort order"
            >
              <option value="none">Sort: None</option>
              <option value="type">Sort: Type</option>
              <option value="asc">Sort: A → Z</option>
              <option value="desc">Sort: Z → A</option>
            </select>
            <label className="lineage-toolbar-check" title="Hide retired / inactive entries">
              <input
                type="checkbox"
                checked={hideRetired}
                onChange={(e) => setHideRetired(e.target.checked)}
              />
              Hide retired
            </label>
          </div>
        </div>
        {visibleEntries.length === 0 ? (
          <div className="lineage-list-empty">
            No {kindLabel.toLowerCase()} match the current filter.
          </div>
        ) : (
          <EntryList
            entries={visibleEntries}
            selectedName={selectedName}
            onSelect={setSelectedName}
          />
        )}
      </div>
      <div className="lineage-right">
        <div className="lineage-header">
          <h3>{selectedName}</h3>
          {versionsLoading && <span className="loading-indicator">loading…</span>}
          <div className="lineage-header-actions">
            {onRediscover && selectedName && (
              <button
                type="button"
                className="lineage-row-action"
                onClick={() => onRediscover(selectedName)}
                title={`Re-run ${selectedName}'s tools/list handshake and refresh cached tools`}
              >
                Rediscover tools 🔄
              </button>
            )}
            {onImport && (
              <button
                type="button"
                className="lineage-row-action"
                onClick={onImport}
                title="Import Claude Code skills / MCP servers"
              >
                Import…
              </button>
            )}
            {onCreateNew && (
              <button
                type="button"
                className="primary lineage-create-cta"
                onClick={onCreateNew}
              >
                + New {newCtaLabel(kind)}
              </button>
            )}
          </div>
        </div>
        {versionsErr && (
          <div className="error-banner">Failed to load: {versionsErr}</div>
        )}
        <LineageTree
          tree={tree}
          activeDefID={activeDefID}
          effectiveDefID={effectiveDefID}
          selectedDefID={selectedDefID}
          onSelect={setSelectedDefID}
          renderDefinition={renderDefinition}
          onEditRow={onEditRow}
          onCloneRow={onCloneRow}
          onRetireRow={onRetireRow}
          onPromoteRow={onPromoteRow}
        />
      </div>
    </Splitter>
  );
}

type TypeFilter = "all" | "dynamic" | "static";
type SortMode = "none" | "type" | "asc" | "desc";

// nameMatches implements the list filter. A trailing "/" or a "*" makes it a
// PREFIX match (so "doc/" and "doc/*" both mean "names under the doc/ group");
// any other text is a case-insensitive substring match.
function nameMatches(name: string, filter: string): boolean {
  const f = filter.trim().toLowerCase();
  if (!f) return true;
  const n = name.toLowerCase();
  if (f.includes("*")) return n.startsWith(f.slice(0, f.indexOf("*")));
  if (f.endsWith("/")) return n.startsWith(f);
  return n.includes(f);
}

// entryRetired reports whether an entry should be hidden by "Hide retired": its
// active def points at a retired row, OR it's a dynamic-only name whose every
// version is retired (no active pointer, zero live versions) — the reclaimable
// "inactive" state. A static (bundled/yaml) entry is never retired. The
// live_version_count / active_retired fields are populated for agents, skills,
// and mcp-servers by the *ListNames summary queries.
function entryRetired(e: LibraryEntry): boolean {
  if (e.active_retired) return true;
  return (
    e.in_substrate &&
    !e.in_static &&
    !e.active_def_id &&
    (e.live_version_count ?? 0) === 0
  );
}

// sourceRank orders entries for the "Type" sort: static-only, then both, then
// dynamic-only.
function sourceRank(e: LibraryEntry): number {
  if (e.in_static && e.in_substrate) return 1; // both
  if (e.in_static) return 0; // static-only
  return 2; // dynamic-only
}

function newCtaLabel(kind: SubstrateKind): string {
  switch (kind) {
    case "agentdef": return "Agent";
    case "skilldef": return "Skill";
    case "mcpserverdef": return "MCP Server";
    case "webhookdef": return "Webhook";
    case "a2aservercarddef": return "A2A Server Card";
    case "a2aagentdef": return "A2A Agent";
    case "memorybackenddef": return "Memory Backend";
    // VolumeDef is FLAT (no lineage); the Volumes tab uses its own flat table,
    // not LineagePanel, so this label is never rendered for it. The arm keeps
    // the switch exhaustive over SubstrateKind.
    case "volumedef": return "Volume";
  }
}

function EntryList({
  entries,
  selectedName,
  onSelect,
}: {
  entries: LibraryEntry[];
  selectedName: string;
  onSelect: (name: string) => void;
}) {
  return (
    <ul className="lineage-name-list">
      {entries.map((e) => (
        <li
          key={e.name}
          className={
            e.name === selectedName
              ? "lineage-name-row lineage-name-selected"
              : "lineage-name-row"
          }
        >
          <button
            type="button"
            className="lineage-name-button"
            onClick={() => onSelect(e.name)}
          >
            <span className="lineage-name-label">{e.name}</span>
            <span className="lineage-name-versions">
              {entryCountLabel(e)}
            </span>
            <span className="lineage-name-chips">
              {e.in_static && (
                <span className="def-chip def-chip-static">static</span>
              )}
              {e.in_substrate && (
                <span className="def-chip def-chip-dynamic">dynamic</span>
              )}
              {e.in_substrate && e.active_def_id && !e.active_retired && (
                <span className="def-chip def-chip-active">
                  v{e.latest_version ?? "?"} ★
                </span>
              )}
              {e.in_substrate && !e.active_def_id && (
                <span className="def-chip def-chip-no-active">no active</span>
              )}
              {/* active pointer references a retired row — a corrupt-legacy
                  state (pre retire-clears-active); flag it. */}
              {e.in_substrate && e.active_retired && (
                <span className="def-chip def-chip-retired">active retired</span>
              )}
              {/* every version retired + not static → inactive, name is
                  reclaimable by a fresh create. */}
              {e.in_substrate &&
                !e.in_static &&
                !e.active_def_id &&
                e.live_version_count === 0 && (
                  <span className="def-chip def-chip-retired">inactive</span>
                )}
            </span>
          </button>
        </li>
      ))}
    </ul>
  );
}

function entryCountLabel(e: LibraryEntry): string {
  if (!e.in_substrate) return "static only";
  const n = e.version_count;
  const base = `${n} version${n === 1 ? "" : "s"}`;
  // When some versions are retired, show the live count too so the operator
  // sees how many are still usable (e.g. "3 versions · 1 live").
  if (e.live_version_count !== undefined && e.live_version_count < n) {
    return `${base} · ${e.live_version_count} live`;
  }
  return base;
}
