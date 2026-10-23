import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import ErrorNotice from "./ErrorNotice";
import { apiError } from "../lib/apiError";

const render = (error: unknown) => renderToStaticMarkup(createElement(ErrorNotice, { error }));

describe("ErrorNotice", () => {
  it("shows an acl_denied refusal as access denied, naming the action, resource and denying rule", () => {
    const e = apiError(
      403,
      "Forbidden",
      JSON.stringify({ code: "acl_denied", error: "x", action: "invoke", resource: "tool:Bash", reasons: [{ acl: "interns", version: 2, rule: 1, effect: "deny" }] }),
    );
    const html = render(e);
    expect(html).toContain("Access denied");
    expect(html).toContain("<code>invoke</code> <code>tool:Bash</code>");
    expect(html).toContain("denied by list <code>interns</code>");
    expect(html).toContain("rule 1");
    expect(html).not.toContain("settings-error");
  });

  it("shows a scope refusal as not permitted, without the status line", () => {
    const e = apiError(403, "Forbidden", JSON.stringify({ code: "insufficient_scope", error: "operators only", required_scope: "substrate:tenant" }));
    const html = render(e);
    expect(html).toContain("Not permitted");
    expect(html).toContain("not permitted — operators only (needs the substrate:tenant scope)");
    expect(html).not.toContain("403 Forbidden");
  });

  it("shows any other error as the usual error line, and nothing for no error", () => {
    expect(render(new Error("500 Internal Server Error: boom"))).toBe('<div class="settings-error">500 Internal Server Error: boom</div>');
    expect(render(null)).toBe("");
  });
});

describe("ErrorNotice — matched allows", () => {
  it("lists only the denying rule, not the allows the deny overrode", () => {
    const e = apiError(
      403,
      "Forbidden",
      JSON.stringify({
        code: "acl_denied",
        action: "author",
        resource: "def:acl/locked-x",
        reasons: [
          { acl: "no-locked", version: 1, rule: 0, effect: "deny" },
          { acl: "builtin:tenant-operator", rule: 15, effect: "allow" },
        ],
      }),
    );
    const html = render(e);
    expect(html).toContain("<code>no-locked</code>");
    expect(html).not.toContain("builtin:tenant-operator");
  });
});
