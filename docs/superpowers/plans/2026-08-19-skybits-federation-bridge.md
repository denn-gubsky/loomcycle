# Skybits Federation Bridge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a standalone Python bridge implementing the peer side of loomcycle's document-federation protocol (`POST /v1/_document`, 9 ops) backed by the Skybits API, so loomcycle can `diff_remote` / `sync pull|push` documents with Skybits — zero loomcycle core changes.

**Architecture:** FastAPI service with one endpoint + `/healthz`. A Skybits REST client (connector API key, `POST /v1/tools/{tool}`), a deterministic projection of Skybits docs into keyed federation chunks (heading-split for flat docs, `natural_key` = Skybits nodeId or derived heading hash), and a SQLite store for chunk metadata Skybits lacks (tags/title/type/status/per-chunk revision). Spec: `docs/SKYBITS-FEDERATION.md` in the loomcycle repo (branch `feature-skybits-integration`).

**Tech Stack:** Python 3.12, FastAPI + uvicorn, SQLite (stdlib `sqlite3`), `httpx`, pytest + FastAPI `TestClient`.

## Global Constraints

- Bridge repo is NEW and standalone: `/Users/W/dev/skybits-loomcycle-bridge` (consumer-side; NOT inside the loomcycle clone). Own git repo, own commits.
- Wire contract is EXACT (from loomcycle v1.59 `internal/tools/builtin/document_sync.go`): one op per `POST /v1/_document`; success = HTTP 200 with op-result JSON; refusals = HTTP 422 `{"code":"tool_refused","error":"<msg>","tool":"Document"}`; bad JSON = 400 `{"code":"bad_request",...}`; fault = 500 `{"code":"internal",...}`.
- `list_facts` and `query_chunks` MUST honor `limit:10000` literally (no silent cap — loomcycle never checks `truncated`).
- Every op payload contains `scope`; validate presence, treat as opaque otherwise.
- `update_chunk` semantics: presence-based field writes; `tags` replace-set; body present → history (Skybits edit), body absent → no history; stale `revision` → 422 revision conflict.
- Nothing is ever deleted by sync; the bridge never calls `trash_documents`.
- Auth: loomcycle→bridge static bearer (`BRIDGE_TOKEN` env); bridge→Skybits connector key (`SKYBITS_API_KEY` env). Skybits REST API is connector-key-only — OAuth access tokens do NOT work there.
- TDD: every behavior starts as a failing test. Commits: conventional style, one per task.
- Skybits doc kinds: MVP handles `doc` only; canvas/spreadsheet → 422 `tool_refused`.

---

### Task 1: Repo scaffold + MetaStore

**Files:**
- Create: `pyproject.toml`, `bridge/__init__.py`, `bridge/store.py`
- Test: `tests/test_store.py`

**Interfaces:**
- Produces: `class MetaStore(path: str)` with
  - `get(document_id: str, natural_key: str) -> dict | None` — returns `{"title":str,"type":str,"status":str,"tags":list[str],"revision":int}`
  - `ensure(document_id: str, natural_key: str, title: str = "") -> dict` — insert defaults `(title, "section", "active", [], 1)` if absent, then `get`
  - `write(document_id: str, natural_key: str, *, title=None, type=None, status=None, tags=None, bump_revision: bool = False) -> dict` — presence-based update; `tags` replace-set; `bump_revision=True` increments `revision`
  - `all(document_id: str) -> dict[str, dict]` — natural_key → meta

- [ ] **Step 1: Write the failing test**

```python
# tests/test_store.py
from bridge.store import MetaStore

def test_ensure_then_get_roundtrip(tmp_path):
    s = MetaStore(str(tmp_path / "m.db"))
    m = s.ensure("sb_doc1", "nk1", title="Goals")
    assert m["revision"] == 1 and m["tags"] == [] and m["type"] == "section"
    assert s.get("sb_doc1", "nk1")["title"] == "Goals"

def test_write_presence_based_and_revision_bump(tmp_path):
    s = MetaStore(str(tmp_path / "m.db"))
    s.ensure("d", "k")
    s.write("d", "k", tags=["a", "b"])
    assert s.get("d", "k")["tags"] == ["a", "b"]
    assert s.get("d", "k")["revision"] == 1            # no bump without body change
    s.write("d", "k", title="T2", bump_revision=True)
    m = s.get("d", "k")
    assert m["title"] == "T2" and m["revision"] == 2 and m["tags"] == ["a", "b"]
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/W/dev/skybits-loomcycle-bridge && python -m pytest tests/test_store.py -v`
Expected: FAIL — `ModuleNotFoundError: bridge.store`

