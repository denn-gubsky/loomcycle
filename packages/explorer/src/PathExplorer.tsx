import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
  type FormEvent,
  type ReactNode,
} from "react";
import type {
  AssistantContext,
  BrowseScope,
  PathScope,
  Principal,
} from "./types";
import { useExplorerData, type ExplorerDataLayer } from "./lib/dataLayer";
import type { DocSummary } from "./lib/colorScheme";
import {
  LazyPathTree,
  LS_PAGE_LIMIT,
  ancestorsOf,
  documentIdsOf,
  listSubtree,
  parentPathOf,
} from "./lib/lazyPathTree";
import {
  ExplorerRoot,
  useResolvedDataLayer,
  type ExplorerDataSource,
} from "./components/ExplorerRoot";
import Splitter from "./components/Splitter";
import DocumentViewerBody from "./components/DocumentViewerBody";
import PathTree, { buildLazyTree, type PathNode } from "./components/PathTree";

// PathExplorer is the embeddable Path VFS console (RFC AL): the unified dirent
// tree with directory + document CRUD, and an inline DocumentViewer for document
// dirents. Identity (tenant + subject) is resolved server-side from the
// authenticated principal — the component sends only `scope` (the subtree
// SELECTOR) + the optional RFC AS `browse` override, never an authority grant.
//
// This is the decoupled port of the loomcycle Web UI's PathTreeView: routing is
// replaced by internal selection state (with optional controlled
// `path`/`onPathChange`), the runtime is reached through an injected data layer
// (connection → client → dataLayer), and browse / principal arrive as props
// instead of context hooks. Styles ship separately:
// `import "@loomcycle/explorer/styles.css"`.

const NAME_RE = /^[A-Za-z0-9._-]+$/;

// joinPath appends a leaf segment to a canonical parent dir ("" = root).
function joinPath(dir: string, name: string): string {
  return `${dir}/${name}`;
}

function findNode(tree: PathNode[], fullPath: string): PathNode | undefined {
  for (const n of tree) {
    if (n.fullPath === fullPath) return n;
    const hit = findNode(n.children, fullPath);
    if (hit) return hit;
  }
  return undefined;
}

export interface PathExplorerProps extends ExplorerDataSource {
  // ---- Theming. When set, the root carries data-theme; when omitted the
  // component inherits an ancestor's data-theme (dark is the default palette).
  theme?: "light" | "dark";

  // ---- Scope. The initial tree scope; the toolbar select lets the operator
  // switch it at runtime (internal state). Default "user".
  defaultScope?: PathScope;

  // ---- Selection. `path` + `onPathChange` make the selected path controlled
  // (so a host that routes can bridge URL ↔ selection); omit both for internal
  // state. On a scope switch the selection clears (onPathChange fires with
  // undefined). Mirrors the library's tab/onTabChange controlled pattern.
  path?: string;
  onPathChange?: (path: string | undefined) => void;

  // ---- RFC AS browse-by-subject override threaded into every path/document
  // call. Unset → the caller's own subject.
  browse?: BrowseScope;

  // ---- Authenticated principal — only `subject` is read (passed to
  // renderAssistant). Optional.
  principal?: Principal;

  // ---- Optional Document Assistant slot, forwarded to the embedded
  // DocumentViewer. Provide it to show an "assistant" toggle; omit → none.
  renderAssistant?: (ctx: AssistantContext) => ReactNode;

  // ---- Errors. Called on a tree-load failure (in addition to the inline
  // banner). The component NEVER redirects on 401 — the host owns the auth flow.
  onError?: (e: unknown) => void;
}

export default function PathExplorer(props: PathExplorerProps) {
  const { theme, defaultScope, path, onPathChange, browse, principal, renderAssistant, onError } =
    props;
  const resolved = useResolvedDataLayer(props);
  return (
    <ExplorerRoot theme={theme} dataLayer={resolved}>
      <PathExplorerBody
        defaultScope={defaultScope}
        path={path}
        onPathChange={onPathChange}
        browse={browse}
        principal={principal}
        renderAssistant={renderAssistant}
        onError={onError}
      />
    </ExplorerRoot>
  );
}

type PathExplorerBodyProps = Omit<PathExplorerProps, keyof ExplorerDataSource | "theme">;

interface ModalField {
  key: string;
  label: string;
  placeholder?: string;
  value?: string;
}
interface ModalState {
  title: string;
  message?: string;
  fields: ModalField[];
  submitLabel: string;
  danger?: boolean;
  onSubmit: (vals: Record<string, string>) => Promise<void>;
}

