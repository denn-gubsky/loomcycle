import type { BrowseScope, PathEntry, PathScope } from "../types";
import type { ExplorerDataLayer, PathLsResult } from "./dataLayer";

// The Path tree is loaded one directory at a time, one page at a time.
//
// WHY NOT ONE RECURSIVE LISTING: the runtime pages every `ls` (500 entries by
// default, 5000 at most) and orders a recursive one by full path. Subject-homed
// facts make /facts as large as the tenant's entity count, so a single recursive
// `ls /` filled its whole first page with /documents/* and /facts/* — and every
// top-level directory sorting after "facts" silently vanished from the tree while
// the data sat intact server-side. Following every cursor instead would still
// fetch tens of thousands of dirents to draw a tree whose directories are
// mostly collapsed. So a directory is listed only when it is opened, and a
// directory too large for one page says so with a "load more" row rather than
// dropping its tail.

// LS_PAGE_LIMIT matches the runtime's default page; LS_MAX_LIMIT is its ceiling
// (a larger limit is clamped server-side, so asking for more is pointless).
export const LS_PAGE_LIMIT = 500;
export const LS_MAX_LIMIT = 5000;

// DirListing is one directory's load state. `entries` are in the runtime's page
// order (by name), which is what makes "the next segment is not in a later page"
// decidable in reveal.
export interface DirListing {
  entries: PathEntry[];
  nextCursor?: string; // set while more pages remain
  loaded: boolean; // at least one page has arrived
  loading: boolean;
  error?: string;
}

// DirMap keys a listing by directory path, spelled like PathNode.fullPath:
// "" is the root, then "/a", "/a/b".
export type DirMap = ReadonlyMap<string, DirListing>;

// lsTarget maps a directory key onto the path the runtime lists.
export function lsTarget(dir: string): string {
  return dir === "" ? "/" : dir;
}

// parentPathOf returns the canonical parent of a node path ("" = root).
export function parentPathOf(fullPath: string): string {
  const idx = fullPath.lastIndexOf("/");
  return idx <= 0 ? "" : fullPath.slice(0, idx);
}

// ancestorsOf returns every directory that must be listed for `path` to show,
// root first: "/a/b/c" → ["", "/a", "/a/b"].
export function ancestorsOf(path: string): string[] {
  const out = [""];
  const segs = path.split("/").filter(Boolean);
  let dir = "";
  for (let i = 0; i < segs.length - 1; i++) {
    dir = `${dir}/${segs[i]}`;
    out.push(dir);
  }
  return out;
}

function errMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

function listingOf(resp: PathLsResult, before: PathEntry[]): DirListing {
  return {
    entries: [...before, ...(resp.entries ?? [])],
    nextCursor: resp.next_cursor || undefined,
    loaded: true,
    loading: false,
  };
}

// LazyPathTree is the per-(scope, browse) cache of directory listings. It is a
// plain subscribable store rather than React state so the paging rules can be
// tested without a DOM; PathExplorer reads it through useSyncExternalStore and
// builds a fresh instance whenever the scope or browsed subject changes, which
// is also what discards a previous subject's in-flight responses.
//
// Every method that fetches records a failure on the directory's listing (the
// tree renders it inline) AND rejects, so the caller can report it.
export class LazyPathTree {
  private dirs = new Map<string, DirListing>();
  private snapshot: DirMap = new Map();
  private readonly listeners = new Set<() => void>();
  private readonly inflight = new Map<string, Promise<void>>();
  // epoch bumps whenever a directory is re-listed from its first page or
  // dropped, so a page that was in flight across that point is discarded
  // instead of being appended to a listing it no longer continues.
  private readonly epoch = new Map<string, number>();

  constructor(
    private readonly data: ExplorerDataLayer,
    private readonly scope: PathScope,
    private readonly browse?: BrowseScope,
    private readonly pageLimit: number = LS_PAGE_LIMIT,
  ) {}

  getSnapshot = (): DirMap => this.snapshot;

  subscribe = (fn: () => void): (() => void) => {
    this.listeners.add(fn);
    return () => {
      this.listeners.delete(fn);
    };
  };

  private publish() {
    this.snapshot = new Map(this.dirs);
    this.listeners.forEach((fn) => fn());
  }

  private set(dir: string, listing: DirListing) {
    this.dirs.set(dir, listing);
    this.publish();
  }

  private track(dir: string, p: Promise<void>): Promise<void> {
    const tracked = p.finally(() => {
      if (this.inflight.get(dir) === tracked) this.inflight.delete(dir);
    });
    this.inflight.set(dir, tracked);
    return tracked;
  }

  // loadDir lists a directory's first page unless it is already listed. A
  // request already in flight for it is joined rather than repeated.
  loadDir(dir: string): Promise<void> {
    const running = this.inflight.get(dir);
    if (running) return running;
    if (this.dirs.get(dir)?.loaded) return Promise.resolve();
    return this.fetchFirst(dir, this.pageLimit);
  }

  // reload re-lists a directory from its first page, asking for as many entries
  // as it already showed so a refresh does not fold away pages the operator
  // loaded (up to the runtime's ceiling; past it the "load more" row returns).
  // A directory that was shown in full gets a page of headroom on top, so the
  // entry just created in it does not split it back into "load more".
  reload(dir: string): Promise<void> {
    const prev = this.dirs.get(dir);
    const shown = prev?.entries.length ?? 0;
    const want = prev?.loaded && !prev.nextCursor ? shown + this.pageLimit : shown;
    return this.fetchFirst(dir, Math.min(LS_MAX_LIMIT, Math.max(this.pageLimit, want)));
  }

