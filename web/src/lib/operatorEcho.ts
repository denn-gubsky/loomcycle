// The run terminal echoes the operator's own messages as it sends them, and
// remembers each echoed text until the server's frame for that same message
// arrives — which it then drops, so each operator message shows once.
//
// Two kinds of echo, because they come back differently:
// - a STEER (a message to a running or parked run, POST /v1/runs/{id}/input)
//   comes back live as the run drains it, a `steer` frame with source "api",
//   or replayed from the store on a re-attach (source "replay");
// - a PROMPT that starts or continues a run comes back as no live steer at
//   all — only as a replay, when the stream reads the run from its stored
//   events (a re-attach, or a duplicate request answered with the existing
//   run). Matched against a live steer, its echo would wait forever and hide
//   the next live steer of the same text from another tab or a bridge.

// A minimal shape of a streamed frame: only what this decision reads.
export interface SteerFrame {
  type?: string;
  user_input?: { text?: string; source?: string };
}

// Echoes is what this client echoed and has not yet seen come back.
export interface Echoes {
  steers: string[];
  prompts: string[];
}

export function newEchoes(): Echoes {
  return { steers: [], prompts: [] };
}

// take removes one text from queue, reporting whether it was there.
function take(queue: string[], text: string): boolean {
  const idx = queue.indexOf(text);
  if (idx === -1) return false;
  queue.splice(idx, 1);
  return true;
}

// dropEchoedSteer reports whether ev is the server's copy of a message this
// client already echoed, consuming that echo so a genuine repeat later still
// shows. A client that echoed nothing (a cold re-attach from another device)
// drops nothing. Any other source — an agent's own child, a retune — is never
// the operator's message, whatever its text.
export function dropEchoedSteer(echoes: Echoes, ev: SteerFrame): boolean {
  if (ev.type !== "steer") return false;
  const text = ev.user_input?.text ?? "";
  switch (ev.user_input?.source) {
    case "api":
      return take(echoes.steers, text);
    case "replay":
      return take(echoes.steers, text) || take(echoes.prompts, text);
  }
  return false;
}
