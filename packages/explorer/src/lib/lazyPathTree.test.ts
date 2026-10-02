import { describe, it, expect } from "vitest";
import type { PathEntry } from "../types";
import type { ExplorerDataLayer, PathLsPage, PathLsResult } from "./dataLayer";
import {
  LazyPathTree,
  ancestorsOf,
  documentIdsOf,
  listSubtree,
} from "./lazyPathTree";
import { buildLazyTree } from "../components/PathTree";

// fakePathServer mirrors the runtime's Path `ls` (internal/tools/builtin/
// pathtool.go) closely enough to page against: a one-level listing returns the
// explicit direct children plus IMPLICIT directories synthesized from deeper
// descendants, sorted by name; a recursive one returns every descendant flat,
// sorted by full path; both are clipped to `limit` (default 500, clamped to
// maxLimit) and continue from an opaque cursor naming the last emitted key.
interface Dirent {
  path: string; // "/a/b"
  kind: string;
  documentId?: string;
}

interface LsCall {
  path: string;
  recursive: boolean;
  limit?: number;
  cursor?: string;
}

function fakePathServer(dirents: Dirent[], opts: { maxLimit?: number } = {}) {
  const calls: LsCall[] = [];
  const maxLimit = opts.maxLimit ?? 5000;
  const entryOf = (d: Dirent): PathEntry => ({
    name: d.path.slice(d.path.lastIndexOf("/") + 1),
    kind: d.kind,
    full_path: d.path,
    ...(d.documentId ? { resource_ref: { document_id: d.documentId } } : {}),
  });
  const ls = (path: string, recursive: boolean, page?: PathLsPage): PathLsResult => {
    calls.push({ path, recursive, limit: page?.limit, cursor: page?.cursor });
    const prefix = path === "/" ? "/" : `${path}/`;
    const under = dirents.filter((d) => d.path.startsWith(prefix));
    let entries: PathEntry[];
    let keyOf: (e: PathEntry) => string;
    if (recursive) {
      entries = under.map(entryOf).sort((a, b) => (a.full_path < b.full_path ? -1 : 1));
      keyOf = (e) => e.full_path;
    } else {
      const direct = new Map<string, PathEntry>();
      const implicit = new Set<string>();
      for (const d of under) {
        const rest = d.path.slice(prefix.length);
        const i = rest.indexOf("/");
        if (i < 0) direct.set(rest, entryOf(d));
        else implicit.add(rest.slice(0, i));
      }
      for (const name of implicit) {
        if (!direct.has(name)) {
          direct.set(name, { name, kind: "directory", full_path: prefix + name });
        }
      }
      entries = [...direct.values()].sort((a, b) => (a.name < b.name ? -1 : 1));
      keyOf = (e) => e.name;
    }
    if (page?.cursor) {
      const after = atob(page.cursor);
      entries = entries.filter((e) => keyOf(e) > after);
    }
    const limit = Math.min(page?.limit && page.limit > 0 ? page.limit : 500, maxLimit);
    if (entries.length > limit) {
      const pageRows = entries.slice(0, limit);
      return {
        path,
        entries: pageRows,
        truncated: true,
        next_cursor: btoa(keyOf(pageRows[pageRows.length - 1])),
      };
    }
    return { path, entries };
  };
  const data = {
    pathLs: async (path: string, _scope: unknown, recursive: boolean, _browse?: unknown, page?: PathLsPage) =>
      ls(path, recursive, page),
  } as unknown as ExplorerDataLayer;
  return { data, calls, dirents };
}

// The live shape that broke the tree: a user scope whose /facts holds more
// subject-homed fact documents than one page, with top-level directories that
// sort after "facts" — one of them (loomcycle) implicit, with no dirent of its
// own.
function liveShapedScope(): Dirent[] {
  const ds: Dirent[] = [];
  for (let i = 0; i < 12; i++) {
    ds.push({ path: `/documents/doc${String(i).padStart(2, "0")}`, kind: "document", documentId: `d${i}` });
  }
  for (let i = 0; i < 583; i++) {
    ds.push({ path: `/facts/f${String(i).padStart(4, "0")}`, kind: "document", documentId: `f${i}` });
  }
  ds.push({ path: "/loomboard", kind: "directory" });
  ds.push({ path: "/loomboard/board", kind: "document", documentId: "lb" });
  ds.push({ path: "/loomcycle/rfcs/lazy-tree", kind: "document", documentId: "rfc" });
  ds.push({ path: "/loomcycle-internal/plan", kind: "document", documentId: "plan" });
  ds.push({ path: "/memory/note", kind: "memory_entry" });
  return ds;
}

