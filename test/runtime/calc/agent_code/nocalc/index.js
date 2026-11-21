// nocalc — tools: [-Calc]. A tool outside the run's toolset gets no JS
// binding, so `typeof Calc` is "undefined" here.
function run(input) {
  return { final_text: "calc_binding=" + (typeof Calc) + " context_binding=" + (typeof Context) };
}
