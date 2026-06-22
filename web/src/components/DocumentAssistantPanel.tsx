import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { type ChunkDetail, type ChunkRow, type DocScope, type LibraryEntry, listLibraryAgents } from "../api";
import { usePrincipal } from "./Layout";
import { useRunStream } from "../hooks/useRunStream";
import { buildChunkTree, type ChunkNode } from "./DocumentChunkTree";
import LiveRunPane from "./LiveRunPane";

// DocumentAssistantPanel is the RFC AM Phase 3 Document Assistant: instead of a
// manual structural-editing UI, the operator types instructions and a
// `doc-manager` agent performs the Document ops (semantic import, move, link,
// delete). It reuses useRunStream + LiveRunPane wholesale.
//
// Lifecycle: lazy — the interactive run is spawned on the FIRST instruction
// (token-responsible: opening a document doesn't start a run). The first
// instruction is prefixed with a `[context]` block carrying the document
// OUTLINE (every chunk's title/type/status/id) + the SELECTED chunk's full
// content (title/fields/body), so the agent is grounded from turn one instead
// of having to blind-probe. Each steer keeps a lighter preamble with the live
// selection's current content. The run's user_id is the principal's subject, so
// the agent's Document tool (scope=user) operates on the same documents the
// viewer shows. onChanged fires at each turn boundary to refresh.

const ASSISTANT_AGENT = "doc-manager";
// Cap the injected outline so a huge document can't blow up the first prompt.
const MAX_OUTLINE_CHUNKS = 200;

export interface DocumentAssistantPanelProps {
  documentId: string;
  scope: DocScope;
  selectedChunkId?: string;
  // The viewer's already-loaded structure + selected-chunk content, injected as
  // start context so the agent reads the document + selected chunk on turn one.
  chunks: ChunkRow[];
  selectedChunk: ChunkDetail | null;
  onChanged: () => void;
}

// outlineText renders the chunk hierarchy as an indented, body-less list
// (title + id + type/status), marking the selected chunk. Bounded by
// MAX_OUTLINE_CHUNKS so a large document degrades to "… use query_chunks".
function outlineText(chunks: ChunkRow[], selectedId?: string): string {
  const roots = buildChunkTree(chunks);
  const lines: string[] = [];
  let truncated = false;
  const walk = (n: ChunkNode, depth: number) => {
    if (lines.length >= MAX_OUTLINE_CHUNKS) {
      truncated = true;
      return;
    }
    const r = n.row;
    const meta = [r.type, r.status].filter(Boolean).join("/");
    const sel = r.id === selectedId ? "  ← selected" : "";
    lines.push(`${"  ".repeat(depth)}- ${r.title || "(untitled)"} [${r.id}${meta ? " " + meta : ""}]${sel}`);
    n.children.forEach((c) => walk(c, depth + 1));
  };
  roots.forEach((n) => walk(n, 0));
  if (truncated) lines.push(`  … (${chunks.length} chunks total — use Document op=query_chunks for the rest)`);
  return lines.join("\n");
}

// selectedBlock renders one chunk's full content for the agent to act on.
function selectedBlock(c: ChunkDetail): string {
  const meta = [c.type && `type=${c.type}`, c.status && `status=${c.status}`, `rev=${c.revision}`]
    .filter(Boolean)
    .join(" ");
  const fields = c.fields ? `\nfields: ${JSON.stringify(c.fields)}` : "";
  return `selected chunk [${c.id}] ${meta}\ntitle: ${c.title}${fields}\nbody:\n${c.body || "(empty)"}`;
}

