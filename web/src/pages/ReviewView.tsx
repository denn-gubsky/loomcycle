import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useFocusTenant, usePrincipal, useUserId } from "../components/Layout";
import {
  getJudgeAgreement,
  listReviewQueue,
  listRulings,
  revertRuling,
  ruleStatements,
  type JudgeAgreementResponse,
  type ReviewFilter,
  type ReviewScope,
  type ReviewStatement,
  type RuleStatementsResponse,
  type RulingRecord,
} from "../api";
import {
  chunkCount,
  RULING_CHUNK,
  runRuling,
  undoRulings,
  type ConfirmReason,
  type RulingProgress,
  type RulingSelection,
} from "@loomcycle/explorer";
import { ApiError } from "../lib/apiError";
import {
  auditFilter,
  auditOf,
  canUndo,
  filtersFromSearch,
  groupLabel,
  groupsFromSummary,
  judgeOf,
  percent,
  reviewKey,
  reviewScopeFromSearch,
  reviewScopeId,
  rulingSelection,
  rulingSummary,
  searchFromFilters,
  undoSummary,
  type ReviewGroup,
} from "../lib/review";

// The Review page (/review): the scope's proposed statements, grouped as the
// queue's summary groups them, ruled on by row, by group or all at once (RFC FS
// P1). Everything it does goes through the review routes, which decide who may
// see and who may rule; a member without the `review` grant gets their refusal.

const PAGE = 50;
// How many earlier rulings the list shows per page.
const RULINGS_PAGE = 10;
const QUOTE_CLIP = 160;
// How many of the dry run's sample of 20 the confirm lists.
const CONFIRM_SAMPLE = 6;

// noReviewRights says whether a refusal means "you may not review here": the
// review routes refuse with 403 (forbidden, or a scope or access-control
// refusal before them).
export function noReviewRights(e: unknown): boolean {
  return e instanceof ApiError && e.status === 403;
}

function errText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

function clip(s: string, max = QUOTE_CLIP): string {
  return s.length > max ? s.slice(0, max - 1) + "…" : s;
}

function docHref(documentId: string | undefined, scope: ReviewScope): string | undefined {
  return documentId ? `/documents/${encodeURIComponent(documentId)}?scope=${scope}` : undefined;
}

// ---- presentational parts (rendered on their own in the tests) ----------------

export function ReviewNoRights({ message }: { message?: string }) {
  return (
    <div className="review-no-rights" role="alert">
      <div className="review-no-rights-head">No review rights here</div>
      <p>
        Reviewing proposed statements is for an admin, a tenant operator, or a member holding the
        access-control action <code>review</code> on the documents&rsquo; paths. Ask an operator for a{" "}
        <code>review</code> grant on the paths you should review.
      </p>
      {message && <p className="settings-muted">{message.replace(/^\d{3} [^:]*: /, "")}</p>}
    </div>
  );
}

export interface ReviewRowsProps {
  rows: ReviewStatement[];
  scope: ReviewScope;
  focusId?: string;
  selected: ReadonlySet<string>;
  expanded: ReadonlySet<string>;
  onToggleSelect?: (id: string) => void;
  onFocus?: (id: string) => void;
  /** Rule on one row: on a judge's acceptance awaiting audit, accept
   *  confirms it and reject overturns it. */
  onRuleOne?: (action: "accept" | "reject", s: ReviewStatement) => void;
}