function PathExplorerBody({
  defaultScope,
  path: pathProp,
  onPathChange,
  browse: browseProp,
  principal,
  renderAssistant,
  onError,
}: PathExplorerBodyProps) {
  const data: ExplorerDataLayer = useExplorerData();

  const [scope, setScope] = useState<PathScope>(defaultScope ?? "user");
  const [internalSelected, setInternalSelected] = useState<string | undefined>(undefined);
  const [err, setErr] = useState<string | null>(null);
  const [modal, setModal] = useState<ModalState | null>(null);
  // Directories the operator has opened. Collapsed by default: opening one is
  // what lists it, so nothing below the root is fetched until it is wanted.
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(() => new Set());

  // Effective selection: controlled `path` prop overrides the internal state.
  const selectedPath = pathProp !== undefined ? pathProp : internalSelected;

  // onPathChange through a ref so the selection setter + scope-reset effect stay
  // stable (a parent passing an inline callback each render doesn't churn them).
  const onPathChangeRef = useRef(onPathChange);
  useEffect(() => {
    onPathChangeRef.current = onPathChange;
  }, [onPathChange]);
  const onErrorRef = useRef(onError);
  useEffect(() => {
    onErrorRef.current = onError;
  }, [onError]);

  const setSelected = useCallback((p: string | undefined) => {
    onPathChangeRef.current?.(p);
    setInternalSelected(p); // harmless when controlled; drives display when not
  }, []);

  // Memoize browse on its primitives so an inline `browse={{...}}` doesn't churn
  // the fetch effect every render.
  const browse = useMemo<BrowseScope | undefined>(
    () =>
      browseProp && (browseProp.scopeId || browseProp.tenant)
        ? { scopeId: browseProp.scopeId, tenant: browseProp.tenant }
        : undefined,
    [browseProp?.scopeId, browseProp?.tenant],
  );

  // One listing cache per (scope, subject): a new instance on a switch is also
  // what makes a previous subject's in-flight pages land nowhere.
  const lazy = useMemo(() => new LazyPathTree(data, scope, browse), [data, scope, browse]);
  const dirs = useSyncExternalStore(lazy.subscribe, lazy.getSnapshot);

  // report hands a listing failure to the host. The tree already shows it
  // inline (root: the banner; a directory: its own row), so this never throws.
  const report = useCallback((e: unknown) => {
    onErrorRef.current?.(e);
  }, []);

  // RFC BN: per-document type/status + color settings, so PathTree can color +
  // badge document rows. Fetched per listed page (≤ one page of ids per call —
  // the bound documents_summary is sized for) and merged, rather than once for a
  // whole tree that is no longer loaded at once. Best-effort — a failure just
  // leaves those rows neutral. `requested` keeps a page from being re-asked on
  // every listing change; summaryGen drops answers from before a reset.
  const [summaries, setSummaries] = useState<Map<string, DocSummary>>(() => new Map());
  const requested = useRef<Set<string>>(new Set());
  const summaryGen = useRef(0);
  const resetSummaries = useCallback(() => {
    summaryGen.current++;
    requested.current = new Set();
    setSummaries(new Map());
  }, []);

  // Start over whenever the scope/browse changes (and on mount). Clear the
  // selection on a CHANGE (not on mount) so a stale path from the previous
  // scope/subject doesn't drive the detail pane — but the first run must respect
  // a controlled parent's initial `path` (clearing on mount would fire
  // onPathChange(undefined) and clobber it).
  const firstLoad = useRef(true);
  useEffect(() => {
    if (firstLoad.current) {
      firstLoad.current = false;
    } else {
      setSelected(undefined);
      setErr(null);
      setExpanded(new Set());
      resetSummaries();
    }
    lazy.loadDir("").catch(report);
  }, [lazy, setSelected, resetSummaries, report]);

  // An opened directory that has never been listed gets listed. Keyed on the
  // listings too, so a directory whose cache was dropped (moved away and back)
  // while it stayed open re-lists instead of sitting empty. A FAILED listing is
  // left for its retry row, so an outage doesn't turn into a request loop.
  useEffect(() => {
    for (const d of expanded) {
      if (!dirs.has(d)) lazy.loadDir(d).catch(report);
    }
  }, [lazy, expanded, dirs, report]);

  // Deep link / post-mutation selection: open the selection's ancestors and list
  // them — paging a directory until the selected entry shows, since it may sit
  // past the first page. Skipped on a scope/subject switch: that render still
  // carries the previous selection, which the effect above is clearing.
  const revealedIn = useRef<LazyPathTree | null>(null);
  useEffect(() => {
    const switched = revealedIn.current !== null && revealedIn.current !== lazy;
    revealedIn.current = lazy;
    if (!selectedPath || switched) return;
    const ancestors = ancestorsOf(selectedPath).filter((d) => d !== "");
    setExpanded((prev) => {
      if (ancestors.every((d) => prev.has(d))) return prev;
      const next = new Set(prev);
      ancestors.forEach((d) => next.add(d));
      return next;
    });
    lazy.reveal(selectedPath).catch(report);
  }, [lazy, selectedPath, report]);

  useEffect(() => {
    if (scope === "tenant") return; // the tree view colors agent|user documents only
    const fresh: string[] = [];
    for (const listing of dirs.values()) {
      for (const id of documentIdsOf(listing.entries)) {
        if (!requested.current.has(id)) {
          requested.current.add(id);
          fresh.push(id);
        }
      }
    }
    const gen = summaryGen.current;
    for (let i = 0; i < fresh.length; i += LS_PAGE_LIMIT) {
      data
        .documentsSummary({ documentIds: fresh.slice(i, i + LS_PAGE_LIMIT) }, scope, browse)
        .then((resp) => {
          if (summaryGen.current !== gen) return;
          setSummaries((prev) => {
            const m = new Map(prev);
            for (const d of resp.documents ?? []) m.set(d.document_id, d);
            return m;
          });
        })
        .catch(() => {});
    }
  }, [data, dirs, scope, browse]);

  const tree = useMemo(() => buildLazyTree(dirs), [dirs]);

  // refresh re-lists the root and every open directory, then re-reveals the
  // selection (it may have been on a page the reload folded away).
  const refresh = useCallback(async () => {
    setErr(null);
    resetSummaries();
    await lazy.refresh(expanded).catch(report);
    if (selectedPath) await lazy.reveal(selectedPath).catch(report);
  }, [lazy, expanded, selectedPath, resetSummaries, report]);

  const toggle = useCallback((dir: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(dir)) next.delete(dir);
      else next.add(dir);
      return next;
    });
  }, []);

  const loadMore = useCallback((dir: string) => lazy.loadMore(dir).catch(report), [lazy, report]);
  const retry = useCallback(
    (dir: string) => {
      const listing = lazy.getSnapshot().get(dir);
      (listing?.loaded ? lazy.loadMore(dir) : lazy.loadDir(dir)).catch(report);
    },
    [lazy, report],
  );

  const selected = useMemo(
    () => (selectedPath ? findNode(tree, selectedPath) : undefined),
    [tree, selectedPath],
  );
  const rootErr = dirs.get("")?.error;

  // Whether the selected directory has its own dirent or exists only through its
  // descendants. A one-level listing reports both as `directory`, so this asks
  // per selection; a data layer without pathIsStored just omits the row.
  const selectedDir = selected?.kind === "directory" ? selected.fullPath : undefined;
  const [stored, setStored] = useState<{ path: string; explicit: boolean } | null>(null);
  useEffect(() => {
    if (!selectedDir || !data.pathIsStored) return;
    let cancelled = false;
    data.pathIsStored(selectedDir, scope, browse).then(
      (explicit) => {
        if (!cancelled) setStored({ path: selectedDir, explicit });
      },
      () => {},
    );
    return () => {
      cancelled = true;
    };
  }, [data, selectedDir, scope, browse]);
  // Only a directory can be implicit; every other kind is a stored entry.
  const storedLabel = !selected
    ? undefined
    : selected.kind !== "directory"
      ? "yes"
      : stored?.path === selected.fullPath
        ? stored.explicit
          ? "yes"
          : "implicit (no entry)"
        : undefined;

  // The directory new items are created under: the selected directory, the
  // parent of a selected leaf, or root.
  const currentDir = useMemo(() => {
    if (!selected) return "";
    return selected.kind === "directory" ? selected.fullPath : parentPathOf(selected.fullPath);
  }, [selected]);
  const currentDirLabel = currentDir || "/";

  const canDocuments = scope !== "tenant"; // Documents are agent|user only.

  const newFolder = useCallback(() => {
    setModal({
      title: `New folder in ${currentDirLabel}`,
      fields: [{ key: "name", label: "Folder name", placeholder: "e.g. launches" }],
      submitLabel: "Create",
      onSubmit: async (v) => {
        const name = v.name.trim();
        if (!NAME_RE.test(name)) throw new Error("name may contain only [A-Za-z0-9._-]");
        const p = joinPath(currentDir, name);
        await data.pathMkdir(p, scope, browse);
        await lazy.invalidate(p).catch(report);
        setSelected(p);
      },
    });
  }, [data, lazy, currentDir, currentDirLabel, scope, browse, report, setSelected]);

  const newDocument = useCallback(() => {
    setModal({
      title: `New document in ${currentDirLabel}`,
      fields: [
        { key: "name", label: "Path name", placeholder: "e.g. launch-plan" },
        { key: "title", label: "Title", placeholder: "e.g. Launch Plan" },
      ],
      submitLabel: "Create",
      onSubmit: async (v) => {
        const name = v.name.trim();
        if (!NAME_RE.test(name)) throw new Error("name may contain only [A-Za-z0-9._-]");
        const title = v.title.trim() || name;
        const p = joinPath(currentDir, name);
        await data.documentCreate(title, p, scope, browse);
        await lazy.invalidate(p).catch(report);
        setSelected(p);
      },
    });
  }, [data, lazy, currentDir, currentDirLabel, scope, browse, report, setSelected]);

  const renameSelected = useCallback(() => {
    if (!selected) return;
    setModal({
      title: `Rename / move ${selected.fullPath}`,
      fields: [{ key: "to", label: "New path", value: selected.fullPath }],
      submitLabel: "Move",
      onSubmit: async (v) => {
        const to = v.to.trim();
        if (!to.startsWith("/")) throw new Error("path must start with /");
        await data.pathMv(selected.fullPath, to, scope, browse);
        await Promise.all([
          lazy.invalidate(selected.fullPath, { dropSubtree: true }),
          lazy.invalidate(to),
        ]).catch(report);
        setSelected(to);
      },
    });
  }, [data, lazy, selected, scope, browse, report, setSelected]);

  const deleteSelected = useCallback(() => {
    if (!selected) return;
    if (selected.kind === "document") {
      const ref = selected.resourceRef as { document_id?: string } | undefined;
      const id = ref?.document_id;
      setModal({
        title: `Delete document ${selected.fullPath}`,
        message:
          "This deletes the document, all its chunks, and its path entry. This cannot be undone.",
        fields: [],
        submitLabel: "Delete",
        danger: true,
        onSubmit: async () => {
          if (!id) throw new Error("this document dirent has no document_id");
          await data.documentDelete(id, scope, browse);
          await lazy.invalidate(selected.fullPath).catch(report);
          setSelected(undefined);
        },
      });
      return;
    }
    // Directory: cascade-delete contained Documents (so no orphaned content),
    // then remove the dirent subtree. Documents only exist in agent|user scope.
    // The tree holds only the directories that were opened, so the branch is
    // listed in full here — the confirmation must count, and the delete must
    // reach, every Document under it, not just the ones on screen.
    const target = selected;
    setErr(null);
    void (async () => {
      let subtree;
      try {
        subtree = await listSubtree(data, target.fullPath, scope, browse);
      } catch (e) {
        setErr(`could not list ${target.fullPath} for delete: ${e instanceof Error ? e.message : String(e)}`);
        return;
      }
      const docIds = scope === "tenant" ? [] : documentIdsOf(subtree);
      setModal({
        title: `Delete branch ${target.fullPath}`,
        message:
          subtree.length === 0
            ? "This removes the (empty) folder."
            : `This removes the folder and everything under it` +
              (docIds.length > 0
                ? ` — including ${docIds.length} document(s), whose chunks are deleted.`
                : "."),
        fields: [],
        submitLabel: "Delete",
        danger: true,
        onSubmit: async () => {
          for (const id of docIds) {
            await data.documentDelete(id, scope, browse);
          }
          await data.pathRm(target.fullPath, scope, true, browse);
          await lazy.invalidate(target.fullPath, { dropSubtree: true }).catch(report);
          setSelected(undefined);
        },
      });
    })();
  }, [data, lazy, selected, scope, browse, report, setSelected]);

  const mutable = selected && (selected.kind === "directory" || selected.kind === "document");

  return (
    <>
      <Splitter
        className="paths-view"
        defaultLeftWidth={420}
        minLeftWidth={280}
        minRightWidth={300}
        storageKey="loomcycle.explorer.split.paths"
      >
        <div className="left">
          <div className="paths-toolbar">
            <label className="paths-scope">
              <span>scope</span>
              <select value={scope} onChange={(e) => setScope(e.target.value as PathScope)}>
                <option value="user">user</option>
                <option value="agent">agent</option>
                <option value="tenant">tenant</option>
              </select>
            </label>
            <div className="paths-toolbar-actions">
              <button type="button" onClick={newFolder} title={`New folder in ${currentDirLabel}`}>
                + folder
              </button>
              <button
                type="button"
                onClick={newDocument}
                disabled={!canDocuments}
                title={
                  canDocuments
                    ? `New document in ${currentDirLabel}`
                    : "Documents are agent/user scope only"
                }
              >
                + document
              </button>
              <button type="button" onClick={() => void refresh()} title="Reload the tree">
                ↻
              </button>
            </div>
          </div>
          <p className="paths-context">
            new items → <code>{currentDirLabel}</code>
            {browse?.scopeId && (
              <>
                {" · "}subject <code>{browse.scopeId}</code>
              </>
            )}
            {browse?.tenant && (
              <>
                {" · "}tenant <code>{browse.tenant}</code>
              </>
            )}
          </p>
          {(err || rootErr) && <div className="paths-err">{err || rootErr}</div>}
          <PathTree
            tree={tree}
            root={dirs.get("")}
            expanded={expanded}
            onToggle={toggle}
            onLoadMore={loadMore}
            onRetry={retry}
            selectedPath={selectedPath}
            onSelect={(n) => setSelected(n.fullPath)}
            summaries={summaries}
          />
        </div>
        <div className="right">
          {selected ? (
            selected.kind === "document" ? (
              <div className="paths-doc">
                <div className="paths-doc-head">
                  <code className="paths-doc-path">{selected.fullPath}</code>
                  <div className="paths-detail-actions">
                    <button type="button" onClick={renameSelected}>
                      rename / move
                    </button>
                    <button type="button" className="danger" onClick={deleteSelected}>
                      delete
                    </button>
                  </div>
                </div>
                <DocumentViewerBody
                  documentId={
                    (selected.resourceRef as { document_id?: string })?.document_id ?? ""
                  }
                  scope={scope}
                  titleHint={selected.name}
                  browse={browse}
                  principal={principal}
                  renderAssistant={renderAssistant}
                />
              </div>
            ) : (
              <div className="paths-detail">
                <h2>
                  <span className="paths-detail-kind">{selected.kind}</span>
                  <code>{selected.fullPath}</code>
                </h2>
                <dl className="paths-detail-meta">
                  <dt>scope</dt>
                  <dd>{scope}</dd>
                  {storedLabel && (
                    <>
                      <dt>stored</dt>
                      <dd>{storedLabel}</dd>
                    </>
                  )}
                </dl>
                {mutable ? (
                  <div className="paths-detail-actions">
                    <button type="button" onClick={renameSelected}>
                      rename / move
                    </button>
                    <button type="button" className="danger" onClick={deleteSelected}>
                      delete
                    </button>
                  </div>
                ) : (
                  <p className="paths-readonly">
                    Read-only — {selected.kind} entries are managed by their own tool.
                  </p>
                )}
              </div>
            )
          ) : (
            <div className="empty">
              <p>Select a path on the left, or create a folder / document.</p>
            </div>
          )}
        </div>
      </Splitter>
      {modal && <PromptModal state={modal} onClose={() => setModal(null)} />}
    </>
  );
}

