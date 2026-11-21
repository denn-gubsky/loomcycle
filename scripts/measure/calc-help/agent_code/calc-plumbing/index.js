// calc-plumbing — the dry run's stand-in for a model. It makes the calls a
// measured run makes (a help read, a Calc call that is refused, one that is
// not) and ends with an ANSWER line, so drive.py's parsing and scoring run
// end to end with no model. It answers 0.3 to every task.
function run(input) {
  Context({ op: "help", topic: "calculations" });
  var refused = "";
  try {
    Calc({ expr: "2x" });
  } catch (e) {
    refused = " (the first call was refused)";
  }
  var r = JSON.parse(Calc({ expr: "0.1 + 0.2" }));
  return { final_text: "Computed with Calc" + refused + ".\nANSWER: " + r.text };
}
