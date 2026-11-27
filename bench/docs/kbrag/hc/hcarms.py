"""Run the RFC HC arms on one set and record one JSON line per run.

    python3 -I bench/docs/kbrag/hc/hcarms.py --set cqa --split test --out <work>/study --workers 4
    python3 -I bench/docs/kbrag/hc/hcarms.py --set p7 --out <work>/study --workers 4
    python3 -I bench/docs/kbrag/hc/hcarms.py --set cqa --split dev --qids dev-1,train-7 --arms kb-first --out <work>/tune

-> <out>/<set>/runs/<arm>.jsonl, one row per (arm, qid, rep, attempt); <out>/<set>/runs/events.jsonl;
<out>/<set>/runs/versions.jsonl (instrument check 9).

Arms (hcconfig): kb-today (kb/ask), kb-first (kb/ask, run metadata first_call: true),
kb-today+answer-first (the answer-first copy of kb/ask; tenant-side setup), rag-lc (kb/answer
spawned directly: the first call's own search and reader), oracle (kb/read on the gold
sections, ConditionalQA only; instrument checks 6 and 7).

Plan: ConditionalQA test = rep 0 of all 800, then reps 1-2 of the fixed 100 (the benchmark's
sample); the walker set = 3 reps of every question; the dev split = --reps (default 1). Order:
rep-major, questions in a seeded order, and per (question, rep) the arms back to back in an
order rotated by a seeded RNG, so drift on the server reaches every arm alike.

Guards before every run: the study-wide spend cap (both tenants' ledger increase over the
baseline + runs in flight at each arm's p90 estimate, against $19), an optional deadline, the
split (a test question in a dev invocation, or the reverse, stops before anything runs).
After every run: a serving (provider, model) outside the policy, or a kb agent not on Haiku,
marks the row provider_violation and stops. Every 20 runs: the usage ledger since the start
and the recorded tenant state (tenant_state.py) are checked again.

Resumable: an (arm, qid, rep) whose latest row is ok is skipped; --retry-failed retries a
failed one once (check 8). Idempotency keys carry the root's name and the split, so a tuning
root never hands its runs to the study root.
"""
import argparse, datetime, json, os, random, re, sys, threading, time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(1, os.path.dirname(HERE))
import cost, hcconfig, splits, tenant_state  # noqa: E402
import common, prompts  # noqa: E402  (../common.py, ../prompts.py)
from common import Usage  # noqa: E402
from docs.darms import safe_id  # noqa: E402  (the server's key charset rule, Study D)


class Stop(Exception):
    """code: 2 spend cap or deadline, 3 a routing violation, 4 the tenant state or split."""

    def __init__(self, msg, code=2):
        super().__init__(msg)
        self.code = code


def iso(t=None):
    return (t or datetime.datetime.now(datetime.timezone.utc)).strftime("%Y-%m-%dT%H:%M:%SZ")


# --- trailers ----------------------------------------------------------------------

def trailer(text):
    """kb/ask's and kb/answer's accounting line as {FIELD: value}: the last line that holds
    ROUTE: or begins with FIRST: (kb/answer called directly writes the first-call fields with
    no ROUTE). Fields are separated by ` | ` (RFC HC §2.4)."""
    lines = [l.strip() for l in (text or "").splitlines()]
    for l in reversed(lines):
        if l.startswith("ROUTE:") or l.startswith("FIRST:"):
            out = {}
            for part in l.split("|"):
                if ":" in part:
                    k, v = part.split(":", 1)
                    out[k.strip().upper()] = v.strip()
            return out
    return {}


def reranked(fields):
    """True / False from `RERANKED: true|false (<reason>)`, None when the field is absent."""
    v = (fields.get("RERANKED") or "").strip().lower()
    if v.startswith("true"):
        return True
    if v.startswith("false"):
        return False
    return None


def rerank_reason(fields):
    m = re.search(r"\(([^)]*)\)", fields.get("RERANKED") or "")
    return m.group(1).strip() if m else None


