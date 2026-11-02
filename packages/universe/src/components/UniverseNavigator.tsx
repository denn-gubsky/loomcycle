import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { LoomcycleClient } from "@loomcycle/client";
import { createLoomcycleClient, type Connection } from "../lib/createClient";
import { dataLayerFromClient, type UniverseDataLayer } from "../lib/dataLayer";
import { addPage, emptyModel, type UniverseModel } from "../lib/model";
import { computeLayout, type Layout } from "../lib/layout";
import { projectionFrame, toFrame } from "../lib/projection";
import { adjacencyOf, neighbourhood } from "../lib/lod";
import { chunkRgb, kindHex, rgbHex } from "../lib/colors";
import { UniverseScene, type UniverseMode } from "../render/scene";
import SearchPanel, { type ResultRow } from "./SearchPanel";
import type { BrowseScope, DocScope, UniverseSelection } from "../types";

// UniverseNavigator is the star map as a navigator: it loads a scope's
// snapshot page by page, draws it, and reports the document (and chunk) the
// reader picks. It is what @loomcycle/explorer's `renderNavigator` slot hosts,
// and it also stands alone (<UniverseView>). It reads nothing and writes
// nothing beyond the snapshot and search ops.

export interface UniverseDataSource {
  connection?: Connection;
  client?: LoomcycleClient;
  dataLayer?: UniverseDataLayer;
}

export interface UniverseNavigatorProps extends UniverseDataSource {
  scope: DocScope;
  browse?: BrowseScope;
  /** Only this subtree (a Path directory), e.g. "/loomcycle". Default: the scope. */
  underPath?: string;
  /** The host's current document / chunk; the map follows it (two-way sync). */
  selection?: UniverseSelection;
  /** Called when the reader picks a star: a document, optionally at a chunk. */
  onSelect?: (sel: UniverseSelection & { documentId: string }) => void;
  /** Bump to reload (after a create, rename or delete in the host). */
  reloadToken?: number;
  /** When provided, a maximize toggle is shown and drives this. */
  maximized?: boolean;
  onToggleMaximize?: () => void;
  /** The signed-in subject, for memory search at user scope when `browse`
   *  names no other subject. */
  subject?: string;
}

const PAGE_DOCUMENTS = 500;
const PUBLISH_EVERY_MS = 1500;
const ANIMATE_KEY = "loomcycle.universe.animate";

function readAnimate(): boolean {
  try {
    return localStorage.getItem(ANIMATE_KEY) !== "0";
  } catch {
    return true;
  }
}

export function useUniverseDataLayer(src: UniverseDataSource): UniverseDataLayer | null {
  const { dataLayer, client, connection } = src;
  return useMemo<UniverseDataLayer | null>(() => {
    if (dataLayer) return dataLayer;
    if (client) return dataLayerFromClient(client);
    if (connection) return dataLayerFromClient(createLoomcycleClient(connection));
    return null;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dataLayer, client, connection?.baseUrl, connection?.token, connection?.fetch]);
}

interface Hover {
  star: number;
  x: number;
  y: number;
}

