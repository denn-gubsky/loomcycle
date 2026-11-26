import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type FocusEvent,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import { createPortal } from "react-dom";
import type { BrowseScope, ChunkRow, DocScope } from "../types";
import type { ExplorerDataLayer } from "../lib/dataLayer";
import { loadAnnotations } from "../lib/annotation";

// The annotation card (RFC GZ): one card for whatever names a document or a
// chunk — a Path tree row, a chunk outline row, a reference link, a breadcrumb
// segment, the header's annotation icon. It shows the title, the type and
// status, the brief and, when there is one, the abstract; with no annotation,
// the title and type only.
//
// It opens after a hover of CARD_DELAY_MS or on keyboard focus, and closes on
// leave, blur or Escape. A plain click never opens it: the click stays
// "select" or "follow". What a card cannot know at once (a brief read from the
// runtime) is loaded on first open and kept for the session.

export const CARD_DELAY_MS = 600;

export interface CardInfo {
  title: string;
  type?: string;
  status?: string;
  // path — where the thing sits in the Path tree, when that is useful.
  path?: string;
  brief?: string;
  abstract?: string;
  // stale — the annotation was written before the text last changed.
  stale?: boolean;
}

// CardSpec is what a trigger knows now, plus how to learn the rest. `load`
// runs on the first open; its answer fills in (never blanks) what is known,
// and is cached under `key` for the session. A load that fails leaves the card
// on what is known — the title at least.
export interface CardSpec extends CardInfo {
  key?: string;
  load?: () => Promise<Partial<CardInfo> | undefined>;
}

type CardCache = Map<string, Promise<Partial<CardInfo> | undefined>>;
const CardCacheContext = createContext<CardCache | null>(null);

// CardCacheProvider holds the session's loaded briefs for every card under it
// (the explorer root provides one).
export function CardCacheProvider({ children }: { children: ReactNode }) {
  const cache = useRef<CardCache>(new Map());
  return <CardCacheContext.Provider value={cache.current}>{children}</CardCacheContext.Provider>;
}

// CardScope is where the cards inside a document view read from: the
// document's scope and browse override (a reference link knows only ids).
export interface CardScope {
  data: ExplorerDataLayer;
  scope: DocScope;
  browse?: BrowseScope;
}
export const CardScopeContext = createContext<CardScope | null>(null);

function mergeInfo(base: CardInfo, extra: Partial<CardInfo> | undefined): CardInfo {
  if (!extra) return base;
  const out: CardInfo = { ...base };
  for (const k of Object.keys(extra) as (keyof CardInfo)[]) {
    const v = extra[k];
    if (v !== undefined && v !== "") (out as unknown as Record<string, unknown>)[k] = v;
  }
  return out;
}

export interface CardTrigger {
  // anchor: the element the card is placed against (never covered by it).
  anchorRef: (el: HTMLElement | null) => void;
  // hover: on the element the pointer rests on.
  hover: {
    onMouseEnter: () => void;
    onMouseLeave: () => void;
    onMouseDown: () => void;
  };
  // focus: on the element that takes the keyboard focus.
  focus: {
    onFocus: (e: FocusEvent<HTMLElement>) => void;
    onBlur: (e: FocusEvent<HTMLElement>) => void;
    onKeyDown: (e: KeyboardEvent<HTMLElement>) => void;
    onMouseDown: () => void;
    "aria-describedby"?: string;
  };
  card: ReactNode;
}