def first_call_ran(arm, fields):
    """Whether kb/answer ran in this row: always for rag-lc; for a kb arm when its route
    starts with the first call (`ROUTE: answer ...`)."""
    if arm == "rag-lc":
        return True
    return (fields.get("ROUTE") or "").split("->")[0].strip().lower() == "answer"


HC_TRAILER = re.compile(r"^\s*(ROUTE|FIRST|COVERAGE|UNCOVERED):", re.I)


def strip_trailer(text):
    """The answer without its accounting lines (kb/ask's and kb/answer's)."""
    return "\n".join(l for l in (text or "").splitlines() if not HC_TRAILER.match(l)).strip()


# --- the oracle's section block ----------------------------------------------------
#
# The oracle measures kb/read alone, so it hands kb/read exactly what kb/answer hands it (RFC HC
# §2.2 steps 3-4). kb/answer's constants (HEAD, ASK, CONFIG.section_chars / context_chars,
# MIN_TEXT) are read at runtime from its code in the bundle, so no hand copy here can drift;
# neutral / clip / where / render_block / oracle_segments are its neutral, clip, where, block
# and ask in Python.

KB_YAML = os.path.normpath(os.path.join(HERE, "..", "..", "..", "..", "bundles", "knowledge-base", "loomcycle.yaml"))


class ContractError(Exception):
    """kb/answer, or a constant the oracle copies from it, is missing from the bundle."""


_JS_STRING = r'("(?:[^"\\\n]|\\.)*")'
_CONTRACT = {
    "HEAD": re.compile(r"^\s*var HEAD = " + _JS_STRING + r";\s*$", re.M),
    "ASK": re.compile(r"^\s*var ASK = " + _JS_STRING + r";\s*$", re.M),
    "MIN_TEXT": re.compile(r"^\s*var MIN_TEXT = (\d+);\s*$", re.M),
    "CONFIG": re.compile(r"^\s*var CONFIG = \{([^}]*)\};\s*$", re.M),
}


def _agent_entry(text, name, path):
    """One agent's entry: the lines after its `name:` key up to the next line indented no deeper."""
    lines = text.split("\n")
    key = re.compile(r"^( *)" + re.escape(name) + r":\s*$")
    hits = [(i, len(m.group(1))) for i, m in ((i, key.match(l)) for i, l in enumerate(lines)) if m]
    if len(hits) != 1:
        raise ContractError("%s: %d %s entries (want exactly one)" % (path, len(hits), name))
    i, ind = hits[0]
    out = []
    for l in lines[i + 1:]:
        if l.strip() and len(l) - len(l.lstrip(" ")) <= ind:
            break
        out.append(l)
    return "\n".join(out)


def answer_contract(path=None):
    """kb/answer's block and prompt constants as {head, ask, section_chars, context_chars,
    min_text}. Raises ContractError naming whatever is missing."""
    path = path or KB_YAML
    with open(path, encoding="utf-8") as f:
        entry = _agent_entry(f.read(), "kb/answer", path)
    found, missing = {}, []
    for name, rx in _CONTRACT.items():
        m = rx.findall(entry)
        if len(m) == 1:
            found[name] = m[0]
        else:
            missing.append(name)
    if "CONFIG" in found:
        cfg = {k: int(v) for k, v in re.findall(r"(\w+)\s*:\s*(\d+)", found["CONFIG"])}
        missing += ["CONFIG.%s" % k for k in ("section_chars", "context_chars") if k not in cfg]
    if missing:
        raise ContractError("kb/answer in %s lacks %s" % (path, ", ".join(missing)))
    try:
        head, ask = json.loads(found["HEAD"]), json.loads(found["ASK"])
    except ValueError as e:
        raise ContractError("kb/answer's HEAD or ASK in %s is not a plain string literal: %s" % (path, e))
    return {"head": head, "ask": ask, "section_chars": cfg["section_chars"], "context_chars": cfg["context_chars"],
            "min_text": int(found["MIN_TEXT"])}