- [ ] **Step 3: Write minimal implementation**

```python
# bridge/store.py
import json, sqlite3

SCHEMA = """CREATE TABLE IF NOT EXISTS chunk_meta(
  document_id TEXT NOT NULL, natural_key TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '', type TEXT NOT NULL DEFAULT 'section',
  status TEXT NOT NULL DEFAULT 'active', tags TEXT NOT NULL DEFAULT '[]',
  revision INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY(document_id, natural_key))"""

class MetaStore:
    def __init__(self, path: str):
        self.db = sqlite3.connect(path, check_same_thread=False)
        self.db.executescript(SCHEMA)

    def get(self, document_id, natural_key):
        row = self.db.execute(
            "SELECT title,type,status,tags,revision FROM chunk_meta "
            "WHERE document_id=? AND natural_key=?", (document_id, natural_key)).fetchone()
        if not row:
            return None
        return {"title": row[0], "type": row[1], "status": row[2],
                "tags": json.loads(row[3]), "revision": row[4]}

    def ensure(self, document_id, natural_key, title=""):
        self.db.execute(
            "INSERT OR IGNORE INTO chunk_meta(document_id,natural_key,title) VALUES(?,?,?)",
            (document_id, natural_key, title))
        self.db.commit()
        return self.get(document_id, natural_key)

    def write(self, document_id, natural_key, *, title=None, type=None,
              status=None, tags=None, bump_revision=False):
        self.ensure(document_id, natural_key)
        sets, vals = [], []
        for col, v in (("title", title), ("type", type), ("status", status),
                       ("tags", json.dumps(tags) if tags is not None else None)):
            if v is not None:
                sets.append(f"{col}=?"); vals.append(v)
        if bump_revision:
            sets.append("revision=revision+1")
        if sets:
            vals += [document_id, natural_key]
            self.db.execute(f"UPDATE chunk_meta SET {', '.join(sets)} "
                            "WHERE document_id=? AND natural_key=?", vals)
            self.db.commit()
        return self.get(document_id, natural_key)

    def all(self, document_id):
        return {r[0]: self.get(document_id, r[0]) for r in self.db.execute(
            "SELECT natural_key FROM chunk_meta WHERE document_id=?", (document_id,))}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `python -m pytest tests/test_store.py -v` — Expected: 2 PASSED

- [ ] **Step 5: Commit**

```bash
cd /Users/W/dev/skybits-loomcycle-bridge && git init -q && git add -A
git commit -m "feat: scaffold + chunk metadata store"
```

---

### Task 2: Skybits client + document projection

**Files:**
- Create: `bridge/skybits.py`, `bridge/projection.py`
- Test: `tests/test_projection.py`, `tests/fakes.py`

**Interfaces:**
- Consumes: nothing from Task 1
- Produces:
  - `class Skybits(api_key: str, host: str = "https://skybits.ai")` with
    `call(tool: str, args: dict) -> any` — `POST {host}/v1/tools/{tool}`, bearer, raises `SkybitsError(msg, code=None)` when the response is `{"ok":false,...}` or non-200
    `read_doc(ref: str) -> dict` — `{"uuid": str, "title": str, "markdown": str, "revision": int, "kind": str}` (wraps `read_document` with `format:"json"`; `ref` accepts a `https://skybits.ai/documents/<uuid>` URL or a bare uuid)
  - `class SkybitsError(Exception)` with `.code: str | None`
  - `project_chunks(document_id: str, markdown: str) -> list[dict]` — each `{"natural_key": str, "title": str, "body": str, "position": int}`; heading-split, deterministic keys
  - `doc_document_id(uuid: str) -> str` (= `"sb_"+uuid`), `root_chunk_id(uuid) -> str` (= `"sbroot_"+uuid`), `chunk_id(document_id, natural_key) -> str` (= `"sbc_"+sha1[:16]`)

- [ ] **Step 1: Write the failing test**

