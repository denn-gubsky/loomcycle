import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type FormEvent,
  type ReactNode,
} from "react";
import type {
  AssistantContext,
  BrowseScope,
  DocScope,
  PathEntry,
  PathScope,
  Principal,
} from "./types";
import { useExplorerData, type ExplorerDataLayer } from "./lib/dataLayer";
import type { DocSummary } from "./lib/colorScheme";
import {
  ExplorerRoot,
  useResolvedDataLayer,
  type ExplorerDataSource,
} from "./components/ExplorerRoot";
import Splitter from "./components/Splitter";
import DocumentViewerBody from "./components/DocumentViewerBody";
import PathTree, {
  buildPathTree,
  collectDocumentIds,
  parentPathOf,
  type PathNode,
} from "./components/PathTree";

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
  const [entries, setEntries] = useState<PathEntry[]>([]);
  const [internalSelected, setInternalSelected] = useState<string | undefined>(undefined);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [modal, setModal] = useState<ModalState | null>(null);

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

  const refresh = useCallback(async () => {
    setLoading(true);
    setErr(null);
    try {
      const resp = await data.pathLs("/", scope, true, browse);
      setEntries(resp.entries ?? []);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      setEntries([]);
      onErrorRef.current?.(e);
    } finally {
      setLoading(false);
    }
  }, [data, scope, browse]);

  // Reload whenever the scope/browse changes (and on mount). Clear the selection
  // on a CHANGE (not on mount) so a stale path from the previous scope/subject
  // doesn't drive the detail pane — but the first run must respect a controlled
  // parent's initial `path` (clearing on mount would fire onPathChange(undefined)
  // and clobber it).
  const firstLoad = useRef(true);
  useEffect(() => {
    if (firstLoad.current) {
      firstLoad.current = false;
    } else {
      setSelected(undefined);
    }
    void refresh();
  }, [refresh, setSelected]);

  const tree = useMemo(() => buildPathTree(entries), [entries]);

  // RFC BN: fetch per-document type/status + color settings for every document
  // dirent in the tree, in ONE documents_summary call, so PathTree can color +
  // badge document rows. Best-effort — a failure just leaves rows neutral.
  const [summaries, setSummaries] = useState<Map<string, DocSummary>>(() => new Map());
  useEffect(() => {
    if (scope === "tenant") {
      setSummaries(new Map()); // documents are agent|user only
      return;
    }
    const ids = entries
      .filter((e) => e.kind === "document")
      .map((e) => (e.resource_ref as { document_id?: string } | undefined)?.document_id)
      .filter((id): id is string => !!id);
    if (ids.length === 0) {
      setSummaries(new Map());
      return;
    }
    let cancelled = false;
    data
      .documentsSummary({ documentIds: ids }, scope as DocScope, browse)
      .then((resp) => {
        if (cancelled) return;
        const m = new Map<string, DocSummary>();
        for (const d of resp.documents ?? []) m.set(d.document_id, d);
        setSummaries(m);
      })
      .catch(() => {
        if (!cancelled) setSummaries(new Map());
      });
    return () => {
      cancelled = true;
    };
  }, [data, entries, scope, browse]);

  const selected = useMemo(
    () => (selectedPath ? findNode(tree, selectedPath) : undefined),
    [tree, selectedPath],
  );

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
        await refresh();
        setSelected(p);
      },
    });
  }, [data, currentDir, currentDirLabel, scope, browse, refresh, setSelected]);

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
        await data.documentCreate(title, p, scope as DocScope, browse);
        await refresh();
        setSelected(p);
      },
    });
  }, [data, currentDir, currentDirLabel, scope, browse, refresh, setSelected]);

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
        await refresh();
        setSelected(to);
      },
    });
  }, [data, selected, scope, browse, refresh, setSelected]);

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
          await data.documentDelete(id, scope as DocScope, browse);
          await refresh();
          setSelected(undefined);
        },
      });
      return;
    }
    // Directory: cascade-delete contained Documents (so no orphaned content),
    // then remove the dirent subtree. Documents only exist in agent|user scope.
    const docIds = scope === "tenant" ? [] : collectDocumentIds(selected);
    const childCount = selected.children.length;
    setModal({
      title: `Delete branch ${selected.fullPath}`,
      message:
        childCount === 0
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
          await data.documentDelete(id, scope as DocScope, browse);
        }
        await data.pathRm(selected.fullPath, scope, true, browse);
        await refresh();
        setSelected(undefined);
      },
    });
  }, [data, selected, scope, browse, refresh, setSelected]);

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
          {err && <div className="paths-err">{err}</div>}
          {loading && entries.length === 0 ? (
            <div className="empty">
              <p>Loading…</p>
            </div>
          ) : (
            <PathTree
              tree={tree}
              selectedPath={selectedPath}
              onSelect={(n) => setSelected(n.fullPath)}
              summaries={summaries}
            />
          )}
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
                  scope={scope === "agent" ? "agent" : "user"}
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
                  <dt>stored</dt>
                  <dd>{selected.explicit ? "yes" : "implicit (no entry)"}</dd>
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
        <h3>{state.title}</h3>
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
        <div className="modal-buttons">
          <button type="button" onClick={onClose} disabled={busy}>
            cancel
          </button>
          <button type="submit" className={state.danger ? "danger" : "primary"} disabled={busy}>
            {busy ? "working…" : state.submitLabel}
          </button>
        </div>
      </form>
    </div>
  );
}
