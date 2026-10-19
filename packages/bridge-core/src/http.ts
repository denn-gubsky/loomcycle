// A small HTTP listener for bridges whose platform posts webhooks.
//
// It hands each route the RAW body: Slack, Discord and Telegram authenticate
// the exact bytes (or a header beside them), and a JSON middleware ahead of
// that check would parse, and re-serialise, before anything is verified. A
// route verifies first, then parses. No web framework: node:http is enough.

import http, { type IncomingHttpHeaders } from "node:http";

import type { Health } from "./health.js";
import type { Logger } from "./ports.js";

export interface RawRequest {
  method: string;
  /** The path without the query string. */
  path: string;
  query: URLSearchParams;
  headers: IncomingHttpHeaders;
  body: Buffer;
}

export interface RawResponse {
  status: number;
  headers?: Record<string, string>;
  /** A string or Buffer is sent as is; anything else as JSON. */
  body?: string | Buffer | object;
}

export type Route = (req: RawRequest) => Promise<RawResponse>;

export interface ServerOptions {
  /** By "METHOD /path", e.g. "POST /slack/events". Exact match. */
  routes: Record<string, Route>;
  health?: Health;
  log: Logger;
  /** Bodies past this are refused with 413 before a route sees them. */
  maxBodyBytes?: number;
}

export const DEFAULT_MAX_BODY_BYTES = 1 << 20;

/** header reads one header value. Node joins a repeated header into one
 *  "a, b" string (and gives set-cookie as a list, read here as undefined), so
 *  a repeated signature or secret header never matches a single expected value. */
export function header(req: RawRequest, name: string): string | undefined {
  const v = req.headers[name.toLowerCase()];
  return typeof v === "string" ? v : undefined;
}

export function createBridgeServer(opts: ServerOptions): http.Server {
  const max = opts.maxBodyBytes ?? DEFAULT_MAX_BODY_BYTES;
  return http.createServer((req, res) => {
    const url = new URL(req.url ?? "/", "http://bridge.local");
    const method = req.method ?? "GET";

    const reply = (r: RawResponse) => {
      if (res.headersSent) return;
      let body: string | Buffer = "";
      const headers: Record<string, string> = { ...(r.headers ?? {}) };
      if (typeof r.body === "string" || Buffer.isBuffer(r.body)) body = r.body;
      else if (r.body !== undefined) {
        body = JSON.stringify(r.body);
        headers["content-type"] ??= "application/json";
      }
      res.writeHead(r.status, headers);
      res.end(body);
    };

    if (method === "GET" && url.pathname === "/healthz" && opts.health) {
      reply({ status: 200, body: opts.health.snapshot() });
      req.resume();
      return;
    }
    const route = opts.routes[`${method} ${url.pathname}`];
    if (!route) {
      reply({ status: 404, body: "not found" });
      req.resume();
      return;
    }

    const declared = Number(req.headers["content-length"]);
    if (Number.isFinite(declared) && declared > max) {
      reply({ status: 413, body: "request too large" });
      req.resume();
      return;
    }
    const chunks: Buffer[] = [];
    let size = 0;
    let over = false;
    req.on("data", (c: Buffer) => {
      if (over) return;
      size += c.length;
      if (size > max) {
        over = true;
        reply({ status: 413, body: "request too large" });
        return;
      }
      chunks.push(c);
    });
    req.on("error", () => {
      over = true;
    });
    req.on("end", () => {
      if (over) return;
      const raw: RawRequest = {
        method,
        path: url.pathname,
        query: url.searchParams,
        headers: req.headers,
        body: Buffer.concat(chunks),
      };
      route(raw).then(reply, (err: unknown) => {
        opts.log.error("request failed", { path: url.pathname, error: (err as Error)?.name });
        reply({ status: 500, body: "internal error" });
      });
    });
  });
}

/** listen starts a server on port and resolves once it is accepting. */
export function listen(server: http.Server, port: number, host?: string): Promise<void> {
  return new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(port, host, () => {
      server.off("error", reject);
      resolve();
    });
  });
}

/** close stops a server, waiting for open requests. */
export function close(server: http.Server): Promise<void> {
  return new Promise((resolve) => server.close(() => resolve()));
}
