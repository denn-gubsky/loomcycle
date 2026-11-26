// team/orchestrator (agent-teams bundle) as a code-js agent: the team/sqldata
// skill's external-data path with the bundle's own Workspace grant and
// Bashbox withheld. It writes rows it holds (as from a SQL Memory query) into
// a workspace, totals them with a script there and reads the report back.
function run(input) {
  Workspace({ op: "open", name: "data" });
  Workspace({ op: "write", name: "data", path: "ledger.csv", content: "item,amount\nrent,1200\nfood,345\n" });
  Workspace({ op: "write", name: "data", path: "total.py", content:
    "import csv\nrows = list(csv.DictReader(open('ledger.csv')))\n" +
    "open('report.txt', 'w').write('total=' + str(sum(int(r['amount']) for r in rows)) + '\\n')\n" });
  Workspace({ op: "exec", name: "data", command: "python3 total.py" });
  return { final_text: "report=" + String(Workspace({ op: "read", name: "data", path: "report.txt" })).trim() };
}
