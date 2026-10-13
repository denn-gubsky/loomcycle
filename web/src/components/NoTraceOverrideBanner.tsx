import { useEffect, useState } from "react";
import { listRetentionOverrides, RetentionOverride } from "../api";

// While an operator has lifted the no-trace mode of an agent, runs of that
// agent are being stored. That is a deliberate, temporary state somebody
// switched on to debug, and it should be hard to forget: this banner sits
// above every page for as long as one is in force, and says until when.
//
// Rendered for an admin or a tenant operator only — the route refuses a
// member, and what is being debugged is not theirs to see.
const POLL_MS = 30_000;

export default function NoTraceOverrideBanner() {
  const [overrides, setOverrides] = useState<RetentionOverride[]>([]);

  useEffect(() => {
    let cancelled = false;
    const poll = async () => {
      try {
        const res = await listRetentionOverrides();
        if (!cancelled) setOverrides(res.overrides ?? []);
      } catch {
        // Not permitted, or no store: there is nothing to show.
        if (!cancelled) setOverrides([]);
      }
    };
    poll();
    const t = setInterval(poll, POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, []);

  if (overrides.length === 0) return null;
  return (
    <div className="no-trace-override-banner" role="status">
      <strong>Runs are being stored for {overrides.length === 1 ? "an agent" : "agents"} that normally store none.</strong>{" "}
      {overrides.map((o, i) => (
        <span key={`${o.tenant}/${o.agent}`}>
          {i > 0 && "; "}
          <code>{o.agent}</code>
          {o.tenant ? ` (tenant ${o.tenant})` : ""} until {new Date(o.until).toLocaleString()}
          {o.set_by ? `, set by ${o.set_by}` : ""}
        </span>
      ))}
      . Delete the runs made meanwhile when the debugging is done.
    </div>
  );
}
