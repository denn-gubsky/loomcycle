import { PathExplorer } from "@loomcycle/explorer";
import { useFocusTenant, usePrincipal, useUserId } from "../components/Layout";
import DocumentAssistantPanel from "../components/DocumentAssistantPanel";
import type { DocScope } from "../api";

// PathTreeView is a thin wrapper around the standalone <PathExplorer> component
// from @loomcycle/explorer (RFC AZ). The full Path VFS console — the dirent tree
// with folder/document CRUD and the inline chunked-graph Document viewer/editor —
// now lives in the package; web consumes its SOURCE via a Vite alias so this
// build compiles it.
//
// CSS: we deliberately do NOT `import "@loomcycle/explorer/styles.css"`. The
// package emits the SAME class names (.paths-*, .doc-*, .chunk-*, .md, .tree,
// .splitter*, .modal*) that web's global styles.css already styles, so the
// existing global sheet renders the component identically with zero token drift.
// The package ships its own scoped styles.css only for EXTERNAL hosts.
//
// The Document Assistant (RFC AM Phase 3) is injected via the package's
// renderAssistant slot: the package keeps the run-stream machinery (LiveRunPane /
// useRunStream) OUT of its bundle, so web supplies its existing
// <DocumentAssistantPanel>. ctx.onChanged is the viewer's own reload, so a turn
// that mutates the document refreshes the tree + selected chunk.

// cookieFetch mirrors api.ts's jsonFetch transport EXACTLY: the Web UI is
// same-origin and rides the `loomcycle_session` HttpOnly cookie (no bearer), and
// a 401 bounces to the login page. Injecting it into the package's
// @loomcycle/client keeps that behavior transparent. Defined at module scope so
// its identity is stable across renders (the package memoizes its client on the
// connection's `fetch` reference).
const cookieFetch: typeof fetch = async (input, init) => {
  const r = await fetch(input, { ...init, credentials: "same-origin" });
  if (r.status === 401) {
    window.location.assign("/ui/login");
    // Navigation underway — never-settling so the caller doesn't flash an error
    // mid-redirect (matches jsonFetch's redirect-then-hang contract).
    return new Promise<Response>(() => {});
  }
  return r;
};

export default function PathTreeView() {
  const principal = usePrincipal();
  const userId = useUserId();
  const focusTenant = useFocusTenant();

  return (
    <PathExplorer
      connection={{ baseUrl: "", fetch: cookieFetch }}
      principal={principal ?? undefined}
      // RFC AS browse-by-subject: the topbar user/tenant picker drives WHICH
      // subject's tree this console browses (?scope_id= / ?tenant=). Empty →
      // the caller's own subject. The server re-authorizes.
      browse={{ scopeId: userId || undefined, tenant: focusTenant || undefined }}
      renderAssistant={(ctx) => (
        <DocumentAssistantPanel
          documentId={ctx.documentId}
          scope={ctx.scope as DocScope}
          // The package's slot ctx does not thread the live chunk selection, so
          // the assistant grounds itself via its own Document tool (query_chunks)
          // rather than a prefilled outline. onChanged refreshes the viewer at
          // each turn boundary; onStopped also refreshes (the panel's LiveRunPane
          // shows the terminal state / error inline).
          chunks={[]}
          selectedChunk={null}
          onChanged={() => ctx.onChanged?.()}
          onStopped={() => ctx.onChanged?.()}
        />
      )}
    />
  );
}