  private fetchFirst(dir: string, limit: number): Promise<void> {
    const ep = (this.epoch.get(dir) ?? 0) + 1;
    this.epoch.set(dir, ep);
    const prev = this.dirs.get(dir);
    // Keep showing what was there while the new listing is in flight.
    this.set(dir, {
      entries: prev?.entries ?? [],
      nextCursor: prev?.nextCursor,
      loaded: prev?.loaded ?? false,
      loading: true,
    });
    const p = this.data
      .pathLs(lsTarget(dir), this.scope, false, this.browse, { limit })
      .then(
        (resp) => {
          if (this.epoch.get(dir) === ep) this.set(dir, listingOf(resp, []));
        },
        (e: unknown) => {
          if (this.epoch.get(dir) === ep) {
            this.set(dir, {
              entries: prev?.entries ?? [],
              nextCursor: prev?.nextCursor,
              loaded: prev?.loaded ?? false,
              loading: false,
              error: errMessage(e),
            });
          }
          throw e;
        },
      );
    return this.track(dir, p);
  }

  // loadMore appends a directory's next page. A no-op when it has none.
  loadMore(dir: string): Promise<void> {
    const running = this.inflight.get(dir);
    if (running) return running;
    const cur = this.dirs.get(dir);
    if (!cur?.nextCursor) return Promise.resolve();
    const ep = this.epoch.get(dir) ?? 0;
    this.set(dir, { ...cur, loading: true, error: undefined });
    const p = this.data
      .pathLs(lsTarget(dir), this.scope, false, this.browse, {
        limit: this.pageLimit,
        cursor: cur.nextCursor,
      })
      .then(
        (resp) => {
          if (this.epoch.get(dir) === ep) this.set(dir, listingOf(resp, cur.entries));
        },
        (e: unknown) => {
          if (this.epoch.get(dir) === ep) {
            this.set(dir, { ...cur, loading: false, error: errMessage(e) });
          }
          throw e;
        },
      );
    return this.track(dir, p);
  }

  // reveal lists every ancestor of `path`, paging each one until the next path
  // segment appears, so a deep link to an entry on a later page still resolves.
  // Resolves false when the path is not there. Because a page is ordered by
  // name, paging stops as soon as a page ends past the wanted name.
  async reveal(path: string): Promise<boolean> {
    const segs = path.split("/").filter(Boolean);
    let dir = "";
    for (const name of segs) {
      await this.loadDir(dir);
      for (;;) {
        const l = this.dirs.get(dir);
        if (!l) return false;
        if (l.entries.some((e) => e.name === name)) break;
        const last = l.entries[l.entries.length - 1];
        if (!l.nextCursor || (last && last.name > name)) return false;
        const cursor = l.nextCursor;
        await this.loadMore(dir);
        // A page that moved nothing forward (a reload raced it, or a runtime
        // that handed back the same cursor) must not spin this loop.
        if (this.dirs.get(dir)?.nextCursor === cursor) return false;
      }
      dir = `${dir}/${name}`;
    }
    return true;
  }

  // invalidate re-lists what a change at `path` can alter: every ancestor that
  // is already listed — not just the parent, because an implicit directory
  // appears with its first descendant and vanishes with its last. dropSubtree
  // also forgets the listings at/under `path` (a moved or removed directory's
  // old children must not linger under its old name).
  async invalidate(path: string, opts?: { dropSubtree?: boolean }): Promise<void> {
    if (opts?.dropSubtree) {
      for (const k of [...this.dirs.keys()]) {
        if (k === path || k.startsWith(`${path}/`)) {
          this.dirs.delete(k);
          this.epoch.set(k, (this.epoch.get(k) ?? 0) + 1);
          this.inflight.delete(k);
        }
      }
      this.publish();
    }
    const dirs = ancestorsOf(path).filter((d) => this.dirs.has(d));
    await Promise.all(dirs.map((d) => this.reload(d)));
  }

  // refresh re-lists the root and every directory in `keep` that is listed, and
  // forgets the rest (a collapsed directory re-lists when it is next opened).
  async refresh(keep: Iterable<string>): Promise<void> {
    const keepSet = new Set(keep);
    keepSet.add("");
    for (const k of [...this.dirs.keys()]) {
      if (!keepSet.has(k)) {
        this.dirs.delete(k);
        this.epoch.set(k, (this.epoch.get(k) ?? 0) + 1);
        this.inflight.delete(k);
      }
    }
    this.publish();
    const dirs = [...keepSet].filter((d) => d === "" || this.dirs.has(d));
    await Promise.all(dirs.map((d) => this.reload(d)));
  }
}

// listSubtree returns every dirent under `dir`, following the runtime's cursor
// to the end. For the operations that genuinely need the whole subtree (a branch
// delete has to find every Document under it) — never for drawing the tree.
export async function listSubtree(
  data: ExplorerDataLayer,
  dir: string,
  scope: PathScope,
  browse?: BrowseScope,
): Promise<PathEntry[]> {
  const out: PathEntry[] = [];
  let cursor: string | undefined;
  for (;;) {
    const resp = await data.pathLs(lsTarget(dir), scope, true, browse, {
      limit: LS_MAX_LIMIT,
      cursor,
    });
    out.push(...(resp.entries ?? []));
    const next = resp.next_cursor || undefined;
    if (!next || next === cursor) return out;
    cursor = next;
  }
}

// documentIdsOf returns the document_id of every `document` entry.
export function documentIdsOf(entries: Iterable<PathEntry>): string[] {
  const ids: string[] = [];
  for (const e of entries) {
    if (e.kind !== "document") continue;
    const id = (e.resource_ref as { document_id?: string } | undefined)?.document_id;
    if (id) ids.push(id);
  }
  return ids;
}
