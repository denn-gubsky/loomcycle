#!/usr/bin/env python3
"""Calc help measurement driver: can a model use Calc from its help alone?

Runs every task of tasks.yaml through POST /v1/runs (SSE) on the agent
calc-measure, whose tools are only Calc and Context, on the provider and model
named per run. Runs are STRICTLY sequential: one run at a time, never two in
flight (a local Ollama host serves one model at a time). Standard library only.

The base URL comes from --base-url or LOOMCYCLE_URL; the bearer from the
environment variable named by --token-env (default LOOMCYCLE_AUTH_TOKEN). The
bearer's value is never printed or written.

  python3 -I scripts/measure/calc-help/drive.py \\
      --provider anthropic --model claude-haiku-5-5 \\
      --out <directory outside the repo>

--plan prints the task counts and the cost ceiling and sends nothing.
--summarize FILE prints the per-section table of an existing results file.
"""

import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
from fractions import Fraction

# The price model the project's measurements use for Claude Haiku (USD per
# million tokens). "free" is for a local model.
PRICES = {
    "haiku": {"input": 0.10, "output": 0.50, "cache_read": 0.01, "cache_write": 0.125},
    "free": {"input": 0.0, "output": 0.0, "cache_read": 0.0, "cache_write": 0.0},
}
# The cost guard assumes a run costs at most this much: a calc task is a few
# model turns over a small prompt (README: about $0.002 a run on Haiku).
RUN_COST_CEILING = 0.02

# Calc refusals that mean the call itself was malformed — the expression did
# not parse, a function got the wrong number of arguments, a name, function or
# unit does not exist, or the call's arguments were wrong. Every other code
# (domain, division_by_zero, …) is a well-formed call that the numbers refused.
PARSE_ERRORS = {"syntax", "arity", "bad_request", "call_refused"}


def is_parse_error(code):
    return code is not None and (code in PARSE_ERRORS or code.startswith("unknown_"))


# --- the task file -----------------------------------------------------------

TASK_KEYS = {"id", "section", "prompt", "answer", "tolerance", "check"}
BARE_WORD = re.compile(r"[A-Za-z][A-Za-z0-9_-]*")


def load_tasks(path):
    """Reads tasks.yaml: `- id: x` opens a task, every other `key: <JSON>` line
    belongs to it. Values are JSON scalars, so no YAML library is needed."""
    out, cur = [], None
    with open(path, encoding="utf-8") as f:
        for n, line in enumerate(f, 1):
            s = line.strip()
            if not s or s.startswith("#") or s == "tasks:":
                continue
            if s.startswith("- id:"):
                cur = {"id": s.split(":", 1)[1].strip()}
                out.append(cur)
                continue
            key, sep, val = s.partition(":")
            if not sep or cur is None or key not in TASK_KEYS:
                raise ValueError(f"{path}:{n}: unexpected line: {s}")
            val = val.strip()
            if BARE_WORD.fullmatch(val) and val not in ("true", "false", "null"):
                cur[key] = val  # a bare word (section: units) is a plain string
            else:
                cur[key] = json.loads(val)
    ids = [t["id"] for t in out]
    if len(set(ids)) != len(ids):
        raise ValueError(f"{path}: duplicate task id")
    for t in out:
        missing = TASK_KEYS - set(t)
        if missing:
            raise ValueError(f"{path}: task {t['id']} lacks {sorted(missing)}")
        parse_tolerance(t["tolerance"])
    return out


def parse_tolerance(s):
    """'exact' -> ('exact', 0); 'abs:0.01' / 'rel:1e-6' -> (kind, Fraction)."""
    if s == "exact":
        return "exact", Fraction(0)
    kind, _, x = s.partition(":")
    if kind not in ("abs", "rel") or not x:
        raise ValueError(f"bad tolerance {s!r}")
    return kind, Fraction(x)


# --- answers -----------------------------------------------------------------

