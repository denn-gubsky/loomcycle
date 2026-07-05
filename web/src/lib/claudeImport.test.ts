import { describe, expect, it } from "vitest";

import type { LibraryEntry } from "../api";
import {
  detectKind,
  parseMcpJson,
  parseSkillMarkdown,
  parseSource,
  resolvePreview,
  rewriteEnvRefs,
  sanitizeDefName,
} from "./claudeImport";

describe("sanitizeDefName", () => {
  it("maps to the AgentDef charset", () => {
    expect(sanitizeDefName("My Cool Skill")).toBe("My-Cool-Skill");
    expect(sanitizeDefName("weird/name.v2")).toBe("weird-name-v2");
    expect(sanitizeDefName("  --edges--  ")).toBe("edges");
    expect(sanitizeDefName("keeps_under-score")).toBe("keeps_under-score");
  });
  it("collapses __ for mcp names (tool-delimiter ambiguity)", () => {
    expect(sanitizeDefName("brave__search", { mcp: true })).toBe("brave_search");
    expect(sanitizeDefName("a___b", { mcp: true })).toBe("a_b");
    // non-mcp keeps the underscores
    expect(sanitizeDefName("a__b")).toBe("a__b");
  });
  it("truncates to 128 and trims trailing hyphens", () => {
    const s = sanitizeDefName("x".repeat(200));
    expect(s.length).toBe(128);
  });
  it("returns empty for name-less input", () => {
    expect(sanitizeDefName("   ")).toBe("");
    expect(sanitizeDefName("///")).toBe("");
  });
});

describe("rewriteEnvRefs", () => {
  it("rewrites bare refs to the LOOMCYCLE_ namespace", () => {
    const r = rewriteEnvRefs("Bearer ${TOKEN}");
    expect(r.value).toBe("Bearer ${LOOMCYCLE_TOKEN}");
    expect(r.refs).toEqual(["LOOMCYCLE_TOKEN"]);
    expect(r.rewrites).toEqual([{ from: "${TOKEN}", to: "${LOOMCYCLE_TOKEN}" }]);
  });
  it("leaves already-prefixed refs untouched", () => {
    const r = rewriteEnvRefs("${LOOMCYCLE_KEY}");
    expect(r.value).toBe("${LOOMCYCLE_KEY}");
    expect(r.rewrites).toEqual([]);
    expect(r.refs).toEqual(["LOOMCYCLE_KEY"]);
  });
  it("handles multiple refs and plain text", () => {
    const r = rewriteEnvRefs("${A}-${B}-plain");
    expect(r.value).toBe("${LOOMCYCLE_A}-${LOOMCYCLE_B}-plain");
    expect(r.refs.sort()).toEqual(["LOOMCYCLE_A", "LOOMCYCLE_B"]);
  });
});

describe("parseSkillMarkdown", () => {
  it("splits frontmatter (hyphenated allowed-tools) from body", () => {
    const md = `---\nname: my-skill\ndescription: does a thing\nallowed-tools: [WebFetch, Read]\n---\n# Body\ninstructions here`;
    const { candidate, error } = parseSkillMarkdown(md);
    expect(error).toBeUndefined();
    expect(candidate!.kind).toBe("skill");
    expect(candidate!.name).toBe("my-skill");
    expect(candidate!.overlay).toEqual({
      body: "# Body\ninstructions here",
      description: "does a thing",
      tools: ["WebFetch", "Read"],
    });
  });
  it("normalizes CRLF and accepts allowed-tools as a comma string", () => {
    const md = `---\r\nname: s\r\nallowed-tools: WebFetch, Read\r\n---\r\nbody text`;
    const { candidate } = parseSkillMarkdown(md);
    expect(candidate!.overlay.tools).toEqual(["WebFetch", "Read"]);
    expect(candidate!.overlay.body).toBe("body text");
  });
  it("treats a no-frontmatter file as body-only, using the fallback name", () => {
    const { candidate, error } = parseSkillMarkdown("just a body", "from-dir");
    expect(error).toBeUndefined();
    expect(candidate!.name).toBe("from-dir");
    expect(candidate!.overlay).toEqual({ body: "just a body" });
  });
  it("errors on empty body", () => {
    const { error } = parseSkillMarkdown(`---\nname: s\n---\n   `);
    expect(error).toMatch(/empty body/);
  });
  it("errors when no name is derivable", () => {
    const { error } = parseSkillMarkdown("body with no name");
    expect(error).toMatch(/no usable name/);
  });
  it("sanitizes the frontmatter name and warns", () => {
    const { candidate } = parseSkillMarkdown(`---\nname: My Skill\n---\nbody`);
    expect(candidate!.name).toBe("My-Skill");
    expect(candidate!.warnings.some((w) => w.includes("sanitized"))).toBe(true);
  });
});

