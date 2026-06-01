package codejs

import (
	"fmt"

	"github.com/dop251/goja"
)

// hardenSandbox enforces the RFC J Decision 2 capability boundary AND installs
// the ambient-determinism hooks (Appendix B) on a fresh runtime, BEFORE any
// operator code runs.
//
// Capability boundary. goja's default global surface is already narrow — no
// fetch/XHR, no require, no filesystem, no setTimeout/setInterval (those only
// exist via goja_nodejs, which we do NOT wire). The active step is removing
// the dynamic-code-evaluation globals: eval and the Function constructor are
// deleted, so eval("…") / Function("…") throw ReferenceError. (Known limit,
// documented in the help topic: the Function constructor is still reachable
// via (function(){}).constructor; the delete stops the naive paths. The real
// boundary is that reconstructed code still cannot reach any capability we
// did not bind — no fetch, no fs, no process to escape TO.)
//
// Ambient determinism (always on). The replay execution model (Appendix B)
// re-executes the pure-JS portion of run() every turn. For replay to be
// correct, every ambient value the JS reads must reproduce identically. Tool
// results already do (they are replayed from the transcript). The remaining
// sources are the clock and the RNG, so we hook them:
//
//   - Math.random() → a PRNG seeded from the per-run seed; the same run
//     regenerates the identical sequence on every replay.
//   - Date.now() → the per-run anchor (real wall-clock at run start) plus a
//     monotonic per-call offset, so time advances within a run yet every
//     replay reproduces it.
//   - new Date() (no args) → routed through the hooked Date.now() via a
//     thin Date wrapper that leaves new Date(ms)/Date.parse/Date.UTC intact.
//
// This makes the no-I/O sandbox fully deterministic given (transcript, seed,
// anchor) — replay cannot diverge. Operators who need a true per-call
// wall-clock value read it from a tool (recorded), the durable-execution
// discipline. seed/anchor are per-run by default and fixed across runs only
// under LOOMCYCLE_CODE_AGENTS_DETERMINISTIC=1 (cross-run reproducibility).
func hardenSandbox(rt *goja.Runtime, seed uint32, anchorMs int64) {
	g := rt.GlobalObject()
	_ = g.Delete("eval")
	_ = g.Delete("Function")
	installAmbientHooks(rt, seed, anchorMs)
}

// ambientPrelude is the JS installed on every runtime to make the clock + RNG
// deterministic. %d slots: seed (uint32), anchorMs (int64). The LCG constants
// are the classic glibc values — quality is irrelevant, only reproducibility.
// The Date wrapper forwards every shape except no-arg construction to the real
// Date, and shares its prototype so `instanceof Date` holds.
const ambientPrelude = `
(function () {
	var __seed = %d >>> 0;
	Math.random = function () {
		__seed = (__seed * 1103515245 + 12345) >>> 0;
		return (__seed & 0x7fffffff) / 0x7fffffff;
	};
	var __t = %d;
	var RealDate = Date;
	function now() { return __t++; }
	function FakeDate(a, b, c, d, e, f, g) {
		if (!(this instanceof FakeDate)) { return new RealDate(now()).toString(); }
		switch (arguments.length) {
			case 0: return new RealDate(now());
			case 1: return new RealDate(a);
			case 2: return new RealDate(a, b);
			case 3: return new RealDate(a, b, c);
			case 4: return new RealDate(a, b, c, d);
			case 5: return new RealDate(a, b, c, d, e);
			case 6: return new RealDate(a, b, c, d, e, f);
			default: return new RealDate(a, b, c, d, e, f, g);
		}
	}
	FakeDate.prototype = RealDate.prototype;
	FakeDate.now = now;
	FakeDate.parse = RealDate.parse;
	FakeDate.UTC = RealDate.UTC;
	Date = FakeDate;
})();
`

func installAmbientHooks(rt *goja.Runtime, seed uint32, anchorMs int64) {
	// A failure here is a host bug (the prelude is a constant), not operator
	// input — panic so it surfaces in tests rather than silently leaving real
	// time/randomness in a run we promised to replay deterministically.
	if _, err := rt.RunString(fmt.Sprintf(ambientPrelude, seed, anchorMs)); err != nil {
		panic(fmt.Sprintf("codejs: ambient-determinism prelude failed: %v", err))
	}
}
