#!/usr/bin/env python3
"""RFC GM P7 measurement driver: kb/walker with and without the taxonomy moves.

Runs every question of questions.yaml on each arm, --runs times, through
POST /v1/runs (SSE), and writes one JSON line per run plus a summary. Standard
library only. The bearer is read from the environment variable named by
--token-env (default LOOMCYCLE_TOKEN); its value is never printed or written.

  python3 -I scripts/measure/walker/drive.py \\
      --base-url http://127.0.0.1:8787 \\
      --arm tax=kb/walker --arm notax=kb/walker-notax \\
      --provider anthropic --model <haiku model id> \\
      --runs 3 --out <directory outside the repo>

--dry-run prints the plan (runs, the cost ceiling) and sends nothing. Score an
existing runs.jsonl again with --score-only.
"""

import argparse
import json
import os
import re
import sys
import time
import urllib.request

# The measurement price model for Haiku (USD per million tokens).
PRICE = {"input": 0.10, "output": 0.50, "cache_read": 0.01, "cache_write": 0.125}
# The ceiling per run used by --dry-run and the cost guard: an overview walk is
# at most 14 tool calls (15 model turns).
RUN_COST_CEILING = 0.04

DOC_LINK = re.compile(r"loom:doc/([0-9a-f]{32})")
CHUNK_LINK = re.compile(r"loom:chunk/([0-9a-f]{32})")


def load_questions(path):
    """Parses the fixed shape of questions.yaml without a YAML library."""
    out, cur = [], None
    for line in open(path, encoding="utf-8"):
        s = line.strip()
        if s.startswith("- id:"):
            cur = {"id": s.split(":", 1)[1].strip(), "gold": []}
            out.append(cur)
        elif cur is not None and s.startswith("kind:"):
            cur["kind"] = s.split(":", 1)[1].strip()
        elif cur is not None and s.startswith("text:"):
            cur["text"] = s.split(":", 1)[1].strip()
        elif cur is not None and s.startswith("gold:"):
            cur["gold"] = json.loads(s.split(":", 1)[1].strip() or "[]")
    return out


