// gitflow — drives the Git tool from a code-js agent over the real loop:
// init a repository on the agent's volume, write a file with Write, stage,
// commit, read the log back, and try the network without git_remote (which
// must be refused). Git is a single-op tool: called as a function with an
// `op` field, returning a JSON string.
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
  var init = JSON.parse(Git({ op: "init", repo: "svc", initial_branch: "trunk" }));
  Write({ path: "svc/hello.txt", content: "hello from gitflow\n" });
  var st = JSON.parse(Git({ op: "status", repo: "svc" }));
  Git({ op: "add", repo: "svc", paths: ["."] });
  var c = JSON.parse(Git({ op: "commit", repo: "svc", message: "add hello" }));
  var log = JSON.parse(Git({ op: "log", repo: "svc", max: 5 }));
  var net = refusal({ op: "ls_remote", url: "https://github.com/acme/app.git" });

  var first = (log.commits && log.commits.length) ? log.commits[0] : {};
  return { final_text:
    "branch=" + init.branch +
    " dirty=" + (st.clean === false) +
    " commit=" + (c.hash ? "yes" : "no") +
    " commits=" + (log.commits ? log.commits.length : -1) +
    " author=" + first.author +
    " trailer=" + (String(first.message).indexOf("Loomcycle-Run: ") >= 0) +
    " net=" + net };
}
