// wsroot — a workspace on a root (RFC GW P13), driven from a code-js agent over
// the real loop: open the user's folder "code" on their personal worker, read
// the user's own file, run a command that writes into the folder, ask for a
// checkpoint (refused: the folder is the user's own), list, and release. The
// driver checks the folder and both files are still there afterwards.
function run(input) {
  var open = JSON.parse(Workspace({ op: "open", name: "src", root: "code" }));
  var mine = Workspace({ op: "read", name: "src", path: "keep.txt" });
  var ex = JSON.parse(Workspace({ op: "exec", name: "src", command: "echo made-by-agent > made.txt; ls" }));
  // A refused call throws in a code agent; its message carries the class.
  var ck = "allowed";
  try { Workspace({ op: "checkpoint", name: "src" }); } catch (e) { ck = /not_on_root/.test(String(e)) ? "not_on_root" : String(e); }
  var list = JSON.parse(Workspace({ op: "list" }));
  var rel = JSON.parse(Workspace({ op: "release", name: "src" }));
  var root = list.workspaces && list.workspaces.length ? list.workspaces[0].root : "";
  return { final_text:
    "state=" + open.state +
    " root=" + open.root +
    " read=" + String(mine).trim() +
    " exit=" + ex.exit_code +
    " checkpoint=" + ck +
    " listed_root=" + root +
    " released=" + (rel.error ? rel.error : "ok") };
}