# kb/answer's neutral(), character for character: its control range, its invisible-character
# class, JS's \s class (spelled out: Python's \s is not defined by the same table) and a trim of
# spaces only.
_CTRL = re.compile("[\u0000-\u001f\u007f-\u009f]")
_INVISIBLE = re.compile("[­͏؜ᅟᅠ឴឵᠋-᠏​-‏‪-‮"
                        "⁠-⁯ㅤ︀-️﻿ﾠ￰-￸]")
_JS_WS = "\t\n\u000b\f\r    -     　﻿"
_JS_SPACE = re.compile("[" + _JS_WS + "]+")
_JS_TRIM = re.compile("^[" + _JS_WS + "]+|[" + _JS_WS + "]+$")


def neutral(s):
    """Control characters to spaces, invisible ones dropped, every whitespace run (line breaks
    included) to one space, <, > and & to look-alikes, leading and trailing spaces trimmed."""
    s = _INVISIBLE.sub("", _CTRL.sub(" ", "" if s is None else str(s)))
    s = _JS_SPACE.sub(" ", s)
    s = s.replace("<", "‹").replace(">", "›").replace("&", "＆")
    return re.sub(r"^ +| +$", "", s)


def _u16(s):
    return s.encode("utf-16-le", "surrogatepass")


def _jslen(s):
    """JS String.length: UTF-16 code units, so an astral character counts 2 as in kb/answer."""
    return len(_u16(s)) // 2


def clip(s, n):
    """kb/answer's clip(): s.slice(0, n - 1) + "…" over UTF-16 units; a cut may split a
    surrogate pair, as it does in JS."""
    if _jslen(s) <= n:
        return s
    return _u16(s)[:2 * max(0, n - 1)].decode("utf-16-le", "surrogatepass") + "…"


def where(s):
    """The document title (its path, else a placeholder) > the section title, each made safe."""
    return (neutral(s.get("doc_title") or s.get("doc_path") or "untitled document") + " > " +
            neutral(s.get("section_title") or "untitled section"))


def render_block(sections, contract):
    """kb/answer's block(): the HEAD line, then one row per section,
    `S<n> | <chunk id> | <document title> > <section title> | <text>`, rows joined by "\\n". The
    text is one neutral line clipped at section_chars; the whole is held to context_chars by
    cutting the last rows, and a row that would keep under min_text characters of text is left
    out. Returns (text, the sections kept, in order)."""
    first = contract["head"]
    rows, left, kept = [first], contract["context_chars"] - _jslen(first), []
    for s in sections:
        head = "S%d | %s | %s | " % (len(kept) + 1, s["chunk_id"], where(s))
        row = head + clip(neutral(s["text"]), contract["section_chars"])
        if _jslen(row) + 1 > left:
            if left - 1 - _jslen(head) < contract["min_text"]:
                break
            row = clip(row, left - 1)
        rows.append(row)
        left -= _jslen(row) + 1
        kept.append(s)
    return "\n".join(rows), kept


def oracle_segments(question, sections, contract):
    """kb/answer's ask() with a trusted question: ASK + "\\n\\nQUESTION: " + the question as
    trusted text (benchmark data, not a user's), the block as an untrusted tool_output. A section
    with no text is skipped first, as kb/answer's read step skips one. Returns (segments, the
    sections kept)."""
    text, kept = render_block([s for s in sections if neutral(s["text"])], contract)
    q = _JS_TRIM.sub("", "" if question is None else str(question))
    return [{"role": "user", "content": [
        {"type": "trusted-text", "text": contract["ask"] + "\n\nQUESTION: " + q},
        {"type": "untrusted-block", "kind": "tool_output", "text": text}]}], kept


SOURCES = re.compile(r"^\s*\**\s*SOURCES\s*\**\s*:(.*)$", re.I | re.M)


