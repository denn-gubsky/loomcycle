// gitnet — holds git_remote: [fetch], so the Git tool's network gate is
// passed, and asks for a host the operator does not list. The refusal must
// come before any request leaves the process.
// A refused tool call throws in code-js; the thrown text carries the Git
// tool's error envelope. refusal returns its class, or "none" when the call
// was not refused at all.
function refusal(args) {
  try {
    Git(args);
    return "none";
  } catch (e) {
    // The thrown text quotes the envelope {"detail":...,"error":"<class>"}
    // as an escaped string, and may be followed by the op's help article,
    // which names other classes: match the envelope's own closing field.
    var m = String(e).match(/\\"error\\":\\"([a-z_]+)\\"\}/);
    return m ? m[1] : "unrecognised";
  }
}

function run(input) {
  var r = refusal({ op: "ls_remote", url: "https://github.com/acme/app.git" });
  var s = refusal({ op: "ls_remote", url: "file:///etc" });
  return { final_text: "host=" + r + " scheme=" + s };
}
