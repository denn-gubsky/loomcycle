// sdlc/reviewer (team-examples bundle) as a code-js agent: the documented
// review with the bundle's own tools and Workspace grant. It reads
// the coder's diff with Git, and runs the tests on a workspace copy.
function run(input) {
  var text = JSON.stringify(JSON.parse(Git({ op: "diff", volume: "work" })));
  JSON.parse(Workspace({ op: "open", name: "review" }));
  File({ op: "copy", from: "volume:work:.", to: "workspace:review:repo" });
  var ex = JSON.parse(Workspace({ op: "exec", name: "review", cwd: "repo", command: "python3 -m unittest -q test_calc 2>&1; echo rc=$?" }));
  var seen = /a \+ b/.test(text), pass = /rc=0/.test(String(ex.output));
  return { final_text: "diff=" + (seen ? "seen" : "missing") +
    " tests=" + (pass ? "pass" : "fail") +
    " verdict=" + (seen && pass ? "approve" : "pushback") };
}
