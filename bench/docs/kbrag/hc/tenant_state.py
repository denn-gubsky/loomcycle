"""The tenant state the study runs under: which def serves each watched agent, its
content_sha256, and that every model-backed one resolves to Haiku (plan §3.1, §3.2).

    python3 -I bench/docs/kbrag/hc/tenant_state.py --set cqa --record   # -> <work>/<set>/tenant_state.json
    python3 -I bench/docs/kbrag/hc/tenant_state.py --set cqa            # exit 0 iff unchanged and pinned

Amendment 0 copies the recorded content_sha256 of every fork. The runner checks the state
before each invocation and every 20 runs and stops when a def changed, was retired, or a
pinned agent no longer resolves to Haiku. Read-only (Library listing, AgentDef get, models).
"""
import argparse, hashlib, json, os, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import client, hcconfig  # noqa: E402


def _body(row):
    for k in ("definition", "def", "overlay", "body"):
        if isinstance(row.get(k), dict):
            return row[k]
    return row


def snapshot(library, get_def, aliases, names=hcconfig.WATCH):
    """{name: {def_id, content_sha256, retired, provider, model}} for every watched name;
    a name with no active tenant def is recorded as {"def_id": None} (the bundle agent serves)."""
    out = {}
    for n in names:
        act = (library.get(n) or {}).get("active_def_id")
        if not act:
            out[n] = {"def_id": None}
            continue
        row = get_def(act)
        b = _body(row)
        prov, model = b.get("provider"), b.get("model")
        if not prov and model in aliases:
            prov, model = aliases[model].get("provider"), aliases[model].get("model")
        out[n] = {"def_id": act, "content_sha256": row.get("content_sha256"), "retired": bool(row.get("retired")),
                  "provider": prov, "model": model}
    return out


def digest(state):
    """One short id of a state: every row records it, so analysis can see one state held."""
    return hashlib.sha256(json.dumps(state, sort_keys=True).encode()).hexdigest()[:16]


def problems(recorded, current, pinned=hcconfig.HAIKU_PINNED):
    """Why the current state is not the recorded one or not pinned (empty when it is fine)."""
    out = []
    for n in sorted(set(recorded) | set(current)):
        a, b = recorded.get(n), current.get(n)
        if a != b:
            out.append("%s: was %s, now %s" % (n, _short(a), _short(b)))
    for n in pinned:
        c = current.get(n) or {}
        if not c.get("def_id"):
            out.append("%s: no active tenant def, so it runs on its bundle tier, not Haiku" % n)
        elif c.get("retired"):
            out.append("%s: active def is retired" % n)
        elif (c.get("provider"), c.get("model")) != hcconfig.HAIKU:
            out.append("%s: resolves to %s/%s, not Haiku" % (n, c.get("provider"), c.get("model")))
    return out


def _short(e):
    if not e:
        return "absent"
    return "%s (%s)" % (e.get("def_id"), (e.get("content_sha256") or "-")[:12])


def current(lc):
    aliases = lc.get("/v1/_models").get("aliases") or {}
    return snapshot(lc.library_agents(), lambda d: lc.agentdef("get", def_id=d), aliases)


def path(set_name):
    return os.path.join(hcconfig.work(), set_name, "tenant_state.json")


def load(set_name):
    with open(path(set_name), encoding="utf-8") as f:
        return json.load(f)["agents"]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--set", required=True, choices=hcconfig.SETS)
    ap.add_argument("--record", action="store_true")
    a = ap.parse_args()
    lc = client.for_set(a.set)
    lc.assert_tenant()
    now = current(lc)
    if a.record:
        os.makedirs(os.path.dirname(path(a.set)), exist_ok=True)
        with open(path(a.set), "w", encoding="utf-8") as f:
            json.dump({"agents": now, "digest": digest(now), "server": lc.version()}, f, indent=1)
        p = problems(now, now)
        print(json.dumps({"recorded": len(now), "digest": digest(now), "problems": p}, indent=1))
        sys.exit(0 if not p else 4)
    p = problems(load(a.set), now)
    print(json.dumps({"ok": not p, "problems": p}, indent=1))
    sys.exit(0 if not p else 4)


if __name__ == "__main__":
    main()
