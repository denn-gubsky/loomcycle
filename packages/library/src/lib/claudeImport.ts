// claudeImport.ts — RFC AU. Pure, browser-side parser that maps Claude Code
// skills (SKILL.md) and MCP tool-servers (mcp.json / .mcp.json) onto loomcycle
// substrate overlays, for the Web-UI import flow. It mirrors the Go reference
// (internal/skills/loader.go parseSkill, internal/claudeimport/{skill,mcp}.go)
// but emits `skilldef` / `mcpserverdef` overlays directly instead of yaml — the
// tenant-safe POST /v1/_skilldef / /v1/_mcpserverdef write path already exists.
//
// No network, no React — unit-testable in isolation. Parsing is intentionally
// client-side (RFC AU decision): the common case is a frontmatter split + a JSON
// map, and the write path is already exposed and tenant-scoped.
// js-yaml v4 `load` is the SAFE loader — DEFAULT_SCHEMA has no code-execution
// tags (the unsafe schema is opt-in). Matches LibraryEditModal's usage.
import { load as yamlLoad } from "js-yaml";

import type { LibraryEntry } from "../types";

export type ImportKind = "skill" | "mcp-server";
export type Transport = "http" | "streamable-http" | "stdio";

// An env-var reference we rewrote to the LOOMCYCLE_ namespace (mirrors the Go
// importer's rewriteEnvRefs). Surfaced in the preview so the operator sees what
// they must provision on the host — a tenant can't set host env vars.
export interface EnvRewrite {
  from: string; // e.g. "${TOKEN}"
  to: string; // e.g. "${LOOMCYCLE_TOKEN}"
}

export interface ImportCandidate {
  kind: ImportKind;
  name: string; // sanitized, substrate-ready
  originalName: string; // as found in the source (pre-sanitization)
  overlay: Record<string, unknown>; // the create/fork overlay body
  transport?: Transport; // mcp-server only
  /** LOOMCYCLE_* env vars this server needs the operator to provision. */
  secretRefs: string[];
  /** ${FOO}→${LOOMCYCLE_FOO} rewrites applied (for the preview). */
  envRewrites: EnvRewrite[];
  warnings: string[];
}

export interface ParseResult {
  candidates: ImportCandidate[];
  /** Fatal errors — the input could not be parsed into any candidate. */
  errors: string[];
  /** Non-fatal, source-level notes (e.g. an unmapped `registries` block). */
  warnings: string[];
}

// ---- name sanitization -------------------------------------------------

// sanitizeDefName maps an arbitrary source name onto the AgentDef charset
// (^[A-Za-z0-9_-]{1,128}$) so imported names are consistent even though the
// skill/mcp substrates don't (yet) validate the charset. For MCP names, `__`
// is collapsed to `_` first: the tool-naming scheme is `mcp__<server>__<tool>`,
// so a `__` inside the server name makes that delimiter ambiguous.
export function sanitizeDefName(raw: string, opts?: { mcp?: boolean }): string {
  let s = (raw ?? "").trim();
  if (opts?.mcp) s = s.replace(/_{2,}/g, "_");
  s = s.replace(/[^A-Za-z0-9_-]+/g, "-"); // disallowed runs → single hyphen
  s = s.replace(/^-+|-+$/g, ""); // strip leading/trailing hyphens
  if (s.length > 128) s = s.slice(0, 128).replace(/-+$/g, "");
  return s;
}

// ---- env-ref rewrite ---------------------------------------------------

const ENV_REF_RE = /\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g;

// rewriteEnvRefs rewrites ${FOO}→${LOOMCYCLE_FOO} (leaving already-prefixed
// refs untouched), mirroring the Go importer. Returns the rewritten string,
// the list of rewrites, and the set of resulting LOOMCYCLE_ var names.
export function rewriteEnvRefs(value: string): {
  value: string;
  rewrites: EnvRewrite[];
  refs: string[];
} {
  const rewrites: EnvRewrite[] = [];
  const refs = new Set<string>();
  const out = value.replace(ENV_REF_RE, (_m, inner: string) => {
    if (inner.startsWith("LOOMCYCLE_")) {
      refs.add(inner);
      return "${" + inner + "}";
    }
    const to = "LOOMCYCLE_" + inner;
    rewrites.push({ from: "${" + inner + "}", to: "${" + to + "}" });
    refs.add(to);
    return "${" + to + "}";
  });
  return { value: out, rewrites, refs: [...refs] };
}

// ---- skill parsing -----------------------------------------------------

interface SkillFrontmatter {
  name?: string;
  description?: string;
  // Claude Code uses the hyphenated key; loomcycle's canonical key is `tools`.
  "allowed-tools"?: unknown;
  tools?: unknown;
}

