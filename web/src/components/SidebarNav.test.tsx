import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";
import SidebarNav from "./SidebarNav";
import { visibleNavTree } from "./navItems";

const render = (path: string, tree: ReturnType<typeof visibleNavTree>, collapsed = false) =>
  renderToStaticMarkup(
    createElement(MemoryRouter, { initialEntries: [path] }, createElement(SidebarNav, { tree, collapsed })),
  );

const count = (html: string, needle: string) => html.split(needle).length - 1;

describe("SidebarNav", () => {
  it("shows both zones' headings, group headings and the current page for an operator", () => {
    const html = render("/access/policies", visibleNavTree(false, true, undefined));
    expect(html).toContain(">workspace</div>");
    expect(html).toContain(">administration</div>");
    expect(html).toContain('<span class="sidebar-label">knowledge</span>');
    expect(html).toContain('<span class="sidebar-label">documents</span>');
    expect(html).toMatch(/class="nav-leaf nav-leaf-nested active" aria-current="page"[^>]*>.*?policies/);
    expect(html).toContain('class="nav-group-head has-current" aria-expanded="true"');
  });

  it("shows a group the viewer sees one page of as that page alone", () => {
    // The server lets this operator open usage but not limits or routing, so
    // cost & models holds one page.
    const html = render("/agents", visibleNavTree(false, true, { limits: false, routing: false }));
    expect(html).not.toContain('<span class="sidebar-label">cost &amp; models</span>');
    expect(html).toContain('<span class="sidebar-label">usage</span>');
  });

  it("drops the zone headings for a viewer with only the workspace", () => {
    const html = render("/run", visibleNavTree(false, false, undefined));
    expect(html).not.toContain("nav-zone-label");
    expect(html).toContain('<span class="sidebar-label">new run</span>');
  });

  it("lists icons only, groups split by a rule, when collapsed to a rail", () => {
    const tree = visibleNavTree(false, true, undefined);
    const html = render("/agents", tree, true);
    expect(html).not.toContain("nav-group-head");
    expect(html).not.toContain("nav-zone-label");
    expect(count(html, "nav-rail-sep")).toBe(tree.flatMap((z) => z.groups).length - 1);
  });
});
