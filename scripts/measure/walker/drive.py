#!/usr/bin/env python3
"""RFC GM P7 measurement driver: kb/walker with and without the taxonomy moves.

Runs every question of questions.yaml on each arm, --runs times, through
POST /v1/runs (SSE), and writes one JSON line per run plus a summary. Standard
library only. The bearer is read from the environment variable named by
--token-env (default LOOMCYCLE_TOKEN); its value is never printed or written.

  python3 -I scripts/measure/walker/drive.py \\
      --base-url http://127.0.0.1:8787 \\
      --arm tax=kb/walker --arm notax=kb/walker-notax \\
      --gold <gold file outside the repo> \\
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
    with open(path, encoding="utf-8") as f:
        lines = f.read().splitlines()
    for line in lines:
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


# --- the gold file -----------------------------------------------------------
#
# The gold lives OUTSIDE the repo: it names documents of a real tenant. Its shape
# is questions.yaml's with more per-question keys, which a flat line scan cannot
# read (gold_topics is a list of mappings), so a small YAML subset parser reads
# it: block mappings and sequences, flow lists of scalars, quoted and plain
# scalars, `|`/`>` block scalars and comments. Anything outside that is an error
# rather than a silent misread. Every scalar stays a string.

GOLD_KEYS = {"id", "kind", "text", "gold", "also", "gold_notes", "confidence", "gold_topics"}


def _scalar_starts_at(s, i):
    # A quote opens a quoted scalar only where a scalar starts: at the line's
    # start or after `: `, `- `, `[` or `,`. Elsewhere (owner's) it is text.
    prev = s[:i].rstrip()
    return (prev == "" or prev == "-" or prev.endswith(" -") or
            prev.endswith(":") or prev.endswith("[") or prev.endswith(","))


def _strip_comment(s):
    q = None
    i = 0
    while i < len(s):
        c = s[i]
        if q:
            if c == q == "'" and s[i + 1:i + 2] == "'":
                i += 1  # '' is an escaped quote, not the close
            elif c == q:
                q = None
        elif c in "'\"" and _scalar_starts_at(s, i):
            q = c
        elif c == "#" and (i == 0 or s[i - 1] in " \t"):
            return s[:i].rstrip()
        i += 1
    return s.rstrip()


def _scalar(s):
    s = s.strip()
    if s in ("", "~", "null"):
        return None
    if s.startswith('"'):
        return json.loads(s)
    if s.startswith("'"):
        if len(s) < 2 or not s.endswith("'"):
            raise ValueError("unterminated quoted scalar: " + s)
        return s[1:-1].replace("''", "'")
    if s.startswith("["):
        if not s.endswith("]"):
            raise ValueError("a flow list must close on its own line: " + s)
        items, cur, q = [], "", None
        body = s[1:-1]
        i = 0
        while i < len(body):
            c = body[i]
            i += 1
            if q:
                cur += c
                if c == q == "'" and body[i:i + 1] == "'":
                    cur += "'"  # '' is an escaped quote, not the close
                    i += 1
                elif c == q:
                    q = None
            elif c in "'\"" and not cur.strip():
                # A quote opens a quoted item only where the item starts: the
                # apostrophe in [owner's view, x] is text.
                q = c
                cur += c
            elif c == ",":
                items.append(cur)
                cur = ""
            else:
                cur += c
        if cur.strip():
            items.append(cur)
        return [_scalar(x) for x in items]
    if s.startswith("{"):
        raise ValueError("flow mappings are not supported: " + s)
    return s


def _is_seq_item(content):
    return content == "-" or content.startswith("- ")


def parse_yaml_subset(text):
    raw = text.splitlines()
    lines = []  # [indent, content, raw line index]
    for n, line in enumerate(raw):
        if "\t" in line[: len(line) - len(line.lstrip())]:
            raise ValueError(f"line {n + 1}: tab indentation")
        c = _strip_comment(line)
        if c.strip():
            lines.append([len(c) - len(c.lstrip()), c.strip(), n])

    def block_scalar(i, parent_indent, style):
        # Block scalars are read from the raw lines: a `#` in them is text.
        end = lines[i - 1][2] + 1
        body = []
        while end < len(raw):
            r = raw[end]
            if r.strip() and len(r) - len(r.lstrip()) <= parent_indent:
                break
            body.append(r)
            end += 1
        while i < len(lines) and lines[i][2] < end:
            i += 1
        while body and not body[-1].strip():
            body.pop()
        ind = min((len(b) - len(b.lstrip()) for b in body if b.strip()), default=0)
        body = [b[ind:] for b in body]
        if style == ">":
            return " ".join(b.strip() for b in body if b.strip()), i
        return "\n".join(body), i

    def value_after(rest, i, indent):
        # The value of `key: rest` or `- rest` on a line at `indent`; i is the
        # index of the next line.
        if rest in ("|", ">", "|-", ">-"):
            return block_scalar(i, indent, rest[0])
        if rest:
            return _scalar(rest), i
        if i < len(lines) and lines[i][0] > indent:
            return block(i, lines[i][0])
        if i < len(lines) and lines[i][0] == indent and _is_seq_item(lines[i][1]):
            return block(i, indent)  # a sequence may sit at its key's indent
        return None, i

    def block(i, indent):
        if _is_seq_item(lines[i][1]):
            seq = []
            while i < len(lines) and lines[i][0] == indent and _is_seq_item(lines[i][1]):
                rest = lines[i][1][1:].strip()
                if re.match(r"^[^'\"\[{][^:]*:(\s|$)", rest):
                    # `- key: v` opens a mapping whose keys sit two columns in.
                    lines[i] = [indent + 2, rest, lines[i][2]]
                    v, i = block(i, indent + 2)
                else:
                    v, i = value_after(rest, i + 1, indent)
                seq.append(v)
            return seq, i
        m = {}
        while i < len(lines) and lines[i][0] == indent and not _is_seq_item(lines[i][1]):
            content = lines[i][1]
            if ":" not in content:
                raise ValueError(f"line {lines[i][2] + 1}: expected `key: value`: {content}")
            k, rest = content.split(":", 1)
            k = k.strip()
            if k in m:
                raise ValueError(f"line {lines[i][2] + 1}: duplicate key {k}")
            m[k], i = value_after(rest.strip(), i + 1, indent)
        return m, i

    if not lines:
        return None
    v, i = block(0, lines[0][0])
    if i != len(lines):
        raise ValueError(f"line {lines[i][2] + 1}: unexpected indentation")
    return v


def _str_list(v, where):
    if v is None:
        return []
    if not isinstance(v, list) or not all(isinstance(x, str) and x for x in v):
        raise ValueError(f"{where}: expected a list of strings")
    return v


def _topics(v, where):
    if v is None:
        return []
    if not isinstance(v, list):
        raise ValueError(f"{where}: expected a list of topics")
    out = []
    for t in v:
        if isinstance(t, str):
            t = {"name": t}
        if not isinstance(t, dict) or not isinstance(t.get("name"), str) or not t["name"].strip():
            raise ValueError(f"{where}: each topic needs a name")
        extra = set(t) - {"name", "aliases"}
        if extra:
            raise ValueError(f"{where}: unknown topic keys {sorted(extra)}")
        out.append({"name": t["name"].strip(), "aliases": _str_list(t.get("aliases"), where + " aliases")})
    return out


def merge_gold(qs, text, warn=None):
    """Merges a gold file's entries into qs by id, in place, and returns qs.
    Refuses (ValueError) an id questions.yaml does not have; calls warn for
    each question left without any gold."""
    doc = parse_yaml_subset(text)
    if not isinstance(doc, dict) or not isinstance(doc.get("questions"), list):
        raise ValueError("gold file: expected a top-level `questions:` list")
    byq = {q["id"]: q for q in qs}
    seen = set()
    for e in doc["questions"]:
        if not isinstance(e, dict) or not isinstance(e.get("id"), str):
            raise ValueError("gold file: every entry needs an id")
        qid = e["id"]
        if qid not in byq:
            raise ValueError(f"gold file: unknown question id {qid} (not in questions.yaml)")
        if qid in seen:
            raise ValueError(f"gold file: duplicate question id {qid}")
        seen.add(qid)
        extra = set(e) - GOLD_KEYS
        if extra:
            raise ValueError(f"gold file: {qid}: unknown keys {sorted(extra)}")
        q = byq[qid]
        q["gold"] = _str_list(e.get("gold"), qid + " gold")
        q["also"] = _str_list(e.get("also"), qid + " also")
        q["gold_topics"] = _topics(e.get("gold_topics"), qid + " gold_topics")
        for t in q["gold_topics"]:
            # An answer that restates its question would name such a topic for
            # free, so the topic cannot be scored on this question.
            if topic_spans(q.get("text") or "", t):
                t["unscorable"] = True
                if warn:
                    warn(f"warning: {qid}: topic {t['name']!r} (name or alias) occurs in the question "
                         "text; it is not scored")
        for k in ("gold_notes", "confidence"):
            if e.get(k) is not None:
                q[k] = e[k]
    if warn:
        for q in qs:
            if not q.get("gold") and not q.get("gold_topics"):
                warn(f"warning: {q['id']} has no gold; it is reported without recall")
    return qs


def topic_spans(text, topic):
    """The (start, end) spans where text names the topic or one of its
    aliases: case-insensitive, on word boundaries (so "authz" does not match
    "authzx"), with any run of whitespace between the words of a multi-word
    name."""
    spans = []
    for term in [topic["name"]] + list(topic.get("aliases") or []):
        words = term.split()
        if not words:
            continue
        pat = r"(?<!\w)" + r"\s+".join(re.escape(w) for w in words) + r"(?!\w)"
        spans += [m.span() for m in re.finditer(pat, text, re.IGNORECASE)]
    return spans


def topic_named(text, topic):
    return bool(topic_spans(text, topic))


def named_topics(text, topics):
    """The names of the topics the text names, where a match lying inside a
    LONGER match of another topic does not count: "access control" names that
    topic, not also a topic "access". Every topic's matches compete,
    unscorable ones included, since the longer term is what the text says."""
    spans = [(i, s) for i, t in enumerate(topics) for s in topic_spans(text, t)]

    def inside_longer(i, s):
        return any(j != i and o[0] <= s[0] and s[1] <= o[1] and o[1] - o[0] > s[1] - s[0]
                   for j, o in spans)

    hit = {i for i, s in spans if not inside_longer(i, s)}
    return [t["name"] for i, t in enumerate(topics) if i in hit]


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
    # `also` lists documents a good answer may cite without having to: they
    # count for precision, never for recall.
    accepted = gold | set(q.get("also") or [])
    doc_scores = {}
    if gold:
        doc_scores["recall"] = len(docs & gold) / len(gold)
        doc_scores["precision"] = len(docs & accepted) / len(docs) if docs else 0.0
    topics = q.get("gold_topics") or []
    if topics:
        # Owner ruling: a question with gold_topics is scored on the topics its
        # answer names, whatever document gold it also carries (kept beside it
        # as doc_recall/doc_precision, outside the summary).
        # A topic named in the question text itself is left out (see
        # merge_gold); if every topic is, the question has no recall.
        scorable = [t for t in topics if not t.get("unscorable")]
        named = [n for n in named_topics(rec["text"], topics) if n in {t["name"] for t in scorable}]
        out["metric"] = "topics"
        out["recall"] = len(named) / len(scorable) if scorable else None
        out["topics_named"] = named
        out["topics_missed"] = [t["name"] for t in scorable if t["name"] not in named]
        out["topics_unscorable"] = [t["name"] for t in topics if t.get("unscorable")]
        if doc_scores:
            out["doc_recall"] = doc_scores["recall"]
            out["doc_precision"] = doc_scores["precision"]
    elif doc_scores:
        out["metric"] = "docs"
        out.update(doc_scores)
    else:
        out["metric"] = None
    return out


def summarise(rows):
    """Rows per (arm, question kind, metric), plus the arms side by side per
    metric: document recall and topic recall measure different things and are
    never averaged together."""
    by = {}
    for r in rows:
        k = (r["arm"], r["kind"], r["score"].get("metric") or "none")
        by.setdefault(k, []).append(r)
    out = []
    for (arm, kind, metric), rs in sorted(by.items()):
        n = len(rs)
        rec = [r["score"].get("recall") for r in rs if r["score"].get("recall") is not None]
        out.append({"arm": arm, "kind": kind, "metric": metric, "runs": n,
                    "failed": sum(1 for r in rs if r["error"] or r["stop"] != "end_turn"),
                    "recall_mean": round(sum(rec) / len(rec), 3) if rec else None,
                    "cited_documents_mean": round(sum(r["score"]["cited_documents"] for r in rs) / n, 1),
                    "tool_calls_mean": round(sum(r["tool_calls"] for r in rs) / n, 1),
                    "complete_share": round(sum(1 for r in rs if r["score"]["coverage"] == "complete") / n, 2),
                    "cost_usd": round(sum(r["cost_usd"] for r in rs), 4)})
    compare = {}
    for r in rows:
        metric = r["score"].get("metric")
        if metric and r["score"].get("recall") is not None:
            compare.setdefault(metric, {}).setdefault(r["arm"], []).append(r["score"]["recall"])
    by_metric = [{"metric": m, "arms": {a: {"runs": len(v), "recall_mean": round(sum(v) / len(v), 3)}
                                        for a, v in sorted(arms.items())}}
                 for m, arms in sorted(compare.items())]
    return {"rows": out, "by_metric": by_metric}


def main(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default="http://127.0.0.1:8787")
    ap.add_argument("--token-env", default="LOOMCYCLE_TOKEN")
    ap.add_argument("--arm", action="append", default=[], help="name=agent, e.g. tax=kb/walker")
    ap.add_argument("--questions", default=os.path.join(os.path.dirname(__file__), "questions.yaml"))
    ap.add_argument("--gold", default="",
                    help="gold file (questions.yaml's shape, kept outside the repo), merged by question id")
    ap.add_argument("--runs", type=int, default=3)
    ap.add_argument("--provider", default="")
    ap.add_argument("--model", default="")
    ap.add_argument("--timeout", type=int, default=600)
    ap.add_argument("--budget-usd", type=float, default=5.0, help="stop when the spend reaches this")
    ap.add_argument("--out", default="", help="output directory (required unless --dry-run)")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--score-only", action="store_true")
    a = ap.parse_args(argv)

    qs = load_questions(a.questions)
    if a.gold:
        try:
            with open(a.gold, encoding="utf-8") as f:
                merge_gold(qs, f.read(), warn=lambda m: print(m, file=sys.stderr))
        except (OSError, ValueError) as e:
            sys.exit(f"--gold {a.gold}: {e}")
    elif a.score_only:
        # Rescoring without gold would only replace a scored summary.json with
        # one that has no recall at all.
        sys.exit("--score-only needs --gold")
    else:
        print("no --gold: runs are scored without recall", file=sys.stderr)
    arms = [x.split("=", 1) for x in a.arm]
    if not a.score_only and (not arms or any(len(x) != 2 for x in arms)):
        sys.exit("pass --arm name=agent at least once")
    total = len(qs) * len(arms) * a.runs
    if a.dry_run:
        metrics = {"docs": 0, "topics": 0}
        for q in qs:
            if q.get("gold_topics"):
                metrics["topics"] += 1
            elif q.get("gold"):
                metrics["docs"] += 1
        print(f"{len(qs)} questions ({metrics['docs']} scored on docs, {metrics['topics']} on topics) "
              f"x {len(arms)} arms x {a.runs} runs = {total} runs; "
              f"ceiling ${total * RUN_COST_CEILING:.2f} at ${RUN_COST_CEILING}/run")
        return
    if not a.out:
        sys.exit("pass --out <directory outside the repo>")
    os.makedirs(a.out, exist_ok=True)
    path = os.path.join(a.out, "runs.jsonl")
    rows = []
    if a.score_only:
        with open(path, encoding="utf-8") as f:
            rows = [json.loads(l) for l in f if l.strip()]
        byq = {q["id"]: q for q in qs}
        for r in rows:
            if r["question"] not in byq:
                sys.exit(f"{path}: run of unknown question {r['question']}")
            r["score"] = score(byq[r["question"]], r)
        # Rescored lines go next to the original, which stays untouched.
        with open(os.path.join(a.out, "runs.scored.jsonl"), "w", encoding="utf-8") as f:
            for r in rows:
                f.write(json.dumps(r) + "\n")
    else:
        token = os.environ.get(a.token_env, "")
        spent = 0.0
        # Interleave arms and repeats so drift on the server hits both alike.
        plan = [(rep, q, arm, agent) for rep in range(a.runs) for q in qs for arm, agent in arms]
        with open(path, "a", encoding="utf-8") as f:
            for rep, q, arm, agent in plan:
                # Stop BEFORE a run that could take the spend past the budget: a
                # run costs at most RUN_COST_CEILING.
                if spent + RUN_COST_CEILING > a.budget_usd:
                    print(f"budget ${a.budget_usd}: ${spent:.2f} spent, the next run could pass it; stopping",
                          file=sys.stderr)
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
                      f"docs={rec['score']['cited_documents']} metric={rec['score']['metric']} "
                      f"${rec['cost_usd']:.4f} total ${spent:.2f}", file=sys.stderr)
    summary = summarise(rows)
    with open(os.path.join(a.out, "summary.json"), "w", encoding="utf-8") as f:
        json.dump(summary, f, indent=2)
    for s in summary["rows"]:
        print(json.dumps(s))
    for s in summary["by_metric"]:
        print(json.dumps(s))


if __name__ == "__main__":
    main()
