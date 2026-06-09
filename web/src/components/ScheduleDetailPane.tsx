import { useCallback, useEffect, useRef, useState } from "react";
import {
  getScheduleState,
  schedulePause,
  scheduleResume,
  scheduleRunNow,
  ScheduleDefRow,
  scheduleDefGet,
  scheduleDefList,
  scheduleDefRetire,
  scheduleDefActivate,
  ScheduleListEntry,
  ScheduleStateView,
} from "../api";
import { SourceChip } from "../pages/SchedulesView";
import ScheduleHookList from "./ScheduleHookList";

interface Props {
  entry: ScheduleListEntry;
  onMutated: () => void;
  onForkTemplate: () => void;
}

// ScheduleDetailPane renders everything to the right of the list:
// identity block, schedule block (cron + next-fire times + action
// buttons), on_complete hooks (display-only in v1; substrate add/
// remove lives in a follow-up), run-state block (last_*).
//
// Polls /state every 5s while mounted so operators see live next/last
// updates without manual refresh.
export default function ScheduleDetailPane({ entry, onMutated, onForkTemplate }: Props) {
  const [substrateRow, setSubstrateRow] = useState<ScheduleDefRow | null>(null);
  const [substrateVersions, setSubstrateVersions] = useState<ScheduleDefRow[]>([]);
  const [state, setState] = useState<ScheduleStateView | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // Re-fetch substrate detail when the selected entry changes. The
  // list endpoint deliberately omits sensitive fields (credentials),
  // so the detail pane fetches the full row via POST /v1/_scheduledef
  // {op:"get"}. For static-only entries this fetch is skipped — the
  // static_definition is already in the list payload.
  useEffect(() => {
    let cancelled = false;
    setErr(null);
    setSubstrateRow(null);
    setState(null);

    if (entry.in_substrate && entry.active_def_id) {
      scheduleDefGet(entry.active_def_id)
        .then((row) => {
          if (!cancelled) setSubstrateRow(row);
        })
        .catch((e) => {
          if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
        });
      scheduleDefList(entry.name)
        .then((resp) => {
          if (!cancelled) setSubstrateVersions(resp.versions ?? []);
        })
        .catch(() => {
          /* lineage is best-effort */
        });
    }
  }, [entry.name, entry.active_def_id, entry.in_substrate]);

  // Poll runtime state if there's an active substrate def_id.
  //
  // Race fix (v1.x review #2): two slow fetches can race within a
  // single effect lifecycle — tick N starts → tick N+1 starts and
  // finishes first with newer state → tick N finally resolves and
  // overwrites with stale data. The `cancelled` flag only protects
  // the unmount/dep-change case, not in-flight ordering. A
  // monotonically-increasing generation counter solves it: each
  // fetch captures its gen at issue-time and only commits state if
  // its gen is the latest. Effectively cheap (one ref bump per
  // fetch) and self-resetting on dep change because the ref persists
  // but newer fetches monotonically advance.
  const stateGen = useRef(0);
  useEffect(() => {
    if (!entry.active_def_id) return;
    let cancelled = false;
    const fetchState = async () => {
      const gen = ++stateGen.current;
      try {
        const s = await getScheduleState(entry.active_def_id!);
        if (cancelled || gen !== stateGen.current) return;
        setState(s);
      } catch (e) {
        if (cancelled || gen !== stateGen.current) return;
        // 404 is expected when state hasn't been seeded yet; ignore.
        if (!(e instanceof Error && e.message.includes("404"))) {
          setErr(e instanceof Error ? e.message : String(e));
        }
      }
    };
    fetchState();
    const t = setInterval(fetchState, 5000);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [entry.active_def_id]);

  const runMutation = useCallback(
    async (label: string, fn: () => Promise<unknown>) => {
      setBusy(true);
      setErr(null);
      try {
        await fn();
        onMutated();
      } catch (e) {
        setErr(`${label}: ${e instanceof Error ? e.message : String(e)}`);
      } finally {
        setBusy(false);
      }
    },
    [onMutated],
  );

  const handleRunNow = () =>
    runMutation("run-now", () => scheduleRunNow(entry.active_def_id!));
  const handlePause = () =>
    runMutation("pause", () => schedulePause(entry.active_def_id!));
  const handleResume = () =>
    runMutation("resume", () => scheduleResume(entry.active_def_id!));
  const handleRetire = () =>
    runMutation("retire", () => scheduleDefRetire(entry.active_def_id!, true));
  const handleUnretire = () =>
    runMutation("un-retire", () => scheduleDefRetire(entry.active_def_id!, false));
  // Re-activate an older version. ScheduleDef has no standalone promote
  // op, so this forks from the chosen version (auto-promote) — the new
  // version inherits its definition and becomes active.
  const handleActivate = (defID: string) =>
    runMutation("activate", () => scheduleDefActivate(entry.name, defID));

  // Static-yaml side renderer: parse static_definition for the
  // identity/schedule/hooks blocks.
  const staticDef = entry.static_definition as Record<string, unknown> | undefined;
  // Substrate side: prefer the full row (has credentials_keys, user_id, etc.).
  const def = (substrateRow?.definition as Record<string, unknown> | undefined) ?? staticDef;

  const isPaused =
    state?.paused_until && new Date(state.paused_until).getTime() > Date.now();

  return (
    <div className="schedule-detail">
      <div className="schedule-detail-header">
        <h3>
          {entry.name} <SourceChip source={entry.source} />
        </h3>
        {entry.in_static && (
          <button className="schedule-detail-action" onClick={onForkTemplate}>
            Fork this template
          </button>
        )}
      </div>

      {err && <div className="schedule-detail-err">Error: {err}</div>}

      {/* Identity block */}
      <section className="schedule-detail-block">
        <h4>Identity</h4>
        <DefField label="user_id" value={(def?.user_id as string) || "—"} />
        {def?.user_credentials ? (
          <DefField
            label="user_credentials"
            value={
              <span>
                {Object.keys(def.user_credentials as Record<string, string>)
                  .map((k) => `${k}: ••••`)
                  .join(", ") || "(none)"}
              </span>
            }
          />
        ) : null}
        {entry.active_def_id && <DefField label="def_id" value={entry.active_def_id} />}
        {entry.latest_version && (
          <DefField label="version" value={`v${entry.latest_version}`} />
        )}
      </section>

      {/* Schedule block */}
      <section className="schedule-detail-block">
        <h4>Schedule</h4>
        {def?.schedule ? (
          <DefField label="cron" value={String(def.schedule)} />
        ) : def?.user_tier_schedules != null ? (
          <>
            <DefField label="user_tier" value={(def.user_tier as string) || "(not set)"} />
            <DefField
              label="user_tier_schedules"
              value={JSON.stringify(def.user_tier_schedules)}
            />
          </>
        ) : (
          <DefField label="cron" value="(none)" />
        )}
        <DefField label="timezone" value={(def?.timezone as string) || "UTC"} />
        <DefField
          label="enabled"
          value={def?.enabled === false ? "false (disabled)" : "true"}
        />
        {state?.next_run_at && (
          <DefField label="next_run_at" value={state.next_run_at} />
        )}
        {state?.last_run_at && (
          <DefField label="last_run_at" value={state.last_run_at} />
        )}
        {state?.last_status && (
          <DefField
            label="last_status"
            value={<span className={`status-chip status-${state.last_status}`}>{state.last_status}</span>}
          />
        )}
        {state?.last_error && (
          <DefField label="last_error" value={<code>{state.last_error}</code>} />
        )}
        {isPaused && <div className="schedule-detail-paused">⏸ paused</div>}
      </section>

      {/* Action buttons — only when there's a substrate def_id to target. */}
      {entry.active_def_id && (
        <section className="schedule-detail-actions">
          <button
            onClick={handleRunNow}
            disabled={busy}
            title="Sets next_run_at to the past so the sweeper fires this def on its next tick. Caveat: if a run is already in progress, the in-flight fire's post-completion next_run_at advance overwrites this admin intent. At most one extra fire is guaranteed."
          >
            Run now
          </button>
          {isPaused ? (
            <button onClick={handleResume} disabled={busy}>
              Resume
            </button>
          ) : (
            <button onClick={handlePause} disabled={busy}>
              Pause
            </button>
          )}
          <button onClick={handleRetire} disabled={busy} className="schedule-detail-danger">
            Retire
          </button>
          {substrateRow?.retired && (
            <button onClick={handleUnretire} disabled={busy}>
              Un-retire
            </button>
          )}
        </section>
      )}

      {/* on_complete hooks — editable when the schedule has a
          substrate-side def_id; display-only otherwise. */}
      <ScheduleHookList
        hooks={(Array.isArray(def?.on_complete) ? def.on_complete : []) as Array<Record<string, unknown>>}
        activeDefID={entry.in_substrate ? entry.active_def_id : undefined}
        onMutated={onMutated}
      />

      {/* Lineage tree — show all versions of this name. */}
      {substrateVersions.length > 1 && (
        <section className="schedule-detail-block">
          <h4>Lineage</h4>
          <ul className="schedule-lineage">
            {substrateVersions.map((v) => (
              <li
                key={v.def_id}
                className={`schedule-lineage-row${
                  v.def_id === entry.active_def_id ? " schedule-lineage-active" : ""
                }`}
              >
                <span className="schedule-lineage-version">v{v.version}</span>
                <code className="schedule-lineage-defid">{v.def_id}</code>
                {v.bootstrapped_from_static && (
                  <span className="schedule-lineage-tag">bootstrapped</span>
                )}
                {v.retired && <span className="schedule-lineage-tag">retired</span>}
                {v.def_id !== entry.active_def_id && !v.retired && (
                  <button
                    type="button"
                    className="schedule-detail-action"
                    onClick={() => handleActivate(v.def_id)}
                    disabled={busy}
                    title="Fork from this version with auto-promote so it becomes the active def"
                  >
                    Activate
                  </button>
                )}
              </li>
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}

function DefField({
  label,
  value,
}: {
  label: string;
  value: string | React.ReactNode;
}) {
  return (
    <div className="schedule-detail-field">
      <span className="schedule-detail-label">{label}</span>
      <span className="schedule-detail-value">{value}</span>
    </div>
  );
}
