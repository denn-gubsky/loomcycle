"""Create, show or retire Study D's rag-lc helper agent (agents/search-rr.json) in the tenant.

    python3 -I bench/docs/kbrag/docs/register.py create    # create (or fork + promote) kbrag/search-rr
    python3 -I bench/docs/kbrag/docs/register.py show
    python3 -I bench/docs/kbrag/docs/register.py retire    # retire every live version this harness made

The search excludes the documents outside the corpus (operational types, dconfig) by id, so
rag-lc searches exactly what rag-ext indexes (PREREG check 1). Every create/retire is logged
in <work>/helper.json with its UTC time.
"""
import argparse, datetime, json, os, sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import dlc, dconfig  # noqa: E402

STATE = os.path.join(dconfig.WORK, "helper.json")


def now():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def spec():
    s = json.load(open(os.path.join(HERE, "agents", "search-rr.json"), encoding="utf-8"))
    inv = json.load(open(dconfig.INVENTORY, encoding="utf-8"))
    corpus = set(inv["corpus"])
    exclude = sorted(d["id"] for d in inv["documents"] if d["id"] not in corpus)
    s["overlay"]["code_body"] = s["overlay"]["code_body"].replace("EXCLUDE_IDS", json.dumps(exclude))
    return s, exclude


def live(name):
    # AgentDef list answers {"name", "versions": [...]}; reading other keys (as ../register_agents.py
    # does) saw no version at all, so `retire` retired nothing on 2026-11-23 (Amendment 3).
    r = dlc.agentdef("list", name=name)
    rows = r.get("versions") or r.get("defs") or r.get("agent_defs") or r.get("items") or []
    return [d for d in rows if not d.get("retired") and not d.get("shared")]


def active(name):
    e = [x for x in dlc.get("/v1/_library/agents")["entries"] if x["name"] == name]
    return e[0].get("active_def_id") if e else None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["create", "show", "retire"])
    a = ap.parse_args()
    dlc.assert_tenant()
    st = json.load(open(STATE)) if os.path.exists(STATE) else {"log": []}
    s, exclude = spec()
    if a.cmd == "show":
        print(json.dumps(dlc.agentdef("list", name=s["name"]), indent=1)[:3000])
        return
    if a.cmd == "create":
        op = "fork" if live(s["name"]) else "create"
        r = dlc.agentdef(op, name=s["name"], overlay=s["overlay"], description=s["description"], promote=True)
        st["log"].append({"at": now(), "op": op, "def_id": r.get("def_id"), "excluded": len(exclude)})
        print(op, r.get("def_id"), "excluding", len(exclude), "documents")
    else:
        mine = {e["def_id"] for e in st["log"] if e["op"] in ("create", "fork")}
        for d in live(s["name"]):
            if d.get("def_id") in mine:
                dlc.agentdef("retire", def_id=d["def_id"], retired=True)
                st["log"].append({"at": now(), "op": "retire", "def_id": d["def_id"]})
                print("retired", d["def_id"])
        left = [d.get("def_id") for d in live(s["name"])]
        print("live versions now:", left, "active:", active(s["name"]))
        if left or active(s["name"]):
            with open(STATE, "w") as f:
                json.dump(st, f, indent=1)
            sys.exit(1)
    with open(STATE, "w") as f:
        json.dump(st, f, indent=1)


if __name__ == "__main__":
    main()
