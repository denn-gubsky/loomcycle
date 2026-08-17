import { useEffect, useMemo, useState } from "react";
import { useMemoryData } from "../lib/dataLayer";
import {
  appendChange,
  describeChange,
  emptyBuffer,
  isDocumentChange,
  matchesFilter,
  CHANGE_BUFFER_CAP,
  CHANGE_FEED_ENV,
  type ChangeBuffer,
  type ChangeFamily,
  type ChangeFilter,
  type MemoryChangeRow,
} from "../lib/changeFeed";

// ChangeFeedPanel — a live tail of the value-free change feed (RFC CF §9).
//
// It answers one question an operator otherwise cannot: IS ANYTHING HAPPENING. A
// consolidation pass takes minutes and reports only at the end, so between starting
// it and reading its summary there is nothing to look at. This is that.
//
// It shows COORDINATES, never values, because that is all the feed carries — a
// deliberate privacy property of the endpoint. There is no "value" column here to
// leave conspicuously empty.
//
// THREE STATES, KEPT DISTINCT, and this is the part that earns the component:
//
//   unsupported  the data layer has no streamChanges (a host-supplied layer, or a
//                bare `client` with no base URL for SSE). Nothing will ever arrive
//                and the runtime is not the reason.
//   disabled     the runtime answered `enabled: false` — the feed exists but
//                LOOMCYCLE_MEMORY_CHANGES_ENABLED is unset, so no write is being
//                captured. Nothing will ever arrive and the fix is one env var.
//   live         connected and capturing. An empty list here genuinely means
//                nothing has changed.
//
// Collapsing those into "no rows yet" is the failure this panel is written against:
// all three look identical, and two of them are misconfigurations that a reader
// would instead take as "the pass is doing nothing".

const FAMILIES: ChangeFamily[] = ["memory", "documents"];

type FeedState = "connecting" | "live" | "disabled" | "unsupported" | "error";

export interface ChangeFeedPanelProps {
  /** Pre-narrow the tail to one scope_id (the console's current selection). The feed
   *  is TENANT-WIDE, so on a shared deployment an unfiltered tail is every user's
   *  activity at once. */
  scopeId?: string;
}

