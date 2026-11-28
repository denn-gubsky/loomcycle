"""The HC study's loomcycle client: ../lc.py's calls, bound to one tenant by environment
variable NAMES given at construction instead of module constants.

../lc.py and docs/dlc.py each hard-bind one tenant's variables at import. This study runs
two tenants from one process (the spend guard sums both ledgers), so the same calls live
on a class here. The call shapes, retries and usage keys are ../lc.py's, unchanged. Added:
`segments` on run() (the oracle hands kb/read an untrusted block) and run_events() (the
replayed events of a finished run, for instrument check 3).

The bearer is read from the environment at construction, kept on the instance and never
printed, logged or written; an error carries a status code and a body, never a header.
"""
import json, os, sys, time, urllib.error, urllib.parse, urllib.request

ALLOWED_PAIRS = {("anthropic", "claude-haiku-5-5")}
ALLOWED_PROVIDERS = {"ollama-local"}
HAIKU_PRICE = {"input": 0.10, "output": 0.50, "cache_read": 0.01, "cache_write": 0.125}
READ_OPS = {"get_document", "get_chunk", "query_chunks", "query_documents", "search", "list_annotations",
            "export_md", "graph_snapshot", "describe_ontology"}


class HTTPFail(Exception):
    def __init__(self, status, body):
        super().__init__("HTTP %s: %s" % (status, body[:500]))
        self.status, self.body = status, body


