import { useParams, useSearchParams } from "react-router-dom";
import { DocumentViewer } from "@loomcycle/explorer";
import { useFocusTenant, usePrincipal, useUserId } from "../components/Layout";
import DocumentAssistantPanel from "../components/DocumentAssistantPanel";
import SubjectMergePanel, { canMergeSubjects } from "../components/SubjectMergePanel";
import SubjectPrincipalPanel from "../components/SubjectPrincipalPanel";
import { startRun, type DocScope } from "../api";
import { canRegenerateAnnotations, regenerateAnnotation } from "../lib/annotationRegen";

// DocumentsView is the deep-link surface for one document
// (/documents/:documentId?scope=user&chunk=<chunk_id>) — a shareable full-page
// viewer, opening on `chunk` when given (a loom:chunk/… link). It is a
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
  // tenant is a real document scope since loomcycle v1.41.0. Reading it out of the
  // URL rather than folding it to "user" is what lets a deep link to a
  // tenant-scope document (the ontology, the deployment-context doc) actually open.
  const rawScope = params.get("scope");
  const scope: DocScope =
    rawScope === "agent" || rawScope === "tenant" ? rawScope : "user";
  const chunkId = params.get("chunk") || undefined;
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
      {canMergeSubjects(principal, scope) && (
        <>
          <SubjectPrincipalPanel documentId={documentId} tenant={focusTenant || undefined} />
          <SubjectMergePanel documentId={documentId} tenant={focusTenant || undefined} />
        </>
      )}
      <DocumentViewer
        documentId={documentId}
        chunkId={chunkId}
        scope={scope}
        connection={{ baseUrl: "", fetch: cookieFetch }}
        principal={principal ?? undefined}
        browse={{ scopeId: userId || undefined, tenant: focusTenant || undefined }}
        // As on the Path page: the annotator runs in the caller's own tenant.
        // Edits are written as the operator's manual annotations; anyone else is
        // refused by the server, so only an operator is offered Edit.
        canEditAnnotations={canRegenerateAnnotations(principal)}
        onRegenerateAnnotation={
          canRegenerateAnnotations(principal) && !focusTenant
            ? (target) => regenerateAnnotation(target, startRun)
            : undefined
        }
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
