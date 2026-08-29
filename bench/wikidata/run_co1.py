#!/usr/bin/env python3
"""RFC CO-1 — the gate. Builds nothing; loads rows and asks questions.

Three arms over the same questions:

  control   empty scope, no retrieval  -> what the model already knows (parametric floor)
  static    whole chain loaded, dated  -> does the fact's own text suffice
  sequential  old fact stored, asked, new fact stored, asked -> STALENESS

The sequential arm is the benchmark. The other two exist so its number can be read:
without the control, a score measures training data; without static, a staleness
result cannot be told apart from "the store never had it".
"""
import json, os, re, sys, time, urllib.request

BASE = "http://truenas.local:8787"
TOK  = os.environ["WIKI_BENCH_TENANT_TOKEN"]
AGENT = "wiki/answerer"

def api(method, path, body=None, timeout=300):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method,
        headers={"Authorization": "Bearer " + TOK, "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        raw = r.read()
    return raw

def put_row(scope_id, key, value, observed_at=None):
    b = {"value": value, "embed": True}
    if observed_at:
        b["observed_at"] = observed_at
    out = json.loads(api("PUT", f"/v1/_memory/scopes/user/{scope_id}/keys/{key}", b))
    if not out.get("embedded"):
        # An unembedded row is invisible to recall, so a run that ignored this would
        # score an empty store and call it a memory failure.
        raise SystemExit(f"row {key} did not embed: {out.get('embed_warning')}")

def purge(scope_id, keys):
    for k in keys:
        try:
            api("DELETE", f"/v1/_memory/scopes/user/{scope_id}/keys/{k}")
        except Exception:
            pass

def scope_rows(scope_id):
    out = json.loads(api("GET", f"/v1/_memory/scopes/user/{scope_id}/keys?limit=5000"))
    return [e["key"] for e in (out.get("entries") or [])]

TRIPLE = re.compile(r"MEMORY:\s*(.*?)\s*\n\s*OWN:\s*(.*?)\s*\n", re.I | re.S)

def ask(scope_id, question):
    """Returns (memory_answer, own_answer, retrieved_bool, raw)."""
    body = {"agent": AGENT, "prompt": question, "user_id": scope_id, "max_iterations": 4}
    raw = api("POST", "/v1/runs", body).decode("utf-8", "replace")
    text, retrieved = "", False
    for line in raw.splitlines():
        if not line.startswith("data: "):
            continue
        try:
            ev = json.loads(line[6:])
        except Exception:
            continue
        if ev.get("type") == "text":
            text += ev.get("text", "")
        # Verify the retrieval ACTUALLY happened rather than trusting the instruction —
        # an RFC CL answerer ignored exactly this kind of rule on every question.
        tu = ev.get("tool_use") or {}
        if tu.get("name") == "Memory" and (tu.get("input") or {}).get("op") in ("recall", "search"):
            retrieved = True
    m = TRIPLE.search(text + "\n")
    if not m:
        return None, None, retrieved, text.strip()[:160]
    return m.group(1).strip(), m.group(2).strip(), retrieved, text.strip()[:160]

def norm(s):
    return re.sub(r"[^a-z ]", "", (s or "").lower()).strip()

def match(answer, label):
    """Grade on surname containment: the fixture's ground truth is a QID and the label
    is its English name, so a reply of "Merz" or "Friedrich Merz" is the same answer."""
    a, l = norm(answer), norm(label)
    if not a or not l:
        return False
    return l in a or a in l or (l.split()[-1] and l.split()[-1] in a)

def main():
    chains = json.load(open(sys.argv[1]))["chains"]
    limit = int(sys.argv[2]) if len(sys.argv) > 2 else 20
    chains = chains[:limit]
    # THE SCOPE MUST BE THE TOKEN'S SUBJECT. In-band `scope=user` is resolved
    # server-side from the run identity, never from the wire, so a row written to any
    # other scope_id is invisible to the agent — it recalls an empty partition and
    # every arm reports NOT_FOUND, which reads as a memory failure and is not one.
    scope = json.loads(api("GET", "/v1/_me"))["subject"]
    print(f"scope (token subject): {scope}", flush=True)
    results = []

    print(f"CO-1 over {len(chains)} chains\n", flush=True)
    for c in chains:
        office, prev, cur = c["office_label"], c["holders"][0], c["holders"][1]
        q = f"Who is the {office}?"
        keys = [f"co1:{c['office']}:prev", f"co1:{c['office']}:cur"]
        purge(scope, keys)

        # ---- control: empty scope. No rows, so recall can only return nothing.
        left = scope_rows(scope)
        if left:
            raise SystemExit(f"scope not empty before control: {len(left)} rows — "
                             "a contaminated control invalidates every arm")
        c_mem, c_own, c_ret, _ = ask(scope, q)

        # ---- sequential: the old fact arrives first, unqualified.
        put_row(scope, keys[0], f"{prev['label']} is the {office}.", prev["start"] + "T00:00:00Z")
        s1_mem, s1_own, s1_ret, _ = ask(scope, q)
        # ...then the new one arrives, exactly as a knowledge update does.
        put_row(scope, keys[1], f"{cur['label']} is the {office}.", cur["start"] + "T00:00:00Z")
        s2_mem, s2_own, s2_ret, s2_raw = ask(scope, q)

        # ---- static: both facts, each carrying its own dates in the text.
        purge(scope, keys)
        put_row(scope, keys[0],
                f"{prev['label']} was the {office} from {prev['start']} to {prev['end']}.",
                prev["start"] + "T00:00:00Z")
        put_row(scope, keys[1],
                f"{cur['label']} has been the {office} since {cur['start']}.",
                cur["start"] + "T00:00:00Z")
        st_mem, st_own, st_ret, _ = ask(scope, q)
        purge(scope, keys)

        row = {
            "office": office, "office_qid": c["office"],
            "prev": prev["label"], "prev_qid": prev["qid"],
            "cur": cur["label"], "cur_qid": cur["qid"],
            "control_own": c_own, "control_mem": c_mem, "control_retrieved": c_ret,
            "seq_before": s1_mem, "seq_after": s2_mem, "seq_own": s2_own,
            "seq_retrieved": s2_ret, "seq_raw": s2_raw,
            "static_mem": st_mem, "static_own": st_own, "static_retrieved": st_ret,
        }
        row["control_knew"] = match(c_own, cur["label"])
        row["seq_current"]  = match(s2_mem, cur["label"])
        row["seq_stale"]    = match(s2_mem, prev["label"]) and not row["seq_current"]
        row["static_current"] = match(st_mem, cur["label"])
        results.append(row)
        print(f"  {office[:34]:36} control_own={str(row['control_knew'])[:5]:5} "
              f"seq={'CURRENT' if row['seq_current'] else ('STALE' if row['seq_stale'] else 'other'):7} "
              f"static={'ok' if row['static_current'] else 'no':3} retrieved={s2_ret}", flush=True)
        json.dump(results, open(sys.argv[3] if len(sys.argv) > 3 else "co1.json", "w"), indent=1)
    print(f"\nwrote {len(results)} rows")

main()
