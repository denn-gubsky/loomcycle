// wsflow — drives the Workspace tool from a code-js agent over the real loop
// on the runtime's local worker (workspace.mode local): open a workspace,
// write a Python script into it, run it, read the file the script wrote, and
// end. The workspace is not released here: the run's end releases it, and the
// driver checks its directory is gone from the worker's disk afterwards.
// Workspace is a single-op tool, called as a function with an `op` field.
function run(input) {
  var open = JSON.parse(Workspace({ op: "open", name: "py", toolchain: "python" }));
  Workspace({ op: "write", name: "py", path: "square.py",
    content: "import sys\nn = int(sys.argv[1])\nopen('out.txt', 'w').write(str(n * n) + '\\n')\nprint('squared', n)\n" });
  var ex = JSON.parse(Workspace({ op: "exec", name: "py", command: "python3 square.py 12" }));
  var out = Workspace({ op: "read", name: "py", path: "out.txt" });
  var list = JSON.parse(Workspace({ op: "list" }));
  return { final_text:
    "state=" + open.state +
    " engine=" + open.engine +
    " exit=" + ex.exit_code +
    " printed=" + String(ex.output).trim().replace(/\s+/g, "_") +
    " result=" + String(out).trim() +
    " listed=" + (list.workspaces ? list.workspaces.length : -1) };
}
