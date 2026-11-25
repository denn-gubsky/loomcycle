// The egress suite's agent (egress.sh): open a workspace with the routes its
// grant names, run the command the prompt gives, and report the workspace's
// engine and routes, the exit code and the output. A command that exits
// non-zero comes back as an error result, which is reported too.
function run(input) {
  var cmd = String(input && input.prompt !== undefined ? input.prompt : input);
  var open = JSON.parse(Workspace({ op: "open", name: "eg" }));
  var raw = Workspace({ op: "exec", name: "eg", command: cmd, timeout_s: 600 });
  var ex = JSON.parse(raw);
  if (ex.isError && typeof ex.error === "string") {
    try { ex = JSON.parse(ex.error); } catch (e) { ex = { exit_code: -1, output: ex.error }; }
  }
  return { final_text: JSON.stringify({
    engine: open.engine, egress: open.egress || [],
    exit: ex.exit_code, output: String(ex.output || ex.detail || "") }) };
}