const names = (t: LazyPathTree, dir = "") => (t.getSnapshot().get(dir)?.entries ?? []).map((e) => e.name);

describe("LazyPathTree", () => {
  it("lists every top-level directory even when a sibling holds more than a page", async () => {
    const srv = fakePathServer(liveShapedScope());
    const t = new LazyPathTree(srv.data, "user");
    await t.loadDir("");
    const top = buildLazyTree(t.getSnapshot()).map((n) => `${n.kind}:${n.name}`);
    expect(top).toEqual([
      "directory:documents",
      "directory:facts",
      "directory:loomboard",
      "directory:loomcycle",
      "directory:loomcycle-internal",
      "directory:memory",
    ]);
    // Opening the root lists the root and nothing under it.
    expect(srv.calls).toEqual([{ path: "/", recursive: false, limit: 500, cursor: undefined }]);
  });

  it("shows an implicit directory (no dirent of its own) as a directory", async () => {
    const srv = fakePathServer(liveShapedScope());
    const t = new LazyPathTree(srv.data, "user");
    await t.loadDir("");
    await t.loadDir("/loomcycle");
    const loomcycle = buildLazyTree(t.getSnapshot()).find((n) => n.name === "loomcycle")!;
    expect(loomcycle.children.map((n) => `${n.kind}:${n.fullPath}`)).toEqual([
      "directory:/loomcycle/rfcs",
    ]);
  });

  it("lists a directory only when it is opened, one page at a time", async () => {
    const srv = fakePathServer(liveShapedScope());
    const t = new LazyPathTree(srv.data, "user");
    await t.loadDir("");
    expect(t.getSnapshot().has("/facts")).toBe(false);

    await t.loadDir("/facts");
    const facts = t.getSnapshot().get("/facts")!;
    expect(facts.entries).toHaveLength(500);
    expect(facts.nextCursor).toBeTruthy();
    expect(srv.calls[1]).toEqual({ path: "/facts", recursive: false, limit: 500, cursor: undefined });

    // A second open of a listed directory is served from the cache.
    await t.loadDir("/facts");
    expect(srv.calls).toHaveLength(2);
  });

  it("load more appends the next page through the cursor and drops nothing", async () => {
    const srv = fakePathServer(liveShapedScope());
    const t = new LazyPathTree(srv.data, "user");
    await t.loadDir("/facts");
    const cursor = t.getSnapshot().get("/facts")!.nextCursor;
    await t.loadMore("/facts");
    const facts = t.getSnapshot().get("/facts")!;
    expect(srv.calls[1]).toEqual({ path: "/facts", recursive: false, limit: 500, cursor });
    expect(facts.entries).toHaveLength(583);
    expect(new Set(facts.entries.map((e) => e.name)).size).toBe(583);
    expect(facts.nextCursor).toBeUndefined();
    // Nothing left: another load more makes no request.
    await t.loadMore("/facts");
    expect(srv.calls).toHaveLength(2);
  });

  it("reveal lists a deep link's ancestors and pages to an entry past the first page", async () => {
    const srv = fakePathServer(liveShapedScope());
    const t = new LazyPathTree(srv.data, "user");
    expect(await t.reveal("/facts/f0550")).toBe(true);
    expect(names(t, "/facts")).toContain("f0550");

    expect(await t.reveal("/loomcycle/rfcs/lazy-tree")).toBe(true);
    expect(names(t, "/loomcycle/rfcs")).toEqual(["lazy-tree"]);
    expect([...t.getSnapshot().keys()].sort()).toEqual(["", "/facts", "/loomcycle", "/loomcycle/rfcs"]);
  });

  it("reveal stops paging once a page ends past the wanted name", async () => {
    const srv = fakePathServer(liveShapedScope());
    const t = new LazyPathTree(srv.data, "user");
    // "a-missing" sorts before every fact, so the first page already proves it absent.
    expect(await t.reveal("/facts/a-missing")).toBe(false);
    expect(srv.calls.filter((c) => c.path === "/facts")).toHaveLength(1);
  });

  it("invalidate re-lists the listed ancestors, keeping how much was shown", async () => {
    const srv = fakePathServer(liveShapedScope());
    const t = new LazyPathTree(srv.data, "user");
    await t.loadDir("");
    await t.loadDir("/facts");
    await t.loadMore("/facts");
    srv.dirents.push({ path: "/facts/f9999", kind: "document", documentId: "new" });
    srv.dirents.push({ path: "/zeta/x", kind: "document", documentId: "z" });
    srv.calls.length = 0;

    await t.invalidate("/facts/f9999");
    // Both were shown in full (6 and 583 entries), so each re-list asks for what
    // was shown plus a page of headroom — the new fact must not split /facts.
    expect(srv.calls.map((c) => `${c.path}:${c.limit}`).sort()).toEqual(["/:506", "/facts:1083"]);
    expect(t.getSnapshot().get("/facts")!.entries).toHaveLength(584);
    expect(names(t)).toContain("zeta");
  });

  it("invalidate with dropSubtree forgets a moved directory's cached listings", async () => {
    const srv = fakePathServer(liveShapedScope());
    const t = new LazyPathTree(srv.data, "user");
    await t.reveal("/loomcycle/rfcs/lazy-tree");
    srv.dirents.forEach((d) => {
      if (d.path.startsWith("/loomcycle/")) d.path = d.path.replace("/loomcycle/", "/archive/");
    });
    await t.invalidate("/loomcycle", { dropSubtree: true });
    const keys = [...t.getSnapshot().keys()];
    expect(keys).not.toContain("/loomcycle");
    expect(keys).not.toContain("/loomcycle/rfcs");
    expect(names(t)).toContain("archive");
    expect(names(t)).not.toContain("loomcycle");
  });

  it("drops a page that was in flight when its directory was re-listed", async () => {
    const srv = fakePathServer(liveShapedScope());
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const slow = {
      pathLs: async (...args: Parameters<ExplorerDataLayer["pathLs"]>) => {
        const resp = await srv.data.pathLs(...args);
        if (args[4]?.cursor) await gate; // hold only the load-more page
        return resp;
      },
    } as unknown as ExplorerDataLayer;
    const t = new LazyPathTree(slow, "user");
    await t.loadDir("/facts");
    const more = t.loadMore("/facts");
    await t.invalidate("/facts", { dropSubtree: true });
    release();
    await more;
    expect(t.getSnapshot().has("/facts")).toBe(false);
  });

  it("records a failed listing on the directory and rejects", async () => {
    const data = {
      pathLs: async () => {
        throw new Error("boom");
      },
    } as unknown as ExplorerDataLayer;
    const t = new LazyPathTree(data, "user");
    await expect(t.loadDir("/facts")).rejects.toThrow("boom");
    const l = t.getSnapshot().get("/facts")!;
    expect(l.error).toBe("boom");
    expect(l.loading).toBe(false);
    expect(l.loaded).toBe(false);
  });
});

describe("listSubtree", () => {
  it("follows the cursor to the end of a subtree larger than the runtime's page", async () => {
    // A runtime that clamps every page to 250 entries.
    const srv = fakePathServer(liveShapedScope(), { maxLimit: 250 });
    const all = await listSubtree(srv.data, "/facts", "user");
    expect(all).toHaveLength(583);
    expect(srv.calls.map((c) => c.recursive)).toEqual([true, true, true]);
    expect(documentIdsOf(all)).toHaveLength(583);
  });
});

describe("ancestorsOf", () => {
  it("names every directory that must be listed for a path to show, root first", () => {
    expect(ancestorsOf("/a/b/c")).toEqual(["", "/a", "/a/b"]);
    expect(ancestorsOf("/a")).toEqual([""]);
  });
});