export default function UniverseNavigator(props: UniverseNavigatorProps) {
  const { scope, browse, underPath, selection, onSelect, reloadToken, maximized, onToggleMaximize, subject } = props;
  const data = useUniverseDataLayer(props);
  const stageRef = useRef<HTMLDivElement | null>(null);
  const sceneRef = useRef<UniverseScene | null>(null);
  const modelRef = useRef<UniverseModel>(emptyModel());
  const [version, setVersion] = useState(0);
  const [loading, setLoading] = useState<{ docs: number; done: boolean; error?: string }>({ docs: 0, done: false });
  const [glError, setGlError] = useState<string | null>(null);
  const [selected, setSelected] = useState<number[]>([]);
  const [results, setResults] = useState<ResultRow[]>([]);
  const [checked, setChecked] = useState<ReadonlySet<number>>(() => new Set());
  const [depth, setDepth] = useState(1);
  const [dimOthers, setDimOthers] = useState(true);
  const [animate, setAnimate] = useState(readAnimate);
  const [hiddenKinds, setHiddenKinds] = useState<ReadonlySet<string>>(() => new Set());
  const [hover, setHover] = useState<Hover | null>(null);
  // The view: stars at their place in the Path tree, or in the embedding space.
  const [viewMode, setViewMode] = useState<UniverseMode>("structure");
  const [planar, setPlanar] = useState(false);
  const [queryXyz, setQueryXyz] = useState<[number, number, number] | null>(null);
  // Bumped once when a projection refit lands between two pages of a load.
  const [refitReload, setRefitReload] = useState(0);
  // Server-side filters (the snapshot's `types` / `statuses`); the choices are
  // what the unfiltered sky held, so narrowing never empties the menus.
  const [typeFilter, setTypeFilter] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [choices, setChoices] = useState<{ types: string[]; statuses: string[] }>({ types: [], statuses: [] });
  // What we last reported, so the host echoing it back is not a new navigation.
  const emitted = useRef<string>("");
  const pendingHost = useRef<UniverseSelection | undefined>(undefined);

  const browseKey = `${browse?.scopeId ?? ""}|${browse?.tenant ?? ""}`;

  // ---- the scene ----
  const onSelectRef = useRef(onSelect);
  onSelectRef.current = onSelect;
  const emit = useCallback((c: number) => {
    const m = modelRef.current;
    if (c < 0 || c >= m.chunkCount) return;
    const di = m.chunk.doc[c];
    const sel = {
      documentId: m.doc.id[di],
      chunkId: m.doc.root[di] === c ? undefined : m.chunk.id[c],
      path: m.doc.paths[di]?.[0],
    };
    emitted.current = `${sel.documentId}|${sel.chunkId ?? ""}`;
    onSelectRef.current?.(sel);
  }, []);

  useEffect(() => {
    const el = stageRef.current;
    if (!el) return;
    let scene: UniverseScene;
    try {
      scene = new UniverseScene(el, {
        onHover: (star, x, y) => setHover(star >= 0 ? { star, x, y } : null),
        onPick: (star, additive) => {
          const m = modelRef.current;
          if (star >= m.chunkCount) {
            sceneRef.current?.flyTo(star);
            return;
          }
          setSelected((cur) => {
            if (!additive) return [star];
            return cur.includes(star) ? cur.filter((s) => s !== star) : [...cur, star];
          });
          if (!additive) emit(star);
        },
        onDoublePick: (star) => sceneRef.current?.flyTo(star),
      });
    } catch (e) {
      setGlError(e instanceof Error ? e.message : String(e));
      return;
    }
    sceneRef.current = scene;
    // The host's theme can change under us (a data-theme flip on <html>).
    const mo = new MutationObserver(() => scene.refreshTheme());
    mo.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme", "class"] });
    const mq = window.matchMedia?.("(prefers-color-scheme: light)");
    const onMq = () => scene.refreshTheme();
    mq?.addEventListener("change", onMq);
    return () => {
      mo.disconnect();
      mq?.removeEventListener("change", onMq);
      scene.dispose();
      sceneRef.current = null;
    };
  }, [emit]);

  useEffect(() => {
    sceneRef.current?.setAnimate(animate);
    try {
      localStorage.setItem(ANIMATE_KEY, animate ? "1" : "0");
    } catch {
      // storage blocked: the toggle still works for this page
    }
  }, [animate]);

  // ---- loading, page by page ----
  useEffect(() => {
    if (!data) return;
    const ac = new AbortController();
    const m = emptyModel();
    modelRef.current = m;
    setSelected([]);
    setResults([]);
    setChecked(new Set());
    setVersion((v) => v + 1);
    setLoading({ docs: 0, done: false });
    let lastPublish = 0;
    (async () => {
      let cursor: string | undefined;
      for (;;) {
        const page = await data.graphSnapshot(
          {
            scope,
            include: ["documents", "chunks", "edges", "dirents", "facts", "projection"],
            page_documents: PAGE_DOCUMENTS,
            under_path: underPath,
            types: typeFilter ? [typeFilter] : undefined,
            statuses: statusFilter ? [statusFilter] : undefined,
            after: cursor,
          },
          browse,
          ac.signal,
        );
        if (ac.signal.aborted) return;
        addPage(m, page);
        cursor = page.page?.next_cursor;
        const now = performance.now();
        // Publish the first page at once, then at most every 1.5 s: each publish
        // re-lays the sky, so a large scope settles in a few steps, not one per page.
        if (!cursor || lastPublish === 0 || now - lastPublish > PUBLISH_EVERY_MS) {
          lastPublish = now;
          setVersion((v) => v + 1);
        }
        setLoading({ docs: m.docCount, done: !cursor });
        if (!cursor && m.projection.mixedBasis) {
          // A refit moved the frame mid-load: load once more in one frame.
          setRefitReload((n) => (n < 1 ? n + 1 : n));
        }
        if (!cursor) {
          if (!typeFilter && !statusFilter)
            setChoices({
              types: [...new Set(m.doc.type)].filter(Boolean).sort(),
              statuses: [...new Set(m.doc.status)].filter(Boolean).sort(),
            });
          return;
        }
      }
    })().catch((e) => {
      if (ac.signal.aborted) return;
      setLoading((l) => ({ ...l, done: true, error: e instanceof Error ? e.message : String(e) }));
    });
    return () => ac.abort();
    // browse by its primitives (browseKey) so an inline object does not reload.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [data, scope, browseKey, underPath, reloadToken, typeFilter, statusFilter, refitReload]);

  const layout = useMemo<Layout>(() => computeLayout(modelRef.current), [version]);
  const adjacency = useMemo(() => adjacencyOf(modelRef.current), [version]);
  const projected = modelRef.current.projection.placed > 0;
  const frame = useMemo(
    () => (modelRef.current.projection.placed > 0 ? projectionFrame(modelRef.current, planar) : null),
    // version: the model mutates in place between pages.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [version, planar],
  );

  useEffect(() => {
    sceneRef.current?.setData(modelRef.current, layout, frame);
  }, [layout, frame]);

  // No projection (not built, or a runtime without one): the meaning view has
  // nowhere to put the stars, so the map stays in structure.
  const effectiveMode: UniverseMode = projected ? viewMode : "structure";
  useEffect(() => {
    sceneRef.current?.setMode(effectiveMode);
  }, [effectiveMode, layout]);

  useEffect(() => {
    sceneRef.current?.setMarker(effectiveMode === "meaning" && frame && queryXyz ? toFrame(frame, queryXyz) : null);
  }, [effectiveMode, frame, queryXyz]);

  // ---- the host's selection → the map ----
  const resolveHost = useCallback((sel: UniverseSelection | undefined): number => {
    const m = modelRef.current;
    if (!sel?.documentId) return -1;
    if (sel.chunkId) {
      const c = m.chunkIndex.get(sel.chunkId);
      if (c !== undefined) return c;
    }
    const di = m.docIndex.get(sel.documentId);
    return di === undefined ? -1 : m.doc.root[di];
  }, []);

  useEffect(() => {
    const key = selection?.documentId ? `${selection.documentId}|${selection.chunkId ?? ""}` : "";
    if (key === emitted.current) return;
    emitted.current = key;
    if (!key) return;
    const c = resolveHost(selection);
    if (c < 0) {
      pendingHost.current = selection;
      return;
    }
    pendingHost.current = undefined;
    setSelected([c]);
    if (!sceneRef.current?.isInView(c)) sceneRef.current?.flyTo(c);
  }, [selection?.documentId, selection?.chunkId, resolveHost, selection]);

  // A host selection that named a document not loaded yet lands when it loads.
  useEffect(() => {
    if (!pendingHost.current) return;
    const c = resolveHost(pendingHost.current);
    if (c < 0) return;
    pendingHost.current = undefined;
    setSelected([c]);
    sceneRef.current?.flyTo(c);
  }, [version, resolveHost]);

  // ---- selection, search hits → the scene ----
  const focusStars = useMemo(() => {
    const seeds = [...new Set([...selected, ...checked])];
    if (seeds.length === 0) return { edges: [], chunks: new Set<number>(), seeds };
    const kinds = hiddenKinds.size ? new Set(modelRef.current.kinds.filter((k) => !hiddenKinds.has(k))) : undefined;
    const n = neighbourhood(modelRef.current, adjacency, seeds, depth, kinds);
    return { ...n, seeds };
    // version: the model mutates in place between pages.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selected, checked, depth, adjacency, hiddenKinds, version]);

  useEffect(() => {
    const s = sceneRef.current;
    if (!s) return;
    const m = modelRef.current;
    let focus: Set<number> | null = null;
    if (dimOthers && focusStars.seeds.length > 0) {
      focus = new Set(focusStars.chunks);
      // Keep the whole constellation of every endpoint bright, not just the chunk.
      for (const c of [...focusStars.chunks]) {
        const di = m.chunk.doc[c];
        focus.add(m.doc.root[di]);
      }
    }
    s.setSelection(focusStars.seeds, focusStars.edges, focus);
  }, [focusStars, dimOthers, layout]);

  useEffect(() => {
    sceneRef.current?.setHits(results.map((r) => r.chunk).filter((c) => c >= 0));
  }, [results, layout]);

  // ---- toolbar actions ----
  const frameSelection = () => {
    const stars = focusStars.seeds.length ? focusStars.seeds : results.map((r) => r.chunk).filter((c) => c >= 0);
    sceneRef.current?.frame(stars);
  };

  const pickResult = (r: ResultRow) => {
    if (r.chunk < 0) return;
    setSelected([r.chunk]);
    sceneRef.current?.flyTo(r.chunk);
    emit(r.chunk);
  };

  const m = modelRef.current;
  const linkKinds = useMemo(() => {
    const counts = new Map<string, number>();
    for (const e of focusStars.edges) counts.set(e.kind, (counts.get(e.kind) ?? 0) + 1);
    return [...counts.entries()].sort((a, b) => b[1] - a[1]);
  }, [focusStars.edges]);

  return (
    <div className="loomcycle-universe">
      <div className="lu-toolbar">
        <div className="lu-switch" role="group" aria-label="View">
          <button
            type="button"
            aria-pressed={effectiveMode === "structure"}
            className={effectiveMode === "structure" ? "active" : undefined}
            onClick={() => setViewMode("structure")}
            title="Stars where the Path tree files them"
          >
            structure
          </button>
          <button
            type="button"
            aria-pressed={effectiveMode === "meaning"}
            className={effectiveMode === "meaning" ? "active" : undefined}
            disabled={!projected}
            onClick={() => setViewMode("meaning")}
            title={
              projected
                ? "Stars where their content places them (the embedding projection)"
                : "No projection yet: the runtime builds it with POST /v1/_document/project or its projection sweeper"
            }
          >
            meaning
          </button>
        </div>
        {effectiveMode === "meaning" && (
          <label className="lu-field" title="Drop the third component: a flat map of the first two">
            <input type="checkbox" checked={planar} onChange={(e) => setPlanar(e.target.checked)} />
            planar
          </label>
        )}
        <button type="button" onClick={() => sceneRef.current?.home()} title="Show the whole sky">
          ⌂
        </button>
        <button type="button" onClick={frameSelection} title="Frame the selection (or the search results)">
          ⤢
        </button>
        <label className="lu-field" title="How many hops of links to draw around the selection">
          depth
          <select value={depth} onChange={(e) => setDepth(Number(e.target.value))}>
            <option value={1}>1</option>
            <option value={2}>2</option>
            <option value={3}>3</option>
          </select>
        </label>
        {(choices.types.length > 1 || typeFilter) && (
          <select value={typeFilter} onChange={(e) => setTypeFilter(e.target.value)} title="Only documents of this type">
            <option value="">all types</option>
            {choices.types.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
        )}
        {(choices.statuses.length > 1 || statusFilter) && (
          <select value={statusFilter} onChange={(e) => setStatusFilter(e.target.value)} title="Only documents in this status">
            <option value="">all statuses</option>
            {choices.statuses.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
        )}
        <label className="lu-field" title="Dim stars outside the selection's neighbourhood">
          <input type="checkbox" checked={dimOthers} onChange={(e) => setDimOthers(e.target.checked)} />
          focus
        </label>
        <label className="lu-field" title="Twinkle, rays and link flow (frozen when the system asks for reduced motion)">
          <input type="checkbox" checked={animate} onChange={(e) => setAnimate(e.target.checked)} />
          animate
        </label>
        <span className="lu-status">
          {loading.error
            ? "load failed"
            : loading.done
              ? `${m.docCount} documents · ${m.chunkCount} chunks${m.edgesTruncated ? " · links capped" : ""}${
                  effectiveMode === "meaning" ? ` · ${m.projection.placed} placed` : ""
                }`
              : `loading… ${loading.docs} documents`}
        </span>
        {onToggleMaximize && (
          <button type="button" onClick={onToggleMaximize} title={maximized ? "Show the document" : "Maximize the map"}>
            {maximized ? "⇥" : "⛶"}
          </button>
        )}
      </div>
      <div className="lu-stage" ref={stageRef}>
        {glError && (
          <div className="lu-notice">
            The star map needs WebGL, which this browser could not start ({glError}). Use the tree view.
          </div>
        )}
        {loading.error && <div className="lu-notice lu-notice--error">Could not load the map: {loading.error}</div>}
        {!loading.error && loading.done && m.docCount === 0 && (
          <div className="lu-notice">No documents to show in this scope.</div>
        )}
        {linkKinds.length > 0 && (
          <div className="lu-legend">
            {linkKinds.map(([k, n]) => (
              <button
                type="button"
                key={k}
                className={hiddenKinds.has(k) ? "lu-kind lu-kind--off" : "lu-kind"}
                onClick={() =>
                  setHiddenKinds((cur) => {
                    const next = new Set(cur);
                    if (next.has(k)) next.delete(k);
                    else next.add(k);
                    return next;
                  })
                }
                title={hiddenKinds.has(k) ? `Show ${k} links` : `Hide ${k} links`}
              >
                <i style={{ background: kindHex(k) }} />
                {k} <span>{n}</span>
              </button>
            ))}
          </div>
        )}
        {hover && <Tooltip model={m} layout={layout} hover={hover} />}
      </div>
      <SearchPanel
        model={m}
        version={version}
        data={data}
        scope={scope}
        browse={browse}
        underPath={underPath}
        results={results}
        onResults={setResults}
        checked={checked}
        onChecked={setChecked}
        onPick={pickResult}
        onFrame={(stars) => sceneRef.current?.frame(stars)}
        withProjection={projected}
        onQueryPoint={setQueryXyz}
        memoryScopeId={scope === "agent" ? undefined : scope === "tenant" ? "" : (browse?.scopeId ?? subject)}
      />
    </div>
  );
}

function Tooltip({ model: m, layout: l, hover }: { model: UniverseModel; layout: Layout; hover: Hover }) {
  const s = hover.star;
  let body: React.ReactNode;
  if (s >= m.chunkCount) {
    const d = l.dirs[s - m.chunkCount];
    if (!d) return null;
    body = (
      <>
        <strong>{d.path}</strong>
        <span>{d.docTotal} documents — click to fly in</span>
      </>
    );
  } else {
    const di = m.chunk.doc[s];
    const isRoot = m.doc.root[di] === s;
    const color = rgbHex(chunkRgb(m, s));
    const type = isRoot ? m.doc.type[di] : m.chunk.type[s];
    const status = isRoot ? m.doc.status[di] : m.chunk.status[s];
    body = (
      <>
        <strong>
          <i className="lu-chip" style={{ background: color }} />
          {(isRoot ? m.doc.title[di] : m.chunk.title[s]) || m.chunk.id[s]}
        </strong>
        {!isRoot && <span>{m.doc.title[di]}</span>}
        {m.doc.paths[di]?.[0] && <code>{m.doc.paths[di][0]}</code>}
        <span>
          {[type, status].filter(Boolean).join(" · ")}
          {isRoot ? ` · ${m.doc.chunkCount[di]} chunks` : ""}
        </span>
      </>
    );
  }
  return (
    <div className="lu-tooltip" style={{ transform: `translate(${hover.x + 14}px, ${hover.y + 14}px)` }}>
      {body}
    </div>
  );
}