def run_once(base, token, agent, prompt, provider, model, timeout):
    body = {"agent": agent, "prompt": prompt, "user_id": "gm-p7-measure",
            "metadata": {"measurement": "rfc-gm-p7"}}
    # The per-run routing override (RFC DC P1): a model pins it, within what the
    # agent's definition may reach.
    if provider:
        body["provider"] = provider
    if model:
        body["model"] = model
    req = urllib.request.Request(base.rstrip("/") + "/v1/runs", data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json", "Accept": "text/event-stream"})
    if token:
        req.add_header("Authorization", "Bearer " + token)
    rec = {"text": "", "tool_calls": 0, "tools": {}, "input": 0, "output": 0, "cache_read": 0,
           "cache_write": 0, "stop": None, "error": None, "run_id": None}
    started = time.time()
    event = None
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            for raw in resp:
                line = raw.decode("utf-8", "replace").rstrip("\n")
                if line.startswith("event:"):
                    event = line[6:].strip()
                    continue
                if not line.startswith("data:"):
                    continue
                try:
                    data = json.loads(line[5:].strip())
                except ValueError:
                    continue
                if event == "agent" and rec["run_id"] is None:
                    rec["run_id"] = data.get("run_id")
                elif event == "text":
                    rec["text"] += data.get("text", "")
                elif event == "tool_call":
                    rec["tool_calls"] += 1
                    name = (data.get("tool_use") or {}).get("name") or data.get("name") or "?"
                    op = ((data.get("tool_use") or {}).get("input") or {}).get("op")
                    key = name + ("/" + op if op else "")
                    rec["tools"][key] = rec["tools"].get(key, 0) + 1
                elif event == "usage":
                    u = data.get("usage") or {}
                    rec["input"] += u.get("input_tokens", 0)
                    rec["output"] += u.get("output_tokens", 0)
                    rec["cache_read"] += u.get("cache_read_input_tokens", 0)
                    rec["cache_write"] += u.get("cache_creation_input_tokens", 0)
                elif event == "done":
                    rec["stop"] = data.get("stop_reason")
                elif event == "error":
                    rec["error"] = data.get("text") or json.dumps(data)
    except Exception as e:  # a failed run is a data point, not a crash
        rec["error"] = str(e)
    rec["seconds"] = round(time.time() - started, 1)
    return rec


def cost(rec):
    return (rec["input"] * PRICE["input"] + rec["output"] * PRICE["output"] +
            rec["cache_read"] * PRICE["cache_read"] + rec["cache_write"] * PRICE["cache_write"]) / 1e6


def score(q, rec):
    docs = set(DOC_LINK.findall(rec["text"]))
    lines = rec["text"].splitlines()
    out = {"cited_documents": len(docs), "cited_chunks": len(set(CHUNK_LINK.findall(rec["text"]))),
           "coverage": next((l.split(":", 1)[1].strip() for l in lines if l.startswith("COVERAGE:")), None),
           "moves": next((l.split(":", 1)[1].strip() for l in lines if l.startswith("MOVES:")), None),
           "used_place": rec["tools"].get("Document/place", 0) > 0,
           "used_measure": rec["tools"].get("Document/measure", 0) > 0}
    gold = set(q.get("gold") or [])
    if gold:
        hit = len(docs & gold)
        out["recall"] = hit / len(gold)
        out["precision"] = hit / len(docs) if docs else 0.0
    return out


def summarise(rows):
    by = {}
    for r in rows:
        k = (r["arm"], r["kind"])
        by.setdefault(k, []).append(r)
    out = []
    for (arm, kind), rs in sorted(by.items()):
        n = len(rs)
        rec = [r["score"].get("recall") for r in rs if r["score"].get("recall") is not None]
        out.append({"arm": arm, "kind": kind, "runs": n,
                    "failed": sum(1 for r in rs if r["error"] or r["stop"] != "end_turn"),
                    "recall_mean": round(sum(rec) / len(rec), 3) if rec else None,
                    "cited_documents_mean": round(sum(r["score"]["cited_documents"] for r in rs) / n, 1),
                    "tool_calls_mean": round(sum(r["tool_calls"] for r in rs) / n, 1),
                    "complete_share": round(sum(1 for r in rs if r["score"]["coverage"] == "complete") / n, 2),
                    "cost_usd": round(sum(r["cost_usd"] for r in rs), 4)})
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default="http://127.0.0.1:8787")
    ap.add_argument("--token-env", default="LOOMCYCLE_TOKEN")
    ap.add_argument("--arm", action="append", default=[], help="name=agent, e.g. tax=kb/walker")
    ap.add_argument("--questions", default=os.path.join(os.path.dirname(__file__), "questions.yaml"))
    ap.add_argument("--runs", type=int, default=3)
    ap.add_argument("--provider", default="")
    ap.add_argument("--model", default="")
    ap.add_argument("--timeout", type=int, default=600)
    ap.add_argument("--budget-usd", type=float, default=5.0, help="stop when the spend reaches this")
    ap.add_argument("--out", required=True)
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--score-only", action="store_true")
    a = ap.parse_args()

    qs = load_questions(a.questions)
    arms = [x.split("=", 1) for x in a.arm]
    if not arms or any(len(x) != 2 for x in arms):
        sys.exit("pass --arm name=agent at least once")
    total = len(qs) * len(arms) * a.runs
    if a.dry_run:
        print(f"{len(qs)} questions x {len(arms)} arms x {a.runs} runs = {total} runs; "
              f"ceiling ${total * RUN_COST_CEILING:.2f} at ${RUN_COST_CEILING}/run")
        return
    os.makedirs(a.out, exist_ok=True)
    path = os.path.join(a.out, "runs.jsonl")
    rows = []
    if a.score_only:
        rows = [json.loads(l) for l in open(path, encoding="utf-8")]
        byq = {q["id"]: q for q in qs}
        for r in rows:
            r["score"] = score(byq[r["question"]], r)
    else:
        token = os.environ.get(a.token_env, "")
        spent = 0.0
        with open(path, "a", encoding="utf-8") as f:
            # Interleave arms and repeats so drift on the server hits both alike.
            for rep in range(a.runs):
                for q in qs:
                    for arm, agent in arms:
                        if spent >= a.budget_usd:
                            print(f"budget ${a.budget_usd} reached; stopping", file=sys.stderr)
                            break
                        rec = run_once(a.base_url, token, agent, q["text"], a.provider, a.model, a.timeout)
                        rec.update({"arm": arm, "agent": agent, "question": q["id"], "kind": q["kind"], "rep": rep})
                        rec["cost_usd"] = round(cost(rec), 6)
                        rec["score"] = score(q, rec)
                        spent += rec["cost_usd"]
                        f.write(json.dumps(rec) + "\n")
                        f.flush()
                        rows.append(rec)
                        print(f"{q['id']} {arm} rep{rep}: {rec['stop']} calls={rec['tool_calls']} "
                              f"docs={rec['score']['cited_documents']} ${rec['cost_usd']:.4f} total ${spent:.2f}",
                              file=sys.stderr)
    summary = summarise(rows)
    with open(os.path.join(a.out, "summary.json"), "w", encoding="utf-8") as f:
        json.dump(summary, f, indent=2)
    for s in summary:
        print(json.dumps(s))


if __name__ == "__main__":
    main()
