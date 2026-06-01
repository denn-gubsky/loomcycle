// counter — 8 sequential atomic incrs on a SHARED agent-scope key. Each incr
// is one loop turn; run() re-executes from the top every turn, replaying the
// 0..k-1 recorded results and dispatching only the k-th. 8 < the 16-iteration
// default so the run completes end_turn — proving an 8-turn replay chain is
// divergence-free. Fired concurrently across many runs, the shared "n" must
// land at exactly 8*N (atomic incr + per-run goja isolation, no lost update).
function run(input) {
  var last;
  for (var i = 0; i < 8; i++) {
    last = Memory.incr({ scope: "agent", key: "n", delta: 1 });
  }
  return { final_text: "done last=" + (last && last.value !== undefined ? last.value : "?") };
}