def cited_indexes(text, k):
    """The S<n> indexes on the SOURCES: line that fall in 1..k (others dropped, as kb/answer does)."""
    m = SOURCES.search(text or "")
    if not m:
        return []
    out = []
    for n in re.findall(r"\bS(\d+)\b", m.group(1)):
        n = int(n)
        if 1 <= n <= k and n not in out:
            out.append(n)
    return out


# --- the first call's searches (instrument check 3) ---------------------------------

def search_calls(events, op="search"):
    """Document op=<op> inputs among a run's replayed events."""
    out = []
    for e in events:
        if e.get("type") != "tool_call":
            continue
        d = e.get("data") or {}
        tu = d.get("tool_use") or d
        inp = tu.get("input") or {}
        if isinstance(inp, str):
            try:
                inp = json.loads(inp)
            except ValueError:
                inp = {}
        if (tu.get("name") or "").lower() == "document" and inp.get("op") == op:
            out.append(inp)
    return out


def annotation_free(calls):
    """(searches, how many asked for annotations or summaries)."""
    bad = sum(1 for c in calls if (c.get("annotations") not in (None, "", "none")) or c.get("with_summary"))
    return len(calls), bad


# --- the arms ----------------------------------------------------------------------

class SetData:
    """What the arms need of one set: its questions, the prompt text, gold sections and a
    chunk -> document map (ConditionalQA from the benchmark's ingest maps)."""

    def __init__(self, name, qs, cqa=None):
        self.name, self.qs, self.cqa = name, qs, cqa

    def prompt(self, q):
        return prompts.question_text(q) if self.name == "cqa" else q["text"]

    def gold_sections(self, q):
        """The gold sections as kb/answer reads them: the document's title, and the section's
        chunk title (ingest.py: the page title for section 0, else its heading)."""
        d = self.cqa
        picks = [(q["doc_id"], i) for i in q["gold"] if d.readable(q["doc_id"], i)][:hcconfig.K]
        out = []
        for did, i in picks:
            s = d.docs[did]["sections"][i]
            out.append({"chunk_id": d.chunkmap[did][i], "doc_title": d.docs[did]["title"],
                        "section_title": s["path"][-1] if s["path"] else d.docs[did]["title"],
                        "text": s["text"], "page": did})
        return out


def key(ctx, arm, qid, rep, attempt):
    return "%s:%s:%s:%s:%d" % (ctx["prefix"], ctx["split"], arm.replace("+", "."), safe_id(qid), rep) + (
        ":a%d" % attempt if attempt > 1 else "")


def tree(lc, run_id, usage, row):
    if not run_id:
        return None
    try:
        t = lc.tree_usage(run_id)
    except Exception as e:  # without the tree the provider guard cannot be checked
        row["status"], row["error"] = "failed", "tree_usage: %s" % str(e)[:300]
        return None
    usage.add_tree(t)
    row["tree"] = [{k: r.get(k) for k in ("run_id", "agent", "provider", "model", "status", "input_tokens",
                                          "output_tokens", "cached_tokens", "cache_creation_tokens")} for r in t["runs"]]
    row["usd"] = round(cost.tree_cost(t), 6)
    return t


def pinned_violations(tree_rows):
    """Model-backed kb/* or kbhc/* runs not served by Haiku (the benchmark's incident signature)."""
    return sorted({(r.get("agent"), r.get("provider"), r.get("model")) for r in tree_rows or []
                   if (r.get("agent") or "").startswith(hcconfig.PINNED_PREFIXES)
                   and r.get("provider") not in ("code-js", "", None)
                   and (r.get("provider"), r.get("model")) != hcconfig.HAIKU})