ANSWER_LINE = re.compile(r"ANSWER\s*:\s*(.+)", re.IGNORECASE)
NUMBER = re.compile(r"[-+]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][-+]?\d+)?(?:\s*/\s*\d+)?")
GROUPED = re.compile(r"(?<![\d.])(\d{1,3}(?:,\d{3})+)(?![\d,])")
ISO_DATE = re.compile(r"\b(\d{4}-\d{2}-\d{2})\b")


def extract_answer(text):
    """The value of the LAST `ANSWER:` line, without markdown emphasis, code
    ticks or a closing full stop; None when the text has no such line."""
    found = None
    for line in (text or "").splitlines():
        m = ANSWER_LINE.search(line.replace("*", ""))
        if m:
            found = m.group(1)
    if found is None:
        return None
    found = found.strip().rstrip(".").strip().strip("`").strip()
    # A full stop inside the ticks (`42.5.`) as well as after them.
    return found.rstrip(".").strip() or None


def to_number(raw):
    """The first number in raw as a Fraction: thousands separators, currency
    symbols, units and a percent sign are ignored; "a/b" is a fraction. None
    when raw holds no number."""
    if raw is None:
        return None
    s = GROUPED.sub(lambda m: m.group(1).replace(",", ""), raw)
    m = NUMBER.search(s)
    if not m:
        return None
    tok = m.group(0).replace(" ", "")
    try:
        if "/" in tok:
            a, b = tok.split("/")
            return Fraction(a) / Fraction(b) if Fraction(b) != 0 else None
        return Fraction(tok)
    except (ValueError, ZeroDivisionError):
        return None


def is_correct(task, raw):
    """Whether raw (an extracted answer) is the task's answer under its
    tolerance."""
    if raw is None:
        return False
    want = task["answer"]
    if isinstance(want, bool):
        word = raw.strip().lower()
        return word in (("true", "yes") if want else ("false", "no"))
    if isinstance(want, str) and ISO_DATE.fullmatch(want):
        m = ISO_DATE.search(raw)
        return bool(m) and m.group(1) == want
    if isinstance(want, str) and to_number(want) is None:
        return re.sub(r"[^a-z]", "", raw.lower()) == re.sub(r"[^a-z]", "", want.lower())
    if isinstance(want, str):
        w = to_number(want)
    elif isinstance(want, float):
        w = Fraction(repr(want))  # the decimal written in the file, not its binary neighbour
    else:
        w = Fraction(want)
    got = to_number(raw)
    if got is None or w is None:
        return False
    kind, tol = parse_tolerance(task["tolerance"])
    if kind == "exact":
        return got == w
    if kind == "abs":
        return abs(got - w) <= tol
    return abs(got - w) <= tol * abs(w)


# --- one run -----------------------------------------------------------------

def new_record():
    return {"text": "", "run_id": None, "provider": None, "model": None,
            "calc_calls": 0, "calc_errors": [], "first_calc_parsed": None, "first_calc_error": None,
            "help_reads": 0, "help_topics": [], "tool_calls": {},
            "input": 0, "output": 0, "cache_read": 0, "cache_write": 0,
            "stop": None, "error": None}


def _json(text):
    try:
        return json.loads(text)
    except (TypeError, ValueError):
        return None


def calc_error_code(data):
    """The Calc refusal code a tool_result carries, or None for a result.

    A refused call reaches the stream wrapped: the result text is
    {"isError": true, "error": "<Calc's own refusal as JSON>", ...}. An error
    that is not Calc's JSON refusal (the dispatcher's argument check, which
    runs before Calc) means the call itself was malformed: "call_refused".
    """
    body = _json(data.get("text"))
    if isinstance(body, dict) and (body.get("isError") or data.get("is_error")):
        inner = body.get("error")
        inner = _json(inner) if isinstance(inner, str) else inner
        if isinstance(inner, dict) and isinstance(inner.get("error"), str):
            return inner["error"]
        return "call_refused"
    if isinstance(body, dict) and isinstance(body.get("error"), str) and "result" not in body:
        return body["error"]  # Calc's refusal, unwrapped
    if data.get("is_error"):
        return "call_refused"
    return None


