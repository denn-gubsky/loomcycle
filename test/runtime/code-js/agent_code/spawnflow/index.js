// spawnflow — a code-js parent reads a sub-agent's outcome as an OBJECT.
//
// With result "object" Agent.spawn returns the child's ids, status, answer
// and token usage as one value, so a program needs neither to strip the
// "[sub-agent …]" line off a text result nor to look the child's run up
// afterwards to learn what it used.
//
// The plain spawn beside it is the contrast, and the regression: without
// `result` the answer still arrives as text behind that line.
function run(input) {
  var r = Agent.spawn({ name: "echo", prompt: "say hello", result: "object" });
  var plain = String(Agent.spawn({ name: "echo", prompt: "say hello" }));

  var u = (r && r.usage) ? r.usage : {};
  return {
    final_text: "type=" + (typeof r) +
      " status=" + r.status +
      " has_ids=" + ((r.agent_id && r.run_id) ? 1 : 0) +
      " model=" + u.model +
      " provider=" + u.provider +
      " out_tokens_type=" + (typeof u.output_tokens) +
      " in_tokens_positive=" + ((u.input_tokens > 0) ? 1 : 0) +
      " answer_has_header=" + ((String(r.final_text).indexOf("[sub-agent") === 0) ? 1 : 0) +
      " answer_nonempty=" + ((String(r.final_text).length > 0) ? 1 : 0) +
      " plain_has_header=" + ((plain.indexOf("[sub-agent") === 0) ? 1 : 0)
  };
}