def inspect_first_call(lc, row):
    """Check 3's input: kb/answer's searches in this row carry no annotations / with_summary."""
    rid = next((r["run_id"] for r in row.get("tree") or [] if r.get("agent") == "kb/answer"), None)
    if not rid:
        row["first_call"] = {"run_id": None, "error": "no kb/answer run in the tree"}
        return
    try:
        ev = lc.run_events(rid)
        n, bad = annotation_free(search_calls(ev))
        # The sections it read, for "not_found while a gold section was read" (reported only).
        read = [c.get("id") for c in search_calls(ev, "get_chunk") if c.get("id")]
        row["first_call"] = {"run_id": rid, "searches": n, "with_annotation": bad, "read_chunks": read}
    except Exception as e:  # recorded, never fatal: check 3 counts it as unread
        row["first_call"] = {"run_id": rid, "error": str(e)[:200]}


def chunk_documents(lc, data, chunks):
    """chunk id -> document id: from the ingest map on ConditionalQA, else a read-only get_chunk."""
    out = {}
    for c in chunks:
        if data.cqa is not None and c in data.cqa.section_of_chunk:
            out[c] = data.cqa.docmap.get(data.cqa.section_of_chunk[c][0])
            continue
        try:
            g = lc.doc("get_chunk", scope="tenant", id=c)
            out[c] = g.get("document_id") or (g.get("chunk") or {}).get("document_id")
        except Exception:
            out[c] = None
    return out


def arm_agent(lc, data, arm, q, rep, attempt, usage, row, ctx):
    """kb-today, kb-first, kb-today+answer-first and rag-lc: one run of the arm's agent."""
    md = dict(hcconfig.METADATA[arm], bench="kbrag-hc", arm=arm, qid=q["qid"], rep=rep)
    r = lc.run(hcconfig.AGENTS[arm], data.prompt(q), metadata=md, user_tier="free",
               idempotency_key=key(ctx, arm, q["qid"], rep, attempt), timeout=2400)
    row["agent"] = hcconfig.AGENTS[arm]
    row["run_ids"] = [r.get("run_id")]
    text = r.get("text") or ""
    row["raw_answer"] = text
    fields = trailer(text)
    row["trailer"] = fields
    row["route"] = fields.get("ROUTE")
    row["first_call_ran"] = first_call_ran(arm, fields)
    row["reranked"], row["rerank_reason"] = reranked(fields), rerank_reason(fields)
    row["citations"] = common.citations(text)
    tree(lc, r.get("run_id"), usage, row)
    row["chunk_docs"] = chunk_documents(lc, data, row["citations"]["chunks"])
    if r.get("status") != "completed":
        row["status"], row["error"] = "failed", "run %s: %s" % (r.get("status"), r.get("error"))
    elif arm in hcconfig.KB_ARMS and "ROUTE" not in fields:
        row["status"], row["error"] = "failed", "no ROUTE: trailer"
    elif arm == "rag-lc" and "FIRST" not in fields:
        row["status"], row["error"] = "failed", "no FIRST: trailer field"
    if row["status"] == "ok" and row["first_call_ran"]:
        inspect_first_call(lc, row)


def arm_oracle(lc, data, arm, q, rep, attempt, usage, row, ctx):
    segs, secs = oracle_segments(data.prompt(q), data.gold_sections(q), ctx["contract"])
    r = lc.run(hcconfig.AGENTS["oracle"], segments=segs,
               metadata={"bench": "kbrag-hc", "arm": "oracle", "qid": q["qid"], "rep": rep}, user_tier="free",
               idempotency_key=key(ctx, arm, q["qid"], rep, attempt))
    row["agent"] = hcconfig.AGENTS["oracle"]
    row["run_ids"] = [r.get("run_id")]
    text = r.get("text") or ""
    row["raw_answer"] = text
    row["answer_line"] = common.answer_line(text)
    row["read_sections"] = [s["chunk_id"] for s in secs]
    row["citations"] = {"docs": [], "chunks": [secs[n - 1]["chunk_id"] for n in cited_indexes(text, len(secs))]}
    tree(lc, r.get("run_id"), usage, row)
    if r.get("status") != "completed":
        row["status"], row["error"] = "failed", "run %s: %s" % (r.get("status"), r.get("error"))


