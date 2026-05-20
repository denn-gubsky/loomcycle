import { useCallback, useEffect, useMemo, useState } from "react";
import { AuditEvent, ListEventsResponse, listEvents } from "../api";

// AuditView — v0.8.21 cross-session event log.
//
// Why a separate view (not "the transcript page, but bigger"): the
// per-session transcript answers "what did THIS run do?". Audit
// answers "across every run, what tool calls happened in the last
// hour?" or "show me every `error` event yesterday." Different
// query shape; different UI primitives (filter toolbar, pagination,
// no agent-tree hierarchy).
//
// Pagination is offset-based (server clamps limit to 500) — fine for
// the audit-view scale we have today. Cursor-based pagination is a
// future migration if event tables grow into millions.

const PAGE_SIZE = 50;

// Well-known event types loomcycle emits. The dropdown lists these
// for discoverability; the user can also type a free-form value (the
// server filters on exact string match).
const COMMON_TYPES = [
  "text",
  "thinking",
  "tool_call",
  "tool_result",
  "usage",
  "done",
  "error",
  "started",
  "retry",
  "cache_invalidated",
  "reasoning_invalidated",
  "fallback_suppressed",
  "interrupt_raised",
  "interrupt_resolved",
];

// toRFC3339 converts an <input type="datetime-local"> value (which is
// "YYYY-MM-DDTHH:MM" in local time) to the RFC3339 the server expects.
// Empty in → empty out (the caller skips the param entirely).
function toRFC3339(local: string): string {
  if (!local) return "";
  const d = new Date(local);
  if (isNaN(d.getTime())) return "";
  return d.toISOString();
}

export default function AuditView() {
  const [filterType, setFilterType] = useState<string>("");
  const [from, setFrom] = useState<string>("");
  const [to, setTo] = useState<string>("");
  const [offset, setOffset] = useState<number>(0);
  const [resp, setResp] = useState<ListEventsResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<Set<number>>(new Set());

  const params = useMemo(
    () => ({
      type: filterType || undefined,
      from: toRFC3339(from) || undefined,
      to: toRFC3339(to) || undefined,
      limit: PAGE_SIZE,
      offset,
    }),
    [filterType, from, to, offset],
  );

  const fetchPage = useCallback(async () => {
    setLoading(true);
    try {
      const r = await listEvents(params);
      setResp(r);
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [params]);

  useEffect(() => {
    fetchPage();
  }, [fetchPage]);

  // Reset offset to 0 whenever a filter dimension changes — paging
  // into "page 4 of the old result set" makes no sense once the set
  // itself moves.
  useEffect(() => {
    setOffset(0);
  }, [filterType, from, to]);

  const total = resp?.total ?? 0;
  const events = resp?.events ?? [];
  const page = Math.floor(offset / PAGE_SIZE) + 1;
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  return (
    <div className="audit-view">
      <div className="audit-toolbar">
        <div className="audit-toolbar-row">
          <label>
            type
            <input
              list="audit-event-types"
              value={filterType}
              onChange={(e) => setFilterType(e.target.value)}
              placeholder="(any)"
            />
            <datalist id="audit-event-types">
              {COMMON_TYPES.map((t) => (
                <option key={t} value={t} />
              ))}
            </datalist>
          </label>
          <label>
            from
            <input
              type="datetime-local"
              value={from}
              onChange={(e) => setFrom(e.target.value)}
            />
          </label>
          <label>
            to
            <input
              type="datetime-local"
              value={to}
              onChange={(e) => setTo(e.target.value)}
            />
          </label>
          <button
            type="button"
            onClick={() => {
              setFilterType("");
              setFrom("");
              setTo("");
            }}
            className="audit-clear"
            disabled={!filterType && !from && !to}
          >
            clear
          </button>
          <button type="button" onClick={fetchPage} className="audit-refresh">
            ↻ refresh
          </button>
        </div>
        <div className="audit-status">
          {loading ? (
            <span className="spin">loading…</span>
          ) : (
            <span>
              {total.toLocaleString()} event{total === 1 ? "" : "s"} · page {page} of {totalPages}
            </span>
          )}
        </div>
      </div>

      {err && <div className="err">{err}</div>}

      <div className="audit-table">
        <div className="audit-row audit-head">
          <div>time</div>
          <div>type</div>
          <div>session / run</div>
          <div>payload</div>
        </div>
        {events.length === 0 && !loading && (
          <div className="empty">no events match the current filter.</div>
        )}
        {events.map((ev) => (
          <AuditRow
            key={ev.seq}
            ev={ev}
            expanded={expanded.has(ev.seq)}
            onToggle={() =>
              setExpanded((prev) => {
                const next = new Set(prev);
                if (next.has(ev.seq)) next.delete(ev.seq);
                else next.add(ev.seq);
                return next;
              })
            }
          />
        ))}
      </div>

      <div className="audit-pager">
        <button
          type="button"
          disabled={offset === 0}
          onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
        >
          ← prev
        </button>
        <span>
          page {page} of {totalPages}
        </span>
        <button
          type="button"
          disabled={offset + PAGE_SIZE >= total}
          onClick={() => setOffset(offset + PAGE_SIZE)}
        >
          next →
        </button>
      </div>
    </div>
  );
}

function AuditRow({
  ev,
  expanded,
  onToggle,
}: {
  ev: AuditEvent;
  expanded: boolean;
  onToggle: () => void;
}) {
  const preview = useMemo(() => {
    try {
      const s = JSON.stringify(ev.payload);
      return s.length > 120 ? s.slice(0, 120) + "…" : s;
    } catch {
      return "(unprintable)";
    }
  }, [ev.payload]);

  const pretty = useMemo(() => {
    try {
      return JSON.stringify(ev.payload, null, 2);
    } catch {
      return String(ev.payload);
    }
  }, [ev.payload]);

  const [copyState, setCopyState] = useState<"idle" | "copied" | "error">("idle");
  const copy = async (e: React.MouseEvent) => {
    e.stopPropagation();
    try {
      await navigator.clipboard.writeText(pretty);
      setCopyState("copied");
      setTimeout(() => setCopyState("idle"), 1200);
    } catch {
      setCopyState("error");
      setTimeout(() => setCopyState("idle"), 1500);
    }
  };

  return (
    <>
      <div className={`audit-row ${expanded ? "expanded" : ""}`} onClick={onToggle}>
        <div title={ev.timestamp}>{new Date(ev.timestamp).toLocaleString()}</div>
        <div>
          <span className={`audit-type type-${ev.type}`}>{ev.type}</span>
        </div>
        <div className="audit-ids">
          <span title={ev.session_id}>{ev.session_id.slice(0, 12)}…</span>
          <span className="muted"> / </span>
          <span title={ev.run_id}>{ev.run_id.slice(0, 12)}…</span>
        </div>
        <div className="audit-preview">{preview}</div>
      </div>
      {expanded && (
        <div className="audit-detail">
          <button
            type="button"
            className={`copy-btn ${
              copyState === "copied"
                ? "copy-state-copied"
                : copyState === "error"
                ? "copy-state-error"
                : ""
            }`}
            onClick={copy}
          >
            {copyState === "copied" ? "✓ copied" : copyState === "error" ? "✗" : "⧉ copy"}
          </button>
          <div className="audit-detail-meta">
            <span>
              <strong>seq</strong> {ev.seq}
            </span>
            <span>
              <strong>session_id</strong> {ev.session_id}
            </span>
            <span>
              <strong>run_id</strong> {ev.run_id}
            </span>
          </div>
          <pre className="audit-detail-payload">{pretty}</pre>
        </div>
      )}
    </>
  );
}