def apply_event(rec, event, data, state):
    """Folds one SSE event into rec. state carries the Calc call ids in the
    order they were made."""
    if event == "agent" and rec["run_id"] is None:
        rec["run_id"] = data.get("run_id")
    elif event == "text":
        rec["text"] += data.get("text", "")
    elif event == "tool_call":
        use = data.get("tool_use") or {}
        name = use.get("name") or "?"
        inp = use.get("input") or {}
        op = inp.get("op") if isinstance(inp, dict) else None
        key = name + ("/" + op if op else "")
        rec["tool_calls"][key] = rec["tool_calls"].get(key, 0) + 1
        if name == "Calc":
            rec["calc_calls"] += 1
            state["calc_ids"].append(use.get("id"))
        elif name == "Context" and op == "help":
            rec["help_reads"] += 1
            rec["help_topics"].append(inp.get("topic") or "(index)")
    elif event == "tool_result":
        use = data.get("tool_use") or {}
        if use.get("name") != "Calc":
            return
        code = calc_error_code(data)
        if code:
            rec["calc_errors"].append(code)
        if state["calc_ids"] and use.get("id") == state["calc_ids"][0] and rec["first_calc_parsed"] is None:
            rec["first_calc_parsed"] = not is_parse_error(code)
            rec["first_calc_error"] = code
    elif event == "usage":
        u = data.get("usage") or {}
        rec["input"] += u.get("input_tokens", 0) or 0
        rec["output"] += u.get("output_tokens", 0) or 0
        rec["cache_read"] += u.get("cache_read_input_tokens", 0) or 0
        rec["cache_write"] += u.get("cache_creation_input_tokens", 0) or 0
        rec["provider"] = u.get("provider") or rec["provider"]
        rec["model"] = u.get("model") or rec["model"]
    elif event == "done":
        rec["stop"] = data.get("stop_reason")
    elif event == "error":
        rec["error"] = data.get("text") or json.dumps(data)


def parse_sse(lines, rec):
    """Folds an SSE stream (an iterable of text lines) into rec."""
    state = {"calc_ids": []}
    event = None
    for line in lines:
        line = line.rstrip("\r\n")
        if line.startswith("event:"):
            event = line[6:].strip()
            continue
        if not line.startswith("data:"):
            continue
        try:
            data = json.loads(line[5:].strip())
        except ValueError:
            continue
        if isinstance(data, dict):
            apply_event(rec, event, data, state)
    return rec