ARM_FN = {"kb-today": arm_agent, "kb-first": arm_agent, "kb-today+answer-first": arm_agent, "rag-lc": arm_agent,
          "oracle": arm_oracle}


# --- the runner --------------------------------------------------------------------

class Runner:
    def __init__(self, lc, data, *, arms, out, split, spend, workers=1, retry_failed=False, deadline=None,
                 budget=hcconfig.BUDGET_USD, key_prefix=None, state_fn=None, log=print, split_map=None,
                 contract=None, kb_yaml=None):
        self.lc, self.data, self.arms, self.split = lc, data, list(arms), split
        bad = [a for a in self.arms if a not in hcconfig.ARMS_FOR[data.name]]
        if bad:
            raise Stop("arm(s) %s do not run on set %s" % (", ".join(bad), data.name), 4)
        # The oracle copies kb/answer's block and prompt; without kb/answer in the bundle it
        # would measure some other reader input, so it stops before any run.
        self.contract = contract
        if "oracle" in self.arms and contract is None:
            try:
                self.contract = answer_contract(kb_yaml)
            except (ContractError, OSError) as e:
                raise Stop("ORACLE: %s" % e, 4)
        self.dir = os.path.join(out, data.name, "runs")
        self.prefix = key_prefix or "%s-%s" % (hcconfig.KEY_BASE, os.path.basename(os.path.abspath(out)))
        self.spend, self.budget, self.workers = spend, budget, max(1, int(workers or 1))
        self.retry_failed, self.deadline, self.log = retry_failed, deadline, log
        self.state_fn = state_fn        # () -> (digest, [problems])
        self.split_map = split_map      # qid -> "dev" | "test" (ConditionalQA only)
        self.digest = None
        self.rows = {a: common.read_jsonl(self.path(a)) for a in hcconfig.ARMS}
        self.lock = threading.RLock()
        self.inflight = {}

    def path(self, arm):
        return os.path.join(self.dir, arm + ".jsonl")

    def event(self, kind, **kw):
        common.append_jsonl(os.path.join(self.dir, "events.jsonl"), dict(kw, ts=iso(), kind=kind, split=self.split))

    def check_split(self, qids):
        """Every question must belong to the invocation's split: a test question never enters a
        tuning run, and a dev question never a scored one."""
        if self.split_map is None:
            return
        wrong = [q for q in qids if self.split_map(q) != self.split]
        if wrong:
            raise Stop("SPLIT: %d question(s) are not in the %s split, e.g. %s" % (len(wrong), self.split, wrong[:3]), 4)

    def check_state(self):
        if not self.state_fn:
            return
        digest, problems = self.state_fn()
        if problems:
            self.event("state_changed", problems=problems)
            raise Stop("TENANT STATE: %s" % "; ".join(problems), 4)
        self.digest = digest

    def todo(self, arm, qid, rep):
        mine = [r for r in self.rows[arm] if r["qid"] == qid and r["rep"] == rep]
        if not mine:
            return True, 1
        if mine[-1]["status"] == "ok":
            return False, None
        return (self.retry_failed and len(mine) < 2), len(mine) + 1

    def jobs(self, plan):
        out = []
        for qid, rep in plan:
            order = list(self.arms)
            k = random.Random("order:%s:%d" % (qid, rep)).randrange(len(order))
            for arm in order[k:] + order[:k]:
                go, attempt = self.todo(arm, qid, rep)
                if go:
                    out.append((arm, qid, rep, attempt))
        return out

    def est(self, arm):
        c = [r["usage"]["total"]["usd_est"] for r in self.rows[arm] if r["status"] == "ok"]
        return common.p90(c) if len(c) >= 5 else hcconfig.COST_FLOOR.get(arm, hcconfig.COST_FLOOR_DEFAULT)

    def guard(self, arm):
        with self.lock:
            if self.deadline and datetime.datetime.now(datetime.timezone.utc) > self.deadline:
                raise Stop("deadline %s passed" % iso(self.deadline))
            spent = self.spend.increase()
            pending = sum(self.est(a) * n for a, n in self.inflight.items())
            if spent + pending + self.est(arm) > self.budget:
                raise Stop("spend cap: increase $%.4f + in flight ~$%.4f + next %s ~$%.4f would pass $%.2f"
                           % (spent, pending, arm, self.est(arm), self.budget))
            self.inflight[arm] = self.inflight.get(arm, 0) + 1

    def ledger_guard(self, since):
        try:
            bad = self.lc.disallowed(self.lc.ledger_pairs(since=since))
        except Exception as e:  # a failed read proves nothing either way; the run trees still guard
            self.log("ledger check skipped: %s" % str(e)[:200])
            return
        if bad:
            self.event("provider_violation", source="usage ledger", served=[list(p) for p in bad])
            raise Stop("PROVIDER VIOLATION in the usage ledger since %s: %s" % (since, bad), 3)

    def one(self, arm, qid, rep, attempt):
        q = self.data.qs[qid]
        usage = Usage()
        row = {"arm": arm, "set": self.data.name, "split": self.split, "qid": qid, "rep": rep, "attempt": attempt,
               "state": self.digest, "status": "ok", "error": None, "ts": iso()}
        t0 = time.time()
        try:
            ARM_FN[arm](self.lc, self.data, arm, q, rep, attempt, usage, row,
                        {"prefix": self.prefix, "split": self.split, "contract": self.contract})
        except Stop:
            raise
        except Exception as e:  # one broken run is a failed row, never a dead runner
            row["status"], row["error"] = "failed", "%s: %s" % (type(e).__name__, str(e)[:300])
        row["wall_s"] = round(time.time() - t0, 2)
        row["usage"] = usage.to_json()
        row["served"] = [list(p) for p in usage.served()]
        bad = usage.violations() or pinned_violations(row.get("tree"))
        if bad:
            row["status"], row["error"] = "provider_violation", "served outside the study's models: %s" % bad
        with self.lock:
            common.append_jsonl(self.path(arm), row)
            self.rows[arm].append(row)
        if bad:
            self.event("provider_violation", arm=arm, qid=qid, rep=rep, served=row["served"])
            raise Stop("PROVIDER VIOLATION on %s %s rep %d: %s" % (arm, qid, rep, row["error"]), 3)
        return row

    def run(self, plan):
        started = iso()
        self.check_split(sorted({q for q, _ in plan}))
        self.check_state()
        jobs = self.jobs(plan)
        self.event("start", arms=self.arms, jobs=len(jobs), workers=self.workers, state=self.digest)
        self.version("start")
        n = {"n": 0}

        def job(j):
            arm, qid, rep, attempt = j
            self.guard(arm)
            try:
                row = self.one(arm, qid, rep, attempt)
            finally:
                with self.lock:
                    self.inflight[arm] -= 1
            with self.lock:
                n["n"] += 1
                if row["status"] != "ok":
                    self.log("%s %s rep %d failed: %s" % (arm, qid, rep, row["error"]))
                if n["n"] % 20 == 0:
                    self.log("%s %d/%d runs; study increase $%.4f" % (iso(), n["n"], len(jobs), self.spend.increase()))
                    self.ledger_guard(started)
                    self.check_state()

        try:
            common.run_jobs(jobs, job, self.workers)
            self.ledger_guard(started)
        except Stop as e:
            self.event("stop", reason=str(e), code=e.code, runs=n["n"])
            raise
        finally:
            self.version("end")
        self.event("end", runs=n["n"])
        return n["n"]

    def version(self, when):
        try:
            v = self.lc.version()
        except Exception as e:
            v = {"error": str(e)[:200]}
        common.append_jsonl(os.path.join(self.dir, "versions.jsonl"), dict(v, when=when, ts=iso()))


