import { useParams, useSearchParams } from "react-router-dom";
import type { DocScope } from "../api";
import DocumentViewer from "../components/DocumentViewer";

// DocumentsView is the deep-link surface for one document
// (/documents/:documentId?scope=user) — a shareable full-page DocumentViewer.
// Scope defaults to `user` (the interop-correct default; see RFC AM §8); pass
// ?scope=agent for an agent-scope document.
export default function DocumentsView() {
  const { documentId } = useParams();
  const [params] = useSearchParams();
  const scope: DocScope = params.get("scope") === "agent" ? "agent" : "user";
  if (!documentId) {
    return (
      <div className="empty">
        <p>No document id in the URL.</p>
      </div>
    );
  }
  return (
    <div className="documents-page">
      <DocumentViewer documentId={documentId} scope={scope} />
    </div>
  );
}