function coerceStringList(v: unknown): string[] {
  if (Array.isArray(v)) return v.map((x) => String(x).trim()).filter(Boolean);
  if (typeof v === "string")
    return v
      .split(",")
      .map((x) => x.trim())
      .filter(Boolean);
  return [];
}

// parseSkillMarkdown maps a SKILL.md (frontmatter + body) to a skilldef
// candidate. fallbackName seeds the name when the frontmatter omits it (e.g.
// the `skills/<name>/SKILL.md` directory, or an uploaded filename's parent).
export function parseSkillMarkdown(
  text: string,
  fallbackName?: string,
): { candidate?: ImportCandidate; error?: string } {
  const norm = text.replace(/\r\n/g, "\n");
  let fm: SkillFrontmatter = {};
  let body = norm;

  if (norm.startsWith("---\n")) {
    // Find the closing fence: a line that is exactly "---".
    const end = norm.indexOf("\n---", 3);
    if (end !== -1) {
      const fmText = norm.slice(4, end + 1);
      // consume the closing fence + following newline
      let rest = norm.slice(end + 4);
      if (rest.startsWith("\n")) rest = rest.slice(1);
      body = rest;
      try {
        const parsed = yamlLoad(fmText);
        if (parsed && typeof parsed === "object") fm = parsed as SkillFrontmatter;
      } catch (e) {
        return { error: `frontmatter is not valid YAML: ${e instanceof Error ? e.message : String(e)}` };
      }
    }
  }

  const warnings: string[] = [];
  const originalName = String(fm.name ?? fallbackName ?? "").trim();
  const name = sanitizeDefName(originalName);
  if (!name) {
    return { error: "skill has no usable name — add a `name:` frontmatter field or set one below." };
  }
  if (name !== originalName && originalName) {
    warnings.push(`name sanitized: "${originalName}" → "${name}"`);
  }
  if (!body.trim()) {
    return { error: `skill "${name}" has an empty body — the substrate refuses empty skills.` };
  }
  if (body.length > 100 * 1024) {
    warnings.push("body is large (>100KB) — may exceed the operator's LOOMCYCLE_SKILL_DEF_MAX_BODY_BYTES cap.");
  }

  const allowedTools = coerceStringList(fm["allowed-tools"] ?? fm.tools);
  const overlay: Record<string, unknown> = { body };
  const description = typeof fm.description === "string" ? fm.description.trim() : "";
  if (description) overlay.description = description;
  if (allowedTools.length > 0) overlay.tools = allowedTools;

  return {
    candidate: {
      kind: "skill",
      name,
      originalName: originalName || name,
      overlay,
      secretRefs: [],
      envRewrites: [],
      warnings,
    },
  };
}

// ---- mcp parsing -------------------------------------------------------

interface RawMcpServer {
  type?: string;
  transport?: string;
  command?: string;
  args?: unknown;
  env?: Record<string, unknown>;
  url?: string;
  headers?: Record<string, unknown>;
  description?: string;
}

function detectTransport(s: RawMcpServer): Transport {
  const t = String(s.type ?? s.transport ?? "").toLowerCase();
  if (t === "stdio") return "stdio";
  // loomcycle has no `sse` transport — map it to streamable-http (the streaming
  // HTTP transport). An explicit `http` stays http.
  if (t === "streamable-http" || t === "streamablehttp" || t === "sse") return "streamable-http";
  if (t === "http") return "http";
  if (s.command) return "stdio";
  // url-only, no type → http (matches the Go importer default).
  return "http";
}

// looksLikeServerMap reports whether a bare object is a map of server-name →
// server-spec (each value an object with command or url), so we can accept the
// un-wrapped `.mcp.json` shape as well as the `{ mcpServers: {...} }` wrapper.
function looksLikeServerMap(o: Record<string, unknown>): boolean {
  const vals = Object.values(o);
  if (vals.length === 0) return false;
  return vals.every(
    (v) => v && typeof v === "object" && ("command" in (v as object) || "url" in (v as object)),
  );
}

