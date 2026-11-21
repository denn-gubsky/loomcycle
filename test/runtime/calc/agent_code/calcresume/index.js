// calcresume — a Calc call, then a wait the driver pauses the runtime
// during, then another Calc call. The driver kills the paused runtime and
// boots a new one, which resumes the run from its transcript: replay feeds
// back the first two results, and the second Calc call runs on the toolset
// the resume rebuilt from the definition.
function run(input) {
  var before = JSON.parse(Calc({ expr: "6 * 7" }));
  Channel.await({ channels: ["calc-hold"], wait_ms: 3000 });
  var after = JSON.parse(Calc({ expr: "0.1 + 0.2" }));
  return { final_text: "before=" + before.text + " after=" + after.text };
}
