import type { EventPayload } from "../api";

// errorFrameStatus is the status a run's `error` frame ends it in. A run
// stopped before it finished (an API cancel, its caller leaving) ends its
// stream on an error frame classified "cancelled"; its row is cancelled, so
// the terminal says so rather than "failed". Only the classification is read,
// never the error's text.
export function errorFrameStatus(ev: EventPayload): "failed" | "cancelled" {
  return ev.error_info?.category === "cancelled" ? "cancelled" : "failed";
}
