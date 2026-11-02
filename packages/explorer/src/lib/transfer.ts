// transfer.ts — the explorer's "copy / move" of a document or a folder's
// documents into another scope (the Document tool's copy_documents and
// move_documents). Pure: the request the dialog sends and the lines it shows,
// kept apart from the component so both are tested without a DOM.

import type { DocScope } from "../types";

export type TransferOp = "copy" | "move";

// TransferRequest is what the dialog asks for. A document is named by id (its
// own path names no folder to list); a folder by its path, every document at or
// under it. `from` → `to` rewrites the names on the way.
export interface TransferRequest {
  op: TransferOp;
  scope: DocScope;
  toScope: DocScope;
  documentId?: string;
  underPath?: string;
  from: string;
  to: string;
  dropDangling?: boolean;
  dryRun: boolean;
  expectCount?: number;
}

export interface TransferRefusal {
  document_id: string;
  code: string;
  detail?: string;
}

export interface TransferDangling {
  statement_id: string;
  kind: string;
  from: string;
  to: string;
  outside: "from" | "to" | "evidence" | string;
}

// TransferResult is the answer of both ops, dry run or not. Fields a call did
// not produce are absent.
export interface TransferResult {
  dry_run: boolean;
  documents: { id: string; new_id?: string; paths: string[]; resumed?: boolean }[];
  counts: Record<string, number>;
  refused: TransferRefusal[];
  refused_count?: number;
  dangling: TransferDangling[];
  dangling_count: number;
  blocked_by?: string;
  dropped?: number;
  moved?: string[];
  to_finish?: string[];
  unfinished?: { document_id: string; code: string; detail?: string }[];
  name_failures?: { document_id: string; path: string; error: string }[];
  body_failures?: number;
  statements_failed?: number;
  statement_error?: string;
}

// transferInput is the Document tool input for a request.
export function transferInput(r: TransferRequest): Record<string, unknown> {
  const input: Record<string, unknown> = {
    op: r.op === "copy" ? "copy_documents" : "move_documents",
    scope: r.scope,
    to_scope: r.toScope,
    dry_run: r.dryRun,
  };
  if (r.documentId) input.document_ids = [r.documentId];
  if (r.underPath) input.under_path = r.underPath;
  const from = trimSlash(r.from);
  const to = trimSlash(r.to);
  if (from && to && from !== to) input.path_map = { [from]: to };
  if (r.op === "move") {
    if (r.dropDangling) input.dangling = "drop";
    if (!r.dryRun) input.expect_count = r.expectCount ?? 0;
  }
  return input;
}

// transferProblem is why a request cannot be sent, or null.
export function transferProblem(r: TransferRequest): string | null {
  const to = r.to.trim();
  if (!to.startsWith("/")) return "the new path must start with /";
  if (r.op === "move" && r.toScope === r.scope) {
    return "a move goes to another scope; to rename within this one, use rename / move";
  }
  if (r.op === "copy" && r.toScope === r.scope && trimSlash(to) === trimSlash(r.from)) {
    return "a copy into the same scope needs a new path";
  }
  return null;
}

// transferSummary is the lines the dialog shows for an answer: what will happen
// (a dry run) or what did (a real call), then what stands in the way.
export function transferSummary(op: TransferOp, res: TransferResult): string[] {
  const c = res.counts ?? {};
  const n = c.documents ?? 0;
  const verb = op === "copy" ? "copied" : "moved";
  const lines: string[] = [];
  const parts = `${c.chunks ?? 0} parts, ${c.revisions ?? 0} revisions, ${c.statements ?? 0} relations, ${c.assets ?? 0} images`;
  if (res.dry_run) {
    lines.push(`${n} ${plural(n, "document")} would be ${verb}: ${parts}.`);
    if (res.to_finish?.length) {
      lines.push(`${res.to_finish.length} ${plural(res.to_finish.length, "document")} left by an earlier interrupted move will be finished.`);
    }
  } else if (op === "move") {
    const m = res.moved?.length ?? 0;
    lines.push(`${m} ${plural(m, "document")} moved: ${parts}.`);
  } else {
    lines.push(`${n} ${plural(n, "document")} copied: ${parts}.`);
  }
  const refused = res.refused_count ?? res.refused?.length ?? 0;
  if (refused > 0) {
    lines.push(`${refused} ${plural(refused, "document")} refused:`);
    for (const r of res.refused ?? []) lines.push(`  ${r.document_id}: ${r.code}${r.detail ? ` — ${r.detail}` : ""}`);
  }
  if (res.dangling_count > 0) {
    if (op === "copy") {
      lines.push(`${res.dangling_count} ${plural(res.dangling_count, "relation")} to documents outside the set stay behind.`);
    } else if (res.dropped !== undefined) {
      lines.push(`${res.dropped} ${plural(res.dropped, "relation")} to documents outside the set were dropped.`);
    } else if (res.blocked_by === "dangling_relations") {
      lines.push(
        `${res.dangling_count} ${plural(res.dangling_count, "relation")} join these documents to ones outside the set. ` +
          "A relation cannot span two scopes: include the other documents, or drop these relations.",
      );
    } else {
      lines.push(`${res.dangling_count} ${plural(res.dangling_count, "relation")} to documents outside the set will be dropped.`);
    }
    for (const d of res.dangling ?? []) lines.push(`  ${d.kind}: ${d.from} → ${d.to} (outside: ${d.outside})`);
  }
  for (const u of res.unfinished ?? []) {
    lines.push(`not finished: ${u.document_id}: ${u.code}${u.detail ? ` — ${u.detail}` : ""}. Run the move again.`);
  }
  for (const f of res.name_failures ?? []) lines.push(`no name ${f.path} for ${f.document_id}: ${f.error}`);
  if (res.body_failures) lines.push(`${res.body_failures} ${plural(res.body_failures, "body", "bodies")} could not be written.`);
  if (res.statements_failed) {
    lines.push(`${res.statements_failed} ${plural(res.statements_failed, "relation")} could not be written${res.statement_error ? `: ${res.statement_error}` : ""}.`);
  }
  return lines;
}

// transferBlocked: the real call would be refused as the dry run stands.
export function transferBlocked(op: TransferOp, res: TransferResult, dropDangling: boolean): boolean {
  if (op === "move" && res.dangling_count > 0 && !dropDangling) return true;
  return (res.counts?.documents ?? 0) === 0 && !(res.to_finish?.length ?? 0);
}

function trimSlash(p: string): string {
  const t = p.trim();
  return t.length > 1 ? t.replace(/\/+$/, "") : t;
}

function plural(n: number, one: string, many = one + "s"): string {
  return n === 1 ? one : many;
}
