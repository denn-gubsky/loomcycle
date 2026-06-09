import { useCallback, useEffect, useMemo, useState } from "react";
import {
  listSchedules,
  ScheduleListEntry,
} from "../api";
import Splitter from "../components/Splitter";
import ScheduleDetailPane from "../components/ScheduleDetailPane";
import ScheduleForkForm from "../components/ScheduleForkForm";
import ScheduleCreateForm from "../components/ScheduleCreateForm";

// SchedulesView is the v1.x RFC E admin tab — list + detail two-pane
// for scheduled-run definitions. Mirrors /ui/library's shape but
// purpose-built for schedules: source chip per row, filter chips
// across the top, per-user + per-template text filters, run-now /
// pause / resume / retire actions in the detail pane.
//
// Polling: 15s for the list (run state changes are fine on this
// cadence). The detail pane has its own faster poll for state.

const REFRESH_MS = 15_000;

type SourceFilter = "all" | "static" | "forked" | "standalone";

export default function SchedulesView() {
  const [entries, setEntries] = useState<ScheduleListEntry[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [selectedName, setSelectedName] = useState<string>("");
  const [refreshKey, setRefreshKey] = useState(0);
  const refreshNow = useCallback(() => setRefreshKey((k) => k + 1), []);

  // Filter state — text + source chips. Text filters are AND'd against
  // each entry's name (substring) so operators can find e.g. all
  // user_alice forks by typing "alice" assuming the operator named
  // them with the user_id suffix.
  const [filterText, setFilterText] = useState("");
  const [sourceFilter, setSourceFilter] = useState<SourceFilter>("all");

  // Fork-form modal state. Opens when the user clicks "Fork this
  // template" on a static-or-both entry. Null = closed.
  const [forkModalTemplate, setForkModalTemplate] = useState<string | null>(null);
  // Create-standalone modal — author a brand-new schedule from scratch.
  const [createOpen, setCreateOpen] = useState(false);

  useEffect(() => {
    let cancelled = false;
    const fetchAll = async () => {
      try {
        const resp = await listSchedules();
        if (cancelled) return;
        setEntries(resp.entries ?? []);
        setErr(null);
        // Auto-select first entry on first load.
        if (selectedName === "" && (resp.entries ?? []).length > 0) {
          setSelectedName(resp.entries[0]!.name);
        }
      } catch (e) {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      }
    };
    fetchAll();
    const t = setInterval(fetchAll, REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [refreshKey]);

  // Apply filters to derive what the list actually renders. Memoised
  // so changing selection doesn't re-run the filter chain.
  const filtered = useMemo(() => {
    return entries.filter((e) => {
      // Source chip filter:
      //   static  = static-only entries (yaml templates)
      //   forked  = substrate entries with an active_def_id (forked
      //             from a template OR freestanding substrate creates)
      //   standalone = static-only AND has explicit `user_id` (template
      //             would have empty user_id). We approximate this as
      //             "static-only" for now since the listEntry has no
      //             user_id field — operators can use the text filter
      //             to drill in.
      if (sourceFilter === "static" && e.source !== "static-only") return false;
      if (sourceFilter === "forked" && e.source === "static-only") return false;
      if (sourceFilter === "standalone" && e.source !== "static-only") return false;
      if (filterText) {
        const f = filterText.toLowerCase();
        if (!e.name.toLowerCase().includes(f)) return false;
      }
      return true;
    });
  }, [entries, sourceFilter, filterText]);

  const selectedEntry = filtered.find((e) => e.name === selectedName) ??
    entries.find((e) => e.name === selectedName);

  return (
    <div className="schedules-view">
      <div className="schedules-header">
        <h2>Schedules</h2>
        <div className="schedules-filter-row">
          <SourceFilterChips value={sourceFilter} onChange={setSourceFilter} entries={entries} />
          <input
            type="text"
            className="schedules-filter-text"
            placeholder="filter by name…"
            value={filterText}
            onChange={(e) => setFilterText(e.target.value)}
          />
          <button className="schedules-refresh-btn" onClick={refreshNow} title="Refresh list">
            ↻
          </button>
          <button
            type="button"
            className="primary"
            onClick={() => setCreateOpen(true)}
            title="Author a brand-new schedule from scratch"
          >
            + New schedule
          </button>
        </div>
        {err && <div className="schedules-err">Error: {err}</div>}
      </div>
      <Splitter storageKey="schedules-splitter" defaultLeftWidth={350} minLeftWidth={250}>
        <div className="schedules-list-pane">
          {filtered.length === 0 ? (
            <div className="schedules-empty">
              {entries.length === 0
                ? "No schedules. Author one with the ScheduleDef tool (POST /v1/_scheduledef) or add a `scheduled_runs:` entry in loomcycle.yaml."
                : "No schedules match the current filter."}
            </div>
          ) : (
            <ul className="schedules-list" role="listbox" aria-label="Schedules">
              {filtered.map((e) => (
                <ScheduleListRow
                  key={e.name}
                  entry={e}
                  selected={e.name === selectedName}
                  onClick={() => setSelectedName(e.name)}
                />
              ))}
            </ul>
          )}
        </div>
        <div className="schedules-detail-pane">
          {selectedEntry ? (
            <ScheduleDetailPane
              entry={selectedEntry}
              onMutated={refreshNow}
              onForkTemplate={() => setForkModalTemplate(selectedEntry.name)}
            />
          ) : (
            <div className="schedules-empty">Select a schedule to view its details.</div>
          )}
        </div>
      </Splitter>
      {forkModalTemplate && (
        <ScheduleForkForm
          templateName={forkModalTemplate}
          onClose={() => setForkModalTemplate(null)}
          onForked={() => {
            setForkModalTemplate(null);
            refreshNow();
          }}
        />
      )}
      {createOpen && (
        <ScheduleCreateForm
          existingNames={entries.map((e) => e.name)}
          onClose={() => setCreateOpen(false)}
          onCreated={() => {
            setCreateOpen(false);
            refreshNow();
          }}
        />
      )}
    </div>
  );
}

interface SourceFilterChipsProps {
  value: SourceFilter;
  onChange: (v: SourceFilter) => void;
  entries: ScheduleListEntry[];
}

// SourceFilterChips renders the all/static/forked/standalone chip row
// at the top of the list pane. Each chip shows the count of matching
// entries so the operator can see filter cardinality before clicking.
function SourceFilterChips({ value, onChange, entries }: SourceFilterChipsProps) {
  const counts = useMemo(() => {
    const c = { all: entries.length, static: 0, forked: 0, standalone: 0 };
    for (const e of entries) {
      if (e.source === "static-only") {
        c.static++;
        c.standalone++;
      } else {
        c.forked++;
      }
    }
    return c;
  }, [entries]);

  const chip = (key: SourceFilter, label: string) => (
    <button
      type="button"
      className={`schedules-chip${value === key ? " schedules-chip-active" : ""}`}
      onClick={() => onChange(key)}
    >
      {label} <span className="schedules-chip-count">{counts[key]}</span>
    </button>
  );

  return (
    <div className="schedules-chip-row">
      {chip("all", "All")}
      {chip("static", "Static")}
      {chip("forked", "Forked")}
      {chip("standalone", "Standalone")}
    </div>
  );
}

interface ScheduleListRowProps {
  entry: ScheduleListEntry;
  selected: boolean;
  onClick: () => void;
}

function ScheduleListRow({ entry, selected, onClick }: ScheduleListRowProps) {
  // Keyboard-accessibility: <li> is not a tabbable element by default,
  // so we add role="option" + tabIndex=0 + an Enter/Space keydown
  // handler. Tab moves the operator's focus through the list rows;
  // Enter/Space activates selection. aria-selected mirrors the visual
  // selected state for screen-reader users.
  const handleKeyDown = (e: React.KeyboardEvent<HTMLLIElement>) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onClick();
    }
  };
  return (
    <li
      role="option"
      tabIndex={0}
      aria-selected={selected}
      className={`schedules-list-row${selected ? " schedules-list-row-selected" : ""}`}
      onClick={onClick}
      onKeyDown={handleKeyDown}
    >
      <div className="schedules-list-row-name">{entry.name}</div>
      <div className="schedules-list-row-meta">
        <SourceChip source={entry.source} />
        {entry.in_substrate && entry.latest_version && (
          <span className="schedules-version-chip">v{entry.latest_version}</span>
        )}
      </div>
    </li>
  );
}

// SourceChip renders the static/dynamic/both badge with consistent
// colours operators recognise from /ui/library.
export function SourceChip({ source }: { source: ScheduleListEntry["source"] }) {
  const label =
    source === "static-only" ? "static" : source === "dynamic-only" ? "dynamic" : "both";
  return <span className={`source-chip source-chip-${source}`}>{label}</span>;
}
