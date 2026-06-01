// overrun — 25 sequential incrs. Each tool call is one loop turn, so this
// asks for 25 sequential calls against the 16-iteration default ceiling. The
// run must terminate with stop_reason=max_iterations BEFORE the loop finishes
// (run() never returns its final_text). For a code-agent MaxIterations is a
// HARD ceiling on sequential tool calls, not a soft model-chatter cap — this
// suite asserts that behaviour and the operator diagnostic that names it.
function run(input) {
  for (var i = 0; i < 25; i++) {
    Memory.incr({ scope: "agent", key: "ovr", delta: 1 });
  }
  return { final_text: "UNREACHABLE: completed 25 calls under the cap" };
}
