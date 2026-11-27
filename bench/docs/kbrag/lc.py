"""Shared client for the KB-vs-RAG benchmark against a remote loomcycle tenant.

Standard library only. The bearer comes from LOOMCYCLE_BENCH_TOKEN_CQA and is
never printed, logged or written anywhere; error messages carry status codes
and response bodies only, never request headers.
"""
import json, os, sys, time, urllib.error, urllib.parse, urllib.request

BASE = os.environ.get("LC_BASE", "").rstrip("/")
_TOKEN = os.environ.get("LOOMCYCLE_BENCH_TOKEN_CQA")
if not _TOKEN:
    raise RuntimeError("LOOMCYCLE_BENCH_TOKEN_CQA is not set: export the tenant bearer before running the harness")

TENANT = "conditional-qa"
# Model-serving (provider, model) pairs the study may use (operator policy,
# 2026-11-22): Claude Haiku on the deployment, anything on ollama-local, and
# the deployment's embedder. Anything else stops the harness.
ALLOWED_PAIRS = {("anthropic", "claude-haiku-5-5")}
ALLOWED_PROVIDERS = {"ollama-local"}
# Claude Haiku 5.5 list price (USD / million tokens, prompts up to 100K), used
# only when the usage ledger leaves an Anthropic call unpriced. The deployment's
# ledger prices it identically (checked against the smoke runs).
HAIKU_PRICE = {"input": 0.10, "output": 0.50, "cache_read": 0.01, "cache_write": 0.125}
_embedder = None


class HTTPFail(Exception):
    def __init__(self, status, body):
        super().__init__("HTTP %s: %s" % (status, body[:500]))
        self.status, self.body = status, body


def _req(method, path, body=None, timeout=600, accept="application/json"):
    if not BASE:
        raise RuntimeError("LC_BASE is not set: export the deployment base URL before running the harness")
    data = None if body is None else json.dumps(body).encode()
    r = urllib.request.Request(BASE + path, data=data, method=method)
    r.add_header("Authorization", "Bearer " + _TOKEN)
    r.add_header("Content-Type", "application/json")
    r.add_header("Accept", accept)
    return urllib.request.urlopen(r, timeout=timeout)


def call(method, path, body=None, *, timeout=600, retries=6, retry=True):
    """JSON request. Retries 429/5xx/network with backoff when retry is set."""
    delay = 2.0
    for attempt in range(retries + 1):
        try:
            with _req(method, path, body, timeout) as resp:
                raw = resp.read().decode()
                return json.loads(raw) if raw.strip() else {}
        except urllib.error.HTTPError as e:
            text = e.read().decode("utf-8", "replace")
            if retry and (e.code == 429 or e.code >= 500) and attempt < retries:
                # Logged (path and status only, never headers) so rate limiting shows in the run logs.
                print("lc: HTTP %s on %s %s, retry %d in %.0f s" % (e.code, method, path.split("?")[0], attempt + 1, delay),
                      file=sys.stderr, flush=True)
                time.sleep(delay); delay = min(delay * 2, 120); continue
            raise HTTPFail(e.code, text)
        except (urllib.error.URLError, TimeoutError, ConnectionError, OSError) as e:
            if retry and attempt < retries:
                time.sleep(delay); delay = min(delay * 2, 120); continue
            raise HTTPFail(0, "network: %s" % e)


def get(path, **kw):
    return call("GET", path, **kw)


def version():
    inst = get("/v1/config").get("instance") or {}
    return {"version": inst.get("version"), "commit": inst.get("commit")}


def doc(op, **fields):
    """POST /v1/_document with the Document tool's input. Read ops retry; writes retry
    only on 429/503 since the server refused them before doing anything."""
    body = dict(fields); body["op"] = op
    readonly = op in {"get_document", "get_chunk", "query_chunks", "query_documents", "search",
                      "list_annotations", "export_md", "list_chunks"}
    try:
        return call("POST", "/v1/_document", body, timeout=900, retry=readonly)
    except HTTPFail as e:
        if not readonly and e.status in (429, 503):
            time.sleep(10)
            return call("POST", "/v1/_document", body, timeout=900, retry=False)
        raise


def path_op(op, **fields):
    body = dict(fields); body["op"] = op
    return call("POST", "/v1/_path", body, retry=op in {"ls", "stat", "resolve"})


def agentdef(op, **fields):
    body = dict(fields); body["op"] = op
    r = call("POST", "/v1/_agentdef", body, retry=op in {"get", "list"})
    if isinstance(r, dict) and r.get("error") and not r.get("def_id"):
        raise HTTPFail(200, json.dumps(r))
    return r


