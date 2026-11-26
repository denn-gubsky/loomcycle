// team/orchestrator (agent-teams bundle) as a code-js agent, with the
// bundle's own Workspace grant and git_remote and Bashbox withheld. Two of
// its skills' paths:
//   - team/sqldata (any prompt): it writes rows it holds (as from a SQL
//     Memory query) into a workspace, totals them with a script there and
//     reads the report back;
//   - team/repo ("repo <url> <credential>", test/runtime/workspace-git): it
//     opens the repository as a git workspace, commits a file and pushes
//     the workspace's own branch, as the skill's steps say.
function run(input) {
  var p = String(input && input.prompt !== undefined ? input.prompt : (input || ""));
  if (p.indexOf("repo ") === 0) {
    return repo(p.split(" "));
  }
  Workspace({ op: "open", name: "data" });
  Workspace({ op: "write", name: "data", path: "ledger.csv", content: "item,amount\nrent,1200\nfood,345\n" });
  Workspace({ op: "write", name: "data", path: "total.py", content:
    "import csv\nrows = list(csv.DictReader(open('ledger.csv')))\n" +
    "open('report.txt', 'w').write('total=' + str(sum(int(r['amount']) for r in rows)) + '\\n')\n" });
  Workspace({ op: "exec", name: "data", command: "python3 total.py" });
  return { final_text: "report=" + String(Workspace({ op: "read", name: "data", path: "report.txt" })).trim() };
}

// repo is the team/repo skill's flow: open, change, status, commit, push,
// release.
function repo(a) {
  var open = JSON.parse(Workspace({ op: "open", name: "repo", repo: a[1], credential: a[2] }));
  Workspace({ op: "write", name: "repo", path: "TEAM.md", content: "made by the team\n" });
  var st = JSON.parse(Workspace({ op: "git_status", name: "repo" }));
  var c = JSON.parse(Workspace({ op: "git_commit", name: "repo", message: "team: add TEAM.md", paths: ["TEAM.md"] }));
  var push = JSON.parse(Workspace({ op: "git_push", name: "repo" }));
  Workspace({ op: "release", name: "repo" });
  return { final_text: "branch=" + open.branch + " changed=" + (st.files || []).length +
    " commit=" + c.commit + " pushed=" + (push.updated || []).join(",") };
}