// ReviewRows is the queue as a table: what links, how, how sure, what a judge
// said, and the evidence beside it.
export function ReviewRows({ rows, scope, focusId, selected, expanded, onToggleSelect, onFocus, onRuleOne }: ReviewRowsProps) {
  if (rows.length === 0) return <p className="settings-muted">No proposals here.</p>;
  return (
    <table className="settings-table review-table">
      <thead>
        <tr>
          <th aria-label="select" />
          <th>from</th>
          <th>kind</th>
          <th>to</th>
          <th>method · actor</th>
          <th>confidence</th>
          <th>judge</th>
          <th>evidence</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((s) => {
          const judge = judgeOf(s);
          const audit = auditOf(s);
          const open = expanded.has(s.id);
          const quotes = s.evidence ?? [];
          const more = Math.max(0, (s.evidence_count ?? quotes.length) - quotes.length);
          const fromHref = docHref(s.from_document_id, scope);
          const toHref = docHref(s.to_document_id, scope);
          return (
            <tr
              key={s.id}
              className={`review-row${s.id === focusId ? " focused" : ""}${selected.has(s.id) ? " selected" : ""}`}
              data-statement={s.id}
              onClick={() => onFocus?.(s.id)}
            >
              <td>
                <input
                  type="checkbox"
                  aria-label={`select ${s.from_title ?? s.from_id} ${s.kind} ${s.to_title ?? s.to_id}`}
                  checked={selected.has(s.id)}
                  onChange={() => onToggleSelect?.(s.id)}
                  onClick={(e) => e.stopPropagation()}
                />
              </td>
              <td className="review-end">
                {fromHref ? <Link to={fromHref}>{s.from_title || s.from_id}</Link> : s.from_title || s.from_id}
              </td>
              <td>
                <code>{s.kind}</code>
              </td>
              <td className="review-end">
                {toHref ? <Link to={toHref}>{s.to_title || s.to_id}</Link> : s.to_title || s.to_id}
              </td>
              <td className="review-method">
                {s.method || "—"}
                {s.actor && <span className="settings-muted-inline"> · {s.actor}</span>}
              </td>
              <td>{typeof s.confidence === "number" ? s.confidence.toFixed(2) : "—"}</td>
              <td>
                {judge ? (
                  <span className={`review-judge ${judge.verdict}`} title={judge.rationale}>
                    {judge.verdict}
                    {typeof judge.confidence === "number" && ` ${judge.confidence.toFixed(2)}`}
                  </span>
                ) : (
                  <span className="settings-muted-inline">—</span>
                )}
                {audit && <span className={`review-audit ${audit}`}> audit: {audit}</span>}
                {audit === "pending" && s.status === "asserted" && onRuleOne && (
                  <span className="review-audit-actions">
                    <button
                      type="button"
                      title="The judge was right: keep it, as a person's ruling"
                      onClick={(e) => {
                        e.stopPropagation();
                        onRuleOne("accept", s);
                      }}
                    >
                      Confirm
                    </button>
                    <button
                      type="button"
                      className="danger"
                      title="The judge was wrong: reject it, with a reason"
                      onClick={(e) => {
                        e.stopPropagation();
                        onRuleOne("reject", s);
                      }}
                    >
                      Overturn…
                    </button>
                  </span>
                )}
                {judge?.rationale && open && <div className="review-rationale">{judge.rationale}</div>}
              </td>
              <td className="review-evidence">
                {quotes.length === 0 ? (
                  <span className="settings-muted-inline">none</span>
                ) : (
                  <ul>
                    {quotes.map((q) => (
                      <li key={q.id} title={open ? undefined : q.quote}>
                        &ldquo;{open ? q.quote : clip(q.quote)}&rdquo;
                        {q.stale && <span className="review-stale"> (source changed)</span>}
                      </li>
                    ))}
                    {more > 0 && <li className="settings-muted-inline">+{more} more</li>}
                  </ul>
                )}
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

const CONFIRM_WHY: Record<ConfirmReason, string | undefined> = {
  first: undefined,
  queue_changed: "The queue changed since the last count, so it was counted again.",
  grew: "More proposals matched while this ruling ran than you confirmed. Confirm the new count to go on.",
};

export interface RuleConfirmProps {
  action: "accept" | "reject";
  dry: RuleStatementsResponse;
  why: ConfirmReason;
}

// RuleConfirm is the dry run as the reviewer sees it before anything is ruled:
// how many match, what they are, and what will be skipped.
export function RuleConfirm({ action, dry, why }: RuleConfirmProps) {
  const verb = action === "accept" ? "Accept" : "Reject";
  const chunks = chunkCount(dry.matched);
  const sample = dry.sample ?? [];
  return (
    <div className="review-confirm">
      {CONFIRM_WHY[why] && <p className="review-warn">{CONFIRM_WHY[why]}</p>}
      <p>
        <strong>
          {verb} {dry.matched.toLocaleString("en-US")} statement{dry.matched === 1 ? "" : "s"}
        </strong>
        {chunks > 1 && <> in {chunks} rulings of up to 1,000 each</>}.
      </p>
      {dry.skipped > 0 && (
        <p className="settings-muted">
          {dry.skipped.toLocaleString("en-US")} of the first {dry.selected.toLocaleString("en-US")} are not
          yours to rule and will be skipped.
        </p>
      )}
      {(dry.breakdown ?? []).length > 0 && (
        <ul className="review-breakdown">
          {(dry.breakdown ?? []).map((b) => (
            <li key={`${b.kind}\u0000${b.method}`}>
              <code>{b.kind}</code> by {b.method}: {b.count.toLocaleString("en-US")}
            </li>
          ))}
        </ul>
      )}
      {sample.length > 0 && (
        <>
          <div className="settings-muted">Sample</div>
          <ul className="review-sample">
            {sample.slice(0, CONFIRM_SAMPLE).map((s) => (
              <li key={s.id}>
                {s.from_title || s.from_id} <code>{s.kind}</code> {s.to_title || s.to_id}
                {s.evidence?.[0] && <span className="settings-muted-inline"> &ldquo;{clip(s.evidence[0].quote, 80)}&rdquo;</span>}
              </li>
            ))}
            {sample.length > CONFIRM_SAMPLE && (
              <li className="settings-muted-inline">+{sample.length - CONFIRM_SAMPLE} more in the sample</li>
            )}
          </ul>
        </>
      )}
    </div>
  );
}

export interface ToastState {
  text: string;
  rulingIds: string[];
  scope: ReviewScope;
  scopeId?: string;
  undoing?: boolean;
  undone?: boolean;
}

// UndoToast reports a ruling and offers to undo it; after an undo it reports
// what came back and what had changed since.
export function UndoToast({ toast, onUndo, onClose }: { toast: ToastState; onUndo?: () => void; onClose?: () => void }) {
  return (
    <div className="review-toast" role="status">
      <span>{toast.text}</span>
      {toast.rulingIds.length > 0 && !toast.undone && (
        <button type="button" disabled={toast.undoing} onClick={onUndo}>
          {toast.undoing ? "Undoing…" : "Undo"}
        </button>
      )}
      <button type="button" className="review-toast-close" aria-label="dismiss" onClick={onClose}>
        ×
      </button>
    </div>
  );
}

// ---- the ruling dialog ---------------------------------------------------------

interface RuleTarget {
  label: string;
  selection: Omit<RulingSelection<ReviewFilter>, "action" | "reason">;
  // Ask the reviewer to confirm the dry run (every bulk action). A row ruled
  // from the keyboard skips it.
  confirm: boolean;
}

interface DialogState {
  action: "accept" | "reject";
  target: RuleTarget;
  step: "reason" | "counting" | "confirm" | "ruling";
  reason: string;
  dry?: RuleStatementsResponse;
  why?: ConfirmReason;
  progress?: RulingProgress;
  error?: unknown;
}

function RuleDialog({
  state,
  onReason,
  onStart,
  onConfirm,
  onCancel,
}: {
  state: DialogState;
  onReason: (r: string) => void;
  onStart: () => void;
  onConfirm: (ok: boolean) => void;
  onCancel: () => void;
}) {
  const reject = state.action === "reject";
  const canStart = !reject || state.reason.trim() !== "";
  return (
    <div className="modal-overlay" onClick={state.step === "ruling" ? undefined : onCancel}>
      <div className="modal review-modal" onClick={(e) => e.stopPropagation()}>
        <h3>
          {reject ? "Reject" : "Accept"} {state.target.label}
        </h3>
        {state.step === "reason" && (
          <form
            onSubmit={(e) => {
              e.preventDefault();
              if (canStart) onStart();
            }}
          >
            <label className="modal-field">
              <span>{reject ? "Reason (required)" : "Reason (optional)"}</span>
              <input
                autoFocus
                value={state.reason}
                placeholder={reject ? "why are these wrong?" : ""}
                onChange={(e) => onReason(e.target.value)}
              />
            </label>
            {state.error !== undefined && <RuleError error={state.error} />}
            <div className="modal-buttons">
              <button type="button" onClick={onCancel}>
                Cancel
              </button>
              <button type="submit" className={reject ? "danger" : "primary"} disabled={!canStart}>
                {state.target.confirm ? "Count" : reject ? "Reject" : "Accept"}
              </button>
            </div>
          </form>
        )}
        {state.step === "counting" && <p className="settings-muted">Counting what matches…</p>}
        {state.step === "confirm" && state.dry && state.why && (
          <>
            <RuleConfirm action={state.action} dry={state.dry} why={state.why} />
            <div className="modal-buttons">
              <button type="button" onClick={() => onConfirm(false)}>
                Cancel
              </button>
              <button type="button" className={reject ? "danger" : "primary"} autoFocus onClick={() => onConfirm(true)}>
                {reject ? "Reject" : "Accept"} {state.dry.matched.toLocaleString("en-US")}
              </button>
            </div>
          </>
        )}
        {state.step === "ruling" && (
          <p className="settings-muted">
            {state.progress
              ? `Ruled ${state.progress.ruled.toLocaleString("en-US")} of ${state.progress.total.toLocaleString("en-US")}…`
              : "Ruling…"}
          </p>
        )}
      </div>
    </div>
  );
}

function RuleError({ error }: { error: unknown }) {
  if (noReviewRights(error)) return <ReviewNoRights message={errText(error)} />;
  return <div className="settings-error">{errText(error)}</div>;
}

// ---- the page ------------------------------------------------------------------

interface RowsState {
  rows: ReviewStatement[];
  next?: string;
  loading: boolean;
  error?: string;
}

const FILTER_FIELDS: { key: keyof ReviewFilter; label: string; placeholder?: string }[] = [
  { key: "kind", label: "kind", placeholder: "cites" },
  { key: "method", label: "method (prefix)", placeholder: "rule:" },
  { key: "actor", label: "actor" },
  { key: "from_under_path", label: "from under path", placeholder: "/board" },
  { key: "to_under_path", label: "to under path" },
  { key: "from_document_id", label: "from document" },
  { key: "to_document_id", label: "to document" },
  { key: "judge_verdict", label: "judge verdict", placeholder: "accept" },
  { key: "created_after", label: "created after", placeholder: "2026-10-01T00:00:00Z" },
  { key: "created_before", label: "created before" },
];

function isEditing(t: EventTarget | null): boolean {
  if (!(t instanceof HTMLElement)) return false;
  return t.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(t.tagName);
}

export default function ReviewView() {
  const [params, setParams] = useSearchParams();
  const tenant = useFocusTenant() || undefined;
  const pickedUser = useUserId();
  const principal = usePrincipal();
  const filter = useMemo(() => filtersFromSearch(params), [params]);
  const scope = reviewScopeFromSearch(params);
  const view = params.get("view") === "list" ? "list" : "groups";
  // The subject picker's choice, for a user-scope review of someone else's
  // documents; undefined for the tenant scope and for the viewer's own.
  const scopeId = reviewScopeId(scope, pickedUser || undefined, principal?.subject);
  const filterKey = searchFromFilters(filter).toString();

  const [draft, setDraft] = useState<ReviewFilter>(filter);
  useEffect(() => setDraft(filter), [filterKey]); // eslint-disable-line react-hooks/exhaustive-deps

  const [reload, setReload] = useState(0);
  const [summary, setSummary] = useState<{ groups: ReviewGroup[]; total: number } | null>(null);
  const [loadErr, setLoadErr] = useState<unknown>(undefined);
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());
  const [groupRows, setGroupRows] = useState<Record<string, RowsState>>({});
  const [listRows, setListRows] = useState<RowsState>({ rows: [], loading: false });
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());
  const [focusId, setFocusId] = useState<string | undefined>(undefined);
  const [dialog, setDialog] = useState<DialogState | null>(null);
  const [toast, setToast] = useState<ToastState | null>(null);
  const confirmRef = useRef<((ok: boolean) => void) | null>(null);

  const api = useMemo(
    () => ({
      rule: (req: Parameters<typeof ruleStatements>[0]) => ruleStatements(req, tenant),
    }),
    [tenant],
  );

  // A new filter, scope or ruling: the summary again, and every open group's
  // first page.
  useEffect(() => {
    let cancelled = false;
    setLoadErr(undefined);
    listReviewQueue(filter, { summarize: true, scope, scopeId, tenant })
      .then((r) => {
        if (cancelled) return;
        setSummary({ groups: groupsFromSummary(r.summary, filter), total: r.total ?? 0 });
      })
      .catch((e) => !cancelled && (setSummary(null), setLoadErr(e)));
    return () => {
      cancelled = true;
    };
  }, [filterKey, scope, scopeId, tenant, reload]); // eslint-disable-line react-hooks/exhaustive-deps

  // The scope's earlier rulings, newest first, again after each ruling or undo.
  const [rulings, setRulings] = useState<{ rows: RulingRecord[]; next?: string; error?: string }>({ rows: [] });
  const [undoingId, setUndoingId] = useState<string | undefined>(undefined);
  const loadRulings = useCallback(
    async (after?: string) => {
      try {
        const r = await listRulings({ limit: RULINGS_PAGE, after, scope, scopeId, tenant });
        setRulings((cur) => ({
          rows: [...(after ? cur.rows : []), ...(r.rulings ?? [])],
          next: r.next_cursor || undefined,
        }));
      } catch (e) {
        setRulings((cur) => ({ ...cur, error: errText(e) }));
      }
    },
    [scope, scopeId, tenant],
  );
  useEffect(() => {
    void loadRulings();
  }, [loadRulings, reload]);

  // The judge's agreement report: an operator's, so a member's 403 just
  // leaves it out.
  const [agreement, setAgreement] = useState<JudgeAgreementResponse | null>(null);
  useEffect(() => {
    let cancelled = false;
    getJudgeAgreement({ scope, scopeId, tenant })
      .then((r) => !cancelled && setAgreement(r))
      .catch(() => !cancelled && setAgreement(null));
    return () => {
      cancelled = true;
    };
  }, [scope, scopeId, tenant, reload]);

  const loadRows = useCallback(
    async (f: ReviewFilter, after?: string): Promise<RowsState> => {
      try {
        const r = await listReviewQueue(f, { limit: PAGE, after, scope, scopeId, tenant });
        return { rows: r.statements ?? [], next: r.next_cursor || undefined, loading: false };
      } catch (e) {
        return { rows: [], loading: false, error: errText(e) };
      }
    },
    [scope, scopeId, tenant],
  );

  // A group's first page: when it is opened, and again with each new summary.
  const loadGroup = useCallback(
    (g: ReviewGroup) => {
      if (!g.filter) return;
      setGroupRows((cur) => ({ ...cur, [g.key]: { ...(cur[g.key] ?? { rows: [] }), loading: true } }));
      void loadRows(g.filter).then((st) => setGroupRows((cur) => ({ ...cur, [g.key]: st })));
    },
    [loadRows],
  );
  const openRef = useRef(open);
  openRef.current = open;
  useEffect(() => {
    if (view !== "groups" || !summary) return;
    for (const g of summary.groups) if (openRef.current.has(g.key)) loadGroup(g);
  }, [summary, view, loadGroup]);

  useEffect(() => {
    if (view !== "list") return;
    let cancelled = false;
    setListRows((cur) => ({ ...cur, loading: true }));
    void loadRows(filter).then((st) => !cancelled && setListRows(st));
    return () => {
      cancelled = true;
    };
  }, [view, filterKey, scope, scopeId, tenant, reload, loadRows]); // eslint-disable-line react-hooks/exhaustive-deps

  // The rows on screen, in screen order: what j/k walk.
  const visible = useMemo<ReviewStatement[]>(() => {
    if (view === "list") return listRows.rows;
    return (summary?.groups ?? []).flatMap((g) => (open.has(g.key) ? groupRows[g.key]?.rows ?? [] : []));
  }, [view, listRows, summary, open, groupRows]);

  // A selection outlives a reload only for rows still on screen.
  useEffect(() => {
    const ids = new Set(visible.map((s) => s.id));
    setSelected((cur) => {
      const next = new Set([...cur].filter((id) => ids.has(id)));
      return next.size === cur.size ? cur : next;
    });
  }, [visible]);

  const applyFilter = (f: ReviewFilter) => setParams(searchFromFilters(f, params));
  const setParam = (k: string, v: string | undefined) => {
    const p = new URLSearchParams(params);
    if (v) p.set(k, v);
    else p.delete(k);
    setParams(p);
  };

  // ---- ruling ----

  const startRuling = (action: "accept" | "reject", target: RuleTarget, reason = "") => {
    setDialog({ action, target, step: "reason", reason });
  };

  const execute = async (d: DialogState) => {
    setDialog({ ...d, step: d.target.confirm ? "counting" : "ruling", error: undefined });
    const out = await runRuling(
      api,
      { ...d.target.selection, action: d.action, reason: d.reason.trim() || undefined, scope, scopeId },
      {
        confirm: d.target.confirm
          ? (dry, why) =>
              new Promise<boolean>((resolve) => {
                confirmRef.current = resolve;
                setDialog((cur) => (cur ? { ...cur, step: "confirm", dry, why } : cur));
              })
          : undefined,
        onProgress: (progress) => setDialog((cur) => (cur ? { ...cur, step: "ruling", progress } : cur)),
      },
    );
    confirmRef.current = null;
    if (out.rulingIds.length > 0 || out.error === undefined) {
      setDialog(null);
      if (out.total > 0 || out.rulingIds.length > 0) {
        const failed = out.error !== undefined ? ` Stopped: ${errText(out.error)}` : "";
        setToast({ text: rulingSummary(d.action, out) + failed, rulingIds: out.rulingIds, scope, scopeId });
        setSelected(new Set());
        setReload((n) => n + 1);
      } else if (!out.cancelled) {
        setToast({ text: "Nothing matches to rule.", rulingIds: [], scope });
      }
      return;
    }
    // Nothing was ruled and a call failed: keep the dialog open with the error.
    setDialog({ ...d, step: "reason", error: out.error });
  };

  const undo = async () => {
    if (!toast) return;
    const t = toast;
    setToast({ ...t, undoing: true });
    const u = await undoRulings((id) => revertRuling(id, { scope: t.scope, scopeId: t.scopeId, tenant }), t.rulingIds);
    setToast({ ...t, undoing: false, undone: true, text: undoSummary(u) });
    setReload((n) => n + 1);
  };

  // Undo one earlier ruling from the list.
  const undoListed = async (r: RulingRecord) => {
    setUndoingId(r.id);
    const u = await undoRulings((id) => revertRuling(id, { scope, scopeId, tenant }), [r.id]);
    setUndoingId(undefined);
    setToast({ text: undoSummary(u), rulingIds: [], scope, scopeId });
    setReload((n) => n + 1);
  };

  // ---- keyboard ----

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (dialog) return;
      const index = visible.findIndex((s) => s.id === focusId);
      const act = reviewKey(
        { key: e.key, ctrlKey: e.ctrlKey, metaKey: e.metaKey, altKey: e.altKey, editing: isEditing(e.target) },
        index,
        visible.length,
      );
      if (!act) return;
      e.preventDefault();
      const s = visible[act.index];
      switch (act.type) {
        case "move":
          setFocusId(s.id);
          document.querySelector(`[data-statement="${CSS.escape(s.id)}"]`)?.scrollIntoView({ block: "nearest" });
          return;
        case "expand":
          setExpanded((cur) => {
            const next = new Set(cur);
            if (next.has(s.id)) next.delete(s.id);
            else next.add(s.id);
            return next;
          });
          return;
        case "accept": {
          const d: DialogState = {
            action: "accept",
            target: { label: "1 statement", selection: { ids: [s.id] }, confirm: false },
            step: "ruling",
            reason: "",
          };
          // The next row takes the focus, so a run of a's reviews down the list.
          setFocusId(visible[act.index + 1]?.id ?? visible[act.index - 1]?.id);
          void execute(d);
          return;
        }
        case "reject":
          startRuling("reject", { label: "1 statement", selection: { ids: [s.id] }, confirm: false });
          setFocusId(visible[act.index + 1]?.id ?? visible[act.index - 1]?.id);
          return;
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });

  // ---- render ----

  if (noReviewRights(loadErr)) {
    return (
      <div className="settings-panel review-page">
        <h2>Review</h2>
        <ReviewNoRights message={errText(loadErr)} />
      </div>
    );
  }

  const toggle = (set: ReadonlySet<string>, id: string) => {
    const next = new Set(set);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    return next;
  };
  const rowsProps = {
    scope,
    focusId,
    selected,
    expanded,
    onToggleSelect: (id: string) => setSelected((cur) => toggle(cur, id)),
    onFocus: (id: string) => setFocusId(id),
    onRuleOne: (action: "accept" | "reject", s: ReviewStatement) =>
      startRuling(action, {
        label: `the judge's acceptance ${s.from_title || s.from_id} ${s.kind} ${s.to_title || s.to_id}`,
        selection: { ids: [s.id] },
        confirm: false,
      }),
  };
  const auditing = filter.judge_audit === "pending";
  const pendingAudit = agreement?.total.pending ?? 0;
  const total = summary?.total ?? 0;
  const selectedIds = [...selected];
  // A ruling names at most RULING_CHUNK statements by id; more is a filter's job.
  const tooMany = selectedIds.length > RULING_CHUNK;

  return (
    <div className="settings-panel review-page">
      <h2>Review</h2>
      {scopeId && (
        <p className="review-warn">
          Reviewing the user documents of <code>{scopeId}</code> (the subject picker&rsquo;s choice).
        </p>
      )}
      <p className="settings-help">
        Proposed statements waiting for a ruling. Accept or reject them by row, by group, or every
        one the filter matches; each bulk ruling is counted first and can be undone while nothing has
        changed the statements since. Keys: <kbd>j</kbd>/<kbd>k</kbd> move, <kbd>a</kbd> accept,{" "}
        <kbd>r</kbd> reject, <kbd>e</kbd> evidence.
      </p>

      <form
        className="review-filters"
        onSubmit={(e) => {
          e.preventDefault();
          applyFilter(draft);
        }}
      >
        {FILTER_FIELDS.map((fld) => (
          <label key={fld.key} className="review-filter">
            <span>{fld.label}</span>
            <input
              value={(draft[fld.key] as string | undefined) ?? ""}
              placeholder={fld.placeholder}
              onChange={(e) => setDraft({ ...draft, [fld.key]: e.target.value || undefined })}
            />
          </label>
        ))}
        <label className="review-filter">
          <span>min confidence</span>
          <input
            type="number"
            min={0}
            max={1}
            step={0.05}
            value={draft.min_confidence ?? ""}
            onChange={(e) =>
              setDraft({ ...draft, min_confidence: e.target.value === "" ? undefined : Number(e.target.value) })
            }
          />
        </label>
        <label className="review-filter">
          <span>judged</span>
          <select
            value={draft.has_judge_verdict === undefined ? "" : String(draft.has_judge_verdict)}
            onChange={(e) =>
              setDraft({ ...draft, has_judge_verdict: e.target.value === "" ? undefined : e.target.value === "true" })
            }
          >
            <option value="">any</option>
            <option value="true">judged</option>
            <option value="false">not judged</option>
          </select>
        </label>
        <label className="review-filter">
          <span>judge audit</span>
          <select
            value={draft.judge_audit ?? ""}
            onChange={(e) => {
              const v = e.target.value;
              if (v === "pending" || v === "confirmed" || v === "overturned") setDraft({ ...draft, ...auditFilter(v) });
              else setDraft({ ...draft, judge_audit: undefined, status: undefined });
            }}
          >
            <option value="">any</option>
            <option value="pending">awaiting audit</option>
            <option value="confirmed">confirmed</option>
            <option value="overturned">overturned</option>
          </select>
        </label>
        <label className="review-filter">
          <span>scope</span>
          <select value={scope} onChange={(e) => setParam("scope", e.target.value === "user" ? "user" : undefined)}>
            <option value="tenant">tenant</option>
            <option value="user">my documents</option>
          </select>
        </label>
        <div className="review-filter-buttons">
          <button type="submit">Apply</button>
          <button type="button" onClick={() => applyFilter({})}>
            Clear
          </button>
        </div>
      </form>

      {loadErr !== undefined && <div className="settings-error">{errText(loadErr)}</div>}

      {pendingAudit > 0 && !auditing && (
        <p className="review-audit-chip">
          <button type="button" onClick={() => applyFilter(auditFilter("pending"))}>
            {pendingAudit.toLocaleString("en-US")} of the judge&rsquo;s acceptance{pendingAudit === 1 ? "" : "s"} await
            {pendingAudit === 1 ? "s" : ""} your audit
          </button>
        </p>
      )}
      {auditing && (
        <p className="settings-help">
          The judge&rsquo;s acceptances sampled for audit. Confirm one the judge got right; overturn one it got
          wrong. Each answer counts toward the judge&rsquo;s agreement rate below.
        </p>
      )}

      <div className="review-toolbar">
        <span className="settings-muted">
          {summary ? `${total.toLocaleString("en-US")} proposal${total === 1 ? "" : "s"} match` : "counting…"}
        </span>
        <button
          type="button"
          disabled={total === 0}
          onClick={() => startRuling("accept", { label: "all matching", selection: { filter }, confirm: true })}
        >
          {auditing ? "Confirm all matching" : "Accept all matching"}
        </button>
        <button
          type="button"
          className="danger"
          disabled={total === 0}
          onClick={() => startRuling("reject", { label: "all matching", selection: { filter }, confirm: true })}
        >
          {auditing ? "Overturn all matching…" : "Reject all matching…"}
        </button>
        <span className="review-toolbar-sep" />
        <span className="settings-muted">
          {selectedIds.length} selected
          {tooMany && ` (at most ${RULING_CHUNK.toLocaleString("en-US")} by hand; use a group or all matching)`}
        </span>
        <button
          type="button"
          disabled={selectedIds.length === 0 || tooMany}
          onClick={() =>
            startRuling("accept", { label: `${selectedIds.length} selected`, selection: { ids: selectedIds }, confirm: true })
          }
        >
          Accept selected
        </button>
        <button
          type="button"
          className="danger"
          disabled={selectedIds.length === 0 || tooMany}
          onClick={() =>
            startRuling("reject", { label: `${selectedIds.length} selected`, selection: { ids: selectedIds }, confirm: true })
          }
        >
          Reject selected…
        </button>
        <span className="review-toolbar-sep" />
        <button type="button" aria-pressed={view === "groups"} onClick={() => setParam("view", undefined)}>
          groups
        </button>
        <button type="button" aria-pressed={view === "list"} onClick={() => setParam("view", "list")}>
          list
        </button>
      </div>

      {view === "groups" ? (
        <ReviewGroupList
          groups={summary?.groups ?? []}
          open={open}
          rows={groupRows}
          onToggle={(g) => {
            if (!open.has(g.key)) loadGroup(g);
            setOpen((cur) => toggle(cur, g.key));
          }}
          onMore={(g) => {
            const st = groupRows[g.key];
            if (!st?.next || !g.filter) return;
            void loadRows(g.filter, st.next).then((more) =>
              setGroupRows((cur) => ({
                ...cur,
                [g.key]: { ...more, rows: [...(cur[g.key]?.rows ?? []), ...more.rows] },
              })),
            );
          }}
          onRule={(action, g) =>
            g.filter &&
            startRuling(action, { label: `the group ${groupLabel(g)}`, selection: { filter: g.filter }, confirm: true })
          }
          onSelectAll={(g) =>
            setSelected((cur) => new Set([...cur, ...(groupRows[g.key]?.rows ?? []).map((s) => s.id)]))
          }
          rowsProps={rowsProps}
        />
      ) : (
        <>
          {listRows.error && <div className="settings-error">{listRows.error}</div>}
          {listRows.loading && listRows.rows.length === 0 ? (
            <p className="settings-muted">loading…</p>
          ) : (
            <ReviewRows rows={listRows.rows} {...rowsProps} />
          )}
          {listRows.next && (
            <button
              type="button"
              onClick={() =>
                void loadRows(filter, listRows.next).then((more) =>
                  setListRows((cur) => ({ ...more, rows: [...cur.rows, ...more.rows] })),
                )
              }
            >
              more
            </button>
          )}
        </>
      )}

      <RecentRulings
        rulings={rulings.rows}
        error={rulings.error}
        undoingId={undoingId}
        onUndo={(r) => void undoListed(r)}
        onMore={rulings.next ? () => void loadRulings(rulings.next) : undefined}
      />

      {agreement && <JudgeAgreement report={agreement} onAudit={() => applyFilter(auditFilter("pending"))} />}

      {dialog && (
        <RuleDialog
          state={dialog}
          onReason={(reason) => setDialog({ ...dialog, reason })}
          onStart={() => void execute(dialog)}
          onConfirm={(ok) => {
            if (ok) setDialog({ ...dialog, step: "ruling", progress: undefined });
            confirmRef.current?.(ok);
          }}
          onCancel={() => {
            confirmRef.current?.(false);
            setDialog(null);
          }}
        />
      )}
      {toast && <UndoToast toast={toast} onUndo={() => void undo()} onClose={() => setToast(null)} />}
    </div>
  );
}

// JudgeAgreement is the judge's agreement report: per kind, the judge's
// acceptances, the sample marked for audit, and how often people agreed. Only
// the sample counts toward the rate; an acceptance someone settled unprompted
// was picked because they doubted it.
export function JudgeAgreement({ report, onAudit }: { report: JudgeAgreementResponse; onAudit?: () => void }) {
  return (
    <section className="review-agreement">
      <h3>Judge agreement</h3>
      {report.total.accepted === 0 ? (
        <p className="settings-muted">The judge has accepted nothing in this scope yet.</p>
      ) : (
        <>
          <p className="settings-help">
            How often a person confirmed the judge&rsquo;s acceptances sampled for audit.
            {report.total.pending > 0 && onAudit && (
              <>
                {" "}
                <button type="button" onClick={onAudit}>
                  Audit the {report.total.pending.toLocaleString("en-US")} pending
                </button>
              </>
            )}
          </p>
          <table className="settings-table">
            <thead>
              <tr>
                <th>kind</th>
                <th>accepted</th>
                <th>sampled</th>
                <th>pending</th>
                <th>confirmed</th>
                <th>overturned</th>
                <th>agreement</th>
              </tr>
            </thead>
            <tbody>
              {[...report.kinds, report.total].map((c) => (
                <tr key={c.kind || "(total)"} className={c === report.total ? "review-agreement-total" : undefined}>
                  <td>{c === report.total ? <strong>all kinds</strong> : <code>{c.kind}</code>}</td>
                  <td>{c.accepted}</td>
                  <td>{c.sampled}</td>
                  <td>{c.pending}</td>
                  <td>{c.confirmed}</td>
                  <td>{c.overturned}</td>
                  <td>{percent(c.agreement)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </section>
  );
}

// formatNs renders a unix-nanosecond time. Nanoseconds overflow a JS number's
// exact range, but a millisecond display does not need them.
function formatNs(ns: number | undefined): string {
  if (!ns) return "";
  return new Date(ns / 1e6).toISOString().replace("T", " ").slice(0, 19);
}

// RecentRulings is the scope's earlier bulk rulings and their reverts, newest
// first, each accept or reject with its own Undo until it has been undone.
export function RecentRulings({
  rulings,
  error,
  undoingId,
  onUndo,
  onMore,
}: {
  rulings: RulingRecord[];
  error?: string;
  undoingId?: string;
  onUndo?: (r: RulingRecord) => void;
  onMore?: () => void;
}) {
  return (
    <section className="review-rulings">
      <h3 className="settings-subhead">Recent rulings</h3>
      {error && <div className="settings-error">{error}</div>}
      {rulings.length === 0 ? (
        <p className="settings-muted">No rulings yet.</p>
      ) : (
        <table className="settings-table">
          <thead>
            <tr>
              <th>when</th>
              <th>ruling</th>
              <th>by</th>
              <th>selected</th>
              <th>ruled</th>
              <th>reason</th>
              <th aria-label="undo" />
            </tr>
          </thead>
          <tbody>
            {rulings.map((r) => (
              <tr key={r.id} className={`review-ruling ${r.action}${r.reverted ? " reverted" : ""}`} data-ruling={r.id}>
                <td className="settings-muted-inline">{formatNs(r.created_at)}</td>
                <td>
                  <code>{r.action}</code>
                </td>
                <td>{r.actor || r.ruled_by}</td>
                <td className="review-method">{rulingSelection(r)}</td>
                <td>
                  {r.ruled.toLocaleString("en-US")}
                  {r.failed > 0 && (
                    <span className="settings-muted-inline">
                      {" "}
                      ({r.failed.toLocaleString("en-US")} {r.action === "revert" ? "changed" : "failed"})
                    </span>
                  )}
                </td>
                <td>{r.reason || ""}</td>
                <td>
                  {r.reverted ? (
                    <span className="settings-muted-inline" title={r.reverted_by ? `revert ${r.reverted_by}` : undefined}>
                      undone {formatNs(r.reverted_at)}
                    </span>
                  ) : canUndo(r) ? (
                    <button type="button" disabled={undoingId !== undefined} onClick={() => onUndo?.(r)}>
                      {undoingId === r.id ? "Undoing…" : "Undo"}
                    </button>
                  ) : null}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {onMore && (
        <button type="button" onClick={onMore}>
          more
        </button>
      )}
    </section>
  );
}

// ReviewGroupList is the summary's groups, each folded until opened; an open
// group lists its rows. A group the queue's filters cannot single out has no
// group action and lists nothing of its own: its rows are in the list view.
export function ReviewGroupList({
  groups,
  open,
  rows,
  onToggle,
  onMore,
  onRule,
  onSelectAll,
  rowsProps,
}: {
  groups: ReviewGroup[];
  open: ReadonlySet<string>;
  rows: Record<string, RowsState>;
  onToggle?: (g: ReviewGroup) => void;
  onMore?: (g: ReviewGroup) => void;
  onRule?: (action: "accept" | "reject", g: ReviewGroup) => void;
  onSelectAll?: (g: ReviewGroup) => void;
  rowsProps: Omit<ReviewRowsProps, "rows">;
}) {
  if (groups.length === 0) return <p className="settings-muted">Nothing waiting for review.</p>;
  return (
    <div className="review-groups">
      {groups.map((g) => {
        const isOpen = open.has(g.key);
        const st = rows[g.key];
        return (
          <section key={g.key} className="review-group">
            <div className="review-group-head">
              <button type="button" className="review-group-toggle" aria-expanded={isOpen} onClick={() => onToggle?.(g)}>
                {isOpen ? "▼" : "▶"} <code>{g.kind}</code> by {g.method || "(no method)"} under{" "}
                {g.fromPathPrefix || "(not filed)"}
                {g.judgeVerdict && <span className={`review-judge ${g.judgeVerdict}`}> judge: {g.judgeVerdict}</span>}
              </button>
              <span className="review-group-count">{g.count.toLocaleString("en-US")}</span>
              {g.filter ? (
                <>
                  {isOpen && (st?.rows.length ?? 0) > 0 && (
                    <button type="button" onClick={() => onSelectAll?.(g)}>
                      select shown
                    </button>
                  )}
                  <button type="button" onClick={() => onRule?.("accept", g)}>
                    Accept group
                  </button>
                  <button type="button" className="danger" onClick={() => onRule?.("reject", g)}>
                    Reject group…
                  </button>
                </>
              ) : (
                <span className="settings-muted-inline" title="The server sent no filter that selects this group alone">
                  rule from the list view
                </span>
              )}
            </div>
            {isOpen &&
              (!g.filter ? (
                <p className="settings-muted">
                  The server sent no filter for this group, so it cannot be listed or ruled on its own
                  here. Switch to the list view to rule its rows.
                </p>
              ) : !st || (st.loading && st.rows.length === 0) ? (
                <p className="settings-muted">loading…</p>
              ) : (
                <>
                  {st.error && <div className="settings-error">{st.error}</div>}
                  <ReviewRows rows={st.rows} {...rowsProps} />
                  {st.next && (
                    <button type="button" onClick={() => onMore?.(g)}>
                      more
                    </button>
                  )}
                </>
              ))}
          </section>
        );
      })}
    </div>
  );
}