// parseMcpJson maps an mcp.json / .mcp.json to per-server mcpserverdef
// candidates. Accepts both `{ mcpServers: {...} }` and a bare `{ name: {...} }`.
export function parseMcpJson(text: string): ParseResult {
  const errors: string[] = [];
  const warnings: string[] = [];
  let obj: Record<string, unknown>;
  try {
    obj = JSON.parse(text) as Record<string, unknown>;
  } catch (e) {
    return { candidates: [], errors: [`not valid JSON: ${e instanceof Error ? e.message : String(e)}`], warnings: [] };
  }
  if (!obj || typeof obj !== "object") {
    return { candidates: [], errors: ["expected a JSON object of MCP servers"], warnings: [] };
  }

  if ("registries" in obj) {
    warnings.push("`registries` block ignored — loomcycle has no remote-registry surface (airgapped-friendly).");
  }

  let servers: Record<string, unknown>;
  if (obj.mcpServers && typeof obj.mcpServers === "object") {
    servers = obj.mcpServers as Record<string, unknown>;
  } else if (looksLikeServerMap(obj)) {
    servers = obj;
  } else {
    return {
      candidates: [],
      errors: ['no MCP servers found (expected a "mcpServers" object or a bare name→server map)'],
      warnings,
    };
  }

  const candidates: ImportCandidate[] = [];
  for (const [rawName, rawSpec] of Object.entries(servers)) {
    if (rawName === "mcpServers" || rawName === "registries") continue;
    if (!rawSpec || typeof rawSpec !== "object") {
      warnings.push(`server "${rawName}": skipped (not an object)`);
      continue;
    }
    const spec = rawSpec as RawMcpServer;
    const warns: string[] = [];
    const name = sanitizeDefName(rawName, { mcp: true });
    if (!name) {
      warnings.push(`server "${rawName}": skipped (name has no usable characters)`);
      continue;
    }
    if (name !== rawName) warns.push(`name sanitized: "${rawName}" → "${name}"`);

    const transport = detectTransport(spec);
    const overlay: Record<string, unknown> = { transport };
    const rewrites: EnvRewrite[] = [];
    const refs = new Set<string>();
    const collect = (v: string): string => {
      const r = rewriteEnvRefs(v);
      rewrites.push(...r.rewrites);
      r.refs.forEach((x) => refs.add(x));
      return r.value;
    };

    if (transport === "stdio") {
      if (!spec.command) {
        warnings.push(`server "${rawName}": stdio server has no command — skipped.`);
        continue;
      }
      overlay.command = collect(String(spec.command));
      const args = coerceStringList(spec.args).map(collect);
      if (args.length > 0) overlay.args = args;
      if (spec.env && typeof spec.env === "object") {
        const env: Record<string, string> = {};
        for (const [k, v] of Object.entries(spec.env)) env[k] = collect(String(v));
        if (Object.keys(env).length > 0) overlay.env = env;
      }
    } else {
      if (!spec.url) {
        warnings.push(`server "${rawName}": http server has no url — skipped.`);
        continue;
      }
      overlay.url = collect(String(spec.url));
      if (spec.headers && typeof spec.headers === "object") {
        const headers: Record<string, string> = {};
        for (const [k, v] of Object.entries(spec.headers)) headers[k] = collect(String(v));
        if (Object.keys(headers).length > 0) overlay.headers = headers;
      }
    }
    if (typeof spec.description === "string" && spec.description.trim()) {
      overlay.description = spec.description.trim();
    }

    candidates.push({
      kind: "mcp-server",
      name,
      originalName: rawName,
      overlay,
      transport,
      secretRefs: [...refs],
      envRewrites: rewrites,
      warnings: warns,
    });
  }

  if (candidates.length === 0 && errors.length === 0) {
    errors.push("no importable MCP servers found.");
  }
  return { candidates, errors, warnings };
}

// ---- auto-detect + unified entry --------------------------------------

export type DetectedKind = "skill" | "mcp" | "unknown";

// detectKind guesses whether a blob is a SKILL.md or an mcp.json, from the
// filename first, then the content. `unknown` → the UI should let the operator
// pick.
export function detectKind(text: string, filename?: string): DetectedKind {
  const fn = (filename ?? "").toLowerCase();
  if (fn.endsWith("skill.md")) return "skill";
  if (fn.endsWith(".mcp.json") || fn.endsWith("mcp.json") || fn.endsWith(".json")) return "mcp";
  if (fn.endsWith(".md")) return "skill";
  const trimmed = text.trimStart();
  if (trimmed.startsWith("---")) return "skill";
  if (trimmed.startsWith("{")) {
    try {
      const o = JSON.parse(text) as Record<string, unknown>;
      if (o && typeof o === "object" && (o.mcpServers || looksLikeServerMap(o))) return "mcp";
    } catch {
      /* not JSON */
    }
  }
  return "unknown";
}

// parseSource parses a single blob given an explicit kind (from auto-detect or
// an operator override) into candidates.
export function parseSource(
  text: string,
  kind: "skill" | "mcp",
  filename?: string,
): ParseResult {
  if (kind === "skill") {
    const fallback = deriveSkillName(filename);
    const { candidate, error } = parseSkillMarkdown(text, fallback);
    if (error) return { candidates: [], errors: [error], warnings: [] };
    return { candidates: candidate ? [candidate] : [], errors: [], warnings: [] };
  }
  return parseMcpJson(text);
}

