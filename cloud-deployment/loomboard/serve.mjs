// Tiny dependency-free static server + loomcycle API proxy for the hosted
// loomboard SPA. Static files under SRV_ROOT with SPA history fallback; /v1/*
// reverse-proxied to a PINNED upstream (the loomcycle runtime). The upstream is
// fixed here, so the app's x-loomcycle-target header is never honored — this is
// not an open relay. No npm deps (Node http + fs only).
import http from "node:http";
import { stat } from "node:fs/promises";
import { createReadStream } from "node:fs";
import { extname, join, resolve, sep } from "node:path";

const ROOT = resolve(process.env.SRV_ROOT || "/srv");
const UP = new URL(process.env.LOOMCYCLE_UPSTREAM || "http://tailscale:8787");
const PORT = Number(process.env.PORT || 8080);

const MIME = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".mjs": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".json": "application/json",
  ".svg": "image/svg+xml",
  ".png": "image/png",
  ".jpg": "image/jpeg",
  ".jpeg": "image/jpeg",
  ".gif": "image/gif",
  ".webp": "image/webp",
  ".ico": "image/x-icon",
  ".woff": "font/woff",
  ".woff2": "font/woff2",
  ".ttf": "font/ttf",
  ".map": "application/json",
  ".txt": "text/plain; charset=utf-8",
  ".wasm": "application/wasm",
  ".mp4": "video/mp4",
  ".webmanifest": "application/manifest+json",
};

function sendFile(res, file) {
  const ct = MIME[extname(file).toLowerCase()] || "application/octet-stream";
  const stream = createReadStream(file);
  stream.on("open", () => res.writeHead(200, { "content-type": ct }));
  stream.on("error", () => {
    if (!res.headersSent) res.writeHead(404);
    res.end("not found");
  });
  stream.pipe(res);
}

const server = http.createServer((req, res) => {
  const url = req.url || "/";

  // API — reverse-proxy /v1/* to the pinned runtime upstream (whoami is /v1/_me,
  // and every @loomcycle/client call lives under /v1).
  if (url === "/v1" || url.startsWith("/v1/")) {
    const headers = { ...req.headers, host: UP.host };
    const up = http.request(
      {
        hostname: UP.hostname,
        port: UP.port || 80,
        path: url,
        method: req.method,
        headers,
      },
      (r) => {
        res.writeHead(r.statusCode || 502, r.headers);
        r.pipe(res);
      },
    );
    up.on("error", () => {
      if (!res.headersSent) res.writeHead(502);
      res.end("upstream error");
    });
    req.pipe(up);
    return;
  }

  // Static + SPA history fallback, contained to ROOT.
  let pathname = "/";
  try {
    pathname = decodeURIComponent(new URL(url, "http://x").pathname);
  } catch {
    /* keep default */
  }
  const target = resolve(ROOT, "." + pathname);
  if (target !== ROOT && !target.startsWith(ROOT + sep)) {
    res.writeHead(400);
    res.end("bad path");
    return;
  }
  stat(target)
    .then((s) => sendFile(res, s.isFile() ? target : join(ROOT, "index.html")))
    .catch(() => sendFile(res, join(ROOT, "index.html")));
});

server.listen(PORT, () =>
  console.log(`loomboard: serving ${ROOT} on :${PORT}, /v1 -> ${UP.href}`),
);
