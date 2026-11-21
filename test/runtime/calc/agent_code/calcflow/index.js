// calcflow — an agent whose definition never names Calc calls it. The
// binding exists only because the run's toolset holds the default. It also
// reads Context op=agents, which counts the tools each agent's run holds.
function run(input) {
  var r = JSON.parse(Calc({ expr: "0.1 + 0.2" }));
  var listed = JSON.parse(Context({ op: "agents" }));
  var counts = {};
  (listed.agents || []).forEach(function (a) { counts[a.name] = a.tools_count; });
  return { final_text:
    "text=" + r.text +
    " exact=" + r.exact +
    " calcflow_tools=" + counts["calcflow"] +
    " nocalc_tools=" + counts["nocalc"] };
}
