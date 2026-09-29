// parentRunHref is the run detail header's "parent run" target: the run
// terminal attached to that exact run. The /agents view is keyed by agent id,
// and an agent id is reused by every run of that agent, so linking the parent
// there could open a different run of the same agent. /run?attach= replays one
// run by its run id, finished or live.
export function parentRunHref(parentRunId: string): string {
  return `/run?attach=${encodeURIComponent(parentRunId)}`;
}