describe("parseMcpJson", () => {
  it("parses the wrapped mcpServers shape with an http server + header rewrite", () => {
    const json = JSON.stringify({
      mcpServers: {
        brave: { type: "http", url: "https://api.brave.com/mcp", headers: { Authorization: "Bearer ${BRAVE_KEY}" } },
      },
    });
    const r = parseMcpJson(json);
    expect(r.errors).toEqual([]);
    expect(r.candidates).toHaveLength(1);
    const c = r.candidates[0]!;
    expect(c.transport).toBe("http");
    expect(c.overlay).toEqual({
      transport: "http",
      url: "https://api.brave.com/mcp",
      headers: { Authorization: "Bearer ${LOOMCYCLE_BRAVE_KEY}" },
    });
    expect(c.secretRefs).toEqual(["LOOMCYCLE_BRAVE_KEY"]);
  });
  it("accepts the bare name→server map and detects stdio from command", () => {
    const json = JSON.stringify({
      fs: { command: "npx", args: ["-y", "@modelcontextprotocol/server-filesystem", "${ROOT}"], env: { TOKEN: "${TOKEN}" } },
    });
    const r = parseMcpJson(json);
    expect(r.candidates).toHaveLength(1);
    const c = r.candidates[0]!;
    expect(c.transport).toBe("stdio");
    expect(c.overlay).toEqual({
      transport: "stdio",
      command: "npx",
      args: ["-y", "@modelcontextprotocol/server-filesystem", "${LOOMCYCLE_ROOT}"],
      env: { TOKEN: "${LOOMCYCLE_TOKEN}" },
    });
    expect(c.secretRefs.sort()).toEqual(["LOOMCYCLE_ROOT", "LOOMCYCLE_TOKEN"]);
  });
  it("maps sse → streamable-http and defaults url-only to http", () => {
    const r = parseMcpJson(
      JSON.stringify({ mcpServers: { a: { type: "sse", url: "https://x/y" }, b: { url: "https://z/w" } } }),
    );
    const byName = Object.fromEntries(r.candidates.map((c) => [c.name, c.transport]));
    expect(byName.a).toBe("streamable-http");
    expect(byName.b).toBe("http");
  });
  it("collapses __ in server names", () => {
    const r = parseMcpJson(JSON.stringify({ mcpServers: { "we__ird": { url: "https://x/y" } } }));
    expect(r.candidates[0]!.name).toBe("we_ird");
  });
  it("warns on an unmapped registries block", () => {
    const r = parseMcpJson(JSON.stringify({ registries: {}, mcpServers: { a: { url: "https://x/y" } } }));
    expect(r.warnings.some((w) => w.includes("registries"))).toBe(true);
  });
  it("errors on invalid JSON", () => {
    expect(parseMcpJson("{not json").errors[0]).toMatch(/not valid JSON/);
  });
});

describe("detectKind", () => {
  it("keys off filename then content", () => {
    expect(detectKind("", "skills/foo/SKILL.md")).toBe("skill");
    expect(detectKind("", "path/.mcp.json")).toBe("mcp");
    expect(detectKind("---\nname: x\n---\nbody")).toBe("skill");
    expect(detectKind(JSON.stringify({ mcpServers: { a: { url: "u" } } }))).toBe("mcp");
    expect(detectKind("plain text no hints")).toBe("unknown");
  });
});

