import { useNavigate } from "react-router-dom";
import { MemoryView } from "@loomcycle/memory-view";
import { useFocusTenant } from "../components/Layout";
import { consolidationRunHref } from "../lib/ontologyEditHref";
import "@loomcycle/memory-view/styles.css";

// MemoryView is the operator Memory console (/memory) — the scope/scope_id/key
// browser + entry editor + Vector Memory reembed flow. It is a thin wrapper
// around the standalone <MemoryView> from @loomcycle/memory-view (RFC BV); web
// consumes the package SOURCE via a Vite alias.
//
// CSS: UNLIKE @loomcycle/library / @loomcycle/explorer (which emit web's global
// class names and are NOT style-imported here), the memory-view package scopes
// ALL its classes under `.loomcycle-memory-view`, so this wrapper DOES import the
// package's styles.css — without it the console renders unstyled.

// cookieFetch mirrors api.ts's jsonFetch transport EXACTLY (same-origin cookie,
// 401 → login). Module scope keeps its identity stable so the package memoizes
// its client on it. (Kept in sync with PathTreeView.tsx / DocumentsView.tsx.)
const cookieFetch: typeof fetch = async (input, init) => {
  const r = await fetch(input, { ...init, credentials: "same-origin" });
  if (r.status === 401) {
    window.location.assign("/ui/login");
    return new Promise<Response>(() => {});
  }
  return r;
};

export default function MemoryViewPage() {
  const navigate = useNavigate();
  // The topbar tenant focus, which every OTHER browse surface (Documents, Paths,
  // Agents, Users) already consumes and this one did not. Without it a
  // super-admin console could only ever read its own tenant's memory — usually
  // the empty one — while that tenant's own operator saw its rows, so admin
  // looked strictly less capable than the role it supersedes. Blank = own tenant.
  const focusTenant = useFocusTenant();
  return (
    <MemoryView
      connection={{ baseUrl: "", fetch: cookieFetch }}
      browse={{ tenant: focusTenant || undefined }}
      // Starting a consolidation pass is the HOST's business: the package cannot know
      // about /run, and an embedder does not have it. This stages the agent and prompt
      // in the terminal rather than starting anything — a click that spends tokens with
      // no preview is a surprise, and a pass is the most expensive thing this console
      // can begin.
      onRunConsolidation={(scopeId) => navigate(consolidationRunHref(scopeId || undefined))}
    />
  );
}
