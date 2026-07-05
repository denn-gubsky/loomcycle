import { useEffect, useMemo, useState } from "react";
import {
  DefRow,
  LibraryEntry,
  listDefVersionsByName,
  type SubstrateKind,
} from "../api";
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
// (LibraryView calls listLibraryAgents / listLibrarySkills / etc.);
// version lineage refreshes on selection change.

export interface LineagePanelProps {
  // Substrate kind used in the listDefVersionsByName API call. Covers
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
  onRetireRow?: (row: DefRow) => void;
  onPromoteRow?: (row: DefRow) => void;
  // v0.10.4 — MCP-only "Rediscover tools" button in the header.
  // Optional + only wired by LibraryView for the mcp-servers tab.
  // The callback receives the currently-selected entry's name so the
  // caller doesn't have to re-derive it.
  onRediscover?: (name: string) => void;
  // RFC AU — "Import" CTA beside "+ New" (skills + mcp-servers tabs).
  // Opens the Claude Code import flow. Optional.
  onImport?: () => void;
}

export default function LineagePanel({
  kind,
  kindLabel,
  entries,
  splitterStorageKey,
  renderDefinition,
  onCreateNew,
  onEditRow,
  onRetireRow,
  onPromoteRow,
  onRediscover,
  onImport,
}: LineagePanelProps) {
  const [selectedName, setSelectedName] = useState<string>(() =>
    entries.length > 0 ? entries[0]!.name : "",
  );
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
    listDefVersionsByName(kind, selectedName)
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
      .catch((e: Error) => {
        if (cancelled) return;
        setVersionsErr(e.message);
        setVersionsLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [kind, selectedName, selectedEntry]);

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
      <EntryList
        entries={entries}
        selectedName={selectedName}
        onSelect={setSelectedName}
      />
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
          onRetireRow={onRetireRow}
          onPromoteRow={onPromoteRow}
        />
      </div>
    </Splitter>
  );
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

