import { useEffect, useMemo, useState } from "react";
import {
  DefNameSummary,
  DefRow,
  listDefVersionsByName,
} from "../api";
import Splitter from "./Splitter";
import LineageTree, { buildLineageTree } from "./LineageTree";

// LineagePanel is the shared shape that backs each Library sub-tab
// (Agents / Skills / MCP Servers). Left pane lists declared NAMES
// supplied by the parent page; right pane shows the lineage tree
// for the selected name + the selected version's definition JSON.
//
// Polling cadence: name list refresh is owned by the parent (Library
// page calls listAgentDefNames / listSkillDefNames / etc.); version
// lineage refreshes on selection change.

export interface LineagePanelProps {
  // Substrate kind used in the listDefVersionsByName API call.
  kind: "agentdef" | "skilldef" | "mcpserverdef";
  // Human-readable label for the empty-state copy.
  kindLabel: string;
  // List of declared names — fetched by the parent so the polling
  // strategy stays uniform with other admin pages.
  names: DefNameSummary[];
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
  names,
  splitterStorageKey,
  renderDefinition,
}: LineagePanelProps) {
  const [selectedName, setSelectedName] = useState<string>(() =>
    names.length > 0 ? names[0]!.name : "",
  );
  const [versions, setVersions] = useState<DefRow[]>([]);
  const [versionsErr, setVersionsErr] = useState<string | null>(null);
  const [versionsLoading, setVersionsLoading] = useState(false);
  const [selectedDefID, setSelectedDefID] = useState<string>("");

  // When the names list updates and the currently-selected name is
  // gone (renamed / retired tree), fall back to the first available.
  useEffect(() => {
    if (selectedName && names.find((n) => n.name === selectedName)) return;
    setSelectedName(names.length > 0 ? names[0]!.name : "");
  }, [names, selectedName]);

  // Fetch the lineage for the selected name. Selecting a different
  // name resets the selectedDefID + clears the previous lineage.
  useEffect(() => {
    if (!selectedName) {
      setVersions([]);
      setSelectedDefID("");
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
        const summary = names.find((n) => n.name === selectedName);
        if (summary?.active_def_id) {
          setSelectedDefID(summary.active_def_id);
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
  }, [kind, selectedName, names]);

  const tree = useMemo(() => buildLineageTree(versions), [versions]);
  const activeDefID = useMemo(
    () => names.find((n) => n.name === selectedName)?.active_def_id ?? "",
    [names, selectedName],
  );
  const selectedRow = useMemo(
    () => versions.find((v) => v.def_id === selectedDefID),
    [versions, selectedDefID],
  );

  if (names.length === 0) {
    return (
      <div className="empty-state">
        No {kindLabel} declared yet. Use the substrate admin API
        (POST /v1/_{kind}) to create one.
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
      <NameList
        names={names}
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
              <span className="lineage-detail-meta">
                created {new Date(selectedRow.created_at).toLocaleString()}
              </span>
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

function NameList({
  names,
  selectedName,
  onSelect,
}: {
  names: DefNameSummary[];
  selectedName: string;
  onSelect: (name: string) => void;
}) {
  return (
    <ul className="lineage-name-list">
      {names.map((n) => (
        <li
          key={n.name}
          className={
            n.name === selectedName
              ? "lineage-name-row lineage-name-selected"
              : "lineage-name-row"
          }
        >
          <button
            type="button"
            className="lineage-name-button"
            onClick={() => onSelect(n.name)}
          >
            <span className="lineage-name-label">{n.name}</span>
            <span className="lineage-name-versions">
              {n.version_count} version{n.version_count === 1 ? "" : "s"}
            </span>
            {n.active_def_id ? (
              <span className="def-chip def-chip-active">v{n.latest_version} ★</span>
            ) : (
              <span className="def-chip def-chip-no-active">no active</span>
            )}
          </button>
        </li>
      ))}
    </ul>
  );
}

function shortenSHA(s: string): string {
  // `sha256:64hex` → `sha256:8hex…` for compact display; hover for full.
  if (s.length <= 18) return s;
  return s.slice(0, 14) + "…";
}
