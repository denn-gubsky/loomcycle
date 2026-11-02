import { describe, expect, it } from "vitest";
import {
  transferBlocked,
  transferInput,
  transferProblem,
  transferSummary,
  type TransferRequest,
  type TransferResult,
} from "./transfer";

const base: TransferRequest = {
  op: "copy",
  scope: "user",
  toScope: "tenant",
  underPath: "/proj",
  from: "/proj",
  to: "/proj",
  dryRun: true,
};

function result(over: Partial<TransferResult> = {}): TransferResult {
  return {
    dry_run: true,
    documents: [],
    counts: { documents: 2, chunks: 9, revisions: 4, statements: 1, assets: 0 },
    refused: [],
    dangling: [],
    dangling_count: 0,
    ...over,
  };
}

describe("transferInput", () => {
  it("a folder copy names the subtree and sends no path_map when the path is kept", () => {
    expect(transferInput(base)).toEqual({
      op: "copy_documents",
      scope: "user",
      to_scope: "tenant",
      dry_run: true,
      under_path: "/proj",
    });
  });

  it("a new path becomes a path_map from the selected path", () => {
    expect(transferInput({ ...base, to: "/kb/proj/" }).path_map).toEqual({ "/proj": "/kb/proj" });
  });

  it("trailing slashes are trimmed, a lone / is kept, and a long run of slashes is cheap", () => {
    expect(transferInput({ ...base, from: "/proj///", to: "/kb//" }).path_map).toEqual({ "/proj": "/kb" });
    expect(transferInput({ ...base, from: "/", to: "/kb" }).path_map).toEqual({ "/": "/kb" });
    const many = "/" + "/".repeat(100000) + "x";
    const start = Date.now();
    transferInput({ ...base, from: many, to: "/kb" });
    expect(Date.now() - start).toBeLessThan(1000);
  });

  it("a document is named by id", () => {
    const input = transferInput({ ...base, underPath: undefined, documentId: "d1", from: "/proj/plan", to: "/proj/plan" });
    expect(input.document_ids).toEqual(["d1"]);
    expect(input.under_path).toBeUndefined();
  });

  it("a real move carries the dry run's count and the dangling choice", () => {
    const input = transferInput({ ...base, op: "move", dryRun: false, expectCount: 2, dropDangling: true });
    expect(input).toMatchObject({ op: "move_documents", dry_run: false, expect_count: 2, dangling: "drop" });
  });

  it("a copy never sends move-only fields", () => {
    const input = transferInput({ ...base, dryRun: false, expectCount: 2, dropDangling: true });
    expect(input.expect_count).toBeUndefined();
    expect(input.dangling).toBeUndefined();
  });
});

describe("transferProblem", () => {
  it("refuses what the server would refuse, before sending it", () => {
    expect(transferProblem({ ...base, to: "proj" })).toMatch(/start with \//);
    expect(transferProblem({ ...base, op: "move", toScope: "user" })).toMatch(/another scope/);
    expect(transferProblem({ ...base, toScope: "user" })).toMatch(/new path/);
    expect(transferProblem({ ...base, toScope: "user", to: "/copy" })).toBeNull();
    expect(transferProblem(base)).toBeNull();
  });
});

describe("transferSummary and transferBlocked", () => {
  it("a dry run says what would happen", () => {
    expect(transferSummary("copy", result())[0]).toBe(
      "2 documents would be copied: 9 parts, 4 revisions, 1 relations, 0 images.",
    );
  });

  it("a move cut by a relation is blocked until the relation is dropped", () => {
    const res = result({
      blocked_by: "dangling_relations",
      dangling_count: 1,
      dangling: [{ statement_id: "e1", kind: "related", from: "c1", to: "c9", outside: "to" }],
    });
    expect(transferBlocked("move", res, false)).toBe(true);
    expect(transferBlocked("move", res, true)).toBe(false);
    const lines = transferSummary("move", res);
    expect(lines.some((l) => l.includes("cannot span two scopes"))).toBe(true);
    expect(lines).toContain("  related: c1 → c9 (outside: to)");
  });

  it("a copy is never blocked by a relation; it stays behind", () => {
    const res = result({ dangling_count: 1 });
    expect(transferBlocked("copy", res, false)).toBe(false);
    expect(transferSummary("copy", res)).toContain("1 relation to documents outside the set stay behind.");
  });

  it("nothing to do is blocked, unless an interrupted move is waiting", () => {
    const empty = result({ counts: { documents: 0 } });
    expect(transferBlocked("move", empty, false)).toBe(true);
    expect(transferBlocked("move", { ...empty, to_finish: ["d1"] }, false)).toBe(false);
  });

  it("a real move reports what moved, what was refused and what is unfinished", () => {
    const lines = transferSummary(
      "move",
      result({
        dry_run: false,
        moved: ["d1"],
        refused: [{ document_id: "d2", code: "id_collision" }],
        unfinished: [{ document_id: "d3", code: "verify_failed", detail: "the target holds 2 chunks, the source 3" }],
      }),
    );
    expect(lines[0]).toMatch(/^1 document moved/);
    expect(lines).toContain("  d2: id_collision");
    expect(lines.some((l) => l.startsWith("not finished: d3: verify_failed"))).toBe(true);
  });
});
