"""Study D preflight: the tenant routes every model-backed kb agent to Haiku and the rag-lc
helper is live, or the runner must not start (Amendment 3).

    python3 -I bench/docs/kbrag/docs/preflight.py      # exit 0 ok, 4 with the problems printed

Checked at the start of every darms.py invocation, every 20 runs inside it, and by the
driver before each set. Read-only (Library listing, AgentDef get).
"""
import json, os, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dconfig  # noqa: E402

HAIKU = ("anthropic", "claude-haiku-5-5")
KB_AGENTS = ("kb/classify", "kb/search", "kb/screen", "kb/walker")


def _body(row):
    for k in ("definition", "def", "overlay", "body"):
        if isinstance(row.get(k), dict):
            return row[k]
    return row


def problems(library, get_def, aliases):
    """library: {name: Library entry}; get_def(def_id) -> AgentDef row; aliases: {alias: {provider, model}}.
    Returns the reasons the tenant is not ready (empty when it is)."""
    out = []
    for n in KB_AGENTS:
        act = (library.get(n) or {}).get("active_def_id")
        if not act:
            out.append("%s: no active tenant def (the static tier agent is in force)" % n)
            continue
        row = get_def(act)
        b = _body(row)
        if row.get("retired"):
            out.append("%s: active def %s is retired" % (n, act))
        prov, model = b.get("provider"), b.get("model")
        if not prov and model in aliases:
            prov, model = aliases[model].get("provider"), aliases[model].get("model")
        if (prov, model) != HAIKU:
            out.append("%s: active def %s resolves to %s/%s, not Haiku" % (n, act, prov, model))
    h = library.get(dconfig.RAGLC_AGENT) or {}
    if not h.get("active_def_id"):
        out.append("%s: not active" % dconfig.RAGLC_AGENT)
    elif get_def(h["active_def_id"]).get("retired"):
        out.append("%s: active def is retired" % dconfig.RAGLC_AGENT)
    return out


def check(lc):
    lib = {e["name"]: e for e in lc.get("/v1/_library/agents")["entries"]}
    aliases = lc.get("/v1/_models").get("aliases") or {}
    return problems(lib, lambda d: lc.agentdef("get", def_id=d), aliases)


def main():
    import dlc
    dlc.assert_tenant()
    p = check(dlc)
    print(json.dumps({"ok": not p, "problems": p}, indent=1))
    sys.exit(0 if not p else 4)


if __name__ == "__main__":
    main()
