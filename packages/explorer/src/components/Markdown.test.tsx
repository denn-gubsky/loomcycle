// @vitest-environment happy-dom
import { act, createElement } from "react";
import { createRoot } from "react-dom/client";
import { expect, it } from "vitest";
import Markdown from "./Markdown";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

function render(source: string): HTMLElement {
  const el = document.createElement("div");
  const root = createRoot(el);
  act(() => root.render(createElement(Markdown, { source })));
  return el;
}

it("shows a bare <br> in a table cell as a line break", () => {
  const el = render("| Basic |\n|---|\n| No test plan<br>Few runs<BR/>Reports |\n");
  const td = el.querySelector("td");
  expect(td?.querySelectorAll("br").length).toBe(2);
  expect(td?.textContent).toBe("No test plan\nFew runs\nReports");
});

it("leaves other raw HTML unrendered, a <br> with attributes included", () => {
  const el = render("| a<br onclick=x>b | <b>c</b> |\n|---|---|\n");
  expect(el.querySelector("[onclick]")).toBeNull();
  expect(el.querySelector("br")).toBeNull();
  expect(el.querySelector("b")).toBeNull();
});