class Client:
    def __init__(self, *, base_env, token_env, tenant_env, environ=None):
        env = os.environ if environ is None else environ
        self.base = (env.get(base_env) or "").rstrip("/")
        self._token = env.get(token_env) or ""
        self.tenant = env.get(tenant_env) or ""
        missing = [n for n, v in ((base_env, self.base), (token_env, self._token), (tenant_env, self.tenant)) if not v]
        if missing:
            raise RuntimeError("not set: %s (export them before running the harness)" % ", ".join(missing))
        self._embedder = None

    def __repr__(self):  # never the bearer
        return "Client(tenant=%r)" % self.tenant

    # --- transport ---------------------------------------------------------------

    def _req(self, method, path, body=None, timeout=600, accept="application/json"):
        data = None if body is None else json.dumps(body).encode()
        r = urllib.request.Request(self.base + path, data=data, method=method)
        r.add_header("Authorization", "Bearer " + self._token)
        r.add_header("Content-Type", "application/json")
        r.add_header("Accept", accept)
        return urllib.request.urlopen(r, timeout=timeout)

    def call(self, method, path, body=None, *, timeout=600, retries=6, retry=True):
        delay = 2.0
        for attempt in range(retries + 1):
            try:
                with self._req(method, path, body, timeout) as resp:
                    raw = resp.read().decode()
                    return json.loads(raw) if raw.strip() else {}
            except urllib.error.HTTPError as e:
                text = e.read().decode("utf-8", "replace")
                if retry and (e.code == 429 or e.code >= 500) and attempt < retries:
                    print("hc: HTTP %s on %s %s, retry %d" % (e.code, method, path.split("?")[0], attempt + 1),
                          file=sys.stderr, flush=True)
                    time.sleep(delay); delay = min(delay * 2, 120); continue
                raise HTTPFail(e.code, text)
            except (urllib.error.URLError, TimeoutError, ConnectionError, OSError) as e:
                if retry and attempt < retries:
                    time.sleep(delay); delay = min(delay * 2, 120); continue
                raise HTTPFail(0, "network: %s" % e)

    def get(self, path, **kw):
        return self.call("GET", path, **kw)

    # --- reads -------------------------------------------------------------------

    def assert_tenant(self):
        """Refuse to go on unless the bearer resolves to the tenant the environment names."""
        me = self.get("/v1/_me")
        if me.get("tenant_id") != self.tenant:
            raise SystemExit("the bearer resolves to another tenant than the one configured: stopping")
        return self.tenant

    def version(self):
        inst = self.get("/v1/config").get("instance") or {}
        return {"version": inst.get("version"), "commit": inst.get("commit")}

    def uptime(self):
        """Seconds since the server process started (GET /healthz), or None when unreadable."""
        try:
            u = self.get("/healthz", retries=2).get("uptime_seconds")
        except Exception:  # a failed read proves nothing; the row keeps its own status
            return None
        return u if isinstance(u, (int, float)) else None

    def doc(self, op, **fields):
        body = dict(fields); body["op"] = op
        return self.call("POST", "/v1/_document", body, timeout=900, retry=op in READ_OPS)

    def agentdef(self, op, **fields):
        body = dict(fields); body["op"] = op
        r = self.call("POST", "/v1/_agentdef", body, retry=op in {"get", "list"})
        if isinstance(r, dict) and r.get("error") and not r.get("def_id"):
            raise HTTPFail(200, json.dumps(r))
        return r

    def library_agents(self):
        return {e["name"]: e for e in self.get("/v1/_library/agents").get("entries") or []}

    # --- runs --------------------------------------------------------------------

    def run(self, agent, prompt=None, *, segments=None, metadata=None, user_tier="free", idempotency_key=None,
            timeout=1800, run_timeout_seconds=None):
        """Start a run over SSE and wait for it. Never raises on a failed run.
        run_timeout_seconds: a code-js agent's active-time budget (../annotate.py raises it)."""
        body = {"agent": agent, "user_tier": user_tier}
        if run_timeout_seconds:
            body["run_timeout_seconds"] = int(run_timeout_seconds)
        if segments is not None:
            body["segments"] = segments
        else:
            body["prompt"] = prompt
        if metadata:
            body["metadata"] = metadata
        if idempotency_key:
            body["idempotency_key"] = idempotency_key
        out = {"run_id": None, "text": "", "status": None, "error": None, "wall_s": None}
        t0 = time.time()
        for attempt in range(4):
            try:
                with self._req("POST", "/v1/runs", body, timeout, accept="text/event-stream") as resp:
                    parse_sse(resp, out)
                break
            except urllib.error.HTTPError as e:
                text = e.read().decode("utf-8", "replace")
                if (e.code == 429 or e.code >= 500) and attempt < 3 and out["run_id"] is None:
                    time.sleep(5 * (attempt + 1)); continue
                out["status"], out["error"] = "failed", "HTTP %s: %s" % (e.code, text[:500])
                break
            except Exception as e:  # a dropped stream falls through to polling
                if out["run_id"] is None:
                    if attempt < 3 and idempotency_key:
                        time.sleep(5 * (attempt + 1)); continue
                    out["status"], out["error"] = "failed", "stream: %s" % e
                    break
                out["error"] = "stream dropped: %s" % e
                break
        if out["run_id"] and out["status"] not in ("completed", "failed", "cancelled"):
            self._poll(out, t0 + timeout)
        out["wall_s"] = round(time.time() - t0, 2)
        return out

    def _poll(self, out, deadline):
        while time.time() < deadline:
            try:
                row = self.get("/v1/runs/" + out["run_id"])
            except HTTPFail as e:
                out["error"] = "poll: %s" % e; time.sleep(15); continue
            st = row.get("status")
            if st in ("completed", "failed", "cancelled", "error"):
                out["status"] = "completed" if st == "completed" else "failed"
                res = row.get("result") or {}
                if isinstance(res, dict) and res.get("final_text") and not out["text"]:
                    out["text"] = res["final_text"]
                out["error"] = row.get("error") or (None if st == "completed" else out["error"])
                return
            time.sleep(10)
        out["status"], out["error"] = "timeout", (out["error"] or "") + " (poll deadline)"

    def run_events(self, run_id, timeout=300):
        """A finished run's events, replayed (GET /v1/runs/{id}/stream?from_seq=0)."""
        events = []
        with self._req("GET", "/v1/runs/%s/stream?from_seq=0" % urllib.parse.quote(run_id), None, timeout,
                       accept="text/event-stream") as resp:
            parse_sse(resp, {"run_id": run_id, "text": "", "status": None, "error": None}, events)
        return events

    def _children(self, run_id):
        rows, cursor = [], None
        while True:
            q = {"parent_run_id": run_id, "limit": 1000}
            if cursor:
                q["cursor"] = cursor
            r = self.get("/v1/runs?" + urllib.parse.urlencode(q))
            batch = r.get("agents") or []
            rows += batch
            cursor = r.get("next_cursor")
            if not cursor or not batch:
                return rows

    def tree_usage(self, run_id):
        """The run and every descendant with its usage and serving provider/model."""
        runs = [tree_row(self.get("/v1/runs/" + run_id))]
        frontier = [run_id]
        while frontier:
            nxt = []
            for rid in frontier:
                for c in self._children(rid):
                    row = tree_row(c)
                    runs.append(row); nxt.append(row["run_id"])
            frontier = nxt
        keys = ("input_tokens", "output_tokens", "cached_tokens", "cache_creation_tokens")
        totals = {k: sum(r[k] or 0 for r in runs) for k in keys}
        totals["runs"] = len(runs)
        provs = sorted({(r["provider"], r["model"]) for r in runs if r["provider"] or r["model"]})
        return {"runs": runs, "totals": totals, "providers": provs}

    # --- model calls and the ledger ------------------------------------------------

    def chat(self, messages, *, model, provider="anthropic", temperature=0, max_tokens=1024, timeout=600):
        body = {"messages": messages, "provider": provider, "model": model,
                "temperature": temperature, "max_tokens": max_tokens, "user_tier": "free"}
        t0 = time.time()
        try:
            r = self.call("POST", "/v1/_llm/chat", body, timeout=timeout)
            text = "".join(b.get("text", "") for b in (r.get("content") or []) if b.get("type") == "text")
            return {"content": text or None, "usage": r.get("usage"), "stop_reason": r.get("stop_reason"),
                    "provider": r.get("provider"), "model": r.get("model"),
                    "wall_s": round(time.time() - t0, 2), "error": None}
        except HTTPFail as e:
            return {"content": None, "usage": None, "stop_reason": None, "wall_s": round(time.time() - t0, 2),
                    "error": str(e)}

    def usage_rows(self, group_by="provider,model", since="2026-01-01T00:00:00Z"):
        q = urllib.parse.urlencode({"group_by": group_by, "from": since})
        return self.get("/v1/_usage?" + q).get("rows") or []

    def spend(self):
        """Cumulative USD this tenant's usage ledger shows."""
        return round(sum(ledger_row_cost(r) for r in self.usage_rows("provider,model")), 6)

    def ledger_pairs(self, since="2026-01-01T00:00:00Z"):
        return sorted({(r.get("provider") or "", r.get("model") or "") for r in self.usage_rows("provider,model", since)})

    def embedder(self):
        if self._embedder is None:
            e = (((self.get("/v1/config").get("features") or {}).get("vector_memory") or {}).get("embedder") or {})
            self._embedder = (e.get("provider"), e.get("model"))
        return self._embedder

    def disallowed(self, pairs):
        emb = self.embedder()
        return [(p, m) for p, m in pairs if (p or m) and not (
            (p, m) in ALLOWED_PAIRS or p in ALLOWED_PROVIDERS or (p, m) == emb or p == "code-js")]