// deriveSkillName pulls a name from a `skills/<name>/SKILL.md` path or a plain
// `<name>.md` filename, for when the frontmatter omits `name`.
function deriveSkillName(filename?: string): string | undefined {
  if (!filename) return undefined;
  const parts = filename.split("/").filter(Boolean);
  const base = parts[parts.length - 1] ?? "";
  if (base.toLowerCase() === "skill.md" && parts.length >= 2) return parts[parts.length - 2];
  if (base.toLowerCase().endsWith(".md")) return base.slice(0, -3);
  return undefined;
}

// ---- preview / collision resolution ------------------------------------

export type PreviewAction = "create" | "fork" | "blocked";

export interface PreviewRow {
  candidate: ImportCandidate;
  action: PreviewAction;
  /** For fork: the parent def to hang off (omit → bootstrap-from-static). */
  parentDefId?: string;
  note?: string; // why fork / why blocked
  warnings: string[]; // candidate warnings + resolution + capability warnings
}

export interface ResolveOpts {
  stdioAllowed: boolean;
  httpAllowlistConfigured: boolean;
  /** A non-admin tenant may create a per-tenant override of a static MCP name;
   *  skills always fork over a static name. */
  isTenant: boolean;
}

// reclaimable mirrors LibraryEditModal.validateLocal: a name with no live active
// version (all retired) and not static can be (re)created — it just allocates
// the next version. (skills/mcp LibraryEntry omit live_version_count, so we key
// on active_def_id + active_retired, matching the modal.)
function reclaimable(hit: LibraryEntry): boolean {
  return !hit.in_static && (!hit.active_def_id || hit.active_retired === true);
}

// resolvePreview turns candidates + the existing tenant Library entries into
// per-row create/fork/blocked decisions and warnings — the dry-run table. Pure:
// the server 422s stay authoritative; this only fails fast + guides.
export function resolvePreview(
  candidates: ImportCandidate[],
  existing: { skills: LibraryEntry[]; mcp: LibraryEntry[] },
  opts: ResolveOpts,
): PreviewRow[] {
  return candidates.map((c) => {
    const warnings = [...c.warnings];
    const entries = c.kind === "skill" ? existing.skills : existing.mcp;
    const hit = entries.find((e) => e.name === c.name);

    // Capability / secret warnings.
    if (c.kind === "mcp-server") {
      if (c.transport === "stdio" && !opts.stdioAllowed) {
        return {
          candidate: c,
          action: "blocked",
          note: "stdio import is off. Set LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO=1 (host RCE — operator opt-in), or declare it in yaml mcp_servers:.",
          warnings,
        };
      }
      if (c.transport === "stdio") {
        warnings.push("⚠ stdio runs an arbitrary command on the loomcycle host (RCE-class trust).");
      }
      if (c.transport !== "stdio" && !opts.httpAllowlistConfigured) {
        warnings.push("no http host allowlist is configured — this will 422 unless the operator lists the host.");
      }
      if (c.secretRefs.length > 0) {
        warnings.push(
          `needs host env: ${c.secretRefs.join(", ")} — a tenant can't set these; ask your operator to provision them (the ref is stored, never the value).`,
        );
      }
    }

    let action: PreviewAction = "create";
    let parentDefId: string | undefined;
    let note: string | undefined;

    if (hit) {
      if (c.kind === "skill") {
        if (hit.in_static || (hit.active_def_id && !reclaimable(hit))) {
          action = "fork";
          parentDefId =
            hit.active_def_id && !hit.active_def_id.startsWith("static:") ? hit.active_def_id : undefined;
          note = hit.in_static
            ? "a skill of this name is defined in yaml — importing forks a tenant version over it."
            : "an active version exists — importing adds a new version (fork).";
        } else if (reclaimable(hit)) {
          note = "a retired version of this name exists — importing reclaims it (new version).";
        }
      } else {
        // MCP: a tenant MAY create a per-tenant override of a static-named
        // server (RFC N); only a live dynamic version forces a fork.
        if (hit.active_def_id && !reclaimable(hit)) {
          action = "fork";
          parentDefId = !hit.active_def_id.startsWith("static:") ? hit.active_def_id : undefined;
          note = "an active version exists — importing adds a new version (fork).";
        } else if (hit.in_static && opts.isTenant) {
          note = "a server of this name is defined in yaml — importing creates a per-tenant override.";
        } else if (hit.in_static && !opts.isTenant) {
          action = "fork";
          note = "a server of this name is defined in yaml — importing forks a version over it.";
        }
      }
    }

    return { candidate: c, action, parentDefId, note, warnings };
  });
}