// ---- preview / collision resolution ------------------------------------

function entry(over: Partial<LibraryEntry> & { name: string }): LibraryEntry {
  return {
    source: "dynamic-only",
    in_static: false,
    in_substrate: true,
    version_count: 1,
    ...over,
  };
}

describe("resolvePreview", () => {
  const opts = { stdioAllowed: false, httpAllowlistConfigured: true, isTenant: true };

  it("Create when no collision", () => {
    const { candidate } = parseSkillMarkdown(`---\nname: fresh\n---\nbody`);
    const rows = resolvePreview([candidate!], { skills: [], mcp: [] }, opts);
    expect(rows[0]!.action).toBe("create");
  });

  it("Fork over a live dynamic skill, parent = active_def_id", () => {
    const { candidate } = parseSkillMarkdown(`---\nname: dup\n---\nbody`);
    const rows = resolvePreview(
      [candidate!],
      { skills: [entry({ name: "dup", active_def_id: "def_123" })], mcp: [] },
      opts,
    );
    expect(rows[0]!.action).toBe("fork");
    expect(rows[0]!.parentDefId).toBe("def_123");
  });

  it("Create (reclaims) when the only versions are retired", () => {
    const { candidate } = parseSkillMarkdown(`---\nname: dup\n---\nbody`);
    const rows = resolvePreview(
      [candidate!],
      { skills: [entry({ name: "dup", active_def_id: "def_9", active_retired: true })], mcp: [] },
      opts,
    );
    expect(rows[0]!.action).toBe("create");
  });

  it("blocks a stdio server when the capability is off", () => {
    const r = parseMcpJson(JSON.stringify({ mcpServers: { s: { command: "x" } } }));
    const rows = resolvePreview(r.candidates, { skills: [], mcp: [] }, opts);
    expect(rows[0]!.action).toBe("blocked");
    expect(rows[0]!.note).toMatch(/LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO/);
  });

  it("allows a stdio server with an RCE warning when the capability is on", () => {
    const r = parseMcpJson(JSON.stringify({ mcpServers: { s: { command: "x" } } }));
    const rows = resolvePreview(r.candidates, { skills: [], mcp: [] }, { ...opts, stdioAllowed: true });
    expect(rows[0]!.action).toBe("create");
    expect(rows[0]!.warnings.some((w) => w.includes("RCE"))).toBe(true);
  });

  it("MCP static-name → per-tenant override (Create) for a tenant, Fork otherwise", () => {
    const r = parseMcpJson(JSON.stringify({ mcpServers: { brave: { url: "https://x/y" } } }));
    const staticHit = { skills: [], mcp: [entry({ name: "brave", in_static: true, source: "static-only" as const, in_substrate: false, active_def_id: undefined })] };
    expect(resolvePreview(r.candidates, staticHit, { ...opts, isTenant: true })[0]!.action).toBe("create");
    expect(resolvePreview(r.candidates, staticHit, { ...opts, isTenant: false })[0]!.action).toBe("fork");
  });

  it("warns about secret refs (tenant can't provision host env)", () => {
    const r = parseMcpJson(JSON.stringify({ mcpServers: { a: { url: "https://x", headers: { A: "${K}" } } } }));
    const rows = resolvePreview(r.candidates, { skills: [], mcp: [] }, opts);
    expect(rows[0]!.warnings.some((w) => w.includes("LOOMCYCLE_K"))).toBe(true);
  });
});

describe("parseSource dispatch", () => {
  it("routes skill vs mcp", () => {
    expect(parseSource("---\nname: s\n---\nbody", "skill").candidates[0]!.kind).toBe("skill");
    expect(parseSource(JSON.stringify({ mcpServers: { a: { url: "u" } } }), "mcp").candidates[0]!.kind).toBe("mcp-server");
  });
});
