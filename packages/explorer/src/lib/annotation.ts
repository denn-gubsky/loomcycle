import type { Annotation, AnnotationEdit, AnnotationLevel, BrowseScope, DocScope } from "../types";
import type { ExplorerDataLayer } from "./dataLayer";

// Annotations live only in the tenant scope: the runtime refuses to write one
// anywhere else and reads return nothing. The viewer therefore makes no call at
// all for another scope rather than asking a question whose answer is known.
export function annotationsApply(scope: DocScope): boolean {
  return scope === "tenant";
}

// The runtime's length caps, in characters (Unicode code points, as the runtime
// counts them). The editor checks them before sending so an over-long draft is
// named in place instead of coming back as a refusal.
export const BRIEF_MAX_CHARS = 240;
export const ABSTRACT_MAX_CHARS = 2000;

const LEVELS: readonly AnnotationLevel[] = ["branch", "document", "path"];

function isLevel(v: unknown): v is AnnotationLevel {
  return typeof v === "string" && (LEVELS as readonly string[]).includes(v);
}

// usableAnnotations keeps the records that describe `chunkId` and carry a
// brief, ordered branch → document → path. A record that would render as an
// empty panel is dropped, not padded: nothing here is ever written in place of
// a missing record.
export function usableAnnotations(records: unknown, chunkId: string): Annotation[] {
  if (!Array.isArray(records)) return [];
  return records
    .filter(
      (r): r is Annotation =>
        !!r &&
        typeof r === "object" &&
        (r as Annotation).chunk_id === chunkId &&
        isLevel((r as Annotation).level) &&
        typeof (r as Annotation).brief === "string" &&
        (r as Annotation).brief.trim() !== "",
    )
    .sort((a, b) => LEVELS.indexOf(a.level) - LEVELS.indexOf(b.level));
}

// loadAnnotations fetches a chunk's records. It answers [] without a call when
// the scope holds no annotations or the data layer cannot read them.
export async function loadAnnotations(
  data: Pick<ExplorerDataLayer, "documentGetAnnotation">,
  chunkId: string,
  scope: DocScope,
  browse?: BrowseScope,
): Promise<Annotation[]> {
  if (!annotationsApply(scope) || !data.documentGetAnnotation || !chunkId) return [];
  const resp = await data.documentGetAnnotation(chunkId, scope, browse);
  return usableAnnotations(resp?.annotations, chunkId);
}

// annotationLevelFor is the level a new annotation of a chunk takes: the root
// chunk stands for the whole document, any other chunk is a branch.
export function annotationLevelFor(isRoot: boolean): AnnotationLevel {
  return isRoot ? "document" : "branch";
}

// codePoints counts characters the way the runtime does (a surrogate pair is
// one character, not two).
export function codePoints(s: string): number {
  return [...s].length;
}

// annotationDraftProblem says what is wrong with an edit before it is sent, or
// undefined when it can go.
export function annotationDraftProblem(brief: string, abstract: string): string | undefined {
  if (brief.trim() === "") return "The brief is required.";
  if (codePoints(brief) > BRIEF_MAX_CHARS) return `The brief is longer than ${BRIEF_MAX_CHARS} characters.`;
  if (abstract.trim() === "") return "The abstract is required.";
  if (codePoints(abstract) > ABSTRACT_MAX_CHARS) {
    return `The abstract is longer than ${ABSTRACT_MAX_CHARS} characters.`;
  }
  return undefined;
}

// setAnnotationInput is the wire input of an operator's edit: always a manual
// record (the background walk then leaves it alone), and never an inputs hash,
// so the runtime stores the hash of the text as it is now.
export function setAnnotationInput(edit: AnnotationEdit, scope: DocScope): Record<string, unknown> {
  return {
    op: "set_annotation",
    scope,
    id: edit.id,
    level: edit.level,
    brief: edit.brief.trim(),
    abstract: edit.abstract.trim(),
    manual: true,
  };
}

// writtenByLabel renders a record's writer for the provenance line.
export function writtenByLabel(writtenBy: string | undefined): string | undefined {
  if (!writtenBy) return undefined;
  if (writtenBy.startsWith("run:")) return `run ${writtenBy.slice(4)}`;
  if (writtenBy.startsWith("principal:")) return writtenBy.slice(10);
  return writtenBy;
}

// writtenAtLabel renders the write time in the reader's locale; an unparseable
// value is shown as the runtime sent it.
export function writtenAtLabel(writtenAt: string | undefined): string | undefined {
  if (!writtenAt) return undefined;
  const t = Date.parse(writtenAt);
  return Number.isNaN(t) ? writtenAt : new Date(t).toLocaleString();
}

// ANNOTATION_ENTRY is the reserved Path name of a folder's annotation document.
export const ANNOTATION_ENTRY = "_annotation";