```python
# tests/test_projection.py
from bridge.projection import project_chunks, chunk_id, doc_document_id

MD = ("# Atlas\n\nintro paragraph\n\n## Goals\n- grow activation\n\n"
      "## Risks\napp-store delay\n")

def test_heading_split_produces_stable_keyed_chunks():
    chunks = project_chunks("sb_u1", MD)
    assert [c["title"] for c in chunks] == ["Atlas", "Goals", "Risks"]
    assert chunks[1]["body"].strip() == "- grow activation"
    assert [c["position"] for c in chunks] == [0, 1, 2]
    # deterministic: same input → same natural keys
    assert [c["natural_key"] for c in project_chunks("sb_u1", MD)] == \
           [c["natural_key"] for c in chunks]

def test_flat_doc_projects_single_chunk():
    chunks = project_chunks("sb_u1", "no headings here at all")
    assert len(chunks) == 1 and chunks[0]["natural_key"] == "doc_u1"

def test_chunk_id_is_stable_and_scoped():
    a = chunk_id("sb_u1", "k1"); b = chunk_id("sb_u2", "k1")
    assert a.startswith("sbc_") and a != b and a == chunk_id("sb_u1", "k1")
    assert doc_document_id("u1") == "sb_u1"
```

- [ ] **Step 2: Run test to verify it fails**

Run: `python -m pytest tests/test_projection.py -v` — Expected: FAIL (module missing)

- [ ] **Step 3: Write minimal implementation**

```python
# bridge/projection.py
import hashlib, re

HEADING_RE = re.compile(r"^(#{1,6})\s+(.+?)\s*$", re.M)

def doc_document_id(uuid): return f"sb_{uuid}"
def root_chunk_id(uuid):   return f"sbroot_{uuid}"

def chunk_id(document_id, natural_key):
    return "sbc_" + hashlib.sha1(f"{document_id}:{natural_key}".encode()).hexdigest()[:16]

def _nk(title, uuid):
    return "h_" + hashlib.sha1(title.encode()).hexdigest()[:12] if title else f"doc_{uuid}"

def project_chunks(document_id, markdown):
    uuid = document_id.removeprefix("sb_")
    heads = list(HEADING_RE.finditer(markdown))
    if not heads:
        return [{"natural_key": f"doc_{uuid}", "title": "", "body": markdown, "position": 0}]
    out = []
    for i, h in enumerate(heads):
        body = markdown[h.end():heads[i + 1].start() if i + 1 < len(heads) else len(markdown)]
        title = h.group(2)
        out.append({"natural_key": _nk(title, uuid), "title": title,
                    "body": body, "position": i})
    return out
```

```python
# bridge/skybits.py
import httpx

class SkybitsError(Exception):
    def __init__(self, msg, code=None):
        super().__init__(msg); self.code = code

class Skybits:
    def __init__(self, api_key, host="https://skybits.ai"):
        self.host = host.rstrip("/")
        self.http = httpx.Client(headers={"Authorization": f"Bearer {api_key}"}, timeout=30)

    def call(self, tool, args):
        r = self.http.post(f"{self.host}/v1/tools/{tool}", json=args)
        if r.status_code != 200:
            raise SkybitsError(f"{tool}: HTTP {r.status_code}: {r.text[:200]}")
        data = r.json()
        if isinstance(data, dict) and data.get("ok") is False:
            raise SkybitsError(data.get("error", tool), data.get("code"))
        return data.get("result", data)

    def read_doc(self, ref):
        uuid = ref.rstrip("/").rsplit("/", 1)[-1]
        meta = self.call("read_document", {"doc_url": uuid, "format": "json"})
        return {"uuid": uuid, "title": meta.get("title", ""),
                "markdown": meta.get("markdown") or meta.get("text", ""),
                "revision": meta.get("revision", 0),
                "kind": meta.get("kind", "doc")}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `python -m pytest tests/test_projection.py -v` — Expected: 3 PASSED

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: skybits client + deterministic chunk projection"
```

---

### Task 3: App skeleton — auth, op router, error contract

**Files:**
- Create: `bridge/app.py`
- Test: `tests/test_app.py`

**Interfaces:**
- Consumes: nothing yet (handlers are stubs raising `SkybitsError("unknown op", code="unknown_op")` for unregistered ops)
- Produces:
  - `create_app(bridge_token: str, handlers: dict[str, callable]) -> FastAPI`
  - `handlers` maps op name → `handler(payload: dict) -> dict`; Task 4/5 register real ones
  - Wire behavior: missing/wrong bearer → 401; non-dict body → 400 `bad_request`; unknown op / `SkybitsError` → 422 `tool_refused`; unexpected exception → 500 `internal`

