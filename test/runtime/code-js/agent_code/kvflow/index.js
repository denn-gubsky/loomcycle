// kvflow — exercises the Memory multi-op meta-tool from a code-js agent over
// the real loop + Appendix-B replay engine. Each tool call is one loop turn;
// run() re-executes from the top on every turn, replaying the recorded
// results, so the value read by get() is the value written by an earlier
// turn's set(). set/incr/get/list all dispatch through the loop's real
// Memory tool against sqlite.
//
// incr and list are the ops that were UNREACHABLE from code-js before the
// meta-tool generic op passthrough fix — calling them here is the runtime
// regression that they are now bound.
function run(input) {
  Memory.set({ scope: "user", key: "favorite_color", value: "purple" });
  var afterIncr = Memory.incr({ scope: "agent", key: "run_count", delta: 1 });
  var got = Memory.get({ scope: "user", key: "favorite_color" });
  var keys = Memory.list({ scope: "user" });

  var color = (got && got.value) ? got.value : "MISSING";
  var count = (afterIncr && afterIncr.value !== undefined && afterIncr.value !== null)
    ? afterIncr.value : "MISSING";
  var nKeys = (keys && keys.entries) ? keys.entries.length : -1;

  return { final_text: "color=" + color + " count=" + count + " user_keys=" + nKeys };
}
