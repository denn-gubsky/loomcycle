// sdlc/coder (team-examples bundle) as a code-js agent: the documented task
// with the bundle's own tools and Workspace grant. It copies the
// `work` volume's checkout into a workspace, fixes the bug in the volume with
// Write, copies the changed file in again, and runs the tests there.
function run(input) {
  var open = JSON.parse(Workspace({ op: "open", name: "build" }));
  var cp = JSON.parse(File({ op: "copy", from: "volume:work:.", to: "workspace:build:repo" }));
  Write({ path: "calc.py", volume: "work", content: "def add(a, b):\n    return a + b\n" });
  File({ op: "copy", from: "volume:work:calc.py", to: "workspace:build:repo/calc.py", overwrite: true });
  var ex = JSON.parse(Workspace({ op: "exec", name: "build", cwd: "repo", command: "python3 -m unittest -q test_calc 2>&1; echo rc=$?" }));
  return { final_text:
    "state=" + open.state +
    " copied=" + (cp.files > 0 ? "yes" : JSON.stringify(cp)) +
    " tests=" + (/rc=0/.test(String(ex.output)) ? "pass" : String(ex.output).trim().replace(/\s+/g, "_")) };
}