def parse_sse(lines, out, events=None):
    """Fold an SSE stream (an iterable of byte lines) into `out` (run_id, text, status, error);
    when `events` is a list, every decoded event is appended to it as {type, data}."""
    event = None
    for raw in lines:
        line = (raw.decode("utf-8", "replace") if isinstance(raw, bytes) else raw).rstrip("\r\n")
        if line.startswith("event:"):
            event = line[6:].strip(); continue
        if not line.startswith("data:"):
            continue
        try:
            d = json.loads(line[5:].strip())
        except ValueError:
            continue
        kind = event or d.get("type")
        if events is not None:
            events.append({"type": kind, "data": d})
        if out["run_id"] is None and isinstance(d, dict) and d.get("run_id"):
            out["run_id"] = d["run_id"]
        if kind == "text":
            out["text"] += d.get("text", "")
        elif kind == "done":
            out["status"] = "completed" if not out["error"] else "failed"
        elif kind == "error":
            out["error"] = d.get("text") or d.get("error") or json.dumps(d)[:500]
            out["status"] = "failed"
    return out


def tree_row(r):
    """One run row of GET /v1/runs: the server's usage keys (cache_read_tokens,
    cache_creation_tokens) onto the names ../common.Usage reads (cached_tokens, ...)."""
    u = r.get("usage") or {}
    return {"run_id": r.get("run_id") or r.get("id"), "agent": r.get("agent"), "parent_run_id": r.get("parent_run_id"),
            "provider": u.get("provider") or "", "model": u.get("model") or "", "status": r.get("status"),
            "stop_reason": r.get("stop_reason"), "input_tokens": u.get("input_tokens", 0),
            "output_tokens": u.get("output_tokens", 0), "cached_tokens": u.get("cache_read_tokens", 0),
            "cache_creation_tokens": u.get("cache_creation_tokens", 0), "error": r.get("error")}


def ledger_row_cost(r):
    """A usage-report row's USD: the ledger's own cost, or list price where it left a Haiku
    call unpriced (as ../lc.py)."""
    cost = r.get("cost") or 0.0
    if r.get("unpriced_calls") and (r.get("provider"), r.get("model")) in ALLOWED_PAIRS:
        cost = (r.get("input_tokens", 0) * HAIKU_PRICE["input"] + r.get("output_tokens", 0) * HAIKU_PRICE["output"]
                + r.get("cache_read_tokens", 0) * HAIKU_PRICE["cache_read"]
                + r.get("cache_creation_tokens", 0) * HAIKU_PRICE["cache_write"]) / 1e6
    return cost


def for_set(name, *, base_env=None, token_env=None, tenant_env=None):
    """The client of one study set's tenant, from hcconfig.TENANTS (each name overridable)."""
    import hcconfig
    t = hcconfig.TENANTS[name]
    return Client(base_env=base_env or t["base_env"], token_env=token_env or t["token_env"],
                  tenant_env=tenant_env or t["tenant_env"])
