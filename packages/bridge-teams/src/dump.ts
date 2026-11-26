// What the bridge logs about an inbound activity, to see what Teams really
// sent: one line for a message whose attachments held no file (always), and
// with TEAMS_BRIDGE_DUMP_ACTIVITIES=1 the whole activity, redacted.
//
// The dump exists because a file a person attaches can reach the bot as
// something filesOf does not read (a live tenant sent only an empty
// text/html attachment), and the Teams SDK's own debug log prints the body
// unredacted, downloadUrl included. A downloadUrl's query is a credential
// while it lives, so the dump keeps every URL's host and path and drops its
// query and fragment.

import type { TeamsAttachment } from "./voice.js";

/** Strings longer than this are cut, so one pasted document does not fill
 *  the log. */
export const DUMP_MAX_STRING = 2000;

const SECRET_KEY = /token|secret|password|authorization|tempauth|signature/i;
const MAX_DEPTH = 24;

/** redactActivity is a copy of an activity safe to log: every URL without
 *  its query or fragment, every field named like a credential replaced,
 *  long strings cut. Everything else (text, names, ids) is kept: the dump is
 *  an operator's opt-in to see the activity as it came. */
export function redactActivity(value: unknown, depth = 0): unknown {
  if (depth > MAX_DEPTH) return "[too deep]";
  if (typeof value === "string") return redactString(value);
  if (Array.isArray(value)) return value.map((v) => redactActivity(v, depth + 1));
  if (value && typeof value === "object") {
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value)) {
      out[k] = SECRET_KEY.test(k) && v !== null && v !== undefined && v !== "" ? "[redacted]" : redactActivity(v, depth + 1);
    }
    return out;
  }
  return value;
}

function redactString(s: string): string {
  const url = asURL(s);
  if (url && (url.search || url.hash || url.username || url.password)) {
    return `${url.protocol}//${url.host}${url.pathname}${url.search ? "?[redacted]" : ""}${url.hash ? "#[redacted]" : ""}`;
  }
  if (s.length > DUMP_MAX_STRING) return `${s.slice(0, DUMP_MAX_STRING)}…(+${s.length - DUMP_MAX_STRING} chars)`;
  return s;
}

function asURL(s: string): URL | undefined {
  if (!/^[a-z][a-z0-9+.-]*:\/\//i.test(s.trim())) return undefined;
  try {
    return new URL(s.trim());
  } catch {
    return undefined;
  }
}

/** The fields of an activity the no-file line and the dump read. */
export interface DumpedActivity {
  type?: string;
  name?: string;
  id?: string;
  conversation?: { id?: string; conversationType?: string };
  attachments?: readonly TeamsAttachment[];
}

/** attachmentsSummary is what may always be logged of an activity's
 *  attachments: how many, and their content types. Never a name, a URL or
 *  content. */
export function attachmentsSummary(attachments: readonly TeamsAttachment[] | undefined): { attachments: number; contentTypes: string[] } {
  const list = attachments ?? [];
  return { attachments: list.length, contentTypes: [...new Set(list.map((a) => a.contentType ?? "(none)"))] };
}

/** dumpFields is one log line's fields for an activity dump. */
export function dumpFields(activity: DumpedActivity): Record<string, unknown> {
  return {
    type: activity.type,
    ...(activity.name ? { name: activity.name } : {}),
    conversationType: activity.conversation?.conversationType,
    ...attachmentsSummary(activity.attachments),
    activity: redactActivity(activity),
  };
}
