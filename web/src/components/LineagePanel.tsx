import { useEffect, useMemo, useState } from "react";
import {
  DefRow,
  LibraryEntry,
  listDefVersionsByName,
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
  // Substrate kind used in the listDefVersionsByName API call.
  kind: "agentdef" | "skilldef" | "mcpserverdef";
  // Human-readable label for the empty-state copy.
  kindLabel: string;
  // Unified entries — fetched by the parent so the polling
  // strategy stays uniform with other admin pages.
  entries: LibraryEntry[];
  // Storage key for the Splitter's persisted width (per sub-tab so
  // the operator can size them independently).
  splitterStorageKey: string;
  // Kind-specific renderer for the version's Definition JSON shape.
  // Used by Agents (system_prompt + allowed_tools) / Skills (body) /
  // MCP Servers (url + headers + discovered_tools).
  renderDefinition: (row: DefRow) => React.ReactNode;
}

export default function LineagePanel({
  kind,
  kindLabel,
  entries,
  splitterStorageKey,
  renderDefinition,
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
    if (!selectedEntry.in_substrate) {
      // Static-only — synthesize the pseudo-row inline. No network.
      const syntheticRow: DefRow = {
        def_id: `static:${selectedEntry.name}`,
        name: selectedEntry.name,
        version: 0,
        created_at: "",
        definition: selectedEntry.static_definition,
      };
      setVersions([syntheticRow]);
      setSelectedDefID(syntheticRow.def_id);
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
        setVersions(r.versions ?? []);
        setVersionsLoading(false);
        // Pre-select the active version if available, else the
        // highest-version (most recent) row.
        if (selectedEntry.active_def_id) {
          setSelectedDefID(selectedEntry.active_def_id);
        } else if (r.versions && r.versions.length > 0) {
          const top = [...r.versions].sort((a, b) => b.version - a.version)[0]!;
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
  const selectedRow = useMemo(
    () => versions.find((v) => v.def_id === selectedDefID),
    [versions, selectedDefID],
  );

  if (entries.length === 0) {
    return (
      <div className="empty-state">
        No {kindLabel} declared yet. Use the substrate admin API
        (POST /v1/_{kind}) or add one to loomcycle.yaml.
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
        </div>
        {versionsErr && (
          <div className="error-banner">Failed to load: {versionsErr}</div>
        )}
        <LineageTree
          tree={tree}
          activeDefID={activeDefID}
          selectedDefID={selectedDefID}
          onSelect={setSelectedDefID}
        />
        {selectedRow && (
          <div className="lineage-detail">
            <div className="lineage-detail-header">
              <span className="mono">{selectedRow.def_id}</span>
              {selectedRow.parent_def_id && (
                <span className="lineage-detail-meta">
                  ← {selectedRow.parent_def_id}
                </span>
              )}
              {selectedRow.created_at && (
                <span className="lineage-detail-meta">
                  created {new Date(selectedRow.created_at).toLocaleString()}
                </span>
              )}
              {selectedRow.content_sha256 && (
                <span className="mono lineage-detail-meta" title={selectedRow.content_sha256}>
                  {shortenSHA(selectedRow.content_sha256)}
                </span>
              )}
            </div>
            {renderDefinition(selectedRow)}
          </div>
        )}
      </div>
    </Splitter>
  );
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
              {e.in_substrate && e.active_def_id && (
                <span className="def-chip def-chip-active">
                  v{e.latest_version ?? "?"} ★
                </span>
              )}
              {e.in_substrate && !e.active_def_id && (
                <span className="def-chip def-chip-no-active">no active</span>
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
  return `${n} version${n === 1 ? "" : "s"}`;
}

function shortenSHA(s: string): string {
  // `sha256:64hex` → `sha256:8hex…` for compact display; hover for full.
  if (s.length <= 18) return s;
  return s.slice(0, 14) + "…";
}