// useAnnotationCard wires one trigger. With no spec it does nothing.
export function useAnnotationCard(spec: CardSpec | undefined): CardTrigger {
  const shared = useContext(CardCacheContext);
  const local = useRef<CardCache>(new Map());
  const cache = shared ?? local.current;
  const specRef = useRef(spec);
  specRef.current = spec;

  const [open, setOpen] = useState(false);
  const [loaded, setLoaded] = useState<{ key: string | undefined; info?: Partial<CardInfo>; done: boolean }>({
    key: undefined,
    done: false,
  });
  const anchor = useRef<HTMLElement | null>(null);
  const cardEl = useRef<HTMLDivElement | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const pressed = useRef(false);
  const seq = useRef(0);
  const id = useId();

  const clear = () => {
    if (timer.current !== undefined) clearTimeout(timer.current);
    timer.current = undefined;
  };
  const close = useCallback(() => {
    clear();
    seq.current++;
    setOpen(false);
  }, []);

  const show = useCallback(() => {
    clear();
    const s = specRef.current;
    if (!s) return;
    setOpen(true);
    if (!s.load) {
      setLoaded({ key: s.key, done: true });
      return;
    }
    const n = ++seq.current;
    let p = s.key ? cache.get(s.key) : undefined;
    if (!p) {
      p = s.load().catch(() => undefined);
      if (s.key) cache.set(s.key, p);
    }
    setLoaded({ key: s.key, done: false });
    void p.then((info) => {
      if (seq.current === n) setLoaded({ key: s.key, info, done: true });
    });
  }, [cache]);

  useEffect(() => clear, []);

  // While open, a scroll anywhere moves the anchor from under the card.
  useEffect(() => {
    if (!open) return;
    const onScroll = () => close();
    window.addEventListener("scroll", onScroll, true);
    return () => window.removeEventListener("scroll", onScroll, true);
  }, [open, close]);

  // Place the card under its anchor, or over it when there is no room below,
  // inside the viewport either way, so it never covers what it describes.
  useLayoutEffect(() => {
    const el = cardEl.current;
    const a = anchor.current;
    if (!open || !el || !a) return;
    const r = a.getBoundingClientRect();
    const vw = window.innerWidth || document.documentElement.clientWidth;
    const vh = window.innerHeight || document.documentElement.clientHeight;
    const w = el.offsetWidth;
    const h = el.offsetHeight;
    const gap = 4;
    const below = r.bottom + gap;
    const top = below + h <= vh - 8 || r.top - gap - h < 8 ? below : r.top - gap - h;
    const left = Math.max(8, Math.min(r.left, vw - w - 8));
    el.style.top = `${Math.round(top)}px`;
    el.style.left = `${Math.round(left)}px`;
  });

  const anchorRef = useCallback((el: HTMLElement | null) => {
    anchor.current = el;
  }, []);
  const press = useCallback(() => {
    // A press is a click on its way: it selects or follows, and the focus it
    // brings is not a reason to show the card.
    pressed.current = true;
    setTimeout(() => {
      pressed.current = false;
    }, 0);
    close();
  }, [close]);

  const info = spec && open ? mergeInfo(spec, loaded.key === spec.key ? loaded.info : undefined) : undefined;
  const loading = !!spec?.load && open && !(loaded.key === spec.key && loaded.done);
  const host =
    typeof document === "undefined" ? null : (anchor.current?.closest(".loomcycle-explorer") ?? document.body);
  const card =
    info && host
      ? createPortal(
          <div className="ex-card" id={id} role="tooltip" ref={cardEl}>
            <CardBody info={info} loading={loading} />
          </div>,
          host,
        )
      : null;

  return {
    anchorRef,
    hover: {
      onMouseEnter: () => {
        if (!specRef.current || pressed.current) return;
        clear();
        timer.current = setTimeout(show, CARD_DELAY_MS);
      },
      onMouseLeave: close,
      onMouseDown: press,
    },
    focus: {
      onFocus: (e) => {
        if (e.target !== e.currentTarget || pressed.current) return;
        show();
      },
      onBlur: (e) => {
        if (e.target === e.currentTarget) close();
      },
      onKeyDown: (e) => {
        if (e.key === "Escape" && open) close();
      },
      onMouseDown: press,
      "aria-describedby": info ? id : undefined,
    },
    card,
  };
}

