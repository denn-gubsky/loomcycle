import { useState } from "react";
import {
  FoldedFieldList,
  headersProblem,
  hookDefRegistry,
  nameProblem,
  type DefValue,
} from "@loomcycle/def-fields";
import type { DefRow } from "../types";
import { useLibraryData } from "../lib/dataLayer";
import { forkOverlay, sourceHookOverlay } from "../lib/hookDefOverlay";
import { explainServerError } from "./LibraryEditModal";

// HookDefEditModal creates or forks a HookDef: one hook, stored once and named
// wherever it is used (an agent's hooks, a tool's own hooks, a team). Every
// field comes from the def-fields registry, so the form is exactly the stored
// definition and nothing else.

export interface HookDefEditModalProps {
  mode: "create" | "fork";
  forkSource?: DefRow;
  onClose: () => void;
  onSaved: (row: DefRow) => void;
}

// hookDefProblem reports what would stop the save, or null.
export function hookDefProblem(name: string, v: DefValue): string | null {
  const n = nameProblem(name.trim());
  if (n) return n;
  if (typeof v.event !== "string" || v.event === "") return "choose the event this hook answers";
  const body = (v.body ?? {}) as { kind?: string; code?: string; url?: string; headers?: Record<string, string> };
  if (body.kind !== "code-js" && body.kind !== "http") return "choose the body kind: code-js or http";
  if (body.kind === "code-js") {
    if (!body.code?.trim()) return "a code-js body needs code defining hook(ev)";
    if (body.url || body.headers) return "a code-js body takes no url or headers";
  } else {
    if (!body.url?.startsWith("http://") && !body.url?.startsWith("https://")) return "an http body needs an http:// or https:// url";
    if (body.code) return "an http body takes no code";
    const h = headersProblem(body.headers);
    if (h) return h;
  }
  return null;
}

export default function HookDefEditModal({ mode, forkSource, onClose, onSaved }: HookDefEditModalProps) {
  const data = useLibraryData();
  const [name, setName] = useState(forkSource?.name ?? "");
  const source = sourceHookOverlay(forkSource?.definition);
  const [value, setValue] = useState<DefValue>(source);
  const [promote, setPromote] = useState(mode === "create");
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const submit = async () => {
    const problem = hookDefProblem(name, value);
    if (problem) {
      setErr(problem);
      return;
    }
    setErr(null);
    setSubmitting(true);
    try {
      const row =
        mode === "create"
          ? await data.createDef("hookdef", name.trim(), { ...value }, promote)
          : await data.forkDef("hookdef", name.trim(), forkOverlay(source, value), promote, forkSource?.def_id);
      onSaved(row);
    } catch (e) {
      setErr(explainServerError(e));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={submitting ? undefined : onClose}>
      <div className="modal library-modal" onClick={(e) => e.stopPropagation()}>
        <h3>{mode === "create" ? "New hook" : `Fork ${forkSource?.name ?? "hook"}`}</h3>

        <div className="library-form-row">
          <label htmlFor="lib-hook-name">name</label>
          <input
            id="lib-hook-name" type="text" value={name} placeholder="deny-internal-http"
            disabled={mode === "fork" || submitting} autoFocus={mode === "create"}
            onChange={(e) => setName(e.target.value)}
          />
        </div>

        <div className="library-form-row">
          <FoldedFieldList
            registry={hookDefRegistry}
            value={value}
            onChange={setValue}
            disabled={submitting}
            defaultOpenGroups={["What it answers", "Body"]}
          />
        </div>

        <div className="library-form-row library-form-row-checkbox">
          <label>
            <input type="checkbox" checked={promote} disabled={submitting}
              onChange={(e) => setPromote(e.target.checked)} />{" "}
            Promote immediately (set as active version)
          </label>
        </div>

        {err && <div className="modal-err">{err}</div>}

        <div className="modal-buttons">
          <button type="button" onClick={onClose} disabled={submitting}>Cancel</button>
          <button type="button" className="primary" onClick={submit} disabled={submitting}>
            {submitting ? "Saving…" : mode === "create" ? "Create" : "Save fork"}
          </button>
        </div>
      </div>
    </div>
  );
}
