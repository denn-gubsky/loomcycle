import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { TeamCheckPanel } from "./TeamCheckPanel";

describe("TeamCheckPanel", () => {
  it("lists every issue with its severity, where it is, and the server's words", () => {
    const html = renderToStaticMarkup(
      createElement(TeamCheckPanel, {
        onClose: () => {},
        result: {
          name: "sdlc",
          valid: false,
          runnable: false,
          checked_as: "fork",
          matches: false,
          deployed: true,
          issues: [
            { kind: "channel_authority", severity: "refused", path: "channels.publish[1]", detail: "not yours to grant" },
            { kind: "agent_missing", severity: "unrunnable", state: "judge", detail: "agent \"judge\" does not resolve" },
          ],
        },
      }),
    );
    expect(html).toContain("A save would be refused: 1 problem to fix.");
    expect(html).toContain("Checked as a fork. Nothing was saved.");
    expect(html.match(/<li/g)).toHaveLength(2);
    expect(html).toContain("<code>channels.publish[1]</code>");
    expect(html).toContain("can&#x27;t run");
    // No path: the state names where it is.
    expect(html).toContain("<code>judge</code>");
  });
});