def run_once(base, token, agent, prompt, provider, model, timeout):
    body = {"agent": agent, "prompt": prompt, "user_id": "calc-help-measure",
            "metadata": {"measurement": "calc-help"}}
    if provider:
        body["provider"] = provider
    if model:
        body["model"] = model
    req = urllib.request.Request(base.rstrip("/") + "/v1/runs", data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json", "Accept": "text/event-stream"})
    if token:
        req.add_header("Authorization", "Bearer " + token)
    rec = new_record()
    started = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            parse_sse((raw.decode("utf-8", "replace") for raw in resp), rec)
    except urllib.error.HTTPError as e:
        # The refusal's own text says why (an unreachable model, a bad agent).
        rec["error"] = f"HTTP {e.code}: {e.read(500).decode('utf-8', 'replace').strip()}"
    except Exception as e:  # a failed run is a data point, not a crash
        rec["error"] = str(e)
    rec["seconds"] = round(time.time() - started, 1)
    return rec


def cost(rec, price):
    p = PRICES[price]
    return (rec["input"] * p["input"] + rec["output"] * p["output"] +
            rec["cache_read"] * p["cache_read"] + rec["cache_write"] * p["cache_write"]) / 1e6


def score(task, rec):
    raw = extract_answer(rec["text"])
    rec["answer"] = raw
    rec["correct"] = is_correct(task, raw)
    return rec


# --- the summary -------------------------------------------------------------

def summarise(results):
    """Per section, then all: runs, the share of runs that called Calc, the
    first-call parse rate (of the runs that called Calc), accuracy, mean Calc
    calls, mean help reads, failed runs and spend."""
    sections = []
    for r in results:
        if r["section"] not in sections:
            sections.append(r["section"])
    rows = []
    for sec in sections + ["all"]:
        rs = [r for r in results if sec == "all" or r["section"] == sec]
        called = [r for r in rs if r["calc_calls"] > 0]
        n = len(rs)
        rows.append({
            "section": sec,
            "runs": n,
            "called_calc": round(len(called) / n, 3) if n else None,
            "first_call_parsed": round(sum(1 for r in called if r["first_calc_parsed"]) / len(called), 3) if called else None,
            "accuracy": round(sum(1 for r in rs if r["correct"]) / n, 3) if n else None,
            "mean_calc_calls": round(sum(r["calc_calls"] for r in rs) / n, 2) if n else None,
            "mean_help_reads": round(sum(r["help_reads"] for r in rs) / n, 2) if n else None,
            "failed": sum(1 for r in rs if r.get("error")),
            "cost_usd": round(sum(r.get("cost_usd", 0) for r in rs), 4),
        })
    return rows


def markdown(rows):
    def pct(x):
        return "—" if x is None else f"{x * 100:.0f}%"
    out = ["| section | runs | called Calc | first call parsed | accuracy | Calc calls | help reads | failed | cost |",
           "|---|---:|---:|---:|---:|---:|---:|---:|---:|"]
    for r in rows:
        out.append(f"| {r['section']} | {r['runs']} | {pct(r['called_calc'])} | {pct(r['first_call_parsed'])} | "
                   f"{pct(r['accuracy'])} | {r['mean_calc_calls']} | {r['mean_help_reads']} | {r['failed']} | "
                   f"${r['cost_usd']:.4f} |")
    return "\n".join(out)


def help_topic_counts(results):
    counts = {}
    for r in results:
        for t in r.get("help_topics", []):
            counts[t] = counts.get(t, 0) + 1
    return dict(sorted(counts.items(), key=lambda kv: (-kv[1], kv[0])))


def model_mismatch(asked, reported):
    """Whether a run ran on a model other than the one it asked for. A
    provider may report a dated id for an alias (claude-haiku-5-5-2026…), so a
    reported id that starts with the asked one matches."""
    return bool(asked and reported and not reported.startswith(asked))


def read_results(path):
    with open(path, encoding="utf-8") as f:
        return [json.loads(line) for line in f if line.strip()]


# --- main --------------------------------------------------------------------

def main(argv=None):
    here = os.path.dirname(os.path.abspath(__file__))
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base-url", default=os.environ.get("LOOMCYCLE_URL", "http://127.0.0.1:8787"))
    ap.add_argument("--token-env", default="LOOMCYCLE_AUTH_TOKEN",
                    help="name of the environment variable holding the bearer")
    ap.add_argument("--agent", default="calc-measure")
    ap.add_argument("--provider", help="per-run provider, e.g. anthropic or ollama-local")
    ap.add_argument("--model", help="per-run model, e.g. claude-haiku-5-5")
    ap.add_argument("--tasks", default=os.path.join(here, "tasks.yaml"))
    ap.add_argument("--only", help="comma-separated task ids or sections to run")
    ap.add_argument("--runs", type=int, default=1, help="runs per task")
    ap.add_argument("--price", choices=sorted(PRICES), default="haiku")
    ap.add_argument("--max-usd", type=float, default=2.0, help="stop before a run that could pass this spend")
    ap.add_argument("--timeout", type=int, default=600, help="seconds per run")
    ap.add_argument("--out", help="directory for results.jsonl and the summary (keep it outside the repo)")
    ap.add_argument("--plan", action="store_true", help="print the plan and the cost ceiling; send nothing")
    ap.add_argument("--summarize", metavar="RESULTS", help="print the summary of an existing results.jsonl")
    args = ap.parse_args(argv)

    if args.summarize:
        results = read_results(args.summarize)
        print(markdown(summarise(results)))
        print("\nhelp topics read:", json.dumps(help_topic_counts(results)))
        return 0

    tasks = load_tasks(args.tasks)
    if args.only:
        keep = set(args.only.split(","))
        tasks = [t for t in tasks if t["id"] in keep or t["section"] in keep]
    per_section = {}
    for t in tasks:
        per_section[t["section"]] = per_section.get(t["section"], 0) + 1
    n_runs = len(tasks) * args.runs
    print(f"{len(tasks)} tasks {json.dumps(per_section)} x {args.runs} run(s) = {n_runs} runs; "
          f"agent {args.agent}, provider {args.provider or '(agent default)'}, model {args.model or '(agent default)'}; "
          f"price model {args.price}; ceiling ${n_runs * RUN_COST_CEILING * (args.price != 'free'):.2f}, "
          f"stop at ${args.max_usd:.2f}", file=sys.stderr)
    if args.plan:
        return 0
    if not args.out:
        ap.error("--out is required to run")
    token = os.environ.get(args.token_env, "")
    if not token:
        print(f"warning: {args.token_env} is empty; sending no bearer", file=sys.stderr)
    os.makedirs(args.out, exist_ok=True)
    results_path = os.path.join(args.out, "results.jsonl")
    results, spent = [], 0.0
    # Run-major order: every task once, then every task again, so drift on
    # the server spreads over all tasks alike. One run at a time.
    order = [(k, t) for k in range(args.runs) for t in tasks]
    with open(results_path, "a", encoding="utf-8") as out:
        for k, t in order:
            if args.price != "free" and spent + RUN_COST_CEILING > args.max_usd:
                print(f"stopping: ${spent:.4f} spent, the next run could pass --max-usd {args.max_usd}",
                      file=sys.stderr)
                break
            rec = run_once(args.base_url, token, args.agent, t["prompt"], args.provider, args.model,
                           args.timeout)
            score(t, rec)
            rec.update({"id": t["id"], "section": t["section"], "run": k + 1, "agent": args.agent,
                        "want": t["answer"], "cost_usd": round(cost(rec, args.price), 6),
                        "model_mismatch": model_mismatch(args.model, rec["model"])})
            spent += rec["cost_usd"]
            results.append(rec)
            out.write(json.dumps(rec) + "\n")
            out.flush()
            print(f"{t['id']} run {k + 1}: answer={rec['answer']!r} want={t['answer']!r} "
                  f"correct={rec['correct']} calc={rec['calc_calls']} parsed1={rec['first_calc_parsed']} "
                  f"help={rec['help_reads']} model={rec['model']} ${rec['cost_usd']:.4f} total ${spent:.4f}"
                  + (" MODEL MISMATCH" if rec["model_mismatch"] else "")
                  + (f" ERROR {rec['error']}" if rec["error"] else ""), file=sys.stderr)
    rows = summarise(results)
    table = markdown(rows)
    models = sorted({r["model"] for r in results if r["model"]})
    mismatched = sum(1 for r in results if r["model_mismatch"])
    with open(os.path.join(args.out, "summary.json"), "w", encoding="utf-8") as f:
        json.dump({"rows": rows, "help_topics": help_topic_counts(results), "spent_usd": round(spent, 4),
                   "provider": args.provider, "model": args.model, "agent": args.agent,
                   "models_seen": models, "model_mismatches": mismatched}, f, indent=2)
    with open(os.path.join(args.out, "summary.md"), "w", encoding="utf-8") as f:
        f.write(table + "\n")
    print(table)
    print("\nhelp topics read:", json.dumps(help_topic_counts(results)))
    print("models seen:", ", ".join(models) or "(none reported)")
    if mismatched:
        print(f"WARNING: {mismatched} run(s) ran on another model than --model {args.model}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
