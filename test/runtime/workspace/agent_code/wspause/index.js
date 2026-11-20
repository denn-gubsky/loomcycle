// wspause — a workspace held across a runtime pause (RFC X). The agent opens
// a workspace and writes a marker, then runs a command that sleeps long
// enough for the driver to pause the runtime: the run parks at the next
// iteration boundary still owning the workspace. After the driver resumes,
// the run reads the marker back through the same workspace and ends, and the
// run's end releases it. Replay re-runs this script from the top with the
// recorded results, so no op runs twice.
function run(input) {
  Workspace({ op: "open", name: "held", toolchain: "python" });
  Workspace({ op: "write", name: "held", path: "marker.txt", content: "kept across the pause\n" });
  var slow = JSON.parse(Workspace({ op: "exec", name: "held", command: "touch started && sleep 4 && echo slept" }));
  var marker = Workspace({ op: "read", name: "held", path: "marker.txt" });
  var after = JSON.parse(Workspace({ op: "exec", name: "held", command: "python3 -c 'print(6*7)'" }));
  return { final_text:
    "slept=" + String(slow.output).trim() +
    " marker=" + String(marker).trim().replace(/\s+/g, "_") +
    " after=" + String(after.output).trim() };
}
