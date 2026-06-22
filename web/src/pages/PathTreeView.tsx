import { useCallback, useEffect, useMemo, useState, type FormEvent } from "react";
import {
  type DocScope,
  type PathEntry,
  type PathScope,
  documentDelete,
  documentCreate,
  pathLs,
  pathMkdir,
  pathMv,
  pathRm,
} from "../api";
import Splitter from "../components/Splitter";
import PathTree, {
  buildPathTree,
  collectDocumentIds,
  parentPathOf,
  type PathNode,
} from "../components/PathTree";

// PathTreeView is the RFC AM Phase 1 Path console: the unified dirent tree
// (RFC AL) with directory + document CRUD. Identity (tenant + the user-scope
// id) is resolved server-side from the cookie principal — the page sends only
// `scope`, which is the subtree SELECTOR, not an authority grant. Defaults to
// `user` scope so the tree lines up with the principal's own agent runs.

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

export default function PathTreeView() {
  const [scope, setScope] = useState<PathScope>("user");
  const [entries, setEntries] = useState<PathEntry[]>([]);
  const [selectedPath, setSelectedPath] = useState<string | undefined>(undefined);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [modal, setModal] = useState<ModalState | null>(null);

  const refresh = useCallback(async () => {
    setLoading(true);
    setErr(null);
    try {
      const resp = await pathLs("/", scope, true);
      setEntries(resp.entries ?? []);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      setEntries([]);
    } finally {
      setLoading(false);
    }
  }, [scope]);

  // Reload whenever the scope changes (and on mount). Clear the selection so a
  // stale path from the previous scope doesn't drive the detail pane.
  useEffect(() => {
    setSelectedPath(undefined);
    void refresh();
  }, [refresh]);

  const tree = useMemo(() => buildPathTree(entries), [entries]);
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
        await pathMkdir(p, scope);
        await refresh();
        setSelectedPath(p);
      },
    });
  }, [currentDir, currentDirLabel, scope, refresh]);

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
        await documentCreate(title, p, scope as DocScope);
        await refresh();
        setSelectedPath(p);
      },
    });
  }, [currentDir, currentDirLabel, scope, refresh]);

  const renameSelected = useCallback(() => {
    if (!selected) return;
    setModal({
      title: `Rename / move ${selected.fullPath}`,
      fields: [{ key: "to", label: "New path", value: selected.fullPath }],
      submitLabel: "Move",
      onSubmit: async (v) => {
        const to = v.to.trim();
        if (!to.startsWith("/")) throw new Error("path must start with /");
        await pathMv(selected.fullPath, to, scope);
        await refresh();
        setSelectedPath(to);
      },
    });
  }, [selected, scope, refresh]);

  const deleteSelected = useCallback(() => {
    if (!selected) return;
    if (selected.kind === "document") {
      const ref = selected.resourceRef as { document_id?: string } | undefined;
      const id = ref?.document_id;
      setModal({
        title: `Delete document ${selected.fullPath}`,
        message: "This deletes the document, all its chunks, and its path entry. This cannot be undone.",
        fields: [],
        submitLabel: "Delete",
        danger: true,
        onSubmit: async () => {
          if (!id) throw new Error("this document dirent has no document_id");
          await documentDelete(id, scope as DocScope);
          await refresh();
          setSelectedPath(undefined);
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
          await documentDelete(id, scope as DocScope);
        }
        await pathRm(selected.fullPath, scope, true);
        await refresh();
        setSelectedPath(undefined);
      },
    });
  }, [selected, scope, refresh]);

  const mutable = selected && (selected.kind === "directory" || selected.kind === "document");

  return (
    <>
      <Splitter
        className="paths-view"
        defaultLeftWidth={420}
        minLeftWidth={280}
        minRightWidth={300}
        storageKey="loomcycle.split.paths"
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
          </p>
          {err && <div className="paths-err">{err}</div>}
          {loading && entries.length === 0 ? (
            <div className="empty">
              <p>Loading…</p>
            </div>
          ) : (
            <PathTree tree={tree} selectedPath={selectedPath} onSelect={(n) => setSelectedPath(n.fullPath)} />
          )}
        </div>
        <div className="right">
          {selected ? (
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
                {selected.kind === "document" && (
                  <>
                    <dt>document_id</dt>
                    <dd>
                      <code>
                        {(selected.resourceRef as { document_id?: string })?.document_id ?? "—"}
                      </code>
                    </dd>
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
              {selected.kind === "document" && (
                <p className="paths-note">
                  Chunk viewing &amp; editing arrives with the Document viewer (Phase 2).
                </p>
              )}
            </div>
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
// (incl. tool refusals surfaced by substratePost) show inline.
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
