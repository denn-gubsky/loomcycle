// Publishes one message to the hooked `inbox` channel and reports what the
// publish returned, so the suite can see awaiting_hooks from inside a run.
function run(input) {
  var res = Channel.publish({ channel: "inbox", value: { text: "from the agent, with a secret" } });
  return { final_text: JSON.stringify(res) };
}
