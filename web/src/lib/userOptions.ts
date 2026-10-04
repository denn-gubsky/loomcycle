// The pure half of the top bar's user field: what its drop-down lists, and what a
// typed id becomes when applied.
import { filterOptions } from "./comboOptions";

// UserOption is one row of the drop-down. `id: ""` is the "no user" row — the
// blank choice the old <select> offered, which clears the picked user.
export interface UserOption {
  id: string;
  // Activity beside the id: "3 running" while anything is running, else the
  // total. Absent on the "no user" row.
  hint?: string;
}

export const NO_USER: UserOption = { id: "" };

// userOptions turns the GET /v1/_users rows into drop-down rows, in the server's
// order (most recently active first), dropping blanks and duplicates.
export function userOptions(
  users: ReadonlyArray<{ user_id: string; running_count: number; total_count: number }>,
): UserOption[] {
  const seen = new Set<string>();
  const out: UserOption[] = [];
  for (const u of users) {
    if (!u.user_id || seen.has(u.user_id)) continue;
    seen.add(u.user_id);
    out.push({
      id: u.user_id,
      hint:
        u.running_count > 0
          ? `${u.running_count} running`
          : `${u.total_count} ${u.total_count === 1 ? "run" : "runs"}`,
    });
  }
  return out;
}

// filterUsers is the drop-down for what has been typed: "no user" first, then the
// matching known users. An id the list does not know matches no user row — that
// is the manual-entry case, and it is applied by Enter, not by a row.
export function filterUsers(known: ReadonlyArray<UserOption>, query: string): UserOption[] {
  return [NO_USER, ...filterOptions(known, query)];
}

// appliedUserId is what Enter applies for the typed text.
//
// ANY id is accepted, known or not: the list is derived from runs, so a subject
// that has never run anything — or one whose documents an operator needs to
// reach — is absent from it and can only be typed. Blank clears the user.
export function appliedUserId(typed: string): string {
  return typed.trim();
}