// CardBody is the card's content, also shown in place (a folder's detail pane
// is its card).
export function CardBody({ info, loading }: { info: CardInfo; loading?: boolean }) {
  const meta = [info.type, info.status].filter(Boolean).join(" · ");
  return (
    <>
      <div className="ex-card-title">{info.title}</div>
      {(meta || info.stale) && (
        <div className="ex-card-meta">
          {meta}
          {info.stale && <span className="ex-card-stale">{meta ? " · " : ""}annotation out of date</span>}
        </div>
      )}
      {info.path && <div className="ex-card-path">{info.path}</div>}
      {info.brief && <p className="ex-card-brief">{info.brief}</p>}
      {info.abstract && <p className="ex-card-abstract">{info.abstract}</p>}
      {loading && !info.brief && <p className="ex-card-loading">Loading…</p>}
    </>
  );
}

// rowSummary reads a chunk row's annotation when the chunk list carries one
// (RFC HA's query_chunks `with_summary` puts `summary` on the rows; a runtime
// before it leaves it out and the card reads the chunk's annotation on first
// open).
export function rowSummary(row: ChunkRow): Partial<CardInfo> | undefined {
  const s = row.summary as { brief?: unknown; abstract?: unknown; stale?: unknown } | undefined;
  if (!s || typeof s.brief !== "string") return undefined;
  return {
    brief: s.brief,
    abstract: typeof s.abstract === "string" ? s.abstract : undefined,
    stale: s.stale === true || undefined,
  };
}

// chunkCard is the card of a chunk: its own fields now, its annotation from
// the chunk list when the list carries it, else read on first open.
// `listed` says the list carries the briefs (some row has one): a row without
// one then has no annotation of its own, and nothing is read for it.
export function chunkCard(row: ChunkRow, src: CardScope | null, listed?: boolean): CardSpec {
  const base: CardSpec = { title: row.title || "(untitled)", type: row.type, status: row.status };
  const inline = rowSummary(row);
  if (inline) return { ...base, ...inline };
  if (!src || listed) return base;
  return {
    ...base,
    key: `${src.scope}:${src.browse?.scopeId ?? ""}:${src.browse?.tenant ?? ""}:chunk:${row.id}`,
    load: async () => {
      const recs = await loadAnnotations(src.data, row.id, src.scope, src.browse);
      const a = recs[0];
      return a ? { brief: a.brief, abstract: a.abstract, stale: a.stale || undefined } : undefined;
    },
  };
}

// documentCard is the card of a document known by id (a reference into
// another document, the breadcrumb's last segment): its summary on first
// open. The brief there is the document's, and exists only for an annotated
// tenant-scope document; without it the card keeps the title it was given.
export function documentCard(
  documentId: string,
  title: string,
  src: Pick<CardScope, "data" | "scope" | "browse"> | null,
  known?: Partial<CardInfo>,
): CardSpec {
  const base: CardSpec = { title, ...known };
  if (!src || known?.brief !== undefined) return base;
  return {
    ...base,
    key: `${src.scope}:${src.browse?.scopeId ?? ""}:${src.browse?.tenant ?? ""}:doc:${documentId}`,
    load: async () => {
      const r = await src.data.documentsSummary({ documentIds: [documentId] }, src.scope, src.browse);
      const d = r.documents?.find((x) => x.document_id === documentId);
      return d ? { title: d.title, type: d.type, status: d.status, brief: d.brief } : undefined;
    },
  };
}

// useButtonCard wires a trigger that is one focusable element (a button), its
// own anchor; spread `props` on it and render `card` beside it.
export function useButtonCard(spec: CardSpec | undefined) {
  const t = useAnnotationCard(spec);
  return {
    props: {
      ref: t.anchorRef,
      onMouseEnter: t.hover.onMouseEnter,
      onMouseLeave: t.hover.onMouseLeave,
      onMouseDown: t.hover.onMouseDown,
      onFocus: t.focus.onFocus,
      onBlur: t.focus.onBlur,
      onKeyDown: t.focus.onKeyDown,
      "aria-describedby": t.focus["aria-describedby"],
    },
    card: t.card,
  };
}