- [ ] **Step 1: Write the failing test**

```python
# tests/test_app.py
from fastapi.testclient import TestClient
from bridge.app import create_app
from bridge.skybits import SkybitsError

def make(**kw):
    return TestClient(create_app("tok", kw))

def test_auth_required():
    c = make()
    assert c.post("/v1/_document", json={"op": "x"}).status_code == 401
    assert c.post("/v1/_document", json={"op": "x"},
                  headers={"Authorization": "Bearer wrong"}).status_code == 401

def test_error_contract():
    c = make(ping=lambda p: {"pong": True},
             boom=lambda p: (_ for _ in ()).throw(SkybitsError("nope")))
    ok = c.post("/v1/_document", json={"op": "ping"},
                headers={"Authorization": "Bearer tok"})
    assert ok.status_code == 200 and ok.json() == {"pong": True}
    r = c.post("/v1/_document", json={"op": "boom"},
               headers={"Authorization": "Bearer tok"})
    assert r.status_code == 422 and r.json()["code"] == "tool_refused"
    r = c.post("/v1/_document", json={"op": "missing"},
               headers={"Authorization": "Bearer tok"})
    assert r.status_code == 422 and "unknown op" in r.json()["error"]
    r = c.post("/v1/_document", content="not json",
               headers={"Authorization": "Bearer tok"})
    assert r.status_code == 400 and r.json()["code"] == "bad_request"
```

- [ ] **Step 2: Run test to verify it fails**

Run: `python -m pytest tests/test_app.py -v` — Expected: FAIL (module missing)

- [ ] **Step 3: Write minimal implementation**

```python
# bridge/app.py
import json
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse
from .skybits import SkybitsError

def _err(status, code, msg):
    return JSONResponse({"code": code, "error": msg, "tool": "Document"}, status)

def create_app(bridge_token, handlers):
    app = FastAPI()

    @app.get("/healthz")
    def healthz():
        return {"ok": True}

    @app.post("/v1/_document")
    async def document(req: Request):
        if req.headers.get("authorization") != f"Bearer {bridge_token}":
            return _err(401, "unauthorized", "bad bearer")
        try:
            payload = await req.json()
            if not isinstance(payload, dict) or "op" not in payload:
                raise ValueError
        except (ValueError, json.JSONDecodeError):
            return _err(400, "bad_request", "expected one JSON object with an op field")
        handler = handlers.get(payload["op"])
        if handler is None:
            return _err(422, "tool_refused", f"unknown op {payload['op']!r}")
        try:
            return handler(payload)
        except SkybitsError as e:
            return _err(422, "tool_refused", str(e))
        except Exception as e:  # noqa: BLE001 — wire contract: never leak a stack
            return _err(500, "internal", f"{type(e).__name__}: {e}")
    return app
```

- [ ] **Step 4: Run test to verify it passes**

Run: `python -m pytest tests/test_app.py -v` — Expected: 4 PASSED

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: app skeleton with auth, op router, error contract"
```

---

### Task 4: Read ops — get_document + list_facts

**Files:**
- Create: `bridge/ops_read.py`
- Modify: `bridge/app.py` (nothing — `create_app` already takes handlers; a new `bridge/main.py` assembles them)
- Create: `bridge/main.py`
- Test: `tests/test_ops_read.py`, `tests/fakes.py`

**Interfaces:**
- Consumes: `MetaStore` (Task 1), `Skybits`/`SkybitsError`, `project_chunks`, `chunk_id`, `doc_document_id`, `root_chunk_id` (Task 2)
- Produces:
  - `build_handlers(sky: Skybits, store: MetaStore) -> dict[str, callable]` — full read-path handler map
  - Handler payloads/results EXACTLY per spec §4: `get_document`, `list_facts`, `get_chunk`, `get_edges`, `query_chunks`
  - `main.py`: `app = create_app(os.environ["BRIDGE_TOKEN"], build_handlers(Skybits(os.environ["SKYBITS_API_KEY"]), MetaStore(os.environ.get("BRIDGE_DB", "bridge.db"))))`, run with `uvicorn bridge.main:app`

- [ ] **Step 1: Write the failing test**

```python
# tests/fakes.py
class FakeSkybits:
    def __init__(self, docs): self.docs = docs  # ref -> read_doc dict
    def read_doc(self, ref):
        uuid = ref.rstrip("/").rsplit("/", 1)[-1]
        if uuid not in self.docs:
            from bridge.skybits import SkybitsError
            raise SkybitsError("remote document not found")
        return self.docs[uuid]

