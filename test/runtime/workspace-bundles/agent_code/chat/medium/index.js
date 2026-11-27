// chat/medium (chat bundle) as a code-js agent: what its prompt tells it to do
// to run code, with the bundle's own Workspace grant and Bash and Bashbox
// withheld. It opens a workspace, writes a script, and runs it there.
function run(input) {
  var open = JSON.parse(Workspace({ op: "open", name: "run" }));
  Workspace({ op: "write", name: "run", path: "fib.py", content:
    "a, b = 0, 1\nfor _ in range(20):\n    a, b = b, a + b\nprint('fib20', a)\n" });
  var ex = JSON.parse(Workspace({ op: "exec", name: "run", command: "python3 fib.py" }));
  return { final_text: "state=" + open.state + " " + String(ex.output).trim().replace(/\s+/g, "=") };
}
