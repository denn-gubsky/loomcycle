"""Create, promote, retire or show the tenant helper agents of Study C (agents/*.json).

    python3 -I bench/docs/kbrag/register_agents.py create            # create, or fork + promote if the name exists
    python3 -I bench/docs/kbrag/register_agents.py show
    python3 -I bench/docs/kbrag/register_agents.py promote --def-id <id>
    python3 -I bench/docs/kbrag/register_agents.py retire  --def-id <id>

Every call is POST /v1/_agentdef with the tenant token (lc.agentdef). The orchestrator runs
this; nothing in the harness runs it implicitly.
"""
import argparse, glob, json, os, sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)


def specs():
    out = []
    for p in sorted(glob.glob(os.path.join(HERE, "agents", "*.json"))):
        with open(p) as f:
            out.append(json.load(f))
    return out


def live_defs(lc, name):
    r = lc.agentdef("list", name=name)
    rows = r.get("defs") or r.get("agent_defs") or r.get("items") or []
    return [d for d in rows if not d.get("retired") and not d.get("shared")]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["create", "show", "promote", "retire"])
    ap.add_argument("--def-id")
    a = ap.parse_args()
    import lc
    if a.cmd in ("promote", "retire"):
        if not a.def_id:
            raise SystemExit("--def-id is required")
        body = {"def_id": a.def_id}
        if a.cmd == "retire":
            body["retired"] = True
        print(json.dumps(lc.agentdef(a.cmd, **body), indent=2))
        return
    for s in specs():
        if a.cmd == "show":
            print(json.dumps(lc.agentdef("list", name=s["name"]), indent=2))
            continue
        if live_defs(lc, s["name"]):
            r = lc.agentdef("fork", name=s["name"], overlay=s["overlay"], description=s["description"], promote=True)
        else:
            r = lc.agentdef("create", name=s["name"], overlay=s["overlay"], description=s["description"], promote=True)
        print(s["name"], json.dumps(r, indent=2))


if __name__ == "__main__":
    main()