# tests/test_ops_read.py
from bridge.ops_read import build_handlers
from bridge.store import MetaStore
from tests.fakes import FakeSkybits

DOC = {"uuid": "u1", "title": "Atlas", "revision": 3, "kind": "doc",
       "markdown": "# Atlas\n\nintro\n\n## Goals\n- grow\n"}

def handlers(tmp_path):
    return build_handlers(FakeSkybits({"u1": DOC}), MetaStore(str(tmp_path / "m.db")))

def test_get_document_resolves_ref(tmp_path):
    h = handlers(tmp_path)
    r = h["get_document"]({"op": "get_document", "path": "u1", "scope": "user"})
    assert r["document_id"] == "sb_u1" and r["root_chunk_id"] == "sbroot_u1"

def test_list_facts_honors_limit_and_keys(tmp_path):
    h = handlers(tmp_path)
    r = h["list_facts"]({"op": "list_facts", "document_id": "sb_u1",
                         "scope": "user", "include_refuted": True, "limit": 10000})
    assert not r.get("truncated")
    keys = [f["entity"]["natural_key"] for f in r["facts"]]
    assert len(keys) == 2 and all(keys)          # both sections keyed
    ids = [f["id"] for f in r["facts"]]
    assert all(i.startswith("sbc_") for i in ids)
```

- [ ] **Step 2: Run test to verify it fails**

Run: `python -m pytest tests/test_ops_read.py -v` — Expected: FAIL (module missing)

- [ ] **Step 3: Write minimal implementation**

```python
# bridge/ops_read.py
from .projection import project_chunks, chunk_id, doc_document_id, root_chunk_id
from .skybits import SkybitsError

def _resolve(sky, document_id_or_ref):
    return sky.read_doc(document_id_or_ref.removeprefix("sb_"))

def _projected(sky, store, document_id):
    doc = _resolve(sky, document_id)
    if doc["kind"] != "doc":
        raise SkybitsError(f"unsupported document kind {doc['kind']!r}", "unsupported_kind")
    chunks = project_chunks(document_id, doc["markdown"])
    for c in chunks:
        store.ensure(document_id, c["natural_key"], title=c["title"])
    return doc, chunks

def build_handlers(sky, store):
    def get_document(p):
        try:
            doc = _resolve(sky, p["path"])
        except SkybitsError:
            return {"document_id": "", "root_chunk_id": ""}
        return {"document_id": doc_document_id(doc["uuid"]),
                "root_chunk_id": root_chunk_id(doc["uuid"]),
                "title": doc["title"], "type": "doc", "status": "active"}

    def list_facts(p):
        _, chunks = _projected(sky, store, p["document_id"])
        return {"facts": [
            {"id": chunk_id(p["document_id"], c["natural_key"]),
             "title": m["title"], "type": m["type"], "status": m["status"],
             "entity": {"natural_key": c["natural_key"], "withheld": False}}
            for c in chunks
            for m in [store.get(p["document_id"], c["natural_key"])]]}

    def get_chunk(p):
        document_id = next(d for d in [p.get("document_id")] if d) if p.get("document_id") else None
        # chunk id is self-describing: find its (document_id, natural_key) via store scan
        doc_id, nk = _lookup_chunk(store, p["id"])
        doc, chunks = _projected(sky, store, doc_id)
        c = next(c for c in chunks if c["natural_key"] == nk)
        m = store.get(doc_id, nk)
        return {"id": p["id"], "body": c["body"], "parent_id": "",
                "position": c["position"], "revision": m["revision"], "tags": m["tags"]}

    def get_edges(p):
        return {"edges": []}

    def query_chunks(p):
        doc_id = p["document_id"]
        _, chunks = _projected(sky, store, doc_id)
        ids = [chunk_id(doc_id, c["natural_key"]) for c in chunks]
        return {"chunks": [{"id": i} for i in [root_chunk_id(doc_id.removeprefix("sb_")), *ids]]}

    return {"get_document": get_document, "list_facts": list_facts,
            "get_chunk": get_chunk, "get_edges": get_edges,
            "query_chunks": query_chunks}
