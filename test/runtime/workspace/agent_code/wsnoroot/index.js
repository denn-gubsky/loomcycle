// wsnoroot — asks to open the user's folder without the workspace.roots grant
// (RFC GW P13): the open is refused before anything is placed.
function run(input) {
  var open = "allowed";
  try { Workspace({ op: "open", name: "src", root: "code" }); } catch (e) { open = /not_granted/.test(String(e)) ? "not_granted" : String(e); }
  return { final_text: "open=" + open };
}
