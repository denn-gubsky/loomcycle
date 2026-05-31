package codejs

import (
	"fmt"

	"github.com/dop251/goja"
)

// hardenSandbox enforces the RFC J Decision 2 capability boundary on a fresh
// runtime, BEFORE any operator code runs. goja's default global surface is
// already narrow — there is no fetch/XHR, no require, no filesystem, no
// setTimeout/setInterval (those only exist via goja_nodejs, which we do NOT
// wire). So the active step is removing the dynamic-code-evaluation globals:
//
//   - eval        — deleted → eval("…") throws ReferenceError.
//   - Function    — deleted → Function("…")() throws ReferenceError.
//
// KNOWN LIMITATION (documented in the help topic): the Function constructor
// is still reachable via the prototype chain ((function(){}).constructor),
// which goja does not let us sever without forking it. The delete stops the
// naive `Function(...)` / `eval(...)` paths; it is defense-in-depth, not a
// hard guarantee. The real boundary is that even reconstructed code cannot
// reach any capability we did not bind — there is no fetch, no fs, no process
// to escape TO. Operator-provided code runs in the operator's own trust
// posture (same as the Bash tool); the sandbox protects loomcycle from the
// runtime handing the JS more than allowed_tools granted, not the operator
// from their own logic.
func hardenSandbox(rt *goja.Runtime, deterministic bool, token string) {
	g := rt.GlobalObject()
	_ = g.Delete("eval")
	_ = g.Delete("Function")

	if deterministic {
		seedDeterminism(rt, token)
	}
}

// seedDeterminism replaces the two ambient non-determinism sources with
// seeded stand-ins (RFC J Decision 13, opt-in via
// LOOMCYCLE_CODE_AGENTS_DETERMINISTIC=1) so a code-agent's pure-JS control
// flow is reproducible for testing/replay. It does NOT make MCP tool calls
// deterministic — upstream is upstream (Decision 13 caveat).
//
//   - Date.now() returns a fixed epoch.
//   - Math.random() is a seeded LCG, so a run yields the same sequence each
//     time. The seed folds in the run token so distinct runs differ while a
//     single run replays identically.
func seedDeterminism(rt *goja.Runtime, token string) {
	// A fixed wall-clock anchor (2023-11-14T22:13:20Z). Stable across runs so
	// snapshot/replay comparisons don't drift on time.
	const fixedEpochMs = 1700000000000

	var seed uint32 = 0x9e3779b9
	for _, r := range token {
		seed = seed*31 + uint32(r)
	}

	// Inject via JS so Date.now / Math.random are replaced in-place on the
	// existing built-ins (operator code sees the standard names). The LCG
	// constants are the classic glibc values; quality is irrelevant — only
	// reproducibility matters here.
	prelude := fmt.Sprintf(`
		Date.now = function () { return %d; };
		Math.random = (function () {
			var s = %d >>> 0;
			return function () {
				s = (s * 1103515245 + 12345) >>> 0;
				return (s & 0x7fffffff) / 0x7fffffff;
			};
		})();
	`, fixedEpochMs, seed)
	// A failure here is a host bug (the prelude is a constant), not operator
	// input — panic so it surfaces in tests rather than silently leaving real
	// time/randomness in a run the operator asked to be deterministic.
	if _, err := rt.RunString(prelude); err != nil {
		panic(fmt.Sprintf("codejs: deterministic prelude failed: %v", err))
	}
}
