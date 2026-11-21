// calcparent — spawns calcchild through the Agent tool and reports what the
// child computed. The child's toolset is built by the sub-agent path.
function run(input) {
  var r = Agent.spawn({ name: "calcchild", prompt: "go", result: "object" });
  return { final_text: "child_status=" + r.status + " child_said=" + String(r.final_text).trim() };
}