```

Note for the implementer: `_lookup_chunk(store, wire_id)` needs a `(document_id, natural_key)` index — add a `chunk_ids` table to `MetaStore` (`wire_id TEXT PRIMARY KEY, document_id, natural_key`) written by `ensure`, and a `lookup(wire_id) -> tuple[str, str] | None` method. Add a regression test for it in `tests/test_store.py` first (TDD), e.g. `test_lookup_roundtrip`. `get_chunk` MUST send `parent_id: ""` (empty = root per the flat-doc mapping, spec §5.5).

- [ ] **Step 4: Run test to verify it passes**

Run: `python -m pytest tests/test_ops_read.py tests/test_store.py -v` — Expected: all PASSED

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: read-path ops (get_document/list_facts/get_chunk/get_edges/query_chunks)"
```

---

### Task 5: End-to-end vs real loomcycle — diff_remote

**Files:**
- Create: `examples/loomcycle.document_sources.yaml` (bridge-repo snippet), `README.md`

**Interfaces:**
- Consumes: running bridge (`uvicorn bridge.main:app`), loomcycle v1.59 binary (`/Users/W/dev/loomcycle-skybits/bin/loomcycle`), exp11 example config.

- [ ] **Step 1: Boot the bridge against the live Skybits workspace**

```bash
cd /Users/W/dev/skybits-loomcycle-bridge
BRIDGE_TOKEN=dev-bridge-token SKYBITS_API_KEY=<connector key from exp11 .env.local> \
  uvicorn bridge.main:app --port 8901
```

- [ ] **Step 2: Declare the source + bind + diff via loomcycle**

Boot loomcycle (exp11 config + `document_sources.skybits.config.base_url: http://127.0.0.1:8901`, `api_key_env: LOOMCYCLE_SKYBITS_BRIDGE_TOKEN` with that env var exported). Run an agent (or `Context op=doc name=Document` guided run) that calls:
`Document op=set_remote path:<local doc> source:skybits remote_ref:<skybits doc_url>` then `Document op=diff_remote`.
Expected: `only_remote` = projected chunk count, everything else 0.

- [ ] **Step 3: sync pull + idempotence + drift**

`Document op=sync direction:pull` → local doc has all chunks, bodies match the Skybits Markdown. Second pull → all `same`. Edit the Skybits doc externally → `diff_remote` shows `diverged` → pull converges. Record outputs in README.

- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "docs: e2e verification vs loomcycle v1.59 (diff_remote + sync pull)"
```

---

### Task 6: Write path — upsert_chunk + update_chunk

**Files:**
- Create: `bridge/ops_write.py`
- Modify: `bridge/ops_read.py` (register write handlers in `build_handlers`)
- Test: `tests/test_ops_write.py`

**Interfaces:**
- Consumes: `Skybits.call` (Task 2), `MetaStore.write/bump_revision` (Task 1), projection (Task 2)
- Produces: handlers per spec §4 ops 6–7:
  - `upsert_chunk` → `{"id": <wire chunk id>}`; body written via Skybits `edit_document` (`insert_after` the previous section, or `replace` when the natural key already maps to a section)
  - `update_chunk` → `{}`; revision conflict → `SkybitsError("revision conflict ...")` (wire: 422)

- [ ] **Step 1: Write the failing test**

```python
# tests/test_ops_write.py
from bridge.ops_read import build_handlers
from bridge.store import MetaStore
from tests.fakes import FakeSkybits
from bridge.skybits import SkybitsError
import pytest

class WriteFake(FakeSkybits):
    def __init__(self, docs): super().__init__(docs); self.edits = []
    def call(self, tool, args):
        if tool == "edit_document": self.edits.append(args); return {"applied": True}
        raise AssertionError(tool)

def h(tmp_path):
    sky = WriteFake({"u1": {"uuid": "u1", "title": "D", "revision": 1,
                            "kind": "doc", "markdown": "# A\nx\n"}})
    return sky, build_handlers(sky, MetaStore(str(tmp_path / "m.db")))

