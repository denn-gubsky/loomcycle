"""Study D: route the kb bundle's model-serving agents to Claude Haiku, in this tenant only.

    python3 -I bench/docs/kbrag/docs/forks.py --record    # write the tenant's current state (no change)
    python3 -I bench/docs/kbrag/docs/forks.py --apply     # fork + promote, ONLY names with no tenant overlay
    python3 -I bench/docs/kbrag/docs/forks.py --check     # what serves each name now, diff vs static
    python3 -I bench/docs/kbrag/docs/forks.py --restore   # retire every def --apply made; statics back

A promoted tenant fork shadows the bundle agent for EVERY run in the tenant (other people's
and scheduled ones too) while it is active, so the window is kept short and --restore is run
right after the last scored run. A name that already has an active tenant overlay is never
touched: it is recorded and run as it is. kb/annotate is not in the list (this study does not
annotate). Every create/retire is logged with its UTC time in <work>/forks.json.
"""
import argparse, datetime, json, os, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dlc, dconfig  # noqa: E402

NAMES = ["kb/classify", "kb/search", "kb/screen", "kb/walker"]
PIN = {"provider": "anthropic", "model": "claude-haiku-5-5"}
STATE = os.path.join(dconfig.WORK, "forks.json")
META = {"def_id", "version", "parent_def_id", "created_at", "created_by", "created_by_agent_id",
        "created_by_run_id", "bootstrapped_from_static", "content_sha256", "name", "tenant_id",
        "retired", "active", "description", "shared", "created_by_subject"}


def now():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def library():
    return {e["name"]: e for e in dlc.get("/v1/_library/agents")["entries"] if e["name"] in NAMES}


def body(row):
    for k in ("definition", "def", "overlay", "body"):
        if isinstance(row.get(k), dict):
            return row[k]
    return row


def diff(static, row):
    """Fields where an active def differs from the static bundle definition (prompts by length)."""
    b = {k: v for k, v in body(row).items() if k not in META}
    if b.get("system_prompt_base") == b.get("system_prompt"):
        b.pop("system_prompt_base", None)
    out = {}
    for k in sorted(set(static) | set(b)):
        x, y = static.get(k), b.get(k)
        if x in ({}, [], None, "", 0, False) and y in ({}, [], None, "", 0, False):
            continue
        if x != y:
            if k in ("system_prompt", "system_prompt_base"):
                out[k] = {"static": "<%d chars>" % len(x or ""), "tenant": "<%d chars>" % len(y or "")}
            else:
                out[k] = {"static": x, "tenant": y}
    return out


def load():
    return json.load(open(STATE, encoding="utf-8")) if os.path.exists(STATE) else {"log": [], "created": {}}


def save(st):
    with open(STATE + ".tmp", "w", encoding="utf-8") as f:
        json.dump(st, f, indent=1)
    os.replace(STATE + ".tmp", STATE)


def snapshot():
    lib = library()
    out = {}
    for n in NAMES:
        e = lib.get(n) or {}
        static = e.get("static_definition") or {}
        act = e.get("active_def_id")
        row = dlc.agentdef("get", def_id=act) if act else None
        out[n] = {"active_def_id": act, "static_tier": static.get("tier"),
                  "active": None if not row else {"version": row.get("version"), "description": row.get("description"),
                                                  "created_at": row.get("created_at"),
                                                  "provider": body(row).get("provider"), "model": body(row).get("model"),
                                                  "tier": body(row).get("tier"), "diff_vs_static": diff(static, row)}}
    return out


def main():
    ap = argparse.ArgumentParser()
    g = ap.add_mutually_exclusive_group(required=True)
    for x in ("record", "apply", "check", "restore"):
        g.add_argument("--" + x, action="store_true")
    a = ap.parse_args()
    dlc.assert_tenant()
    st = load()
    if a.record or a.check:
        snap = snapshot()
        if a.record:
            st.setdefault("before", {"at": now(), "state": snap})
            save(st)
        print(json.dumps(snap, indent=1))
        return
    if a.apply:
        before = st.get("before")
        if not before:
            raise SystemExit("run --record first")
        for n in NAMES:
            if before["state"][n]["active_def_id"] and n not in st["created"]:
                print(n, "has a tenant overlay already (%s): left as it is" % before["state"][n]["active_def_id"])
                continue
            if n in st["created"] and not st["created"][n].get("retired_at"):
                print(n, "already forked by this harness:", st["created"][n]["def_id"])
                continue
            r = dlc.agentdef("fork", name=n, overlay=PIN, promote=True,
                             description="kbrag benchmark Study D: pin to anthropic/claude-haiku-5-5; routing only; retire after the runs")
            did = r.get("def_id")
            st["created"][n] = {"def_id": did, "created_at": now(), "parent_def_id": r.get("parent_def_id")}
            st["log"].append({"at": now(), "op": "fork+promote", "name": n, "def_id": did})
            save(st)
            print(n, "->", did)
        print(json.dumps(snapshot(), indent=1))
        return
    # --restore: retire what this harness made, then any def that became active through it
    # (a fork of a name with no active pointer bootstraps a static snapshot first).
    for n, c in st["created"].items():
        for _ in range(5):
            act = (library().get(n) or {}).get("active_def_id")
            if not act:
                break
            if act != c["def_id"] and act != c.get("parent_def_id"):
                print(n, "active def %s is not this harness's: left alone" % act)
                break
            dlc.agentdef("retire", def_id=act, retired=True)
            st["log"].append({"at": now(), "op": "retire", "name": n, "def_id": act})
            save(st)
            print(n, "retired", act)
        c["retired_at"] = now()
        save(st)
    snap = snapshot()
    st["after_restore"] = {"at": now(), "state": snap}
    save(st)
    ok = all(snap[n]["active_def_id"] == st["before"]["state"][n]["active_def_id"] for n in NAMES)
    print(json.dumps(snap, indent=1))
    print("RESTORED: every name resolves as before" if ok else "NOT RESTORED: compare with forks.json 'before'")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
