// calcchild — a sub-agent with no tools list; reports whether it holds Calc
// and what Calc gave.
function run(input) {
  if (typeof Calc === "undefined") {
    return { final_text: "child_calc=missing" };
  }
  var r = JSON.parse(Calc({ expr: "2^100" }));
  return { final_text: "child_calc=" + r.text };
}
