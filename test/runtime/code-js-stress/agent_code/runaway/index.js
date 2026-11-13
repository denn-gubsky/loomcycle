// runaway — an unbounded tool-call loop. With the MaxIterations cap disabled
// for code-agents, the ONLY thing that stops this is the run-level wall-clock
// timeout (providers.code-js.options.run_timeout_seconds), enforced as a whole-run
// deadline. The suite asserts the run terminates (NOT end_turn) shortly after
// the timeout — proving "the timeout is the bound" rather than the run hanging
// forever.
function run(input) {
  for (;;) {
    Memory.incr({ scope: "agent", key: "runaway", delta: 1 });
  }
}
