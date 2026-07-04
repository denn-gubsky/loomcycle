import { useParams, useSearchParams } from "react-router-dom";
import { DocumentViewer } from "@loomcycle/explorer";
import { useFocusTenant, usePrincipal, useUserId } from "../components/Layout";
import DocumentAssistantPanel from "../components/DocumentAssistantPanel";
import type { DocScope } from "../api";

// DocumentsView is the deep-link surface for one document
// (/documents/:documentId?scope=user) — a shareable full-page viewer. It is a
// thin wrapper around the standalone <DocumentViewer> from @loomcycle/explorer
// (RFC AZ); web consumes the package SOURCE via a Vite alias. Scope defaults to
// `user` (the interop-correct default; see RFC AM §8); pass ?scope=agent for an
// agent-scope document.
//
// CSS + Document Assistant wiring: see the note in PathTreeView.tsx (same
// rationale — global class names, injected assistant slot).

// cookieFetch mirrors api.ts's jsonFetch transport EXACTLY (same-origin cookie,
// 401 → login). Module scope keeps its identity stable so the package memoizes
// its client on it. (Kept in sync with PathTreeView.tsx / LibraryView.tsx.)
const cookieFetch: typeof fetch = async (input, init) => {
  const r = await fetch(input, { ...init, credentials: "same-origin" });
  if (r.status === 401) {
    window.location.assign("/ui/login");
    return new Promise<Response>(() => {});
  }
  return r;
};

export default function DocumentsView() {
  const { documentId } = useParams();
  const [params] = useSearchParams();
  const scope: DocScope = params.get("scope") === "agent" ? "agent" : "user";
  const principal = usePrincipal();
  const userId = useUserId();
  const focusTenant = useFocusTenant();

  if (!documentId) {
    return (
      <div className="empty">
        <p>No document id in the URL.</p>
      </div>
    );
  }
  return (
    <div className="documents-page">
      <DocumentViewer
        documentId={documentId}
        scope={scope}
        connection={{ baseUrl: "", fetch: cookieFetch }}
        principal={principal ?? undefined}
        browse={{ scopeId: userId || undefined, tenant: focusTenant || undefined }}
        renderAssistant={(ctx) => (
          <DocumentAssistantPanel
            documentId={ctx.documentId}
            scope={ctx.scope as DocScope}
            chunks={[]}
            selectedChunk={null}
            onChanged={() => ctx.onChanged?.()}
            onStopped={() => ctx.onChanged?.()}
          />
        )}
      />
    </div>
  );
}
