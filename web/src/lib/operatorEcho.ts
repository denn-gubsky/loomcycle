// The run terminal echoes the operator's own messages as it sends them
// (the opening prompt, each steer, each continuation), and remembers each
// echoed text until the server's frame for that same message arrives — which
// it then drops, so each operator message shows once.

// A minimal shape of a streamed frame: only what this decision reads.
export interface SteerFrame {
  type?: string;
  user_input?: { text?: string; source?: string };
}

// The server's copy arrives as a `steer` frame in one of two forms: "api", the
// live frame of a message the run drained from POST /v1/runs/{id}/input, and
// "replay", the stored operator turn a re-attach (GET /v1/runs/{id}/stream)
// replays. Any other source — an agent's own child, a retune — is never the
// operator's message, whatever its text.
const operatorSources = new Set(["api", "replay"]);

// dropEchoedSteer reports whether ev is the server's copy of a message this
// client already echoed, consuming that echo so a genuine repeat later still
// shows. A client that echoed nothing (a cold re-attach from another device)
// drops nothing.
export function dropEchoedSteer(echoed: string[], ev: SteerFrame): boolean {
  if (ev.type !== "steer" || !operatorSources.has(ev.user_input?.source ?? "")) return false;
  const idx = echoed.indexOf(ev.user_input?.text ?? "");
  if (idx === -1) return false;
  echoed.splice(idx, 1);
  return true;
}
