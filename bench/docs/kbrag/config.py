"""Model profiles, the provider policy and the working paths for Study C (ConditionalQA).

Models are configuration: every runner takes `--profile haiku|local`, and every output
row records the profile and the (provider, model) that actually served each call. A study
never mixes profiles across arms (analyze.py refuses one that does).
"""
import os

PROFILES = {
    # Operator policy 2026-11-22: Claude Haiku through the deployment under a hard $20 cap
    # for the whole study; the local profile is the fallback if Haiku is too dear.
    "haiku": {"reader": ("anthropic", "claude-haiku-5-5")},
    "local": {"reader": ("ollama-local", "qwen3.6:latest")},
}
DEFAULT_PROFILE = "haiku"
# The judge is free and differs from the reader in both profiles (PREREG: never the reader).
JUDGE = ("ollama-local", "ornith-1.5:35b")

# Serving pairs the study may use; "*" = any model of that provider. The embedder is not a
# chat call and is checked by its own path (lc.embed), so it is not listed here.
ALLOWED = {("anthropic", "claude-haiku-5-5"), ("ollama-local", "*")}
# A code-js run has no model of its own; its children and its usage rows are what serve.
NO_MODEL_PROVIDERS = {"", None, "code-js"}

# USD per million tokens, used only to ESTIMATE a row's cost (the ledger, lc.spend(), is
# the truth the spend guard reads). Same list price lc.py prices unpriced Haiku calls at.
# Claude Haiku 5.5 list price, prompts up to 100K tokens: $0.10 in / $0.50 out per million,
# cache read $0.01, cache write $0.125 -- the deployment's ledger prices it identically
# (checked on the live smoke, 2026-11-23). The earlier $1/$5 here was Haiku 4.5's price.
PRICE = {("anthropic", "claude-haiku-5-5"): {"input": 0.10, "output": 0.50, "cached": 0.01, "cache_write": 0.125}}
# Seed for the spend guard before an arm has rows with a known cost.
COST_FLOOR = {"kb": 0.05}
COST_FLOOR_DEFAULT = 0.01
BUDGET_USD = 19.0

HERE = os.path.dirname(os.path.abspath(__file__))
WORK = os.environ.get("KBRAG_CQA_WORK", "/home/denn/workspace/kbrag-work/cqa")
PREPARED = os.path.join(WORK, "prepared")
DOCMAP = os.path.join(WORK, "docmap.json")
CHUNKMAP = os.path.join(WORK, "chunks.json")
RUNS = os.path.join(WORK, "runs")
CONDENSED = os.path.join(WORK, "condensed")
JUDGED = os.path.join(WORK, "judge")
RAGEXT = os.path.join(WORK, "ragext")
SUBSET = os.path.join(HERE, "..", "measure", "reader_subset.json")

ARMS = ("kb", "rag-lc", "rag-ext", "oracle")
K = 5                       # sections the reader sees (PREREG, Study C)
SPREAD_SEED = 20261122      # the fixed 100-question spread sample
SPREAD_N = 100
SPREAD_REPS = 3             # rep 0 for every question, reps 1-2 for the spread sample
RAGLC_AGENT = "kbrag/search-rr"
RAGLC_LIMIT = 20
# rag-ext rerank strategy: "listwise" (one reader call) or "none". The pre-registered
# cross-encoder does not exist on the deployment; this is an amendment (ARMS.md).
RAGEXT_RERANK = "listwise"


def reader(profile):
    if profile not in PROFILES:
        raise SystemExit("unknown profile %r (choose from %s)" % (profile, ", ".join(sorted(PROFILES))))
    return PROFILES[profile]["reader"]


def allowed(provider, model):
    if provider in NO_MODEL_PROVIDERS:
        return True
    return (provider, model) in ALLOWED or (provider, "*") in ALLOWED


def price(provider, model, usage):
    """Estimated USD of one usage dict ({input_tokens, output_tokens, cached_tokens})."""
    p = PRICE.get((provider, model))
    if p is None:
        return 0.0
    return (usage.get("input_tokens", 0) * p["input"] + usage.get("output_tokens", 0) * p["output"]
            + usage.get("cached_tokens", 0) * p["cached"]
            + usage.get("cache_creation_tokens", 0) * p["cache_write"]) / 1e6