def plan(qids, reps, spread=None, spread_reps=hcconfig.SPREAD_REPS):
    """[(qid, rep)], rep-major, questions in a seeded order. With `spread` (a set of qids),
    reps 1..spread_reps-1 run only for those (the ConditionalQA test plan); else every
    question runs `reps` times."""
    ids = sorted(qids)
    random.Random(hcconfig.ORDER_SEED).shuffle(ids)
    if spread is not None:
        return [(q, 0) for q in ids] + [(q, r) for r in range(1, spread_reps) for q in ids if q in spread]
    return [(q, r) for r in range(reps) for q in ids]


def load_set(name, work_dir):
    if name == "cqa":
        d = os.path.join(work_dir, "cqa")
        data = common.Data(prepared=os.path.join(d, "prepared"), docmap=os.path.join(d, "docmap.json"),
                           chunkmap=os.path.join(d, "chunks.json"), subset=hcconfig.SUBSET)
        if not data.docmap or not data.chunkmap:
            raise SystemExit("docmap.json / chunks.json are missing under %s (the benchmark's ingest maps)" % d)
        return SetData("cqa", data.qs, data)
    from docs import questions
    qs = {}
    for q in questions.p7():
        if q["gold"] or q["gold_topics"]:
            qs[q["id"]] = dict(q, qid=q["id"])
    return SetData("p7", qs)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--set", required=True, choices=hcconfig.SETS)
    ap.add_argument("--split", choices=("test", "dev"), default="test",
                    help="ConditionalQA only: test (the 800) or dev (tuning); the walker set is test")
    ap.add_argument("--arms", help="comma-separated (default: every arm of the set)")
    ap.add_argument("--out", required=True, help="output root (outside the repo)")
    ap.add_argument("--qids")
    ap.add_argument("--reps", type=int, help="runs per question (default: the plan of the set and split)")
    ap.add_argument("--workers", type=int, default=4)
    ap.add_argument("--retry-failed", action="store_true")
    ap.add_argument("--budget-usd", type=float, default=hcconfig.BUDGET_USD)
    ap.add_argument("--deadline")
    ap.add_argument("--key-prefix")
    a = ap.parse_args()
    import spendguard
    work = hcconfig.work()
    clients = spendguard.all_clients()
    for c in clients.values():
        c.assert_tenant()
    lc = clients[a.set]
    data = load_set(a.set, work)
    split = a.split if a.set == "cqa" else "test"
    sp = splits.load()
    split_map = (lambda q: splits.which(sp, q)) if a.set == "cqa" else None
    if a.qids:
        qids = a.qids.split(",")
    elif a.set == "cqa":
        qids = list(sp[split]) if split == "dev" else list(data.cqa.subset)
    else:
        qids = sorted(data.qs)
    if a.set == "cqa" and split == "test" and a.reps is None:
        pl = plan(qids, 1, spread=common.spread_sample(data.cqa.subset))
    else:
        pl = plan(qids, a.reps or (hcconfig.P7_REPS if a.set == "p7" else 1))
    recorded = tenant_state.load(a.set)

    def state_fn():
        now = tenant_state.current(lc)
        return tenant_state.digest(now), tenant_state.problems(recorded, now)

    deadline = datetime.datetime.fromisoformat(a.deadline.replace("Z", "+00:00")) if a.deadline else None
    r = Runner(lc, data, arms=[x for x in (a.arms or ",".join(hcconfig.ARMS_FOR[a.set])).split(",") if x],
               out=a.out, split=split, spend=spendguard.Spend(clients, spendguard.load_baseline()),
               workers=a.workers, retry_failed=a.retry_failed, deadline=deadline, budget=a.budget_usd,
               key_prefix=a.key_prefix, state_fn=state_fn, log=lambda s: print(s, flush=True), split_map=split_map)
    try:
        n = r.run(pl)
    except Stop as e:
        print("STOPPED: %s" % e, flush=True)
        sys.exit(e.code)
    print("done: %d runs" % n, flush=True)


if __name__ == "__main__":
    main()
