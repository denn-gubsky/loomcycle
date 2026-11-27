"""Pin the kb bundle's model-serving agents for this study, in the tenant only.

    python3 -I bench/docs/kbrag/setup.py                  # --target haiku (default): fork + promote
    python3 -I bench/docs/kbrag/setup.py --target local   # retire the forks: static agents run on their tiers
    python3 -I bench/docs/kbrag/setup.py --check          # verify what is active now

A promoted tenant fork shadows the static bundle agent for every run in the tenant
(including children of the code-js parents kb/ask and kb/annotator, which a per-run
provider/model pin does not reach). The fork changes routing only: provider+model,
which clears the static `tier`. Every other field is diffed against the static
definition and the script refuses to go on if anything else differs.
Runs are always started with user_tier "free", so an unpinned (tier-routed) agent
can only reach ollama-local and never the paid fallback cascade.
"""
import argparse, json, os, sys, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import lc

AGENTS = ["kb/classify", "kb/search", "kb/screen", "kb/walker", "kb/annotate"]
PIN = {"provider": "anthropic", "model": "claude-haiku-5-5"}
WORK = "/home/denn/workspace/kbrag-work/cqa"
APPLIED = os.path.join(WORK, "agents_applied.json")
# Fields the runtime stamps on a stored def that carry no behaviour.
META = {"def_id", "version", "parent_def_id", "created_at", "created_by", "created_by_agent_id",
        "created_by_run_id", "bootstrapped_from_static", "content_sha256", "name", "tenant_id",
        "retired", "active", "description", "shared", "created_by_subject"}


def statics():
    return {e["name"]: e["static_definition"] for e in lc.get("/v1/_library/agents")["entries"]
            if e["name"] in AGENTS}


def def_body(row):
    """The definition fields of an AgentDef get/list row, whatever the envelope."""
    for k in ("definition", "def", "overlay", "body"):
        if isinstance(row.get(k), dict):
            return row[k]
    return row


def diff(static, fork):
    body = {k: v for k, v in def_body(fork).items() if k not in META}
    # The store keeps the un-templated prompt as system_prompt_base; when it is
    # the same text as system_prompt it is bookkeeping, not a change.
    if body.get("system_prompt_base") == body.get("system_prompt") == static.get("system_prompt"):
        body.pop("system_prompt_base")
    out = {}
    for k in sorted(set(static) | set(body)):
        a, b = static.get(k), body.get(k)
        if a in ({}, [], None, "", 0, False) and b in ({}, [], None, "", 0, False):
            continue
        if a != b:
            out[k] = {"static": a if k != "system_prompt" else "<%d chars>" % len(a or ""),
                      "fork": b if k != "system_prompt" else "<%d chars>" % len(b or "")}
    return out


def expected(static, target):
    if target == "local":
        return {}
    want = {"provider": {"static": None, "fork": PIN["provider"]},
            "model": {"static": None, "fork": PIN["model"]}}
    if static.get("tier"):
        want["tier"] = {"static": static["tier"], "fork": None}
    return want


def active(name):
    """The tenant's active def for a name, or None when the static agent is in force."""
    e = library().get(name) or {}
    if not e.get("active_def_id"):
        return None
    return lc.agentdef("get", def_id=e["active_def_id"])


def library():
    return {e["name"]: e for e in lc.get("/v1/_library/agents")["entries"] if e["name"] in AGENTS}


def apply_haiku(st, applied):
    for name in AGENTS:
        cur = active(name)
        if cur and def_body(cur).get("provider") == PIN["provider"] and def_body(cur).get("model") == PIN["model"]:
            print(name, "already pinned:", cur.get("def_id")); applied[name] = cur.get("def_id"); continue
        prev = applied.get(name)
        if prev:  # re-activate the fork this harness made earlier
            lc.agentdef("retire", def_id=prev, retired=False)
            r = lc.agentdef("promote", def_id=prev)
        else:
            r = lc.agentdef("fork", name=name, overlay=PIN, promote=True,
                            description="kbrag benchmark (Study C): pin to anthropic/claude-haiku-5-5; routing only")
        did = r.get("def_id") or prev
        print(name, "->", did, json.dumps({k: r[k] for k in ("version", "active") if k in r}))
        applied[name] = did


def apply_local(applied):
    # Retiring the active def clears the tenant's active pointer, so the static
    # agent (tier-routed) is back in force; repeat in case an older version
    # (the bootstrap v1) was left active underneath.
    for name in AGENTS:
        for _ in range(5):
            did = (library().get(name) or {}).get("active_def_id")
            if not did:
                break
            lc.agentdef("retire", def_id=did, retired=True)
            print(name, "retired", did)


def check(st, target):
    ok = True
    for name in AGENTS:
        cur = active(name)
        if target == "local":
            good = cur is None
            print(name, "static (tier %s)" % st[name].get("tier") if good else "ACTIVE FORK %s" % cur.get("def_id"))
            ok &= good; continue
        if cur is None:
            print(name, "NO ACTIVE FORK (static tier agent in force)"); ok = False; continue
        d = diff(st[name], cur)
        good = d == expected(st[name], target)
        print(name, cur.get("def_id"), "OK" if good else "UNEXPECTED DIFF")
        print("  diff vs static:", json.dumps(d))
        ok &= good
    return ok


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--target", choices=["haiku", "local"], default="haiku")
    ap.add_argument("--retire", action="store_true", help="same as --target local")
    ap.add_argument("--check", action="store_true")
    a = ap.parse_args()
    target = "local" if a.retire else a.target
    st = statics()
    missing = [n for n in AGENTS if n not in st]
    if missing:
        raise SystemExit("not static agents on this deployment: %s" % missing)
    if a.check:
        sys.exit(0 if check(st, target) else 1)
    os.makedirs(WORK, exist_ok=True)
    applied = json.load(open(APPLIED)).get("defs", {}) if os.path.exists(APPLIED) else {}
    try:
        if target == "haiku":
            apply_haiku(st, applied)
        else:
            apply_local(applied)
    except lc.HTTPFail as e:
        raise SystemExit("AgentDef API refused: %s" % e)
    json.dump({"target": target, "pin": PIN if target == "haiku" else None, "defs": applied,
               "at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "server": lc.version()},
              open(APPLIED, "w"), indent=1)
    sys.exit(0 if check(st, target) else 1)


if __name__ == "__main__":
    main()
