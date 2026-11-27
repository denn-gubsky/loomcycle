// The git suite's agent (run.sh): the prompt is a JSON list of Workspace
// calls; it makes each in turn and reports every answer, parsed, in order.
// A refused call (which code-js raises) is reported as {error, detail}; a
// plain-text answer as {text}.
function parse(text, refused) {
  var s = String(text);
  var i = s.indexOf("{");
  var v;
  try { v = JSON.parse(s.slice(i < 0 ? 0 : i)); } catch (e) {
    // A plain-text answer (write's) is no refusal; a raised one is.
    return refused ? { error: "unparsed", detail: s } : { text: s };
  }
  if (v && v.isError && typeof v.error === "string") {
    try { v = JSON.parse(v.error); } catch (e) { v = { error: "unparsed", detail: v.error }; }
  }
  return v;
}
function run(input) {
  var steps = JSON.parse(String(input && input.prompt !== undefined ? input.prompt : input));
  var out = [];
  for (var i = 0; i < steps.length; i++) {
    var v;
    try { v = parse(Workspace(steps[i]), false); } catch (e) { v = parse(e && e.message !== undefined ? e.message : e, true); }
    out.push(v);
  }
  return { final_text: JSON.stringify(out) };
}
