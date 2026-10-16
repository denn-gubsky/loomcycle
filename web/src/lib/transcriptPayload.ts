import type { UserInputPayload } from "../api";

// promptSegments reads a user_input row's payload as its prompt segments.
//
// A no-trace run stores each event as a marker — {type, retained:false, bytes}
// — so its user_input payload is an object, not the segment array. Reading it
// as an array threw inside render and blanked the whole runs page. Anything
// that is not an array of segments reads as no segments.
export function promptSegments(payload: unknown): UserInputPayload[] {
  if (!Array.isArray(payload)) return [];
  return payload.filter(
    (s): s is UserInputPayload => typeof s === "object" && s !== null && typeof s.role === "string",
  );
}