def run(agent, prompt, *, metadata=None, user_tier="free", idempotency_key=None, timeout=1800,
        provider=None, model=None, run_timeout_seconds=None):
    """Start a run over SSE and wait for it. Never raises on a failed run.
    run_timeout_seconds (extra, optional): a code-js agent's active-time budget
    for this run (the deployment's default is 1 h)."""
    body = {"agent": agent, "prompt": prompt, "user_tier": user_tier}
    if run_timeout_seconds: body["run_timeout_seconds"] = int(run_timeout_seconds)
    if metadata: body["metadata"] = metadata
    if idempotency_key: body["idempotency_key"] = idempotency_key
    if provider: body["provider"] = provider
    if model: body["model"] = model
    out = {"run_id": None, "text": "", "status": None, "error": None,
           "events_usage": {"input_tokens": 0, "output_tokens": 0, "cache_read_tokens": 0, "cache_creation_tokens": 0},
           "wall_s": None}
    t0 = time.time()
    deadline = t0 + timeout
    for attempt in range(4):
        try:
            _stream(body, out, timeout)
            break
        except urllib.error.HTTPError as e:
            text = e.read().decode("utf-8", "replace")
            if (e.code == 429 or e.code >= 500) and attempt < 3 and out["run_id"] is None:
                print("lc: HTTP %s starting a %s run, retry %d" % (e.code, agent, attempt + 1), file=sys.stderr, flush=True)
                time.sleep(5 * (attempt + 1)); continue
            out["status"], out["error"] = "failed", "HTTP %s: %s" % (e.code, text[:500])
            break
        except Exception as e:  # dropped stream: fall through to polling
            if out["run_id"] is None:
                if attempt < 3 and idempotency_key:
                    time.sleep(5 * (attempt + 1)); continue
                out["status"], out["error"] = "failed", "stream: %s" % e
                break
            out["error"] = "stream dropped: %s" % e
            break
    if out["run_id"] and out["status"] not in ("completed", "failed", "cancelled"):
        _poll(out, deadline)
    out["wall_s"] = round(time.time() - t0, 2)
    return out


def _stream(body, out, timeout):
    with _req("POST", "/v1/runs", body, timeout, accept="text/event-stream") as resp:
        event = None
        for raw in resp:
            line = raw.decode("utf-8", "replace").rstrip("\r\n")
            if line.startswith("event:"):
                event = line[6:].strip(); continue
            if not line.startswith("data:"):
                continue
            try:
                d = json.loads(line[5:].strip())
            except ValueError:
                continue
            kind = event or d.get("type")
            if out["run_id"] is None and d.get("run_id"):
                out["run_id"] = d["run_id"]
            if kind == "text":
                out["text"] += d.get("text", "")
            elif kind == "usage":
                u = d.get("usage") or d
                eu = out["events_usage"]
                eu["input_tokens"] += u.get("input_tokens", 0) or 0
                eu["output_tokens"] += u.get("output_tokens", 0) or 0
                eu["cache_read_tokens"] += u.get("cache_read_input_tokens", u.get("cache_read_tokens", 0)) or 0
                eu["cache_creation_tokens"] += u.get("cache_creation_input_tokens", u.get("cache_creation_tokens", 0)) or 0
            elif kind == "done":
                out["status"] = "completed" if not out["error"] else "failed"
                out["stop_reason"] = d.get("stop_reason")
            elif kind == "error":
                out["error"] = d.get("text") or d.get("error") or json.dumps(d)[:500]
                out["status"] = "failed"


def _poll(out, deadline):
    """Re-attach after a dropped stream: poll the run row until it ends."""
    while time.time() < deadline:
        try:
            row = get("/v1/runs/" + out["run_id"])
        except HTTPFail as e:
            out["error"] = "poll: %s" % e; time.sleep(15); continue
        st = row.get("status")
        if st in ("completed", "failed", "cancelled", "error"):
            out["status"] = "completed" if st == "completed" else "failed"
            res = row.get("result") or {}
            if isinstance(res, dict) and res.get("final_text") and not out["text"]:
                out["text"] = res["final_text"]
            if row.get("error"):
                out["error"] = row["error"]
            elif st == "completed":
                out["error"] = None
            return
        time.sleep(10)
    out["status"], out["error"] = "timeout", (out["error"] or "") + " (poll deadline)"


def _children(run_id):
    rows, cursor = [], None
    while True:
        q = {"parent_run_id": run_id, "limit": 1000}
        if cursor: q["cursor"] = cursor
        r = get("/v1/runs?" + urllib.parse.urlencode(q))
        batch = r.get("agents") or []
        rows += batch
        cursor = r.get("next_cursor")
        if not cursor or not batch:
            return rows


