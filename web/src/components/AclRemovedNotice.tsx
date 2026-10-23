import { useMemo } from "react";
import type { TranscriptEvent } from "../api";
import { aclRemovedTools } from "../lib/aclAccess";

// AclRemovedNotice tells the operator which tools the caller's access-control
// rules withheld from this run (the run-start capability_inert events with gate
// "acl"). Informational, not an error: the run proceeds without them, and the
// model never sees them in its tool schema.
export default function AclRemovedNotice({ events }: { events: TranscriptEvent[] }) {
  const removed = useMemo(() => aclRemovedTools(events), [events]);
  if (removed.length === 0) return null;
  return (
    <div className="live-run-acl-notice" role="note">
      Your access rules withheld {removed.length === 1 ? "a tool" : `${removed.length} tools`} from this run:{" "}
      {removed.map((t, i) => (
        <span key={t}>
          {i > 0 && ", "}
          <code>{t}</code>
        </span>
      ))}
      . The agent cannot call {removed.length === 1 ? "it" : "them"}.
    </div>
  );
}
