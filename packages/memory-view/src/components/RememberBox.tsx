import { useState } from "react";
import type { MemoryScope } from "../types";
import { useMemoryData } from "../lib/dataLayer";

// RememberBox — write down something a PERSON wants remembered (RFC CF §6).
//
// WHAT YOU TYPE IS BOTH THE FACT AND ITS EVIDENCE. The statement is stored verbatim and
// becomes its own source span, so it is checkable rather than merely asserted — which is
// why the placeholder asks for a sentence to record, not an instruction about one.
// "Remember that Ada runs the platform team" stores that whole phrase, including the
// word "remember"; "Ada runs the platform team" is the fact.
//
// ADDITIVE ONLY. There is no "forget" here: an instruction that deletes on a fuzzy match
// is how data disappears quietly. Removing a fact is a verdict on the fact itself (which
// withholds rather than deletes, and is reversible).
export default function RememberBox({
  scope,
  scopeId,
  onRemembered,
}: {
  scope: MemoryScope;
  scopeId?: string;
  onRemembered?: () => void;
}) {
  const data = useMemoryData();
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [flash, setFlash] = useState<string | null>(null);

  const save = async () => {
    const t = text.trim();
    if (!t || busy) return;
    setBusy(true);
    try {
      await data.remember(scope, t, scopeId ? { scopeId } : undefined);
      setText("");
      setErr(null);
      setFlash("remembered");
      onRemembered?.();
      window.setTimeout(() => setFlash(null), 2000);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="fact-remember">
      <input
        value={text}
        placeholder="remember a fact — one sentence, as you want it recorded"
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter") void save();
        }}
      />
      <button type="button" onClick={() => void save()} disabled={busy || !text.trim()}>
        {busy ? "saving…" : "remember"}
      </button>
      {flash && <span className="fact-remember-flash">{flash}</span>}
      {err && <span className="err">{err}</span>}
    </div>
  );
}