export default function ChangeFeedPanel({ scopeId }: ChangeFeedPanelProps) {
  const data = useMemoryData();
  const supported = typeof data.streamChanges === "function";

  const [buf, setBuf] = useState<ChangeBuffer>(emptyBuffer);
  const [state, setState] = useState<FeedState>(
    supported ? "connecting" : "unsupported",
  );
  const [err, setErr] = useState<string>("");
  const [paused, setPaused] = useState(false);

  const [filter, setFilter] = useState<ChangeFilter>({
    family: "",
    scope: "",
    scopeId: scopeId ?? "",
    type: "",
  });

  // The effect below depends on `paused` and NOT on the filter: filtering is a VIEW
  // over what has already arrived, so re-opening the stream on a filter change would
  // silently discard rows the operator had seen and reset the counters they were
  // reading.
  useEffect(() => {
    if (!supported || paused) return;
    const ac = new AbortController();
    let cancelled = false;
    // Each family is its own endpoint, so both are tailed and merged here. A single
    // status is enough: the two share the flag, and reporting per-family would
    // invite a UI that shows one half live and the other silent for no real reason.
    const pumps = FAMILIES.map(async (family) => {
      for await (const frame of data.streamChanges!(family, {
        signal: ac.signal,
      })) {
        if (cancelled) return;
        if (frame.kind === "status") {
          setState(frame.status.enabled ? "live" : "disabled");
          continue;
        }
        setBuf((b) => appendChange(b, frame.change));
      }
    });
    Promise.all(pumps).catch((e: unknown) => {
      if (cancelled || ac.signal.aborted) return;
      setState("error");
      setErr(e instanceof Error ? e.message : String(e));
    });
    return () => {
      cancelled = true;
      ac.abort();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [supported, paused, data]);

  const visible = useMemo(
    () => buf.rows.filter((r) => matchesFilter(r, filter)),
    [buf, filter],
  );

  return (
    <div className="change-feed">
      <div className="change-feed-head">
        <FeedBadge state={state} />
        <div className="change-feed-filters">
          <select
            value={filter.family}
            onChange={(e) =>
              setFilter((f) => ({
                ...f,
                family: e.target.value as ChangeFilter["family"],
              }))
            }
            aria-label="family"
          >
            <option value="">both families</option>
            <option value="memory">memory</option>
            <option value="documents">documents</option>
          </select>
          <select
            value={filter.scope}
            onChange={(e) =>
              setFilter((f) => ({
                ...f,
                scope: e.target.value as ChangeFilter["scope"],
              }))
            }
            aria-label="scope"
          >
            <option value="">any scope</option>
            <option value="agent">agent</option>
            <option value="user">user</option>
            <option value="tenant">tenant</option>
          </select>
          <input
            className="change-feed-scopeid"
            placeholder="scope_id contains…"
            value={filter.scopeId}
            onChange={(e) =>
              setFilter((f) => ({ ...f, scopeId: e.target.value }))
            }
            aria-label="scope_id filter"
          />
          <button
            type="button"
            className="change-feed-btn"
            onClick={() => setPaused((p) => !p)}
            disabled={!supported || state === "disabled"}
          >
            {paused ? "resume" : "pause"}
          </button>
          <button
            type="button"
            className="change-feed-btn"
            onClick={() => setBuf(emptyBuffer)}
            disabled={!buf.rows.length}
          >
            clear
          </button>
        </div>
      </div>

      {state === "unsupported" && (
        <div className="change-feed-note">
          This build cannot tail the change feed — it needs a{" "}
          <code>connection</code> (base URL + fetch), which a prebuilt client or
          a custom data layer does not expose. Nothing will arrive here; the
          runtime is not the reason.
        </div>
      )}
      {state === "disabled" && (
        <div className="change-feed-note">
          The change feed is <strong>off</strong> in this runtime, so no write
          is being recorded and nothing will ever arrive here. Set{" "}
          <code>{CHANGE_FEED_ENV}=1</code> and restart to turn it on.
        </div>
      )}
      {state === "error" && <div className="err">{err}</div>}

      <Counters buf={buf} shown={visible.length} state={state} />

      {visible.length === 0 ? (
        <div className="empty">
          {state === "live"
            ? buf.rows.length
              ? "No change matches these filters."
              : "Connected. Nothing has changed yet."
            : "—"}
        </div>
      ) : (
        <table className="change-feed-table">
          <thead>
            <tr>
              <th>seq</th>
              <th>when</th>
              <th>type</th>
              <th>scope</th>
              <th>coordinate</th>
            </tr>
          </thead>
          <tbody>
            {visible.map((r) => (
              <ChangeRow key={`${r.type}:${r.seq}`} row={r} />
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

function ChangeRow({ row }: { row: MemoryChangeRow }) {
  const deleted = row.type.endsWith(".delete") || row.type.endsWith(".deleted");
  return (
    <tr>
      <td className="change-feed-seq">{row.seq}</td>
      <td className="change-feed-at">{formatAt(row.at)}</td>
      <td>
        <span
          className={
            "change-feed-type" +
            (deleted ? " change-feed-type-delete" : "") +
            (isDocumentChange(row.type) ? " change-feed-type-doc" : "")
          }
        >
          {row.type}
        </span>
      </td>
      <td className="change-feed-scope">
        {row.scope}
        {row.scope_id ? `/${row.scope_id}` : ""}
      </td>
      <td className="change-feed-coord">{describeChange(row)}</td>
    </tr>
  );
}

function FeedBadge({ state }: { state: FeedState }) {
  const label: Record<FeedState, string> = {
    connecting: "connecting…",
    live: "live",
    disabled: "feed off",
    unsupported: "unavailable",
    error: "error",
  };
  return (
    <span className={`change-feed-badge change-feed-badge-${state}`}>
      {label[state]}
    </span>
  );
}

// Counters states what is on screen against what arrived, and names the bound when
// it bit. A tail that quietly forgets makes "showing 200" read as "200 things
// happened" — the same silent-truncation trap as a capped scan reporting a clean
// zero.
function Counters({
  buf,
  shown,
  state,
}: {
  buf: ChangeBuffer;
  shown: number;
  state: FeedState;
}) {
  if (state !== "live" && buf.seen === 0) return null;
  const parts = [`${shown} shown`];
  if (buf.seen !== shown) parts.push(`${buf.seen} received`);
  if (buf.dropped > 0) {
    parts.push(
      `${buf.dropped} older dropped (this view keeps the latest ${CHANGE_BUFFER_CAP})`,
    );
  }
  return <div className="change-feed-counters">{parts.join(" · ")}</div>;
}

// formatAt shows the wall-clock time only. A change feed is read for ordering and
// recency, and a full RFC3339 stamp per row crowds out the coordinate that matters.
// An unparseable value is shown verbatim rather than as "Invalid Date".
function formatAt(at: string): string {
  const d = new Date(at);
  if (Number.isNaN(d.getTime())) return at;
  return d.toLocaleTimeString();
}
