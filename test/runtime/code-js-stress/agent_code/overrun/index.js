// overrun — 25 sequential incrs. Each tool call is one loop turn, so this
// asks for 25 sequential calls — well past the old 16-iteration default. A
// code-agent is EXEMPT from the MaxIterations soft-cap (it is bounded by the
// run timeout, not an iteration count), so run() reaches its final_text: this
// suite asserts the run COMPLETES (end_turn) with all 25 incrs executed.
function run(input) {
  for (var i = 0; i < 25; i++) {
    Memory.incr({ scope: "agent", key: "ovr", delta: 1 });
  }
  return { final_text: "completed 25 sequential calls (no iteration cap)" };
}