// PromptModal is a minimal create/rename/confirm dialog reusing the shared
// .modal-* anchors. Zero fields + a message renders a confirm; submit errors
// (incl. tool refusals surfaced by the data layer) show inline.
function PromptModal({ state, onClose }: { state: ModalState; onClose: () => void }) {
  const [vals, setVals] = useState<Record<string, string>>(() =>
    Object.fromEntries(state.fields.map((f) => [f.key, f.value ?? ""])),
  );
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      await state.onSubmit(vals);
      onClose();
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : String(ex));
      setBusy(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <form className="modal" onClick={(e) => e.stopPropagation()} onSubmit={submit}>
        {/* RFC BN P1: Save/Cancel pinned to the top for consistency with the chunk editor. */}
        <div className="modal-header sticky-top">
          <h3>{state.title}</h3>
          <div className="modal-buttons modal-buttons-top">
            <button type="button" onClick={onClose} disabled={busy}>
              cancel
            </button>
            <button type="submit" className={state.danger ? "danger" : "primary"} disabled={busy}>
              {busy ? "working…" : state.submitLabel}
            </button>
          </div>
        </div>
        {state.message && <p className="modal-context">{state.message}</p>}
        {state.fields.map((f, i) => (
          <label key={f.key} className="path-field">
            <span>{f.label}</span>
            <input
              className="path-modal-input"
              value={vals[f.key]}
              placeholder={f.placeholder}
              autoFocus={i === 0}
              onChange={(e) => setVals((v) => ({ ...v, [f.key]: e.target.value }))}
            />
          </label>
        ))}
        {err && <div className="modal-err">{err}</div>}
      </form>
    </div>
  );
}
