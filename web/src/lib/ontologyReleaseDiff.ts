import type {
  DiffOntologyReleasesResult,
  OntologyDiffSide,
  OntologyEntryDiff,
  OntologyEntryState,
  OntologyRelease,
} from "../api";
import { ApiError } from "./apiError";

// The ontology page's Releases tab (RFC GQ P3.7): how a release and a diff
// between two of them read. Pure, so the wording an operator decides from is
// tested without mounting the page.

/** What took the release, in the operator's words. */
export function releaseReasonLabel(reason: string): string {
  switch (reason) {
    case "confirm":
      return "confirmed";
    case "apply":
      return "change applied";
    case "rollback":
      return "rolled back";
    case "catch_up":
      return "catch-up";
    default:
      return reason || "unknown";
  }
}

/** created_at (unix nanoseconds) as "YYYY-MM-DD HH:MM UTC". The value is past a
 *  JS number's exact range, so it is display only; minute precision survives. */
export function releaseTime(ns: number): string {
  if (!Number.isFinite(ns) || ns <= 0) return "—";
  const iso = new Date(Math.floor(ns / 1e6)).toISOString();
  return `${iso.slice(0, 10)} ${iso.slice(11, 16)} UTC`;
}

/** One side of a diff, named. */
export function diffSideLabel(side: OntologyDiffSide): string {
  return side.current ? "the ontology in force" : `release ${side.seq ?? "?"}`;
}

/** The release before seq in a newest-first list, or undefined for the oldest kept. */
export function previousRelease(releases: OntologyRelease[], seq: number): OntologyRelease | undefined {
  return releases.filter((r) => r.seq < seq).sort((a, b) => b.seq - a.seq)[0];
}

/** One field of an entry that differs: its label and its value on each side
 *  ("" where the side has none). */
export interface DiffLine {
  field: string;
  before: string;
  after: string;
}

/** One entry of the diff as the tab shows it. */
export interface DiffRow {
  chunkId: string;
  kind: string;
  change: OntologyEntryDiff["change"];
  /** The entry's name on the To side, or the From side for a removed one. */
  name: string;
  /** What differs, before and after. An added entry lists what it declares
   *  (before ""); a removed one what it declared (after ""). */
  lines: DiffLine[];
}

const IN_FORCE = "in force";
const TOP = "(top of its section)";

function parentText(s: OntologyEntryState): string {
  if (s.parent_name) return s.parent_name;
  return s.parent ? s.parent : TOP;
}

function statusText(s: OntologyEntryState): string {
  return s.status ? s.status : IN_FORCE;
}

// canonical is a value's JSON with object keys sorted, so two directive values
// that differ only in key order compare equal.
function canonical(v: unknown): string {
  if (v === undefined) return "";
  if (v === null || typeof v !== "object") return JSON.stringify(v);
  if (Array.isArray(v)) return `[${v.map(canonical).join(",")}]`;
  const o = v as Record<string, unknown>;
  return `{${Object.keys(o)
    .sort()
    .map((k) => `${JSON.stringify(k)}:${canonical(o[k])}`)
    .join(",")}}`;
}

function directiveValue(v: unknown): string {
  if (v === undefined) return "";
  if (typeof v === "string") return v;
  if (v === true) return "yes";
  if (Array.isArray(v) && v.every((x) => typeof x === "string")) return v.join(", ");
  return canonical(v);
}

/** The directives that differ, one line per key (`@key`), keys sorted. A key
 *  only one side has reads "" on the other. */
export function directiveLines(
  before: Record<string, unknown> | null | undefined,
  after: Record<string, unknown> | null | undefined,
): DiffLine[] {
  const b = before ?? {};
  const a = after ?? {};
  const keys = Array.from(new Set([...Object.keys(b), ...Object.keys(a)])).sort();
  return keys
    .filter((k) => canonical(b[k]) !== canonical(a[k]))
    .map((k) => ({ field: `@${k}`, before: directiveValue(b[k]), after: directiveValue(a[k]) }));
}

// declared lists what one side of an added or removed entry declares, so the
// operator sees the whole entry rather than only its name.
function declared(s: OntologyEntryState, side: "before" | "after"): DiffLine[] {
  const put = (field: string, value: string): DiffLine =>
    side === "after" ? { field, before: "", after: value } : { field, before: value, after: "" };
  const lines: DiffLine[] = [put("name", s.name)];
  if (s.parent || s.parent_name) lines.push(put("under", parentText(s)));
  if (s.status) lines.push(put("status", s.status));
  if (s.meaning) lines.push(put("meaning", s.meaning));
  const dirs = side === "after" ? directiveLines(undefined, s.directives) : directiveLines(s.directives, undefined);
  return lines.concat(dirs);
}

/** diffRows turns the server's diff into what the tab renders: per entry, the
 *  fields that changed with their before and after. Only the fields the
 *  server lists in `changed` are shown for a changed entry, so an unchanged
 *  meaning is not repeated on a rename. */
export function diffRows(d: DiffOntologyReleasesResult): DiffRow[] {
  return (d.changes ?? []).map((c) => {
    const name = c.after?.name ?? c.before?.name ?? c.chunk_id;
    const row: DiffRow = { chunkId: c.chunk_id, kind: c.kind, change: c.change, name, lines: [] };
    if (c.change === "added" && c.after) {
      row.lines = declared(c.after, "after");
      return row;
    }
    if (c.change === "removed" && c.before) {
      row.lines = declared(c.before, "before");
      return row;
    }
    const b = c.before;
    const a = c.after;
    if (!b || !a) return row;
    for (const what of c.changed ?? []) {
      switch (what) {
        case "renamed":
          row.lines.push({ field: "name", before: b.name, after: a.name });
          break;
        case "moved":
          row.lines.push({ field: "under", before: parentText(b), after: parentText(a) });
          break;
        case "kind":
          // The diff carries the To side's kind only.
          row.lines.push({ field: "kind", before: "another kind", after: c.kind });
          break;
        case "status":
          row.lines.push({ field: "status", before: statusText(b), after: statusText(a) });
          break;
        case "meaning":
          row.lines.push({ field: "meaning", before: b.meaning ?? "", after: a.meaning ?? "" });
          break;
        case "directives":
          row.lines.push(...directiveLines(b.directives, a.directives));
          break;
      }
    }
    return row;
  });
}

/** "2 changed, 1 added, 0 removed", or "no differences". */
export function diffSummary(d: DiffOntologyReleasesResult): string {
  const cs = d.changes ?? [];
  if (cs.length === 0) return "no differences";
  const n = (k: OntologyEntryDiff["change"]) => cs.filter((c) => c.change === k).length;
  const erased = n("erased");
  const base = `${n("changed")} changed, ${n("added")} added, ${n("removed")} removed`;
  return erased > 0 ? `${base}, ${erased} erased` : base;
}

/** Whether an error is the server refusing the caller the releases (403): an
 *  operator's read, refused to a member and to an operator held to the run plane. */
export function releasesRefused(e: unknown): boolean {
  return e instanceof ApiError && e.status === 403;
}

/** The server's own sentence from a refusal: an api.ts error reads
 *  "403 Forbidden: <body>", and the body is JSON with an `error` field. */
export function refusalText(e: unknown): string {
  if (!(e instanceof Error)) return "";
  const body = e.message.replace(/^\d{3} [^:]*: /, "");
  try {
    const parsed: unknown = JSON.parse(body);
    if (parsed && typeof parsed === "object" && typeof (parsed as { error?: unknown }).error === "string") {
      return (parsed as { error: string }).error;
    }
  } catch {
    // not JSON: the body text is the sentence
  }
  return body;
}