def _row(r):
    u = r.get("usage") or {}
    return {"run_id": r.get("run_id") or r.get("id"), "agent": r.get("agent"),
            "parent_run_id": r.get("parent_run_id"),
            "provider": u.get("provider") or "", "model": u.get("model") or "",
            "status": r.get("status"), "stop_reason": r.get("stop_reason"),
            "input_tokens": u.get("input_tokens", 0), "output_tokens": u.get("output_tokens", 0),
            "cached_tokens": u.get("cache_read_tokens", 0),
            "cache_creation_tokens": u.get("cache_creation_tokens", 0),
            "error": r.get("error")}


def tree_usage(run_id):
    """The run and every descendant with its usage and serving provider/model."""
    runs = [_row(get("/v1/runs/" + run_id))]
    frontier = [run_id]
    while frontier:
        nxt = []
        for rid in frontier:
            for c in _children(rid):
                row = _row(c)
                runs.append(row); nxt.append(row["run_id"])
        frontier = nxt
    keys = ("input_tokens", "output_tokens", "cached_tokens", "cache_creation_tokens")
    totals = {k: sum(r[k] or 0 for r in runs) for k in keys}
    totals["runs"] = len(runs)
    provs = sorted({(r["provider"], r["model"]) for r in runs if r["provider"] or r["model"]})
    return {"runs": runs, "totals": totals, "providers": provs}


def disallowed(pairs):
    """The (provider, model) pairs outside the study's policy."""
    emb = embedder()
    bad = []
    for p, m in pairs:
        if not p and not m:
            continue
        if (p, m) in ALLOWED_PAIRS or p in ALLOWED_PROVIDERS or (p, m) == emb or p == "code-js":
            continue
        bad.append((p, m))
    return bad


def embedder():
    global _embedder
    if _embedder is None:
        e = (((get("/v1/config").get("features") or {}).get("vector_memory") or {}).get("embedder") or {})
        _embedder = (e.get("provider"), e.get("model"))
    return _embedder


def chat(messages, *, model, provider="anthropic", temperature=0, max_tokens=1024, timeout=600):
    """POST /v1/_llm/chat. Never raises; failures come back in `error`.
    (Default provider follows the 2026-11-22 policy change: Haiku on `anthropic`.)"""
    body = {"messages": messages, "provider": provider, "model": model,
            "temperature": temperature, "max_tokens": max_tokens, "user_tier": "free"}
    t0 = time.time()
    try:
        r = call("POST", "/v1/_llm/chat", body, timeout=timeout)
        text = "".join(b.get("text", "") for b in (r.get("content") or []) if b.get("type") == "text")
        return {"content": text or None, "usage": r.get("usage"), "stop_reason": r.get("stop_reason"),
                "provider": r.get("provider"), "model": r.get("model"),
                "wall_s": round(time.time() - t0, 2), "error": None}
    except HTTPFail as e:
        return {"content": None, "usage": None, "stop_reason": None,
                "wall_s": round(time.time() - t0, 2), "error": str(e)}


def embed(texts, batch=64):
    out = []
    for i in range(0, len(texts), batch):
        r = call("POST", "/v1/embeddings", {"input": texts[i:i + batch]}, timeout=600)
        data = sorted(r["data"], key=lambda d: d["index"])
        out += [d["embedding"] for d in data]
    return out


def usage_rows(group_by="provider,model,purpose", since="2026-01-01T00:00:00Z"):
    q = urllib.parse.urlencode({"group_by": group_by, "from": since})
    return get("/v1/_usage?" + q).get("rows") or []


def _row_cost(r):
    cost = r.get("cost") or 0.0
    if r.get("unpriced_calls") and (r.get("provider"), r.get("model")) in ALLOWED_PAIRS:
        # The ledger left an Anthropic call unpriced: price it from tokens at
        # list price (approximate: the unpriced share is assumed to be all).
        cost = (r.get("input_tokens", 0) * HAIKU_PRICE["input"] + r.get("output_tokens", 0) * HAIKU_PRICE["output"]
                + r.get("cache_read_tokens", 0) * HAIKU_PRICE["cache_read"]
                + r.get("cache_creation_tokens", 0) * HAIKU_PRICE["cache_write"]) / 1e6
    return cost


def spend():
    """Cumulative USD this tenant has spent (all model calls in the usage ledger)."""
    return round(sum(_row_cost(r) for r in usage_rows("provider,model")), 6)


def ledger_pairs(since="2026-01-01T00:00:00Z"):
    """Every (provider, model) the tenant's usage ledger shows since `since`."""
    return sorted({(r.get("provider") or "", r.get("model") or "") for r in usage_rows("provider,model", since)})