export default function DocumentAssistantPanel({
  documentId,
  scope,
  selectedChunkId,
  chunks,
  selectedChunk,
  onChanged,
}: DocumentAssistantPanelProps) {
  const principal = usePrincipal();
  const run = useRunStream();
  const [available, setAvailable] = useState<boolean | null>(null); // null = checking
  const [started, setStarted] = useState(false);
  const [draft, setDraft] = useState("");

  // Read the live selection + loaded content through refs so the send closures
  // always reflect the current viewer state (selection changes between turns).
  const chunkRef = useRef<string | undefined>(selectedChunkId);
  const chunksRef = useRef<ChunkRow[]>(chunks);
  const selectedRef = useRef<ChunkDetail | null>(selectedChunk);
  useEffect(() => {
    chunkRef.current = selectedChunkId;
    chunksRef.current = chunks;
    selectedRef.current = selectedChunk;
  }, [selectedChunkId, chunks, selectedChunk]);

  // Graceful degrade: confirm the assistant agent is registered.
  useEffect(() => {
    let cancelled = false;
    listLibraryAgents()
      .then((r) => {
        if (cancelled) return;
        const ok = (r.entries ?? []).some(
          (e: LibraryEntry) => e.name === ASSISTANT_AGENT && (e.in_static || e.in_substrate),
        );
        setAvailable(ok);
      })
      .catch(() => !cancelled && setAvailable(false));
    return () => {
      cancelled = true;
    };
  }, []);

  // contextPreamble grounds the agent in the live document state. On the FIRST
  // turn it includes the full outline + selected-chunk content; later turns keep
  // a lighter block (the selected chunk's current content) — the agent already
  // has the outline and re-reads via query_chunks as it works.
  const contextPreamble = useCallback((first: boolean) => {
    const id = chunkRef.current;
    const lines = [`[context] document_id=${documentId} scope=${scope}${id ? ` selected_chunk_id=${id}` : ""}`];
    if (first && chunksRef.current.length > 0) {
      lines.push("", "document outline (titles only — get_chunk for any body):", outlineText(chunksRef.current, id));
    }
    if (selectedRef.current && selectedRef.current.id === id) {
      lines.push("", selectedBlock(selectedRef.current));
    }
    lines.push("[/context]");
    return lines.join("\n");
  }, [documentId, scope]);

  const send = useCallback(
    (text: string) => {
      const t = text.trim();
      if (!t) return;
      if (!started) {
        run.start({
          agent: ASSISTANT_AGENT,
          prompt: `${contextPreamble(true)}\n\n${t}`,
          user_id: principal?.subject || undefined,
          interactive: true,
          metadata: { document_id: documentId, scope, selected_chunk_id: chunkRef.current || "" },
        });
        setStarted(true);
      } else {
        run.send(`${contextPreamble(false)}\n\n${t}`);
      }
    },
    [started, run, contextPreamble, principal, documentId, scope],
  );

  // Refresh the viewer at each turn boundary: when the agent parks awaiting
  // input, or when the run completes. Edge-triggered so it fires once per turn.
  const prevAwait = useRef(false);
  useEffect(() => {
    if (run.awaitingInput && !prevAwait.current) onChanged();
    prevAwait.current = run.awaitingInput;
  }, [run.awaitingInput, onChanged]);
  const prevDone = useRef(false);
  useEffect(() => {
    const done = run.status === "completed";
    if (done && !prevDone.current) onChanged();
    prevDone.current = done;
  }, [run.status, onChanged]);

  if (available === false) {
    return (
      <div className="doc-assistant doc-assistant-hint">
        <p>
          The Document Assistant needs the <code>{ASSISTANT_AGENT}</code> agent. Enable the{" "}
          <code>bundles/document-agent</code> bundle (see its README), then reload.
        </p>
      </div>
    );
  }

  if (started) {
    return (
      <div className="doc-assistant">
        <LiveRunPane
          events={run.events}
          status={run.status}
          agentId={run.agentId}
          sessionId={run.sessionId}
          runId={run.runId}
          error={run.error}
          onCancel={run.cancel}
          onSend={send}
          awaitingInput={run.awaitingInput}
          lastUsage={run.lastUsage}
          pendingInterrupt={run.pendingInterrupt}
          onAnswerInterrupt={run.answerInterrupt}
          compact
        />
      </div>
    );
  }

  const submit = (e: FormEvent) => {
    e.preventDefault();
    send(draft);
    setDraft("");
  };
  return (
    <div className="doc-assistant">
      <form className="doc-assistant-start" onSubmit={submit}>
        <textarea
          className="doc-assistant-input"
          rows={2}
          value={draft}
          placeholder={
            available === null
              ? "Checking assistant…"
              : "Ask the assistant to restructure, import, link, or clean up this document…"
          }
          disabled={available !== true}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              send(draft);
              setDraft("");
            }
          }}
        />
        <button type="submit" className="primary" disabled={available !== true || !draft.trim()}>
          send
        </button>
      </form>
    </div>
  );
}
