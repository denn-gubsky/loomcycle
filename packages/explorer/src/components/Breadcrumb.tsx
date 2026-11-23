import { useButtonCard, type CardSpec } from "./AnnotationCard";

// Breadcrumb is the document header's path (RFC GZ): each folder segment is a
// button that selects and reveals that folder in the Path tree, and the last
// segment is the document itself. Every segment carries its annotation card.
//
// It is distinct from the followed-links trail (DocTrail), which records the
// documents a reader came through, not where this one is filed.
export interface BreadcrumbProps {
  // path is the document's canonical Path name ("/kb/docs/architecture").
  path: string;
  // onFolder selects and reveals a folder ("/kb", "/kb/docs").
  onFolder: (folder: string) => void;
  // cardFor gives a segment's card: a folder's from the tree, the document's
  // from its summary.
  cardFor?: (segmentPath: string, isDocument: boolean) => CardSpec | undefined;
}

export function breadcrumbSegments(path: string): { name: string; path: string }[] {
  const parts = path.split("/").filter(Boolean);
  return parts.map((name, i) => ({ name, path: `/${parts.slice(0, i + 1).join("/")}` }));
}

export default function Breadcrumb({ path, onFolder, cardFor }: BreadcrumbProps) {
  const segs = breadcrumbSegments(path);
  return (
    <nav className="doc-crumbs" aria-label="Where this document is filed">
      <ol className="doc-crumbs-list">
        {segs.map((s, i) =>
          i === segs.length - 1 ? (
            <li className="doc-crumbs-item" key={s.path}>
              <Segment name={s.name} card={cardFor?.(s.path, true)} />
            </li>
          ) : (
            <li className="doc-crumbs-item" key={s.path}>
              <Segment name={s.name} card={cardFor?.(s.path, false)} onClick={() => onFolder(s.path)} />
            </li>
          ),
        )}
      </ol>
    </nav>
  );
}

function Segment({ name, card, onClick }: { name: string; card?: CardSpec; onClick?: () => void }) {
  const c = useButtonCard(card);
  if (!onClick) {
    // The document: not a link (it is open), but focusable for its card.
    return (
      <>
        <span className="doc-crumbs-current" aria-current="page" tabIndex={card ? 0 : undefined} {...c.props}>
          {name}
        </span>
        {c.card}
      </>
    );
  }
  return (
    <>
      <button type="button" className="doc-crumbs-folder" onClick={onClick} {...c.props}>
        {name}
      </button>
      {c.card}
    </>
  );
}
