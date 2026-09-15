#!/usr/bin/env python3
"""RFC DB-3 — the retrieval layer the arms differ by.

Every arm hands the answerer a FACTS block and nothing else. Retrieval is done
HERE rather than by giving the answerer tools, for two reasons RFC DB already
paid for: section 6 requires an equal CONTENT budget, which is only enforceable
if the harness decides what goes in the prompt; and "the agent declined to call
the tool" is a confound that has nothing to do with whether traversal helps.

So the arms differ only in WHICH facts fill the same budget:

  oracle     the question's true supports
  nomem      nothing
  single     vector search on the question text, in rank order
  traversal  the same search results as SEEDS, expanded fact -> entity -> fact
  shuffled   the same expansion over a seeded random rewiring of the edges

The last is the control that separates "traversal helps" from "traversal
retrieves more": same facts, same budget, same depth, same code — only the edge
targets are permuted. A traversal arm that still wins on a rewired graph is
winning on volume or lexical luck, not on the relations.
"""
import json, os, random, subprocess, urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
SQLMEM_DB = os.environ.get("DB3_SQLMEM_DB", "loomcycle_db2_sqlmem")


def _psql(db, sql):
    out = subprocess.run(["psql", "-d", db, "-tAF\x1f", "-c", sql],
                         capture_output=True, text=True, check=True).stdout
    return [line.split("\x1f") for line in out.splitlines() if line.strip()]


def busiest_scope_schema(db=SQLMEM_DB):
    """The scope schema holding the corpus. Chosen by ROW count: every scope has
    the same tables, so picking by table count picks an empty one."""
    best, bestn = None, -1
    for (ns,) in _psql(db, "select nspname from pg_namespace where nspname like 'sqlmem_s_%%'"):
        try:
            n = int(_psql(db, "select count(*) from %s.chunks" % ns)[0][0])
        except Exception:
            continue
        if n > bestn:
            best, bestn = ns, n
    if best is None:
        raise SystemExit("no sqlmem scope schema found in %s" % db)
    return best


class Graph:
    """Fact <-> entity adjacency, read from the store.

    Edges are STRUCTURE, not ranked retrieval, so they are read directly rather
    than through the runtime — which also lets the shuffled arm run the identical
    traversal code over a permuted edge set.
    """

    def __init__(self, schema, db=SQLMEM_DB):
        self.bodies, self.titles, self.kind = {}, {}, {}
        for cid, typ, title in _psql(db,
                "select id, coalesce(type,''), coalesce(title,'') from %s.chunks" % schema):
            self.kind[cid] = typ
            self.titles[cid] = title
            if typ == "fact":
                self.bodies[cid] = title
        self.edges = [(a, b) for a, b in _psql(db,
                      "select from_id, to_id from %s.chunk_edges where kind='about'" % schema)]
        self._index(self.edges)

    def _index(self, edges):
        self.fact_to_entity, self.entity_to_fact = {}, {}
        for a, b in edges:
            self.fact_to_entity.setdefault(a, []).append(b)
            self.entity_to_fact.setdefault(b, []).append(a)

    def shuffled(self, seed):
        """A rewiring that preserves edge COUNT and per-fact out-degree, and
        changes only which entity each edge points at."""
        rng = random.Random(seed)
        targets = [b for _, b in self.edges]
        rng.shuffle(targets)
        g = object.__new__(Graph)
        g.bodies, g.titles, g.kind = self.bodies, self.titles, self.kind
        g.edges = [(a, t) for (a, _), t in zip(self.edges, targets)]
        g._index(g.edges)
        return g

    def expand(self, seed_facts, depth):
        """Facts reachable from the seeds, fact -> entity -> fact, in BFS order.

        The seeds come first so both retrieval arms spend the start of the budget
        on the same content and differ only in what they spend the REST on.
        """
        order, seen = list(seed_facts), set(seed_facts)
        frontier = list(seed_facts)
        for _ in range(depth):
            nxt = []
            for f in frontier:
                for ent in self.fact_to_entity.get(f, []):
                    for g in self.entity_to_fact.get(ent, []):
                        if g not in seen:
                            seen.add(g)
                            nxt.append(g)
                            order.append(g)
            if not nxt:
                break
            frontier = nxt
        return order


def search(base, token, query, top_k=50, scope_id="bench"):
    """Vector search through the RUNTIME — the real embedder and the real ranker.

    ⚠️ The field is `top_k`. `limit` is accepted by the JSON decoder, ignored, and
    the search silently runs at its default of 10 — which starved the single-hop
    arm to 7 facts and would have let traversal win on volume alone.

    `sources=["facts"]` for the same reason: unnarrowed, the result slots fill
    with bodiless entity nodes that spend a slot and say nothing, so the arm that
    is supposed to represent today's retrieval gets a fraction of its budget.
    """
    body = json.dumps({"scope": "user", "scope_id": scope_id,
                       "query": query, "top_k": top_k, "sources": ["facts"]}).encode()
    req = urllib.request.Request(base + "/v1/_memory/search", data=body, headers={
        "Authorization": "Bearer " + token, "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=120) as r:
        d = json.load(r)
    out = []
    for e in d.get("entries", []):
        if e.get("kind") != "fact":
            continue
        key = e.get("key", "")
        cid = e.get("chunk_id") or (key.split("doc.chunk:", 1)[1] if "doc.chunk:" in key else "")
        if cid:
            out.append(cid)
    return out


def fill(graph, ids, budget_chars):
    """Take facts in order until the content budget is spent.

    The budget is on CONTENT handed to the answerer, never on rows retrieved:
    traversal fetches more rows than single-hop by construction, and an arm that
    wins on volume is the trap this control exists to avoid.
    """
    picked, used = [], 0
    for cid in ids:
        b = (graph.bodies.get(cid) or "").strip()
        if not b:
            continue
        if used + len(b) + 3 > budget_chars:
            continue
        picked.append(b)
        used += len(b) + 3
    return picked, used