def test_upsert_creates_section_and_returns_id(tmp_path):
    sky, handlers = h(tmp_path)
    r = handlers["upsert_chunk"]({"op": "upsert_chunk", "document_id": "sb_u1",
        "scope": "user", "natural_key": "nk_new", "title": "New", "type": "section",
        "status": "active", "body": "hello", "tags": ["t"]})
    assert r["id"].startswith("sbc_") and sky.edits, "expected one Skybits edit"

def test_update_chunk_revision_conflict_refused(tmp_path):
    sky, handlers = h(tmp_path)
    fid = handlers["list_facts"]({"op": "list_facts", "document_id": "sb_u1",
        "scope": "user, ", "include_refuted": True, "limit": 10000})["facts"][0]["id"]
    with pytest.raises(SkybitsError, match="revision conflict"):
        handlers["update_chunk"]({"op": "update_chunk", "id": fid, "revision": 999,
                                  "scope": "user", "title": "x", "type": "section",
                                  "status": "active", "tags": []})
```

- [ ] **Step 2: Run test to verify it fails** — `update_chunk`/`upsert_chunk` unknown ops

- [ ] **Step 3: Implement `bridge/ops_write.py`** — `build_write_handlers(sky, store)` returning `{"upsert_chunk": ..., "update_chunk": ...}`; `update_chunk` compares `payload["revision"]` to `store.get(...)["revision"]`, refuses on mismatch, writes body via `edit_document` (pattern = current body, replace = new body) and `store.write(..., bump_revision=True)` ONLY when body present; merge into `build_handlers`.

- [ ] **Step 4: Run tests** — `python -m pytest tests/ -v` all PASSED

- [ ] **Step 5: Commit** — `git commit -m "feat: write path (upsert_chunk/update_chunk) with revision semantics"`

---

### Task 7: move_chunk + link_chunks, push E2E

**Files:**
- Modify: `bridge/ops_write.py`, `tests/test_ops_write.py`
- Modify: `README.md`

- [ ] **Step 1: Failing tests** — `move_chunk` idempotent no-op when already in place (no Skybits edit emitted), emits `edit_document` `move` when position differs; `link_chunks` records edge in a new `edges` table and `get_edges` returns it with `auto:false`; second identical `link_chunks` does not duplicate.

- [ ] **Step 2: Implement** — add `edges` table to `MetaStore` (`document_id, from_nk, to_nk, kind`, PK all four); wire into `build_handlers`; extend `get_edges` to read it.

- [ ] **Step 3: Push E2E vs real loomcycle** — bind a loomcycle-native doc, `sync direction:push`, verify Skybits doc gains the sections, `document_history` attributes them to the connector, stale-revision `update_chunk` comes back 422. Record in README.

- [ ] **Step 4: Commit** — `git commit -m "feat: move_chunk/link_chunks + push e2e verified"`

---

### Task 8: Scheduled reconciliation + packaging

**Files:**
- Create: `examples/exp-skybits-federation/README.md` (loomcycle schedule yaml that runs `Document op=sync` on a cadence), `Dockerfile`

- [ ] **Step 1:** Add a loomcycle schedule example (periodic agent run calling `Document op=sync direction:pull` on bound docs) — Skybits has no webhooks, scheduled pull is the sync model.
- [ ] **Step 2:** `Dockerfile` (python:3.12-slim, `pip install .`, `uvicorn bridge.main:app`).
- [ ] **Step 3:** Final README: architecture diagram, config snippets, op coverage table, MVP-vs-phase-2 scope, link to `docs/SKYBITS-FEDERATION.md`.
- [ ] **Step 4: Commit** — `git commit -m "docs: scheduled sync example + packaging"`

---

## Self-Review

- **Spec coverage:** §4 ops 1–5 → Tasks 4–5; ops 6–9 → Tasks 6–7; §5.2 metadata → Task 1; §5.1 projection → Task 2; §8 error contract → Task 3; §6 auth → Task 3; §9 verification → Tasks 5, 7; phase 3 scheduling → Task 8. Covered.
- **Known soft spot (accepted):** `get_chunk` needs the wire-id index — called out inline in Task 4 with its own TDD step rather than hidden.
- **Type consistency:** `MetaStore.{get,ensure,write,all,lookup}`, `build_handlers(sky, store)`, `project_chunks` field names (`natural_key/title/body/position`), `read_doc` field names (`uuid/title/markdown/revision/kind`) — checked consistent across tasks.
